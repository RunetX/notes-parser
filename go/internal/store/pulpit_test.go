package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPulpitClaimOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	ok, err := st.TryClaimPulpitNote(ctx, "312900", PulpitQueued, "", now)
	if err != nil || !ok {
		t.Fatalf("первый claim: %v %v", ok, err)
	}
	ok, err = st.TryClaimPulpitNote(ctx, "312900", PulpitQueued, "", now)
	if err != nil || ok {
		t.Fatalf("повторный claim должен провалиться: %v %v", ok, err)
	}
}

// TestPulpitClaimRace — два входа (свой обход ленты и колбэк зеркала) видят
// заметку одновременно; занять её должен ровно один.
func TestPulpitClaimRace(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.TryClaimPulpitNote(ctx, "312901", PulpitQueued, "", time.Now())
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("claim выиграли %d горутин, ожидалась одна", won)
	}
}

// TestPulpitPostOnce — точка невозврата одна: второй TryStartPulpitPost по той
// же заметке отправку не разрешает (дубль комментария необратим).
func TestPulpitPostOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()
	if _, err := st.TryClaimPulpitNote(ctx, "n1", PulpitQueued, "", now); err != nil {
		t.Fatal(err)
	}

	ok, err := st.TryStartPulpitPost(ctx, "n1", "укор", "текст", now)
	if err != nil || !ok {
		t.Fatalf("первый переход: %v %v", ok, err)
	}
	ok, err = st.TryStartPulpitPost(ctx, "n1", "укор", "текст", now)
	if err != nil || ok {
		t.Fatalf("второй переход должен провалиться: %v %v", ok, err)
	}

	row, err := st.PulpitNote(ctx, "n1")
	if err != nil || row.State != PulpitPosting || row.Text != "текст" || row.Form != "укор" {
		t.Fatalf("строка после старта отправки: %+v %v", row, err)
	}
	if row.PostedAt.IsZero() {
		t.Error("posted_at не проставлен")
	}
}

func TestPulpitConfirmAndChecks(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()
	if _, err := st.TryClaimPulpitNote(ctx, "n1", PulpitQueued, "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryStartPulpitPost(ctx, "n1", "притча", "текст", now); err != nil {
		t.Fatal(err)
	}

	checks, err := st.BumpPulpitCheck(ctx, "n1", now)
	if err != nil || checks != 1 {
		t.Fatalf("первая проверка: %d %v", checks, err)
	}
	checks, err = st.BumpPulpitCheck(ctx, "n1", now)
	if err != nil || checks != 2 {
		t.Fatalf("вторая проверка: %d %v", checks, err)
	}

	if err := st.ConfirmPulpitComment(ctx, "n1", 555, now); err != nil {
		t.Fatal(err)
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil || row.State != PulpitConfirmed || row.CommentID != 555 {
		t.Fatalf("подтверждение: %+v %v", row, err)
	}

	confirmed, err := st.PulpitConfirmedSince(ctx, now.Add(-time.Hour))
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("подтверждённые за час: %d %v", len(confirmed), err)
	}
}

func TestPulpitCASState(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()
	if _, err := st.TryClaimPulpitNote(ctx, "n1", PulpitQueued, "", now); err != nil {
		t.Fatal(err)
	}

	ok, err := st.CASPulpitState(ctx, "n1", PulpitQueued, PulpitSkipped, "stale")
	if err != nil || !ok {
		t.Fatalf("переход queued→skipped: %v %v", ok, err)
	}
	ok, err = st.CASPulpitState(ctx, "n1", PulpitQueued, PulpitPosting, "")
	if err != nil || ok {
		t.Fatalf("переход из чужого состояния должен провалиться: %v %v", ok, err)
	}
}

func TestPulpitStatsAndSentSince(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()
	day := now.Add(-24 * time.Hour)

	// Старая (за пределами суток) и свежая реплики.
	if _, err := st.TryClaimPulpitNote(ctx, "old", PulpitQueued, "", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryStartPulpitPost(ctx, "old", "укор", "старая", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryClaimPulpitNote(ctx, "new", PulpitQueued, "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryStartPulpitPost(ctx, "new", "притча", "свежая", now); err != nil {
		t.Fatal(err)
	}
	// Пропущенная заметка в счёт не идёт: POST по ней не было.
	if _, err := st.TryClaimPulpitNote(ctx, "skip", PulpitSkipped, "cold_start", now); err != nil {
		t.Fatal(err)
	}

	total, dayCount, last, err := st.PulpitStats(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || dayCount != 1 {
		t.Fatalf("статистика: всего %d, за сутки %d", total, dayCount)
	}
	if last.NoteID != "new" || last.Text != "свежая" {
		t.Fatalf("последняя реплика: %+v", last)
	}
}

func TestPulpitReplyDecidedOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()
	r := PulpitReply{ReplyToID: 42, NoteID: "n1", AuthorID: "u7", State: PulpitQueued, DecidedAt: now}

	ok, err := st.TryDecideReply(ctx, r)
	if err != nil || !ok {
		t.Fatalf("первое решение: %v %v", ok, err)
	}
	// Монетка бросается один раз: второе решение по той же реплике не пишется,
	// даже если оно противоположное.
	r.State, r.Reason = PulpitSkipped, "coin"
	ok, err = st.TryDecideReply(ctx, r)
	if err != nil || ok {
		t.Fatalf("повторное решение должно игнорироваться: %v %v", ok, err)
	}

	replies, err := st.PulpitRepliesByNote(ctx, "n1")
	if err != nil || len(replies) != 1 || replies[0].State != PulpitQueued {
		t.Fatalf("решения заметки: %+v %v", replies, err)
	}

	ok, err = st.TryStartPulpitReply(ctx, 42, "ответ", now)
	if err != nil || !ok {
		t.Fatalf("старт отправки ответа: %v %v", ok, err)
	}
	ok, err = st.TryStartPulpitReply(ctx, 42, "ответ", now)
	if err != nil || ok {
		t.Fatalf("повторная отправка ответа должна провалиться: %v %v", ok, err)
	}
	n, err := st.PulpitReplySentSince(ctx, now.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("ответов за час: %d %v", n, err)
	}
}

func TestFlags(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if _, found, err := st.Flag(ctx, FlagPulpitEnabled); err != nil || found {
		t.Fatalf("флага быть не должно: found=%v err=%v", found, err)
	}
	if err := st.SetFlag(ctx, FlagPulpitEnabled, "1", "admin:1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFlag(ctx, FlagPulpitEnabled, "0", "fuse", time.Now()); err != nil {
		t.Fatal(err)
	}
	v, found, err := st.Flag(ctx, FlagPulpitEnabled)
	if err != nil || !found || v != "0" {
		t.Fatalf("флаг после перезаписи: %q %v %v", v, found, err)
	}
}

func TestSessionForProfile(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	old := time.Now().Add(-time.Hour)
	now := time.Now()

	if err := st.UpsertSession(ctx, MessengerTelegram, 1, `[]`, old); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionIdentity(ctx, MessengerTelegram, 1, "1472546", "p1", "Монах"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, MessengerMax, 2, `[]`, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionIdentity(ctx, MessengerMax, 2, "1472546", "p1", "Рантье"); err != nil {
		t.Fatal(err)
	}

	m, id, err := st.SessionForProfile(ctx, "1472546")
	if err != nil || m != MessengerMax || id != 2 {
		t.Fatalf("свежая сессия анкеты: %s/%d %v", m, id, err)
	}

	// Протухшая сессия не годится: под ней ничего не отправить.
	if err := st.SetSessionValid(ctx, MessengerMax, 2, false, now); err != nil {
		t.Fatal(err)
	}
	m, id, err = st.SessionForProfile(ctx, "1472546")
	if err != nil || m != MessengerTelegram || id != 1 {
		t.Fatalf("запасная сессия анкеты: %s/%d %v", m, id, err)
	}

	if _, _, err := st.SessionForProfile(ctx, "999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("сессии чужой анкеты быть не должно: %v", err)
	}
}

func TestOwnerComments(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "t", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rows := []Comment{
		{ID: 1, NoteID: "n1", AuthorName: "Я", AuthorLink: "https://love.ngs.ru/profile/1472546/", Text: "своя реплика подлиннее", CreatedAt: time.Now()},
		{ID: 2, NoteID: "n1", AuthorName: "Я", AuthorLink: "https://love.ngs.ru/profile/1472546/", Text: "ок", CreatedAt: time.Now()},
		{ID: 3, NoteID: "n1", AuthorName: "Чужой", AuthorLink: "https://love.ngs.ru/profile/999/", Text: "чужая реплика подлиннее", CreatedAt: time.Now()},
	}
	for _, c := range rows {
		if _, err := st.InsertComment(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	texts, err := st.OwnerComments(ctx, "1472546", 5, 400, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 1 || texts[0] != "своя реплика подлиннее" {
		t.Fatalf("свои комментарии: %q", texts)
	}
}

// TestMigrateV9Additive — v9 только добавляет таблицы: данные v8 на месте,
// откат бинарника безопасен.
func TestMigrateV9Additive(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	var version int
	if err := st.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion() {
		t.Fatalf("версия схемы %d, ожидалась %d", version, schemaVersion())
	}
	for _, table := range []string{"pulpit_comments", "pulpit_replies", "settings"} {
		var name string
		if err := st.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("таблица %s: %v", table, err)
		}
	}
}
