package dmbot

import (
	"context"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

// newTestTalksLogic — движок бота переписки на той же БД, что и бот команд.
func newTestTalksLogic(t *testing.T, st *store.Store, messenger string) (*Logic, *fakeTransport) {
	t.Helper()
	tr := &fakeTransport{}
	return NewTalksLogic(st, tr, messenger, slog.Default()), tr
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Бот переписки не берёт на себя команды бота 1 и не трогает сайт.
func TestTalksOnlyRejectsCommandBotCommands(t *testing.T) {
	ctx := context.Background()
	const user = 42
	st := openTestStore(t)
	l, tr := newTestTalksLogic(t, st, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})

	for _, cmd := range []string{"/login", "/add_note", "/add_anonymous_note", "/status",
		"/subscribe слово", "/unsubscribe слово", "/mysubs"} {
		l.HandleText(ctx, user, "1", cmd)
		if !strings.Contains(tr.lastSent(), "основного бота") {
			t.Errorf("%s должен отправлять к основному боту: %q", cmd, tr.lastSent())
		}
	}
	// Состояние логина при этом не заводится.
	if state, err := st.DialogState(ctx, store.MessengerTelegram+":talks", user); err != nil || state != "" {
		t.Errorf("бот переписки не должен заводить состояний: %q err=%v", state, err)
	}
}

// Меню бота переписки и список разрешённых ему команд — один источник: команда,
// которую мессенджер показывает в списке, не должна получать отлуп.
func TestTalksMenuCommandsAccepted(t *testing.T) {
	ctx := context.Background()
	const user = 43
	st := openTestStore(t)
	l, tr := newTestTalksLogic(t, st, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})

	for _, c := range botCommands(true, true, true) {
		l.HandleText(ctx, user, "1", "/"+c.Name)
		if strings.Contains(tr.lastSent(), "Здесь только личная переписка") {
			t.Errorf("/%s есть в меню бота переписки, но отфутболена: %q", c.Name, tr.lastSent())
		}
	}
}

// Приветствия под роль: у бота переписки свой список, у бота команд строки про
// диалоги появляются только когда переписку ведёт он сам.
func TestStartMessageByRole(t *testing.T) {
	ctx := context.Background()
	const user = 7
	st := openTestStore(t)

	talksLogic, talksTr := newTestTalksLogic(t, st, store.MessengerTelegram)
	talksLogic.HandleText(ctx, user, "1", "/start")
	if !strings.Contains(talksTr.lastSent(), "/talks") ||
		strings.Contains(talksTr.lastSent(), "/add_note") {
		t.Errorf("приветствие бота переписки: %q", talksTr.lastSent())
	}

	cmdLogic, cmdTr, _, _ := newTestLogic(t, store.MessengerTelegram)
	cmdLogic.HandleText(ctx, user, "1", "/start")
	if strings.Contains(cmdTr.lastSent(), "/talks") {
		t.Errorf("без роутера переписки в приветствии не должно быть /talks: %q", cmdTr.lastSent())
	}
	if !strings.Contains(cmdTr.lastSent(), "/add_note") {
		t.Errorf("приветствие бота команд без заметок: %q", cmdTr.lastSent())
	}

	cmdLogic.SetTalkRouter(&fakeRouter{ret: true})
	cmdLogic.HandleText(ctx, user, "1", "/start")
	if !strings.Contains(cmdTr.lastSent(), "/talks") {
		t.Errorf("с роутером /talks должен появиться: %q", cmdTr.lastSent())
	}
}

// Состояния двух ботов одного мессенджера не пересекаются: залипание на
// диалоге у бота переписки не ломает ввод заметки у бота команд.
func TestStateNamespacesIsolated(t *testing.T) {
	ctx := context.Background()
	const user = 100
	st := openTestStore(t)

	talksLogic, _ := newTestTalksLogic(t, st, store.MessengerTelegram)
	router := &fakeRouter{ret: true}
	talksLogic.SetTalkRouter(router)
	peerID, err := st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: store.MessengerTelegram, OwnerUserID: user, PassportID: "777", Nick: "Мария"})
	if err != nil {
		t.Fatal(err)
	}
	talksLogic.HandleText(ctx, user, "1", "/talk "+strconv.FormatInt(peerID, 10))

	// Бот команд — на той же БД: его состояние логина не должно пересечься
	// с залипшим диалогом бота переписки.
	site := &fakeSite{}
	cmdTr := &fakeTransport{}
	cmdLogic := NewLogic(st, site, cmdTr, store.MessengerTelegram, slog.Default())
	cmdLogic.HandleText(ctx, user, "2", "/login")
	cmdLogic.HandleText(ctx, user, "3", "логин пароль")
	if site.loginCalls != 1 || !strings.Contains(cmdTr.lastSent(), "Успешный вход") {
		t.Fatalf("вход у бота команд не прошёл: calls=%d (%q)", site.loginCalls, cmdTr.lastSent())
	}
	if len(router.calls) != 0 {
		t.Errorf("текст ушёл в чужой диалог: %+v", router.calls)
	}

	// И наоборот: залипший диалог бота переписки жив.
	talksLogic.HandleText(ctx, user, "4", "привет")
	if len(router.calls) != 1 || router.calls[0].peerID != peerID {
		t.Errorf("залипший диалог бота переписки: %+v", router.calls)
	}
}

// Легаси-состояние «pm:<id>» у бота команд, когда переписка уехала к боту 2:
// состояние сбрасывается, пользователь получает внятный ответ.
func TestStalePMStateClearedWhenTalksMoved(t *testing.T) {
	ctx := context.Background()
	const user = 55
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	if err := st.SetDialogState(ctx, store.MessengerTelegram, user, "pm:1", time.Now()); err != nil {
		t.Fatal(err)
	}

	l.HandleText(ctx, user, "1", "привет")
	if !strings.Contains(tr.lastSent(), "бота переписки") {
		t.Errorf("ответ про переезд переписки: %q", tr.lastSent())
	}
	if state, err := st.DialogState(ctx, store.MessengerTelegram, user); err != nil || state != "" {
		t.Errorf("залипшее состояние должно быть снято: %q err=%v", state, err)
	}
}
