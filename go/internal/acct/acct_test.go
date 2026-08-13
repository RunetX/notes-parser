package acct

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/secret"
)

func testKey(t *testing.T) secret.Key {
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

func openAt(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTest(t *testing.T, opts ...Option) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "accounts.db"), opts...)
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	cookies := `[{"name":"ngs_ttq","value":"SESSION"}]`

	a := Account{Name: "reserve", ProfileID: "1472546", PassportID: "999", Nick: "Ник", Note: "резерв"}
	if err := s.Put(ctx, a, cookies, now); err != nil {
		t.Fatal(err)
	}
	got, gotCookies, err := s.Get(ctx, "reserve")
	if err != nil {
		t.Fatal(err)
	}
	if gotCookies != cookies {
		t.Fatalf("куки %q, ожидались %q", gotCookies, cookies)
	}
	if got.Nick != "Ник" || got.ProfileID != "1472546" || got.Note != "резерв" || !got.Valid {
		t.Fatalf("аккаунт прочитан неверно: %+v", got)
	}
	if got.UpdatedAt.IsZero() || !got.LastOKAt.IsZero() {
		t.Fatalf("времена: updated=%v last_ok=%v", got.UpdatedAt, got.LastOKAt)
	}
	if got.Title() != "reserve (Ник, u1472546)" {
		t.Fatalf("Title() = %q", got.Title())
	}
}

func TestGetNotFound(t *testing.T) {
	_, _, err := openTest(t).Get(context.Background(), "нет")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ожидался ErrNotFound, получено: %v", err)
	}
}

// Повторный вход обновляет куки и оживляет сессию, но не теряет заметку и дату
// заведения: заметку пишут один раз, а входят регулярно.
func TestPutKeepsNoteAndCreatedAt(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	first := time.Now().Add(-48 * time.Hour)
	if err := s.Put(ctx, Account{Name: "reserve", Note: "резерв"}, `["старые"]`, first); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValid(ctx, "reserve", false, first); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, Account{Name: "reserve", Nick: "Новый"}, `["свежие"]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, cookies, err := s.Get(ctx, "reserve")
	if err != nil {
		t.Fatal(err)
	}
	if cookies != `["свежие"]` {
		t.Fatalf("куки не обновились: %q", cookies)
	}
	if got.Note != "резерв" {
		t.Fatalf("заметка затёрта: %q", got.Note)
	}
	if !got.CreatedAt.Equal(first.UTC().Truncate(time.Second)) {
		t.Fatalf("created_at = %v, ожидалось %v", got.CreatedAt, first.UTC())
	}
	if !got.Valid {
		t.Fatal("повторный вход не оживил сессию")
	}
}

func TestListAndForget(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, name := range []string{"reserve", "main"} {
		if err := s.Put(ctx, Account{Name: name}, `["куки"]`, now); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "main" || list[1].Name != "reserve" {
		t.Fatalf("список: %+v", list)
	}

	ok, err := s.Forget(ctx, "main")
	if err != nil || !ok {
		t.Fatalf("удаление: ok=%v err=%v", ok, err)
	}
	if ok, err = s.Forget(ctx, "main"); err != nil || ok {
		t.Fatalf("повторное удаление: ok=%v err=%v", ok, err)
	}
	if list, err = s.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("после удаления: %+v (err=%v)", list, err)
	}
}

func TestSetValidAndIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	if err := s.Put(ctx, Account{Name: "reserve", Nick: "Старый"}, `["куки"]`, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValid(ctx, "reserve", true, now); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get(ctx, "reserve")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOKAt.IsZero() {
		t.Fatal("успешная проверка не отметилась в last_ok_at")
	}

	if err := s.SetValid(ctx, "reserve", false, now); err != nil {
		t.Fatal(err)
	}
	if got, _, err = s.Get(ctx, "reserve"); err != nil || got.Valid {
		t.Fatalf("сессия осталась валидной: %+v (err=%v)", got, err)
	}
	if err := s.SetValid(ctx, "нет", true, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetValid по несуществующему имени: %v", err)
	}

	if err := s.SetIdentity(ctx, "reserve", "123", "456", "Новый"); err != nil {
		t.Fatal(err)
	}
	if got, _, err = s.Get(ctx, "reserve"); err != nil || got.Nick != "Новый" || got.ProfileID != "123" {
		t.Fatalf("идентичность не обновилась: %+v (err=%v)", got, err)
	}
}

func TestCookiesEncryptedOnDisk(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, WithSecret(testKey(t)))
	if err := s.Put(ctx, Account{Name: "reserve"}, `[{"value":"SESSION"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := s.rawCookies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !secret.Encrypted(raw["reserve"]) || strings.Contains(raw["reserve"], "SESSION") {
		t.Fatalf("на диске не шифротекст: %q", raw["reserve"])
	}
	_, cookies, err := s.Get(ctx, "reserve")
	if err != nil || cookies != `[{"value":"SESSION"}]` {
		t.Fatalf("расшифровка: %q err=%v", cookies, err)
	}
}

func TestGetWithoutKeyOverEncrypted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "accounts.db")

	enc := openAt(t, path, WithSecret(testKey(t)))
	if err := enc.Put(ctx, Account{Name: "reserve"}, `["куки"]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	enc.Close()

	if _, _, err := openAt(t, path).Get(ctx, "reserve"); !errors.Is(err, secret.ErrNoKey) {
		t.Fatalf("ожидался ErrNoKey, получено: %v", err)
	}
}

func TestReencryptAndRotate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "accounts.db")

	plain := openAt(t, path)
	if err := plain.Put(ctx, Account{Name: "reserve"}, `["куки"]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	plain.Close()

	key := testKey(t)
	s := openAt(t, path, WithSecret(key))
	if st, err := s.SecretStatsOf(ctx); err != nil || st.Plain != 1 {
		t.Fatalf("до перешивки: %+v (err=%v)", st, err)
	}
	n, err := s.Reencrypt(ctx, secret.Key{})
	if err != nil || n != 1 {
		t.Fatalf("перешивка: n=%d err=%v", n, err)
	}
	if n, err = s.Reencrypt(ctx, secret.Key{}); err != nil || n != 0 {
		t.Fatalf("повтор изменил %d строк, err=%v", n, err)
	}
	if st, err := s.SecretStatsOf(ctx); err != nil || st.Encrypted != 1 {
		t.Fatalf("после перешивки: %+v (err=%v)", st, err)
	}
	s.Close()

	rotated := openAt(t, path, WithSecret(testKey(t)))
	if st, err := rotated.SecretStatsOf(ctx); err != nil || st.Unreadable != 1 {
		t.Fatalf("новым ключом должно не читаться: %+v (err=%v)", st, err)
	}
	if n, err = rotated.Reencrypt(ctx, key); err != nil || n != 1 {
		t.Fatalf("ротация: n=%d err=%v", n, err)
	}
	if _, cookies, err := rotated.Get(ctx, "reserve"); err != nil || cookies != `["куки"]` {
		t.Fatalf("после ротации: %q err=%v", cookies, err)
	}
}

func TestReencryptNeedsKey(t *testing.T) {
	if _, err := openTest(t).Reencrypt(context.Background(), secret.Key{}); err == nil {
		t.Fatal("перешивка без ключа должна отказывать")
	}
}
