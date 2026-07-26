package mirror

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// --- фейки ---

type fakeSite struct {
	notes    []love.Note
	notesErr error
	comments map[string][]love.Comment
	avatar   []byte
}

func (f *fakeSite) FetchNotes(context.Context) ([]love.Note, error) { return f.notes, f.notesErr }
func (f *fakeSite) FetchComments(_ context.Context, id string) ([]love.Comment, error) {
	return f.comments[id], nil
}
func (f *fakeSite) FetchMedia(context.Context, string) ([]byte, error) { return f.avatar, nil }

type sinkCall struct {
	kind   string
	noteID string
	comID  int64
	userID int64
}

type fakeSink struct {
	name   string
	calls  []sinkCall
	nextID int64
}

func (f *fakeSink) Name() string {
	if f.name == "" {
		return store.MessengerTelegram
	}
	return f.name
}

func (f *fakeSink) id() string { return strconv.FormatInt(f.nextID, 10) }

func (f *fakeSink) PostNote(_ context.Context, n store.Note, _ []byte) (string, error) {
	f.nextID++
	f.calls = append(f.calls, sinkCall{kind: "note", noteID: n.ID})
	return f.id(), nil
}

func (f *fakeSink) PostComment(_ context.Context, n store.Note, _ string, c store.Comment, _ []byte) (string, error) {
	f.nextID++
	f.calls = append(f.calls, sinkCall{kind: "comment", noteID: n.ID, comID: c.ID})
	return f.id(), nil
}

func (f *fakeSink) PostNoteImage(_ context.Context, _ string, imageURL string, _ []byte) (string, error) {
	f.nextID++
	f.calls = append(f.calls, sinkCall{kind: "image", noteID: imageURL})
	return f.id(), nil
}

func (f *fakeSink) NotifySubscriber(_ context.Context, userID int64, n store.Note, c store.Comment, _, _ string) error {
	f.calls = append(f.calls, sinkCall{kind: "notify", noteID: n.ID, comID: c.ID, userID: userID})
	return nil
}

func newTestMirror(t *testing.T, site *fakeSite, sink *fakeSink, seed bool) (*Mirror, *store.Store) {
	return newTestMirrorAlert(t, site, sink, seed, nil)
}

func newTestMirrorAlert(t *testing.T, site *fakeSite, sink *fakeSink, seed bool,
	alertSend func(ctx context.Context, text string)) (*Mirror, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, site, []Sink{sink}, Config{
		NotesLimit:   5,
		FeedInterval: time.Minute,
		SeedFirst:    seed,
		AlertSend:    alertSend,
	}, slog.Default())
	return m, st
}

// --- тесты ---

func TestFeedCycleSeedDoesNotPost(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", Text: "т1"}, {ID: "n2", Text: "т2"}}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, true)

	if !m.feedCycle(ctx, true) {
		t.Fatal("feedCycle должен вернуть true")
	}
	if len(sink.calls) != 0 {
		t.Errorf("seed-режим не должен постить: %v", sink.calls)
	}
	seeded, err := st.NotesByStatus(ctx, store.StatusSeeded)
	if err != nil || len(seeded) != 2 {
		t.Errorf("seeded-заметок: %d, err %v", len(seeded), err)
	}
}

func TestFeedCyclePostsNewNotesOldestFirst(t *testing.T) {
	ctx := context.Background()
	// Лента: новые сверху (n3 новее n2 новее n1).
	site := &fakeSite{notes: []love.Note{{ID: "n3", Text: "т"}, {ID: "n2", Text: "т"}, {ID: "n1", Text: "т"}}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)

	m.feedCycle(ctx, false)

	if len(sink.calls) != 3 {
		t.Fatalf("постов: %d", len(sink.calls))
	}
	// Старые постятся первыми.
	if sink.calls[0].noteID != "n1" || sink.calls[2].noteID != "n3" {
		t.Errorf("порядок постинга: %v", sink.calls)
	}
	posted, _ := st.NotesByStatus(ctx, store.StatusPosted)
	if len(posted) != 3 {
		t.Errorf("posted-заметок: %d", len(posted))
	}
	// Повторный цикл ничего не постит.
	sink.calls = nil
	m.feedCycle(ctx, false)
	if len(sink.calls) != 0 {
		t.Errorf("повторный цикл не должен постить: %v", sink.calls)
	}
}

func TestPollCommentsQueuedUntilThreadCaptured(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			// Страница desc: новый (2) раньше старого (1).
			"n1": {{ID: 2, AuthorName: "Б", Text: "второй"}, {ID: 1, AuthorName: "А", Text: "первый"}},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false) // постит n1, tgID=1

	n, _ := st.NoteByID(ctx, "n1")
	if n.TGThreadID != 0 {
		t.Fatal("тред ещё не должен быть пойман")
	}

	// Тред не пойман: комментарии сохраняются, но не отправляются.
	m.pollComments(ctx, n)
	for _, c := range sink.calls {
		if c.kind == "comment" {
			t.Fatalf("комментарий отправлен до захвата треда: %v", sink.calls)
		}
	}
	ids, _ := st.CommentIDs(ctx, "n1")
	if len(ids) != 2 {
		t.Fatalf("комментарии не сохранены: %v", ids)
	}

	// Автофорвард пойман — застрявшие уходят от старых к новым.
	noteID, ok, err := st.CaptureNoteThread(ctx, store.MessengerTelegram,
		strconv.FormatInt(n.TGMessageID, 10), "777")
	if err != nil || !ok || noteID != "n1" {
		t.Fatalf("захват треда: %q %v %v", noteID, ok, err)
	}
	n, _ = st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	var sent []int64
	for _, c := range sink.calls {
		if c.kind == "comment" {
			sent = append(sent, c.comID)
		}
	}
	if len(sent) != 2 || sent[0] != 1 || sent[1] != 2 {
		t.Errorf("порядок отправки: %v", sent)
	}

	// Повторный цикл ничего не шлёт.
	before := len(sink.calls)
	m.pollComments(ctx, n)
	if len(sink.calls) != before {
		t.Errorf("повторная отправка: %v", sink.calls[before:])
	}
}

func TestNoteImagePostedBeforeComments(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т", Images: []string{"https://cdn/illustration.jpg"}}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "первый"}},
		},
		avatar: []byte("media-bytes"), // FetchMedia отдаёт байты и для иллюстрации, и для аватара
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false) // постит n1 и сохраняет её иллюстрацию

	n, _ := st.NoteByID(ctx, "n1")
	st.CaptureNoteThread(ctx, store.MessengerTelegram, strconv.FormatInt(n.TGMessageID, 10), "777")
	n, _ = st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	firstImage, firstComment := -1, -1
	for i, c := range sink.calls {
		if c.kind == "image" && firstImage == -1 {
			firstImage = i
		}
		if c.kind == "comment" && firstComment == -1 {
			firstComment = i
		}
	}
	if firstImage == -1 {
		t.Fatalf("иллюстрация не отправлена: %+v", sink.calls)
	}
	if firstComment == -1 || firstImage > firstComment {
		t.Errorf("иллюстрация должна быть раньше комментариев: %+v", sink.calls)
	}
}

func TestWorkerArchivesClosedNote(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т", CommentsClosed: true}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "последний"}},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false) // постит n1 и помечает её comments_closed

	n, _ := st.NoteByID(ctx, "n1")
	if !n.CommentsClosed {
		t.Fatal("обход ленты должен пометить заметку закрытой")
	}
	// Ловим тред, чтобы финальный дозабор комментариев прошёл.
	st.CaptureNoteThread(ctx, store.MessengerTelegram, strconv.FormatInt(n.TGMessageID, 10), "777")

	// Закрытая заметка архивируется на первой же итерации (без сна), поэтому
	// pollNote завершается быстро; таймаут — страховка от зависания при регрессе.
	done, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	m.pollNote(done, "n1")

	n, _ = st.NoteByID(ctx, "n1")
	if n.Status != store.StatusArchived {
		t.Fatalf("закрытая заметка должна уйти в архив, статус %q", n.Status)
	}
	// Последний комментарий всё же отправлен перед архивацией.
	sent := false
	for _, c := range sink.calls {
		if c.kind == "comment" && c.comID == 1 {
			sent = true
		}
	}
	if !sent {
		t.Errorf("последний комментарий должен уйти до архивации: %+v", sink.calls)
	}
}

func TestNotifySubscribersOnKeyword(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "выпьем рюмку чая"}},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false)
	st.AddSubscription(ctx, store.MessengerTelegram, "рюмк", 42)
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "1", "777")

	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	var notified []int64
	for _, c := range sink.calls {
		if c.kind == "notify" {
			notified = append(notified, c.userID)
		}
	}
	if len(notified) != 1 || notified[0] != 42 {
		t.Errorf("уведомления: %v", notified)
	}
}

func TestFeedDriftAlertsAdminOnce(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notesErr: &love.MarkupError{Selector: ".lv-notes__note-item", Context: "лента заметок"}}
	sink := &fakeSink{}

	var mu sync.Mutex
	var alerts []string
	send := func(_ context.Context, text string) {
		mu.Lock()
		alerts = append(alerts, text)
		mu.Unlock()
	}
	m, _ := newTestMirrorAlert(t, site, sink, false, send)

	// Порог алерта — 3 подряд; первые два цикла молчат.
	for i := 0; i < alertThreshold+2; i++ {
		m.feedCycle(ctx, false)
	}
	if len(alerts) != 1 {
		t.Fatalf("ожидался ровно один алерт о дрейфе, получено %d: %v", len(alerts), alerts)
	}
	if !strings.Contains(alerts[0], keyFeedDrift) {
		t.Errorf("текст алерта: %q", alerts[0])
	}

	// Вёрстка «починилась» — лента снова парсится: приходит «восстановилось».
	site.notesErr = nil
	site.notes = []love.Note{{ID: "n1", Text: "т"}}
	m.feedCycle(ctx, false)
	if len(alerts) != 2 || !strings.Contains(alerts[1], "восстановилось") {
		t.Errorf("ожидалось уведомление о восстановлении, получено: %v", alerts)
	}
}

func TestPollInterval(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name        string
		firstSeen   time.Time
		lastComment time.Time
		want        time.Duration
	}{
		{"свежая", now.Add(-time.Hour), time.Time{}, 30 * time.Second},
		{"живая старая", now.Add(-48 * time.Hour), now.Add(-5 * time.Minute), 30 * time.Second},
		{"суточная", now.Add(-12 * time.Hour), time.Time{}, 3 * time.Minute},
		{"трёхдневная", now.Add(-50 * time.Hour), time.Time{}, 15 * time.Minute},
		{"старая", now.Add(-100 * time.Hour), time.Time{}, 30 * time.Minute},
	} {
		if got := PollInterval(now, tc.firstSeen, tc.lastComment); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestShouldArchive(t *testing.T) {
	now := time.Now()
	week := 7 * 24 * time.Hour
	if ShouldArchive(now, now.Add(-time.Hour), time.Time{}) {
		t.Error("свежая заметка не архивируется")
	}
	if !ShouldArchive(now, now.Add(-week-time.Hour), time.Time{}) {
		t.Error("старая тихая заметка архивируется")
	}
	if ShouldArchive(now, now.Add(-week-time.Hour), now.Add(-time.Hour)) {
		t.Error("старая, но живая заметка не архивируется")
	}
	if !ShouldArchive(now, now.Add(-3*week), now.Add(-2*week)) {
		t.Error("неделя тишины — в архив")
	}
}

func TestFanOutDualSinks(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "первый"}},
		},
	}
	tg := &fakeSink{name: store.MessengerTelegram}
	mx := &fakeSink{name: store.MessengerMax}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, site, []Sink{tg, mx}, Config{NotesLimit: 5, FeedInterval: time.Minute}, slog.Default())

	m.feedCycle(ctx, false)
	if len(tg.calls) != 1 || len(mx.calls) != 1 {
		t.Fatalf("заметка должна уйти в оба приёмника: tg=%v max=%v", tg.calls, mx.calls)
	}
	// Повторный цикл идемпотентен для обоих.
	m.feedCycle(ctx, false)
	if len(tg.calls) != 1 || len(mx.calls) != 1 {
		t.Fatalf("повторный цикл не должен постить: tg=%v max=%v", tg.calls, mx.calls)
	}

	// Тред пойман только в MAX — комментарий уходит только туда.
	msgID, _, _, _ := st.Target(ctx, store.MessengerMax, store.TargetNotePost, "n1")
	if _, ok, err := st.CaptureNoteThread(ctx, store.MessengerMax, msgID, "mid.th1"); err != nil || !ok {
		t.Fatalf("захват треда max: %v %v", ok, err)
	}
	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)
	if comments(mx) != 1 {
		t.Errorf("комментарий должен уйти в max: %v", mx.calls)
	}
	if comments(tg) != 0 {
		t.Errorf("в telegram тред не пойман — комментарий рано: %v", tg.calls)
	}

	// Догнал и telegram.
	tgMsg, _, _, _ := st.Target(ctx, store.MessengerTelegram, store.TargetNotePost, "n1")
	st.CaptureNoteThread(ctx, store.MessengerTelegram, tgMsg, "777")
	n, _ = st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)
	if comments(tg) != 1 || comments(mx) != 1 {
		t.Errorf("после захвата треда telegram комментарий доехал ровно по разу: tg=%v max=%v", tg.calls, mx.calls)
	}
}

// comments — число comment-вызовов приёмника.
func comments(f *fakeSink) int {
	n := 0
	for _, c := range f.calls {
		if c.kind == "comment" {
			n++
		}
	}
	return n
}
