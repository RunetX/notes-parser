package talks

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// --- фейки ---

type fakeSite struct {
	dialogs    []love.TalkDialog
	history    map[string][]love.TalkMessage
	dialogsErr error
	historyErr error

	sent       []sentToSite
	sendReturn love.TalkMessage
	sendErr    error
}

type sentToSite struct {
	passportID string
	text       string
}

func (f *fakeSite) Dialogs(_ context.Context, _ []*http.Cookie, _ int) ([]love.TalkDialog, error) {
	if f.dialogsErr != nil {
		return nil, f.dialogsErr
	}
	return f.dialogs, nil
}

func (f *fakeSite) History(_ context.Context, _ []*http.Cookie, passportID, _ string, _ int) ([]love.TalkMessage, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	return f.history[passportID], nil
}

func (f *fakeSite) Send(_ context.Context, _ []*http.Cookie, passportID, text string) (love.TalkMessage, error) {
	if f.sendErr != nil {
		return love.TalkMessage{}, f.sendErr
	}
	f.sent = append(f.sent, sentToSite{passportID, text})
	return f.sendReturn, nil
}

type fakeTransport struct {
	name     string
	sendErr  error
	nextID   int
	sent     []sentPM
	confirms []confirmCall
}

type sentPM struct {
	userID int64
	html   string
	msgID  string
}

type confirmCall struct {
	userID int64
	msgID  string
	ok     bool
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) SendPM(_ context.Context, userID int64, html string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.nextID++
	id := "d" + itoa(int64(f.nextID))
	f.sent = append(f.sent, sentPM{userID, html, id})
	return id, nil
}

func (f *fakeTransport) Confirm(_ context.Context, userID int64, msgID string, ok bool) {
	f.confirms = append(f.confirms, confirmCall{userID, msgID, ok})
}

// --- хелперы ---

const testOwner = 42

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSession(t *testing.T, st *store.Store, owner int64) {
	t.Helper()
	ctx := context.Background()
	ck := []*http.Cookie{{Name: "sess", Value: "abc", Domain: "love.ngs.ru", Path: "/", Expires: time.Now().Add(24 * time.Hour)}}
	js, err := love.CookiesToJSON(ck, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, store.MessengerTelegram, owner, js, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func testConfig() Config {
	return Config{
		AdminOnly: true, AdminIDs: map[string]int64{store.MessengerTelegram: testOwner},
		AllowSend: true, StoreText: false, MaxReqPerMin: 6000, ForbiddenLimit: 3,
		BaseURL: "https://love.ngs.ru",
	}
}

// --- тесты ---

func TestPollDeliversOnceAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Мария", ProfileID: "555", LastMsgID: "m2"}},
		history: map[string][]love.TalkMessage{
			"777": {{SiteMsgID: "m1", Text: "привет"}, {SiteMsgID: "m2", Text: "как дела?"}},
		},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("ожидалось 2 доставки, got %d", len(tr.sent))
	}
	// HTML содержит ник и ссылку на анкету.
	if got := tr.sent[0].html; !strings.Contains(got, "Мария") || !strings.Contains(got, "/profile/555/") {
		t.Errorf("HTML без ника/анкеты: %q", got)
	}

	// Повторный опрос: активности нет (LastMsgID == курсор) — доставок не прибавилось.
	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("повторная доставка (задвоение): %d", len(tr.sent))
	}

	// Новое сообщение: fake History отдаёт всё, дедуп по target не даёт задвоить.
	site.dialogs[0].LastMsgID = "m3"
	site.history["777"] = append(site.history["777"], love.TalkMessage{SiteMsgID: "m3", Text: "ещё"})
	w.pollOnce(ctx)
	if len(tr.sent) != 3 {
		t.Fatalf("ожидалась одна новая доставка, всего %d", len(tr.sent))
	}
}

func TestStoreTextFalseKeepsDBEmptyButDeliversText(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Ева", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "секрет"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)
	w.pollOnce(ctx)

	if len(tr.sent) != 1 || !strings.Contains(tr.sent[0].html, "секрет") {
		t.Fatalf("живой текст должен уйти в мессенджер: %+v", tr.sent)
	}
	// В БД текст пуст (store_text=false).
	peers, _ := st.TalkPeers(ctx, store.MessengerTelegram, testOwner)
	if len(peers) != 1 {
		t.Fatalf("ожидался 1 диалог, got %d", len(peers))
	}
}

func TestDeliveryFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Ника", LastMsgID: "m2"}},
		history: map[string][]love.TalkMessage{
			"777": {{SiteMsgID: "m1", Text: "раз"}, {SiteMsgID: "m2", Text: "два"}},
		},
	}
	tr := &fakeTransport{name: store.MessengerTelegram, sendErr: errors.New("мессенджер недоступен")}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx) // доставка падает, курсор не двигается
	if len(tr.sent) != 0 {
		t.Fatalf("при ошибке доставка не должна учитываться: %d", len(tr.sent))
	}
	tr.sendErr = nil
	w.pollOnce(ctx) // повтор: оба доходят по разу
	if len(tr.sent) != 2 {
		t.Fatalf("после восстановления ожидалось 2 доставки, got %d", len(tr.sent))
	}
}

func TestHandleReplyAtMostOnceAndJurisdiction(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs:    []love.TalkDialog{{PassportID: "777", Nick: "Юля", LastMsgID: "m1"}},
		history:    map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "привет"}}},
		sendReturn: love.TalkMessage{SiteMsgID: "out1", FromSelf: true},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)
	w.pollOnce(ctx)
	delivered := tr.sent[0].msgID // id доставленного ЛС, на него отвечаем реплаем

	// Реплай от владельца — одна отправка на сайт.
	if !w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "мой ответ") {
		t.Fatal("реплай на talks-сообщение должен быть обработан")
	}
	if len(site.sent) != 1 || site.sent[0].passportID != "777" || site.sent[0].text != "мой ответ" {
		t.Fatalf("ответ на сайт: %+v", site.sent)
	}
	// Повторный тот же реплай (переигранный апдейт) — at-most-once, второй раз не шлём.
	w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "мой ответ")
	if len(site.sent) != 1 {
		t.Fatalf("at-most-once нарушен: %d", len(site.sent))
	}

	// Реплай от чужого пользователя — обработан, но на сайт не уходит.
	w.HandleReply(ctx, store.MessengerTelegram, "r2", 99, delivered, "чужой")
	if len(site.sent) != 1 {
		t.Fatalf("чужой реплай не должен слаться на сайт: %d", len(site.sent))
	}

	// Реплай не на talks-сообщение — не наш.
	if w.HandleReply(ctx, store.MessengerTelegram, "r3", testOwner, "неизвестное", "x") {
		t.Fatal("реплай на не-talks сообщение не должен считаться нашим")
	}
}

func TestHandleReplyReadOnly(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	cfg := testConfig()
	cfg.AllowSend = false
	w := New(st, site, []PMTransport{tr}, cfg, nil)
	w.pollOnce(ctx)
	delivered := tr.sent[0].msgID

	w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "ответ")
	if len(site.sent) != 0 {
		t.Fatalf("в read-only ответ на сайт не уходит: %d", len(site.sent))
	}
	if len(tr.confirms) != 1 || tr.confirms[0].ok {
		t.Fatalf("должен быть один отрицательный Confirm: %+v", tr.confirms)
	}
}

func TestKillSwitchOnForbidden(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	var alerted int
	site := &fakeSite{dialogsErr: love.ErrForbidden}
	cfg := testConfig()
	cfg.AlertSend = func(context.Context, string) { alerted++ }
	w := New(st, site, []PMTransport{&fakeTransport{name: store.MessengerTelegram}}, cfg, nil)

	for i := 0; i < 3; i++ {
		w.pollOnce(ctx)
	}
	if !w.stopped {
		t.Fatal("после ForbiddenLimit подряд 403 поллер должен остановиться")
	}
	if alerted == 0 {
		t.Error("kill-switch должен уведомить админа")
	}
}
