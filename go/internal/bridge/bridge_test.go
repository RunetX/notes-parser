package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/love"
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
