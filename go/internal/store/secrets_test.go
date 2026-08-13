package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/secret"
)

func testSecretKey(t *testing.T) secret.Key {
	t.Helper()
	b64, err := secret.Generate()
	if err != nil {
		t.Fatal(err)
	}
	k, err := secret.Parse(b64)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// openAt открывает базу по конкретному пути — так один и тот же файл можно
// переоткрыть с другим ключом (проверка ротации и потери ключа).
func openAt(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	st, err := Open(context.Background(), path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// rawCookies читает колонку в обход расшифровки — только так видно, что на
// диске действительно лежит шифротекст.
func rawCookies(t *testing.T, st *Store, messenger string, userID int64) string {
	t.Helper()
	var raw string
	if err := st.db.QueryRowContext(context.Background(),
		`SELECT cookies FROM sessions WHERE messenger = ? AND user_id = ?`,
		messenger, userID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSessionCookiesEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openAt(t, filepath.Join(t.TempDir(), "test.db"), WithSecret(testSecretKey(t)))
	cookies := `[{"name":"ngs_ttq","value":"SESSION"}]`

	if err := st.UpsertSession(ctx, MessengerTelegram, 42, cookies, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw := rawCookies(t, st, MessengerTelegram, 42)
	if !secret.Encrypted(raw) {
		t.Fatalf("на диске не шифротекст: %q", raw)
	}
	if strings.Contains(raw, "SESSION") {
		t.Fatal("значение куки видно на диске")
	}

	got, valid, err := st.SessionCookies(ctx, MessengerTelegram, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !valid || got != cookies {
		t.Fatalf("прочитано %q (valid=%v), ожидалось %q", got, valid, cookies)
	}
}

// Без ключа поведение прежнее: куки лежат открыто. Это ветка совместимости —
// на неё опирается откат бинарника и работа до включения шифрования.
func TestSessionCookiesWithoutKeyStaysPlain(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	cookies := `[{"name":"sid","value":"abc"}]`
	if err := st.UpsertSession(ctx, MessengerTelegram, 7, cookies, time.Now()); err != nil {
		t.Fatal(err)
	}
	if raw := rawCookies(t, st, MessengerTelegram, 7); raw != cookies {
		t.Fatalf("на диске %q, ожидался открытый JSON", raw)
	}
	got, _, err := st.SessionCookies(ctx, MessengerTelegram, 7)
	if err != nil || got != cookies {
		t.Fatalf("прочитано %q, err=%v", got, err)
	}
}

// Записи, сделанные до включения шифрования, должны читаться и после.
func TestSessionCookiesReadsLegacyPlaintext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	cookies := `[{"name":"sid","value":"старая"}]`

	plainStore := openAt(t, path)
	if err := plainStore.UpsertSession(ctx, MessengerTelegram, 42, cookies, time.Now()); err != nil {
		t.Fatal(err)
	}
	plainStore.Close()

	withKey := openAt(t, path, WithSecret(testSecretKey(t)))
	got, _, err := withKey.SessionCookies(ctx, MessengerTelegram, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != cookies {
		t.Fatalf("прочитано %q, ожидалось %q", got, cookies)
	}
}

// Ключ потеряли — это должно быть громкой ошибкой, а не пустыми куками:
// иначе демон молча попросит всех перелогиниться.
func TestSessionCookiesWithoutKeyOverEncrypted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	enc := openAt(t, path, WithSecret(testSecretKey(t)))
	if err := enc.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"sid"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	enc.Close()

	_, _, err := openAt(t, path).SessionCookies(ctx, MessengerTelegram, 42)
	if !errors.Is(err, secret.ErrNoKey) {
		t.Fatalf("ожидался ErrNoKey, получено: %v", err)
	}
}

// Шифротекст, переставленный в чужую строку, читаться не должен: AAD связывает
// запись с её мессенджером и пользователем.
func TestSessionCookiesRejectsMovedRow(t *testing.T) {
	ctx := context.Background()
	st := openAt(t, filepath.Join(t.TempDir(), "test.db"), WithSecret(testSecretKey(t)))
	if err := st.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"sid"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	stolen := rawCookies(t, st, MessengerTelegram, 42)
	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO sessions (messenger, user_id, cookies, valid, updated_at)
		VALUES (?, ?, ?, 1, ?)`, MessengerTelegram, 43, stolen, fmtTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionCookies(ctx, MessengerTelegram, 43); err == nil {
		t.Fatal("чужая строка расшифровалась")
	}
}

func TestReencryptSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	cookies := `[{"name":"sid","value":"abc"}]`

	plainStore := openAt(t, path)
	for _, uid := range []int64{1, 2} {
		if err := plainStore.UpsertSession(ctx, MessengerTelegram, uid, cookies, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	plainStore.Close()

	key := testSecretKey(t)
	st := openAt(t, path, WithSecret(key))
	stats, err := st.SessionSecretStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Plain != 2 || stats.Encrypted != 0 {
		t.Fatalf("до перешивки: %+v", stats)
	}

	n, err := st.ReencryptSessions(ctx, secret.Key{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("перешито %d строк, ожидалось 2", n)
	}
	if stats, err = st.SessionSecretStats(ctx); err != nil || stats.Encrypted != 2 || stats.Plain != 0 {
		t.Fatalf("после перешивки: %+v (err=%v)", stats, err)
	}
	if got, _, err := st.SessionCookies(ctx, MessengerTelegram, 1); err != nil || got != cookies {
		t.Fatalf("после перешивки прочитано %q, err=%v", got, err)
	}

	// Идемпотентность: повтор после сбоя ничего не должен трогать.
	if n, err = st.ReencryptSessions(ctx, secret.Key{}); err != nil || n != 0 {
		t.Fatalf("повтор изменил %d строк, err=%v", n, err)
	}
	st.Close()

	// Ротация: новый ключ + старый через параметр.
	rotated := openAt(t, path, WithSecret(testSecretKey(t)))
	if stats, err = rotated.SessionSecretStats(ctx); err != nil || stats.Unreadable != 2 {
		t.Fatalf("новым ключом должно не читаться: %+v (err=%v)", stats, err)
	}
	if n, err = rotated.ReencryptSessions(ctx, key); err != nil || n != 2 {
		t.Fatalf("ротация: перешито %d, err=%v", n, err)
	}
	if got, _, err := rotated.SessionCookies(ctx, MessengerTelegram, 2); err != nil || got != cookies {
		t.Fatalf("после ротации прочитано %q, err=%v", got, err)
	}
}

// Строку, которую не открыть ни одним ключом, пропускать нельзя: это потеря
// сессии на следующей ротации.
func TestReencryptSessionsFailsOnUnreadable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	foreign := openAt(t, path, WithSecret(testSecretKey(t)))
	if err := foreign.UpsertSession(ctx, MessengerTelegram, 1, `[{"name":"sid"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	foreign.Close()

	st := openAt(t, path, WithSecret(testSecretKey(t)))
	if _, err := st.ReencryptSessions(ctx, secret.Key{}); err == nil {
		t.Fatal("перешивка не заметила нечитаемую строку")
	}
}

func TestReencryptSessionsNeedsKey(t *testing.T) {
	if _, err := openTest(t).ReencryptSessions(context.Background(), secret.Key{}); err == nil {
		t.Fatal("перешивка без ключа должна отказывать")
	}
}
