package dmbot

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

// fakeRouter фиксирует вызовы SendToDialog (реплаи проходят мимо — здесь false).
type sdCall struct {
	messenger   string
	userID      int64
	peerID      int64
	ackID, text string
}

type fakeRouter struct {
	calls []sdCall
	ret   bool
}

func (f *fakeRouter) HandleReply(context.Context, string, string, int64, string, string) bool {
	return false
}

func (f *fakeRouter) SendToDialog(_ context.Context, messenger string, userID, peerID int64, ackID, text string) bool {
	f.calls = append(f.calls, sdCall{messenger, userID, peerID, ackID, text})
	return f.ret
}

// fakeSiteID реализует SiteAuth и SiteIdentifier (для проверки снятия
// идентичности при /login).
type fakeSiteID struct {
	cookies                 []*http.Cookie
	profile, passport, nick string
}

func (f *fakeSiteID) Login(context.Context, string, string) ([]*http.Cookie, error) {
	return f.cookies, nil
}
func (f *fakeSiteID) PostNote(context.Context, []*http.Cookie, string, bool) error { return nil }
func (f *fakeSiteID) SiteIdentity(context.Context, []*http.Cookie) (string, string, string, error) {
	return f.profile, f.passport, f.nick, nil
}

func TestTalksCommands(t *testing.T) {
	ctx := context.Background()
	const user = 42
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	router := &fakeRouter{ret: true}
	l.SetTalkRouter(router)

	// Нет диалогов.
	l.HandleText(ctx, user, "1", "/talks")
	if !strings.Contains(tr.lastSent(), "нет диалогов") {
		t.Fatalf("/talks без диалогов: %q", tr.lastSent())
	}

	// Заводим диалог и проверяем список.
	peerID, err := st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: store.MessengerTelegram, OwnerUserID: user, PassportID: "777", Nick: "Мария"})
	if err != nil {
		t.Fatal(err)
	}
	l.HandleText(ctx, user, "2", "/talks")
	if got := tr.lastSent(); !strings.Contains(got, "#"+strconv.FormatInt(peerID, 10)) || !strings.Contains(got, "Мария") {
		t.Fatalf("/talks со списком: %q", got)
	}

	// /talk <id> — залипаем на диалоге.
	l.HandleText(ctx, user, "3", "/talk "+strconv.FormatInt(peerID, 10))
	if s, _ := st.DialogState(ctx, store.MessengerTelegram, user); s != statePMPrefix+strconv.FormatInt(peerID, 10) {
		t.Fatalf("состояние pm не установлено: %q", s)
	}

	// Текст в залипшем диалоге → SendToDialog с верными аргументами.
	l.HandleText(ctx, user, "50", "привет ему")
	if len(router.calls) != 1 {
		t.Fatalf("ожидался один SendToDialog, got %d", len(router.calls))
	}
	if c := router.calls[0]; c.messenger != store.MessengerTelegram || c.userID != user ||
		c.peerID != peerID || c.ackID != "50" || c.text != "привет ему" {
		t.Fatalf("аргументы SendToDialog: %+v", c)
	}

	// /cancel — выходим.
	l.HandleText(ctx, user, "4", "/cancel")
	if s, _ := st.DialogState(ctx, store.MessengerTelegram, user); s != "" {
		t.Fatalf("состояние не сброшено: %q", s)
	}
}

func TestTalkOpenRejectsForeignAndBadID(t *testing.T) {
	ctx := context.Background()
	const user = 42
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})

	l.HandleText(ctx, user, "1", "/talk абв")
	if !strings.Contains(tr.lastSent(), "Укажите номер") {
		t.Errorf("нечисловой id: %q", tr.lastSent())
	}

	// Диалог другого владельца — не найден, состояние не ставится.
	foreign, _ := st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: store.MessengerTelegram, OwnerUserID: 999, PassportID: "888"})
	l.HandleText(ctx, user, "2", "/talk "+strconv.FormatInt(foreign, 10))
	if !strings.Contains(tr.lastSent(), "не найден") {
		t.Errorf("чужой диалог должен быть не найден: %q", tr.lastSent())
	}
	if s, _ := st.DialogState(ctx, store.MessengerTelegram, user); s != "" {
		t.Errorf("чужой диалог не должен ставить состояние: %q", s)
	}
}

func TestLoginCapturesIdentity(t *testing.T) {
	ctx := context.Background()
	const user = 42
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "id.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	site := &fakeSiteID{
		cookies:  []*http.Cookie{{Name: "s", Value: "v", Domain: "love.ngs.ru", Path: "/", Expires: time.Now().Add(time.Hour)}},
		profile:  "1472546",
		passport: "280703879",
		nick:     "Рантье",
	}
	l := NewLogic(st, site, &fakeTransport{}, store.MessengerTelegram, slog.Default())

	l.HandleText(ctx, user, "1", "/login")
	l.HandleText(ctx, user, "2", "user pass")

	pid, pass, nick, err := st.SessionIdentity(ctx, store.MessengerTelegram, user)
	if err != nil || pid != "1472546" || pass != "280703879" || nick != "Рантье" {
		t.Fatalf("site-идентичность после /login: %q %q %q %v", pid, pass, nick, err)
	}
}
