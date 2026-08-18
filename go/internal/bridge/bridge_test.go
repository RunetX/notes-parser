package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

const (
	channelID    = -1001000000000
	discussionID = -1002000000000
	userID       = 555
)

// fakeSite фиксирует отправленные на сайт комментарии.
type fakeSite struct {
	mu    sync.Mutex
	posts []sitePost
	err   error
}

type sitePost struct {
	noteID   string
	comAPIID string
	text     string
	cookies  int
}

func (f *fakeSite) PostComment(_ context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.posts = append(f.posts, sitePost{noteID, comAPIID, text, len(cookies)})
	return nil
}

func setup(t *testing.T) (*store.Store, *fakeSite, *Handler, []string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	var mu sync.Mutex
	var notifications []string
	notify := func(_ context.Context, uid int64, text string) {
		mu.Lock()
		notifications = append(notifications, text)
		mu.Unlock()
	}
	site := &fakeSite{}
	h := New(st, site, notify, channelID, discussionID, slog.Default())
	return st, site, h, notifications
}

// seedUserSession кладёт валидную сессию с одной живой кукой.
func seedUserSession(t *testing.T, st *store.Store, uid int64) {
	t.Helper()
	cookies := []*http.Cookie{{Name: "sid", Value: "live"}}
	j, err := love.CookiesToJSON(cookies, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(context.Background(), store.MessengerTelegram, uid, j, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func replyUpdate(replyToID, msgID int, text string) *models.Update {
	return &models.Update{Message: &models.Message{
		ID:             msgID,
		Chat:           models.Chat{ID: discussionID, Type: models.ChatTypeGroup},
		From:           &models.User{ID: userID, IsBot: false},
		Text:           text,
		ReplyToMessage: &models.Message{ID: replyToID},
	}}
}

func TestReplyToThreadRootPostsComment(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	// Заметка с пойманным тредом (корень = 900).
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")

	h.Handle(ctx, replyUpdate(900, 5, "мой ответ"))

	if len(site.posts) != 1 {
		t.Fatalf("постов на сайт: %d", len(site.posts))
	}
	p := site.posts[0]
	if p.noteID != "n1" || p.comAPIID != "" || p.text != "мой ответ" || p.cookies != 1 {
		t.Errorf("ответ в корень заметки разобран неверно: %+v", p)
	}
}

func TestReplyToCommentPrefixesAuthor(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	// Комментарий бота с tg_message_id=800 от автора «Мария».
	st.InsertComment(ctx, store.Comment{ID: 42, NoteID: "n1", AuthorName: "Мария",
		Text: "привет", CreatedAt: time.Now()})
	st.SetTarget(ctx, store.MessengerTelegram, store.TargetComment, "42", "800", "")

	h.Handle(ctx, replyUpdate(800, 6, "и тебе привет"))

	if len(site.posts) != 1 {
		t.Fatalf("постов: %d", len(site.posts))
	}
	p := site.posts[0]
	if p.noteID != "n1" || p.comAPIID != "42" || p.text != "Мария, и тебе привет" {
		t.Errorf("ответ на комментарий разобран неверно: %+v", p)
	}
}

func TestReplyProcessedOnce(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")

	// Одно и то же обновление доставлено дважды (рестарт getUpdates).
	upd := replyUpdate(900, 5, "ответ")
	h.Handle(ctx, upd)
	h.Handle(ctx, upd)

	if len(site.posts) != 1 {
		t.Errorf("повторная доставка задвоила комментарий: %d", len(site.posts))
	}
}

func TestReplyWithoutSessionNotifies(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	// Сессию НЕ создаём.
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")

	// notify пишет в общий срез — проверяем, что был вызов.
	var notified bool
	h.core.notify = func(context.Context, int64, string) { notified = true }
	h.Handle(ctx, replyUpdate(900, 5, "ответ"))

	if len(site.posts) != 0 {
		t.Errorf("без сессии на сайт постить нельзя: %d", len(site.posts))
	}
	if !notified {
		t.Error("пользователь должен получить подсказку про /login")
	}
}

// Сайт отвечает 500: повтора не будет (at-most-once стоит до отправки), значит
// человек обязан узнать сам — иначе он уверен, что комментарий ушёл, а его нет.
func TestSiteErrorTellsTheAuthor(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = errors.New("отправка комментария к заметке n1: статус 500")

	var said string
	h.core.notify = func(_ context.Context, _ int64, text string) { said = text }
	h.Handle(ctx, replyUpdate(900, 5, "ответ"))

	if !strings.Contains(said, "не принял") {
		t.Fatalf("автору должны сказать, что комментарий не ушёл: %q", said)
	}
	// Сессию при этом не гасим: 500 — это про сайт, а не про вход.
	if _, valid, err := st.SessionCookies(ctx, store.MessengerTelegram, userID); err != nil || !valid {
		t.Errorf("сессия после ошибки сайта: valid=%v err=%v", valid, err)
	}
}

func TestReplyToUnknownMessageIgnored(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")

	// Ответ на чужое сообщение (не корень треда и не наш комментарий).
	h.Handle(ctx, replyUpdate(12345, 5, "ответ в никуда"))

	if len(site.posts) != 0 {
		t.Errorf("ответ на неизвестное сообщение не должен постить: %d", len(site.posts))
	}
}

func TestBotReplyIgnored(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")

	upd := replyUpdate(900, 5, "я бот")
	upd.Message.From.IsBot = true
	h.Handle(ctx, upd)

	if len(site.posts) != 0 {
		t.Errorf("ответы ботов игнорируются: %d", len(site.posts))
	}
}

// Автофорвард потерялся (сбой Bot API) — тред ловится из первого же ответа
// на корень ветки, и этот же ответ уходит на сайт.
func TestThreadCapturedFromReplyWhenForwardLost(t *testing.T) {
	ctx := context.Background()
	st, site, h, _ := setup(t)
	seedUserSession(t, st, userID)
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.SetTarget(ctx, store.MessengerTelegram, store.TargetNotePost, "n1", "10", "")
	// Захвата автофорварда не было: note_thread отсутствует.

	upd := replyUpdate(900, 5, "мой ответ")
	upd.Message.MessageThreadID = 900
	upd.Message.ReplyToMessage.ForwardOrigin = &models.MessageOrigin{
		Type: models.MessageOriginTypeChannel,
		MessageOriginChannel: &models.MessageOriginChannel{
			Chat: models.Chat{ID: channelID}, MessageID: 10,
		},
	}
	h.Handle(ctx, upd)

	_, thread, found, err := st.Target(ctx, store.MessengerTelegram, store.TargetNoteThread, "n1")
	if err != nil || !found || thread != "900" {
		t.Fatalf("тред не пойман запасным путём: thread=%q found=%v err=%v", thread, found, err)
	}
	if len(site.posts) != 1 || site.posts[0].noteID != "n1" {
		t.Errorf("ответ не ушёл на сайт: %+v", site.posts)
	}
}

// Ядро с messenger=max: реплаи по строковым mid, включая ответ на комментарий.
func TestCoreMaxReplies(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	site := &fakeSite{}
	core := NewCore(st, site, nil, store.MessengerMax, slog.Default())

	cookies := []*http.Cookie{{Name: "sid", Value: "live"}}
	j, _ := love.CookiesToJSON(cookies, time.Now())
	if err := st.UpsertSession(ctx, store.MessengerMax, userID, j, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.InsertNote(ctx, store.Note{ID: "n1", Text: "т", Status: store.StatusPosted, FirstSeenAt: time.Now()})
	st.SetTarget(ctx, store.MessengerMax, store.TargetNotePost, "n1", "mid.post", "")
	st.SetTarget(ctx, store.MessengerMax, store.TargetNoteThread, "n1", "", "mid.root")
	st.InsertComment(ctx, store.Comment{ID: 42, NoteID: "n1", AuthorName: "Мария",
		Text: "привет", CreatedAt: time.Now()})
	st.SetTarget(ctx, store.MessengerMax, store.TargetComment, "42", "mid.com42", "")

	// Ответ на корень треда → комментарий к заметке.
	core.ProcessReply(ctx, "mid.r1", userID, "mid.root", "мой ответ")
	// Ответ на комментарий → префикс автора и com_api_id.
	core.ProcessReply(ctx, "mid.r2", userID, "mid.com42", "и тебе привет")
	// Повторная доставка того же ответа — дедуп.
	core.ProcessReply(ctx, "mid.r1", userID, "mid.root", "мой ответ")
	// Ответ на чужое сообщение — игнор.
	core.ProcessReply(ctx, "mid.r3", userID, "mid.unknown", "в никуда")

	if len(site.posts) != 2 {
		t.Fatalf("постов на сайт: %d, %+v", len(site.posts), site.posts)
	}
	if p := site.posts[0]; p.noteID != "n1" || p.comAPIID != "" || p.text != "мой ответ" {
		t.Errorf("ответ в корень: %+v", p)
	}
	if p := site.posts[1]; p.noteID != "n1" || p.comAPIID != "42" || p.text != "Мария, и тебе привет" {
		t.Errorf("ответ на комментарий: %+v", p)
	}
}

// --- площадка запасным адресатом --------------------------------------------

// fakePlatform — площадка для моста: принимает реплики и знает, к какой заметке
// относится нативная.
type fakePlatform struct {
	mu       sync.Mutex
	made     []platform.NewComment
	nextID   int64
	err      error
	comments map[int64]platform.Comment
}

func (f *fakePlatform) CreateComment(_ context.Context, in platform.NewComment) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.made = append(f.made, in)
	if f.nextID == 0 {
		f.nextID = platform.NativeIDBase
	}
	f.nextID++
	return f.nextID, nil
}

func (f *fakePlatform) CommentRow(_ context.Context, id int64) (platform.Comment, error) {
	if c, ok := f.comments[id]; ok {
		return c, nil
	}
	return platform.Comment{}, errors.New("нет такой реплики")
}

// withPlatform поднимает мост с подключённой площадкой и сессией, у которой
// заполнена анкета: без неё участника площадки не найти.
func withPlatform(t *testing.T) (*store.Store, *fakeSite, *Handler, *fakePlatform, *[]string) {
	t.Helper()
	st, site, h, _ := setup(t)
	var said []string
	h.core.notify = func(_ context.Context, _ int64, text string) { said = append(said, text) }
	seedUserSession(t, st, userID)
	if err := st.SetSessionIdentity(context.Background(), store.MessengerTelegram, userID,
		"1443311", "9000001", "Dr. David Livesey"); err != nil {
		t.Fatal(err)
	}
	p := &fakePlatform{comments: map[int64]platform.Comment{}}
	h.core.SetPlatform(p, "https://t3h.ru/")
	return st, site, h, p, &said
}

// Сайт отказал — ответ не пропадает, а уходит на площадку от имени того же
// человека. До этой ветки чужие реплики терялись молча: 17.08.2026 НГС отвечал
// 500 на любой комментарий.
func TestSiteRefusalFallsBackToPlatform(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, said := withPlatform(t)
	st.InsertNote(ctx, store.Note{ID: "313028", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = errors.New("статус 500")

	h.Handle(ctx, replyUpdate(900, 5, "ответ из телеграма"))

	if len(p.made) != 1 {
		t.Fatalf("на площадку ушло %d реплик: %+v", len(p.made), p.made)
	}
	got := p.made[0]
	if got.NoteID != 313028 || got.AuthorID != 1443311 || got.Body != "ответ из телеграма" {
		t.Errorf("реплика площадки: %+v", got)
	}
	if len(*said) == 0 || !strings.Contains((*said)[0], "t3h.ru") {
		t.Errorf("человеку не сказали, куда уехал ответ: %v", *said)
	}
}

// Своя реплика уже стоит в треде — это сообщение самого человека. Отметка
// message_targets гасит эхо: исходящий обход площадки копию сюда не принесёт.
func TestPlatformReplyIsMarkedSentToItsOwnMessenger(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, _ := withPlatform(t)
	st.InsertNote(ctx, store.Note{ID: "313028", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = errors.New("статус 500")

	h.Handle(ctx, replyUpdate(900, 5, "ответ"))

	msg, _, found, err := st.Target(ctx, store.MessengerTelegram, store.TargetComment,
		strconv.FormatInt(p.nextID, 10))
	if err != nil || !found {
		t.Fatalf("реплика не отмечена отправленной: found=%v err=%v", found, err)
	}
	if msg != "5" {
		t.Errorf("отмечено сообщение %q, ждали 5 — сообщение самого человека", msg)
	}
}

// Ответ на реплику, написанную на площадке, идёт СРАЗУ туда и сайта не
// касается: такой реплики на НГС нет вовсе, отвечать там нечему.
func TestReplyToNativeCommentSkipsSite(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, _ := withPlatform(t)
	native := platform.NativeIDBase + 7
	p.comments[native] = platform.Comment{ID: native, NoteID: 313028}
	if err := st.SetTarget(ctx, store.MessengerTelegram, store.TargetComment,
		strconv.FormatInt(native, 10), "77", ""); err != nil {
		t.Fatal(err)
	}

	h.Handle(ctx, replyUpdate(77, 5, "и тебе привет"))

	if len(site.posts) != 0 {
		t.Errorf("на сайт ушло: %+v", site.posts)
	}
	if len(p.made) != 1 || p.made[0].ReplyToID != native || p.made[0].NoteID != 313028 {
		t.Fatalf("реплика площадки: %+v", p.made)
	}
	// Обращение «Ник, » в тело НЕ подставляется: на площадке оно ребро, и
	// дорисовывается на показе — иначе вышло бы «Ник, Ник, ».
	if p.made[0].Body != "и тебе привет" {
		t.Errorf("тело реплики: %q", p.made[0].Body)
	}
}

// Ответ в тред заметки, написанной на площадке: сайта тоже не касается.
func TestReplyToNativeNoteGoesToPlatform(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, _ := withPlatform(t)
	native := strconv.FormatInt(platform.NativeIDBase, 10)
	if err := st.SetTarget(ctx, store.MessengerTelegram, store.TargetNoteThread,
		native, "", "900"); err != nil {
		t.Fatal(err)
	}

	h.Handle(ctx, replyUpdate(900, 5, "первый"))

	if len(site.posts) != 0 {
		t.Errorf("на сайт ушло: %+v", site.posts)
	}
	if len(p.made) != 1 || p.made[0].NoteID != platform.NativeIDBase || p.made[0].ReplyToID != 0 {
		t.Fatalf("реплика площадки: %+v", p.made)
	}
}

// Человек, который на площадку ещё не входил, получает приглашение, а не
// «что-то пошло не так»: он пишет прямо сейчас, и это лучший момент позвать.
func TestNonMemberIsInvited(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, said := withPlatform(t)
	st.InsertNote(ctx, store.Note{ID: "313028", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = errors.New("статус 500")
	p.err = platform.ErrNotMember

	h.Handle(ctx, replyUpdate(900, 5, "ответ"))

	if len(*said) != 1 || !strings.Contains((*said)[0], "https://t3h.ru") {
		t.Fatalf("приглашения на площадку нет: %v", *said)
	}
	if !strings.Contains((*said)[0], "войдите") {
		t.Errorf("в приглашении не сказано, что делать: %q", (*said)[0])
	}
}

// Протухшая сессия сайта на площадку НЕ переносится: человеку в любом случае
// идти делать /login, и второе письмо про площадку только запутает.
func TestExpiredSessionDoesNotFallBack(t *testing.T) {
	ctx := context.Background()
	st, site, h, p, said := withPlatform(t)
	st.InsertNote(ctx, store.Note{ID: "313028", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = love.ErrUnauthorized

	h.Handle(ctx, replyUpdate(900, 5, "ответ"))

	if len(p.made) != 0 {
		t.Errorf("реплика уехала на площадку при протухшей сессии: %+v", p.made)
	}
	if len(*said) != 1 || !strings.Contains((*said)[0], "/login") {
		t.Fatalf("подсказка про /login: %v", *said)
	}
}

// Про переезд ответа говорят ОДИН раз: при мёртвом НГС иначе выходит письмо на
// каждую реплику.
func TestPlatformNoticeIsSentOnce(t *testing.T) {
	ctx := context.Background()
	st, site, h, _, said := withPlatform(t)
	st.InsertNote(ctx, store.Note{ID: "313028", Text: "т", Status: store.StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	st.CaptureNoteThread(ctx, store.MessengerTelegram, "10", "900")
	site.err = errors.New("статус 500")

	h.Handle(ctx, replyUpdate(900, 5, "раз"))
	h.Handle(ctx, replyUpdate(900, 6, "два"))

	if len(*said) != 1 {
		t.Fatalf("сказано %d раз: %v", len(*said), *said)
	}
}

// Ответ на реплику площадки от НЕучастника: приглашение говорит, что заметка
// живёт здесь, а не «НГС не принял» — сайт тут вообще ни при чём.
func TestNativeRefusalNamesTheRightReason(t *testing.T) {
	ctx := context.Background()
	st, _, h, p, said := withPlatform(t)
	native := platform.NativeIDBase + 7
	p.comments[native] = platform.Comment{ID: native, NoteID: 313028}
	p.err = platform.ErrNotMember
	if err := st.SetTarget(ctx, store.MessengerTelegram, store.TargetComment,
		strconv.FormatInt(native, 10), "77", ""); err != nil {
		t.Fatal(err)
	}

	h.Handle(ctx, replyUpdate(77, 5, "привет"))

	if len(*said) != 1 {
		t.Fatalf("сказано: %v", *said)
	}
	if strings.Contains((*said)[0], "НГС ваш ответ не принял") {
		t.Errorf("названа не та причина: %q", (*said)[0])
	}
	if !strings.Contains((*said)[0], "на НГС её нет") || !strings.Contains((*said)[0], "https://t3h.ru") {
		t.Errorf("приглашение: %q", (*said)[0])
	}
}

// У своей заметки об успехе не объявляют: человек и так отвечает нашему.
func TestNativeReplySaysNothingOnSuccess(t *testing.T) {
	ctx := context.Background()
	st, _, h, p, said := withPlatform(t)
	native := strconv.FormatInt(platform.NativeIDBase, 10)
	if err := st.SetTarget(ctx, store.MessengerTelegram, store.TargetNoteThread,
		native, "", "900"); err != nil {
		t.Fatal(err)
	}

	h.Handle(ctx, replyUpdate(900, 5, "первый"))

	if len(p.made) != 1 {
		t.Fatalf("реплика не ушла: %+v", p.made)
	}
	if len(*said) != 0 {
		t.Errorf("лишнее сообщение человеку: %v", *said)
	}
}
