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
	notes       []love.Note
	notesErr    error
	comments    map[string][]love.Comment
	commentsErr error
	// headers — шапка заметки на странице комментариев (nil — не разобралась).
	headers map[string]*love.Note
	avatar  []byte
}

func (f *fakeSite) FetchNotes(context.Context) ([]love.Note, error) { return f.notes, f.notesErr }
func (f *fakeSite) FetchCommentsPage(_ context.Context, id string) (love.CommentsPage, error) {
	if f.commentsErr != nil {
		return love.CommentsPage{}, f.commentsErr
	}
	return love.CommentsPage{Comments: f.comments[id], Note: f.headers[id]}, nil
}
func (f *fakeSite) FetchMedia(context.Context, string) ([]byte, error) { return f.avatar, nil }

type sinkCall struct {
	kind     string
	noteID   string
	comID    int64
	userID   int64
	subKind  string // вид сработавшей подписки (kind == "notify")
	target   string // цель сработавшей подписки
	postID   string // пост заметки в канале (уведомление о новой заметке)
	threadID string // корень треда (kind == "comment")
	replyTo  string // сообщение адресата реплики, "" — отвечаем корню
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

func (f *fakeSink) PostComment(_ context.Context, n store.Note, threadID, replyToID string, c store.Comment, _ []byte) (string, error) {
	f.nextID++
	f.calls = append(f.calls, sinkCall{kind: "comment", noteID: n.ID, comID: c.ID,
		threadID: threadID, replyTo: replyToID})
	return f.id(), nil
}

func (f *fakeSink) PostNoteImage(_ context.Context, _ string, imageURL string, _ []byte) (string, error) {
	f.nextID++
	f.calls = append(f.calls, sinkCall{kind: "image", noteID: imageURL})
	return f.id(), nil
}

// notify — ЛС-бот мессенджера (Config.SubNotify). Пишем в тот же журнал
// вызовов: приёмник уведомления больше не шлёт, но проверять их удобно рядом.
func (f *fakeSink) notify(_ context.Context, userID int64, ev SubEvent) {
	f.calls = append(f.calls, sinkCall{kind: "notify", noteID: ev.Note.ID, comID: ev.Comment.ID,
		userID: userID, subKind: ev.Sub.Kind, target: ev.Sub.Target, postID: ev.PostMsgID})
}

// threadSink — приёмник, открывающий тред сам (как MAX).
type threadSink struct {
	fakeSink
	threads int
}

func (f *threadSink) StartThread(_ context.Context, n store.Note, postMsgID string) (string, error) {
	f.threads++
	f.calls = append(f.calls, sinkCall{kind: "thread", noteID: n.ID, comID: int64(len(postMsgID))})
	return "thread-" + n.ID, nil
}

// MAX-порядок: тред открывается ДО поста в канал — иначе кнопка «Обсудить» не
// знает mid ветки и ведёт в чат целиком.
func TestThreadStartedBeforeNotePost(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", Text: "т"}}}
	sink := &threadSink{fakeSink: fakeSink{name: store.MessengerMax}}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, site, []Sink{sink}, Config{NotesLimit: 5, FeedInterval: time.Minute}, slog.Default())

	m.feedCycle(ctx, false)

	if len(sink.calls) < 2 || sink.calls[0].kind != "thread" || sink.calls[1].kind != "note" {
		t.Fatalf("ожидался тред до поста: %+v", sink.calls)
	}
	if _, thread, found, err := st.Target(ctx, store.MessengerMax, store.TargetNoteThread, "n1"); err != nil ||
		!found || thread != "thread-n1" {
		t.Errorf("корень треда не зафиксирован: %q found=%v err=%v", thread, found, err)
	}
	// Повторный цикл не плодит копий заметки в чате.
	m.feedCycle(ctx, false)
	if sink.threads != 1 {
		t.Errorf("StartThread вызван %d раз(а)", sink.threads)
	}
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
		SubNotify:    map[string]SubNotify{sink.Name(): sink.notify},
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

// OnNewNote — страховочный вход амвона. Зовётся на каждую новую заметку и
// ДО постинга в канал (пост идёт через лимитеры мессенджеров и стоит секунд, а
// подписчику важна каждая), но в seed-режиме молчит: там новых заметок нет.
func TestFeedCycleCallsOnNewNote(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n2", Text: "т"}, {ID: "n1", Text: "т"}}}
	sink := &fakeSink{}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	var seen []string
	postsAtCall := 0
	m := New(st, site, []Sink{sink}, Config{
		NotesLimit:   5,
		FeedInterval: time.Minute,
		OnNewNote: func(_ context.Context, n love.Note) {
			seen = append(seen, n.ID)
			postsAtCall = len(sink.calls)
		},
	}, slog.Default())

	m.feedCycle(ctx, true) // seed: колбэк молчит
	if len(seen) != 0 {
		t.Fatalf("в seed-режиме колбэк не зовётся: %v", seen)
	}

	site.notes = append([]love.Note{{ID: "n3", Text: "т"}}, site.notes...)
	m.feedCycle(ctx, false)
	if len(seen) != 1 || seen[0] != "n3" {
		t.Fatalf("колбэк: %v", seen)
	}
	if postsAtCall != 0 {
		t.Errorf("колбэк должен идти до поста в канал, а постов уже %d", postsAtCall)
	}

	// Повторный обход дублей не даёт.
	m.feedCycle(ctx, false)
	if len(seen) != 1 {
		t.Errorf("колбэк позвали повторно: %v", seen)
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

// Заметка, съехавшая ниже notes_limit за время простоя демона, всё равно
// должна быть подхвачена: окно ленты режется только на холодном старте, а
// notes_limit ограничивает лишь темп догона (по заметке за обход).
func TestFeedCycleCatchesNotesBelowLimit(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", Text: "т"}}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.notesLimit = 1
	m.feedCycle(ctx, false) // холодный старт: знаем только n1

	// Простой демона: сверху появились n2..n4, n1 уехал вниз.
	site.notes = []love.Note{{ID: "n4", Text: "т"}, {ID: "n3", Text: "т"},
		{ID: "n2", Text: "т"}, {ID: "n1", Text: "т"}}
	sink.calls = nil
	for i := 0; i < 3; i++ { // догон по одной заметке за обход
		m.feedCycle(ctx, false)
	}

	if len(sink.calls) != 3 {
		t.Fatalf("постов при догоне: %d (%v)", len(sink.calls), sink.calls)
	}
	if sink.calls[0].noteID != "n2" || sink.calls[2].noteID != "n4" {
		t.Errorf("порядок догона: %v", sink.calls)
	}
	posted, _ := st.NotesByStatus(ctx, store.StatusPosted)
	if len(posted) != 4 {
		t.Errorf("posted-заметок: %d", len(posted))
	}
}

// На холодном старте (пустая БД) лента режется по notes_limit — незачем
// вываливать в канал всю историю.
func TestFeedCycleColdStartLimited(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n3", Text: "т"}, {ID: "n2", Text: "т"}, {ID: "n1", Text: "т"}}}
	sink := &fakeSink{}
	m, _ := newTestMirror(t, site, sink, false)
	m.notesLimit = 1

	m.feedCycle(ctx, false)

	if len(sink.calls) != 1 || sink.calls[0].noteID != "n3" {
		t.Errorf("холодный старт должен запостить только верх ленты: %v", sink.calls)
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

// Реплика с обращением «Ник, …» уходит ответом на сообщение адресата — тогда
// мессенджер сам покажет цитату исходного комментария. Всё остальное, как и
// раньше, отвечает корню треда (то есть самой заметке).
func TestCommentRepliesToAddressee(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {
				{ID: 4, AuthorName: "Гость", Text: "Хатуль, а ты как думаешь?"},
				{ID: 3, AuthorName: "Пётр", Text: "просто мысль вслух"},
				{ID: 2, AuthorName: "Пётр", Text: "ЯГОДА, согласен"},
				{ID: 1, AuthorName: "Ягода", Text: "первая мысль"},
			},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false) // постит n1, сообщение «1»

	n, _ := st.NoteByID(ctx, "n1")
	if _, _, err := st.CaptureNoteThread(ctx, store.MessengerTelegram,
		strconv.FormatInt(n.TGMessageID, 10), "777"); err != nil {
		t.Fatal(err)
	}
	n, _ = st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	// Комментарии получают сообщения 2..5 по порядку id.
	want := map[int64]string{
		1: "",  // корневая реплика — обращения нет
		2: "2", // «ЯГОДА, …» → сообщение комментария 1, регистр не помеха
		3: "",  // обращения нет
		4: "",  // Хатуль в этой заметке не писал — падаем на корень треда
	}
	seen := 0
	for _, c := range sink.calls {
		if c.kind != "comment" {
			continue
		}
		seen++
		if c.threadID != "777" {
			t.Errorf("комментарий %d ушёл мимо треда: %q", c.comID, c.threadID)
		}
		if w, ok := want[c.comID]; !ok {
			t.Errorf("неожиданный комментарий %d", c.comID)
		} else if c.replyTo != w {
			t.Errorf("комментарий %d: адресат %q, ожидался %q", c.comID, c.replyTo, w)
		}
	}
	if seen != len(want) {
		t.Errorf("отправлено комментариев: %d, ожидалось %d", seen, len(want))
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

// Пометка «не актуальна» — справочная: сайт вешает её почти сразу после
// публикации, а комментарии продолжают приходить. Снимать такую заметку с
// отслеживания нельзя — иначе зеркалятся первые пару комментариев из сотен.
func TestClosedNoteStaysTracked(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т", CommentsClosed: true}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "первый"}},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false) // постит n1 и фиксирует пометку сайта

	n, _ := st.NoteByID(ctx, "n1")
	if !n.CommentsClosed {
		t.Fatal("обход ленты должен зафиксировать пометку «не актуальна»")
	}
	st.CaptureNoteThread(ctx, store.MessengerTelegram, strconv.FormatInt(n.TGMessageID, 10), "777")

	// Свежая заметка не архивируется — воркер продолжит опрос, поэтому ждём
	// его выхода только по отмене контекста.
	done, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	m.pollNote(done, "n1")

	n, _ = st.NoteByID(ctx, "n1")
	if n.Status == store.StatusArchived {
		t.Fatal("заметка с пометкой «не актуальна» не должна архивироваться досрочно")
	}
	sent := false
	for _, c := range sink.calls {
		if c.kind == "comment" && c.comID == 1 {
			sent = true
		}
	}
	if !sent {
		t.Errorf("комментарий должен уйти в тред: %+v", sink.calls)
	}
}

// Пропавшая из БД заметка — постоянное состояние: воркер выходит, а не
// ретраит вечно.
func TestPollNoteExitsWhenNoteGone(t *testing.T) {
	m, _ := newTestMirror(t, &fakeSite{}, &fakeSink{}, false)
	m.retryPause = 10 * time.Millisecond
	done := make(chan struct{})
	go func() { m.pollNote(context.Background(), "нет-такой"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("воркер должен завершиться, когда заметки нет в БД")
	}
}

// Ошибка БД (не «не найдено») — преходящая: воркер не умирает, а ретраит,
// пока его не отменят. До этой правки разовая ошибка NoteByID убивала воркер
// навсегда, и комментарии заметки переставали зеркалиться до рестарта демона.
func TestPollNoteSurvivesDBError(t *testing.T) {
	m, st := newTestMirror(t, &fakeSite{}, &fakeSink{}, false)
	if _, err := st.InsertNote(context.Background(), store.Note{
		ID: "n1", Text: "т", Status: store.StatusPosted, FirstSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	st.Close() // теперь NoteByID возвращает ошибку БД, но не ErrNotFound
	m.retryPause = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.pollNote(ctx, "n1"); close(done) }()
	select {
	case <-done:
		t.Fatal("воркер умер от ошибки БД")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("воркер не вышел по отмене контекста")
	}
}

// Менеджер чистит running по завершении воркера и подбирает posted-заметки
// ре-сканом: заметка, чей воркер закончился (архив), после возврата в posted
// снова получает воркер — без события в канале events и без рестарта демона.
func TestManagerRescanRestartsWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, st := newTestMirror(t, &fakeSite{}, &fakeSink{}, false)
	m.rescanEvery = 20 * time.Millisecond
	m.retryPause = 10 * time.Millisecond

	// Старая тихая заметка: воркер заархивирует её и завершится.
	old := time.Now().Add(-8 * 24 * time.Hour)
	if _, err := st.InsertNote(ctx, store.Note{
		ID: "n1", Text: "т", Status: store.StatusPosted, FirstSeenAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	managerDone := make(chan struct{})
	go func() { m.managePollers(ctx); close(managerDone) }()

	waitStatus := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if n, err := st.NoteByID(ctx, "n1"); err == nil && n.Status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("заметка не перешла в %q", want)
	}

	waitStatus(store.StatusArchived) // первый воркер отработал и вышел

	// Возврат в posted: подобрать заметку может только ре-скан, и только если
	// running очищен по done — иначе start() решит, что воркер ещё жив.
	if err := st.SetNoteStatusPosted(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	waitStatus(store.StatusArchived) // второй воркер стартовал и снова заархивировал

	cancel()
	select {
	case <-managerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("менеджер не вышел по отмене контекста")
	}
}

// notifies отбирает из журнала приёмника только уведомления подписчикам.
func notifies(calls []sinkCall) []sinkCall {
	var out []sinkCall
	for _, c := range calls {
		if c.kind == "notify" {
			out = append(out, c)
		}
	}
	return out
}

// subscribe заводит подписку прямо в сторе (кнопки — забота dmbot).
func subscribe(t *testing.T, st *store.Store, kind, target string, userID int64) {
	t.Helper()
	if _, err := st.AddSubscription(context.Background(), store.Subscription{
		Messenger: store.MessengerTelegram, UserID: userID, Kind: kind, Target: target,
	}); err != nil {
		t.Fatal(err)
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
	subscribe(t, st, store.SubKeyword, "рюмк", 42)
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "1", "777")

	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	notified := notifies(sink.calls)
	if len(notified) != 1 || notified[0].userID != 42 {
		t.Fatalf("уведомления: %v", notified)
	}
	// Сработавшая подписка доходит целиком — из неё собирается повод в ЛС.
	if notified[0].subKind != store.SubKeyword || notified[0].target != "рюмк" {
		t.Errorf("подписка в уведомлении: %+v", notified[0])
	}
}

// Подписка на комментарии заметки бьёт по её треду и только по нему.
func TestNotifySubscribersOnNoteComments(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "без ключевых слов"}},
		},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false)
	subscribe(t, st, store.SubNoteComments, "n1", 42)
	subscribe(t, st, store.SubNoteComments, "n2", 43) // чужая заметка
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "1", "777")

	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	notified := notifies(sink.calls)
	if len(notified) != 1 || notified[0].userID != 42 || notified[0].subKind != store.SubNoteComments {
		t.Errorf("уведомления: %+v", notified)
	}
}

// Один комментарий — одно ЛС на человека, даже если сработали две подписки;
// побеждает подписка на заметку: она точнее, слово могло совпасть случайно.
func TestNotifySubscribersDedup(t *testing.T) {
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
	subscribe(t, st, store.SubKeyword, "рюмк", 42)
	subscribe(t, st, store.SubNoteComments, "n1", 42)
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "1", "777")

	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	notified := notifies(sink.calls)
	if len(notified) != 1 {
		t.Fatalf("на один комментарий должно уйти одно ЛС: %+v", notified)
	}
	if notified[0].subKind != store.SubNoteComments {
		t.Errorf("выиграть должна подписка на заметку: %+v", notified[0])
	}
}

// Новая заметка автора: ЛС уходит с ссылкой на пост канала, у анонима — нет,
// и повторный обход ленты дублей не делает.
func TestNotifyAuthorSubscribersOnNewNote(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{
		{ID: "n1", AuthorID: "515996", AuthorName: "Ягода", Text: "т"},
		{ID: "n2", AuthorID: "0", AuthorName: "Аноним", Text: "т"},
	}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	subscribe(t, st, store.SubAuthorNotes, "515996", 42)
	subscribe(t, st, store.SubAuthorNotes, "0", 43) // подписка на анонима не сработает

	m.feedCycle(ctx, false)

	notified := notifies(sink.calls)
	if len(notified) != 1 || notified[0].userID != 42 || notified[0].noteID != "n1" {
		t.Fatalf("уведомления о новой заметке: %+v", notified)
	}
	if notified[0].postID == "" {
		t.Error("ссылка на пост канала строится по его id — он обязан доехать")
	}
	if notified[0].comID != 0 {
		t.Error("у повода «новая заметка» комментария нет")
	}

	m.feedCycle(ctx, false)
	if len(notifies(sink.calls)) != 1 {
		t.Errorf("повторный обход ленты не должен слать второе ЛС: %+v", notifies(sink.calls))
	}
}

// В seed-режиме заметки только фиксируются — рассылать по ним нечего.
func TestSeedDoesNotNotifyAuthorSubscribers(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", AuthorID: "515996", Text: "т"}}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, true)
	subscribe(t, st, store.SubAuthorNotes, "515996", 42)

	m.feedCycle(ctx, true)

	if len(notifies(sink.calls)) != 0 {
		t.Errorf("seed не должен уведомлять: %+v", notifies(sink.calls))
	}
}

// Архивация снимает подписки на комментарии заметки — новых уже не будет.
func TestArchiveDropsNoteSubscriptions(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", Text: "т"}}}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false)
	subscribe(t, st, store.SubNoteComments, "n1", 42)
	subscribe(t, st, store.SubKeyword, "рюмк", 42)

	m.archiveNote(ctx, "n1", time.Now(), "тест")

	left, err := st.SubscriptionsByUser(ctx, store.MessengerTelegram, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Kind != store.SubKeyword {
		t.Errorf("после архивации должно остаться только слово: %+v", left)
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

// Успех ленты не гасит счётчик 403 тредов: ключи раздельные, иначе частичный
// бан (лента отвечает, треды 403) не набирал бы порог никогда.
func TestCommentsForbiddenAlertSurvivesFeedSuccess(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", Text: "т"}}}
	var alerted []string
	m, st := newTestMirrorAlert(t, site, &fakeSink{}, false, func(_ context.Context, text string) {
		alerted = append(alerted, text)
	})
	m.feedCycle(ctx, false) // постит n1
	n, err := st.NoteByID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}

	site.commentsErr = love.ErrForbidden
	for i := 0; i < alertThreshold; i++ {
		m.feedCycle(ctx, false) // лента живая — счётчик тредов не сбрасывается
		m.pollComments(ctx, n)  // треды под 403
	}
	if len(alerted) != 1 || !strings.Contains(alerted[0], keyCommentsForbidden) {
		t.Fatalf("ожидался один алерт 403 комментариев, получено: %v", alerted)
	}
}

// Тред, из которого мы уже зеркалили реплики, сам не пустеет: ноль на такой
// заметке — молчащий источник, и админ обязан узнать об этом за минуты.
func TestSilentCommentsSourceAlertsAdmin(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes:    []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{"n1": {{ID: 1, AuthorName: "А", Text: "раз"}}},
	}
	var alerted []string
	m, st := newTestMirrorAlert(t, site, &fakeSink{}, false, func(_ context.Context, text string) {
		alerted = append(alerted, text)
	})
	m.feedCycle(ctx, false)
	n, err := st.NoteByID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	m.pollComments(ctx, n) // комментарий известен зеркалу

	site.comments["n1"] = nil // страница цела, а списка нет
	for i := 0; i < alertThreshold; i++ {
		m.pollComments(ctx, n)
	}
	if len(alerted) != 1 || !strings.Contains(alerted[0], keyCommentsSilent) {
		t.Fatalf("ожидался один алерт о молчащем источнике, получено: %v", alerted)
	}

	site.comments["n1"] = []love.Comment{{ID: 1, AuthorName: "А", Text: "раз"}}
	m.pollComments(ctx, n)
	if len(alerted) != 2 || !strings.Contains(alerted[1], "восстановилось") {
		t.Fatalf("ожидалось уведомление о восстановлении, получено: %v", alerted)
	}
}

// Свежая заметка без комментариев ничего не доказывает, и счётчик алертов ею
// двигать нельзя — иначе на боевой ленте порог не набрался бы никогда: пустые
// заметки сбрасывали бы счёт, набранный сломанными тредами.
func TestEmptyFreshNoteDoesNotTouchSilentCounter(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes:    []love.Note{{ID: "n1", Text: "т"}, {ID: "n2", Text: "т2"}},
		comments: map[string][]love.Comment{"n1": {{ID: 1, AuthorName: "А", Text: "раз"}}},
	}
	var alerted []string
	m, st := newTestMirrorAlert(t, site, &fakeSink{}, false, func(_ context.Context, text string) {
		alerted = append(alerted, text)
	})
	m.feedCycle(ctx, false)
	n1, err := st.NoteByID(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := st.NoteByID(ctx, "n2")
	if err != nil {
		t.Fatal(err)
	}
	m.pollComments(ctx, n1)

	site.comments["n1"] = nil
	for i := 0; i < alertThreshold; i++ {
		m.pollComments(ctx, n1)
		m.pollComments(ctx, n2) // пустая заметка между сломанными тактами
	}
	if len(alerted) != 1 || !strings.Contains(alerted[0], keyCommentsSilent) {
		t.Fatalf("пустая заметка не должна сбрасывать счётчик: %v", alerted)
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

// Заметка, запощенная до включения второго приёмника (нет note_post-цели),
// в его цикле не участвует: комментарии ему не очередятся, а досрочная
// архивация «не актуальна» не ждёт его треда.
func TestPreexistingNoteSkipsLateSink(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorName: "А", Text: "к"}},
		},
	}
	tg := &fakeSink{name: store.MessengerTelegram}
	mx := &fakeSink{name: store.MessengerMax}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Заметка из «до-MAX» эпохи: телеграмные цели есть, MAX-целей нет.
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, TGThreadID: 900, FirstSeenAt: time.Now()})
	st.MarkNoteCommentsClosed(ctx, "n1")

	m := New(st, site, []Sink{tg, mx}, Config{NotesLimit: 5, FeedInterval: time.Minute}, slog.Default())
	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	if comments(tg) != 1 {
		t.Errorf("комментарий должен уйти в telegram: %v", tg.calls)
	}
	if len(mx.calls) != 0 {
		t.Errorf("MAX не должен участвовать в старой заметке: %v", mx.calls)
	}
}

// Модераторы добавляют/заменяют иллюстрации после публикации: новая картинка
// из шапки страницы комментариев доезжает в тред, повторный опрос не дублирует.
func TestModeratorImageUpdatePostedToThread(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes:    []love.Note{{ID: "n1", Text: "т"}}, // без картинок при публикации
		comments: map[string][]love.Comment{},
		avatar:   []byte("img-bytes"),
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false)
	n, _ := st.NoteByID(ctx, "n1")
	st.CaptureNoteThread(ctx, store.MessengerTelegram, strconv.FormatInt(n.TGMessageID, 10), "777")
	n, _ = st.NoteByID(ctx, "n1")

	// Пока картинок нет — в тред ничего не уходит.
	m.pollComments(ctx, n)
	if images(sink) != 0 {
		t.Fatalf("картинок ещё нет: %v", sink.calls)
	}

	// Модератор добавил картинку.
	site.headers = map[string]*love.Note{"n1": {Images: []string{"https://cdn/v1.jpg"}}}
	m.pollComments(ctx, n)
	if images(sink) != 1 {
		t.Fatalf("новая картинка должна уйти в тред: %v", sink.calls)
	}

	// Модератор заменил картинку на новую (другой URL).
	site.headers["n1"] = &love.Note{Images: []string{"https://cdn/v2.jpg"}}
	m.pollComments(ctx, n)
	if images(sink) != 2 {
		t.Fatalf("заменённая картинка должна уйти в тред: %v", sink.calls)
	}

	// Повторный опрос ничего не дублирует.
	m.pollComments(ctx, n)
	if images(sink) != 2 {
		t.Errorf("дубли картинок: %v", sink.calls)
	}
}

// «Комментарии запрещены» на странице заметки закрывают её и без ленты
// (заметка могла выпасть из окна ленты до заморозки).
func TestClosedMarkerFromNotePage(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes:    []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{},
	}
	sink := &fakeSink{}
	m, st := newTestMirror(t, site, sink, false)
	m.feedCycle(ctx, false)

	site.headers = map[string]*love.Note{"n1": {CommentsClosed: true}}
	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n)

	n, _ = st.NoteByID(ctx, "n1")
	if !n.CommentsClosed {
		t.Error("признак «комментарии запрещены» со страницы должен закрывать заметку")
	}
}

// images — число image-вызовов приёмника.
func images(f *fakeSink) int {
	n := 0
	for _, c := range f.calls {
		if c.kind == "image" {
			n++
		}
	}
	return n
}
