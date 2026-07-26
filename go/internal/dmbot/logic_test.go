package dmbot

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"lovegw/internal/store"
)

// fakeTransport собирает отправленные ЛС и удалённые сообщения.
type fakeTransport struct {
	sent    []string
	deleted []string
}

func (f *fakeTransport) Send(_ context.Context, _ int64, text string) {
	f.sent = append(f.sent, text)
}

func (f *fakeTransport) DeleteMessage(_ context.Context, _ int64, messageID string) {
	f.deleted = append(f.deleted, messageID)
}

func (f *fakeTransport) lastSent() string {
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

// fakeSite отвечает на логин фиксированной кукой.
type fakeSite struct {
	loginCalls int
	noteTexts  []string
}

func (f *fakeSite) Login(_ context.Context, login, password string) ([]*http.Cookie, error) {
	f.loginCalls++
	return []*http.Cookie{{Name: "sid", Value: login + ":" + password}}, nil
}

func (f *fakeSite) PostNote(_ context.Context, _ []*http.Cookie, text string, _ bool) error {
	f.noteTexts = append(f.noteTexts, text)
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

	// Публикация заметки по сохранённой сессии.
	l.HandleText(ctx, uid, "mid.5", "/add_note")
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
