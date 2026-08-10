package dmbot

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// fakeTransport собирает отправленные ЛС, удалённые сообщения, показанные
// клавиатуры, ответы на нажатия и правки сообщений.
type fakeTransport struct {
	mu       sync.Mutex
	sent     []string
	deleted  []string
	kbs      []sentKB   // отправленные сообщения с кнопками
	answers  []string   // тосты ответов на нажатия ("" — молча)
	edits    []editedKB // правки сообщений
	commands []kbd.Command
}

// sentKB — сообщение с кнопками; editedKB — правка уже отправленного.
type sentKB struct {
	text string
	kb   *kbd.Keyboard
}

type editedKB struct {
	messageID string
	text      string
	kb        *kbd.Keyboard
}

func (f *fakeTransport) Send(_ context.Context, _ int64, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
}

func (f *fakeTransport) DeleteMessage(_ context.Context, _ int64, messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
}

// SendKeyboard пишет текст и в sent — проверкам содержимого всё равно, было
// ли у сообщения меню.
func (f *fakeTransport) SendKeyboard(_ context.Context, _ int64, text string, kb *kbd.Keyboard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	f.kbs = append(f.kbs, sentKB{text: text, kb: kb})
}

func (f *fakeTransport) AnswerCallback(_ context.Context, _ kbd.Callback, toast string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, toast)
}

func (f *fakeTransport) EditMessage(_ context.Context, _ int64, messageID, text string, kb *kbd.Keyboard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, editedKB{messageID: messageID, text: text, kb: kb})
}

func (f *fakeTransport) SetCommands(_ context.Context, cmds []kbd.Command) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = cmds
}

func (f *fakeTransport) lastSent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

// lastKB — клавиатура последнего сообщения с кнопками.
func (f *fakeTransport) lastKB() *kbd.Keyboard {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.kbs) == 0 {
		return nil
	}
	return f.kbs[len(f.kbs)-1].kb
}

// lastEdit — последняя правка сообщения.
func (f *fakeTransport) lastEdit() editedKB {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return editedKB{}
	}
	return f.edits[len(f.edits)-1]
}

// buttonTexts — подписи всех кнопок клавиатуры подряд.
func buttonTexts(kb *kbd.Keyboard) []string {
	if kb.Empty() {
		return nil
	}
	var out []string
	for _, row := range kb.Rows {
		for _, b := range row {
			out = append(out, b.Text)
		}
	}
	return out
}

// fakeSite отвечает на логин фиксированной кукой.
type fakeSite struct {
	loginCalls    int
	noteTexts     []string
	lastAnonymous bool
}

func (f *fakeSite) Login(_ context.Context, login, password string) ([]*http.Cookie, error) {
	f.loginCalls++
	return []*http.Cookie{{Name: "sid", Value: login + ":" + password}}, nil
}

func (f *fakeSite) PostNote(_ context.Context, _ []*http.Cookie, text string, anonymous bool) error {
	f.noteTexts = append(f.noteTexts, text)
	f.lastAnonymous = anonymous
	return nil
}

func newTestLogic(t *testing.T, messenger string) (*Logic, *fakeTransport, *fakeSite, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tr := &fakeTransport{}
	site := &fakeSite{}
	return NewLogic(st, site, tr, messenger, slog.Default()), tr, site, st
}

// Полный цикл /login в MAX: состояние, удаление сообщения с паролем,
// сохранение сессии, /status.
func TestLogicLoginFlowMax(t *testing.T) {
	ctx := context.Background()
	l, tr, site, st := newTestLogic(t, store.MessengerMax)
	const uid = 25978651

	l.HandleText(ctx, uid, "mid.1", "/status")
	if !strings.Contains(tr.lastSent(), "/login") {
		t.Errorf("без сессии /status зовёт к /login: %q", tr.lastSent())
	}

	l.HandleText(ctx, uid, "mid.2", "/login")
	if !strings.Contains(tr.lastSent(), "логин и пароль") {
		t.Fatalf("приглашение к вводу: %q", tr.lastSent())
	}
	l.HandleText(ctx, uid, "mid.3", "user secret")
	if len(tr.deleted) != 1 || tr.deleted[0] != "mid.3" {
		t.Errorf("сообщение с паролем должно удаляться: %v", tr.deleted)
	}
	if site.loginCalls != 1 {
		t.Errorf("вызовов Login: %d", site.loginCalls)
	}
	if !strings.Contains(tr.lastSent(), "Успешный вход") {
		t.Errorf("ответ на вход: %q", tr.lastSent())
	}
	// Сессия легла в измерение max, состояние сброшено.
	if _, valid, err := st.SessionCookies(ctx, store.MessengerMax, uid); err != nil || !valid {
		t.Errorf("сессия max: valid=%v err=%v", valid, err)
	}
	if s, _ := st.DialogState(ctx, store.MessengerMax, uid); s != "" {
		t.Errorf("состояние после входа: %q", s)
	}

	l.HandleText(ctx, uid, "mid.4", "/status")
	if !strings.Contains(tr.lastSent(), "Сессия активна") {
		t.Errorf("статус после входа: %q", tr.lastSent())
	}

	// Публикация заметки по сохранённой сессии: команда спрашивает авторство,
	// выбор делается кнопкой.
	l.HandleText(ctx, uid, "mid.5", "/add_note")
	l.HandleCallback(ctx, uid, kbd.Callback{
		AnswerID: "cb.1", MessageID: "mid.kb", Payload: kbd.Pack(verbNote, argNoteOwn)})
	l.HandleText(ctx, uid, "mid.6", "текст заметки")
	if len(site.noteTexts) != 1 || site.noteTexts[0] != "текст заметки" {
		t.Errorf("заметка на сайт: %v", site.noteTexts)
	}
}

// Подписки живут в измерении своего мессенджера.
func TestLogicSubscriptionsPerMessenger(t *testing.T) {
	ctx := context.Background()
	l, tr, _, st := newTestLogic(t, store.MessengerMax)
	const uid = 42

	l.HandleText(ctx, uid, "mid.1", "/subscribe Граф")
	if !strings.Contains(tr.lastSent(), "Подписал") {
		t.Fatalf("подписка: %q", tr.lastSent())
	}
	l.HandleText(ctx, uid, "mid.2", "/mysubs")
	if !strings.Contains(tr.lastSent(), "Граф") {
		t.Errorf("список подписок: %q", tr.lastSent())
	}
	// В telegram-измерении подписки нет.
	if kws, _ := st.SubscriptionsByUser(ctx, store.MessengerTelegram, uid); len(kws) != 0 {
		t.Errorf("подписка утекла в telegram: %v", kws)
	}
	subs, _ := st.Subscriptions(ctx)
	if len(subs) != 1 || subs[0].Messenger != store.MessengerMax {
		t.Errorf("подписки: %+v", subs)
	}
}

// Команда прерывает незавершённый диалог, мусор без состояния — подсказка.
func TestLogicStateInterruptAndFallback(t *testing.T) {
	ctx := context.Background()
	l, tr, site, _ := newTestLogic(t, store.MessengerMax)
	const uid = 7

	l.HandleText(ctx, uid, "mid.1", "/login")
	l.HandleText(ctx, uid, "mid.2", "/mysubs") // команда сбрасывает ожидание пароля
	l.HandleText(ctx, uid, "mid.3", "user secret")
	if site.loginCalls != 0 {
		t.Errorf("после сброса состояния логина быть не должно: %d", site.loginCalls)
	}
	if !strings.Contains(tr.lastSent(), "/start") {
		t.Errorf("мусор без состояния — подсказка: %q", tr.lastSent())
	}
}
