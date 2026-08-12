package dmbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// seedOwner заводит сессию сайта с паспортом и временем входа.
func seedOwner(t *testing.T, st *store.Store, messenger string, userID int64, passport string, loginAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertSession(ctx, messenger, userID, "[]", loginAt); err != nil {
		t.Fatal(err)
	}
	if passport == "" {
		return
	}
	if err := st.SetSessionIdentity(ctx, messenger, userID, "p"+passport, passport, "ник"); err != nil {
		t.Fatal(err)
	}
}

// Настройка «куда слать ЛС» у человека с двумя входами: показывает нынешнего
// получателя, а нажатие «присылать сюда» гасит доставку во второй мессенджер.
func TestDeliveryChoiceSwitchesMessenger(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	old := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedOwner(t, st, store.MessengerTelegram, user, "777", old)
	seedOwner(t, st, store.MessengerMax, 200, "777", old.Add(24*time.Hour))

	l.HandleText(ctx, user, "1", "/delivery")
	got := tr.lastSent()
	if !strings.Contains(got, "MAX") {
		t.Fatalf("показываем нынешнего получателя: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 2 {
		t.Fatalf("выбор из двух кнопок: %v", btns)
	}

	l.HandleCallback(ctx, user, kbd.Callback{MessageID: "1", Payload: kbd.Pack(verbDelivSet, argDeliveryOn)})
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "сюда") || !strings.Contains(edit.text, "Доставку в MAX выключил") {
		t.Fatalf("итог выбора: %q", edit.text)
	}
	acc, err := st.TalksAccount(ctx, store.MessengerTelegram, user)
	if err != nil {
		t.Fatal(err)
	}
	win, ok := store.PickDelivery(acc)
	if !ok || win.Messenger != store.MessengerTelegram {
		t.Fatalf("получатель после выбора: %+v %v", win, ok)
	}
	// Кнопка «присылать сюда» пропала — состояние и так такое.
	if btns := buttonTexts(edit.kb); len(btns) != 1 || btns[0] != btnDeliverOff {
		t.Errorf("лишняя кнопка в новом состоянии: %v", btns)
	}
}

// Отказ: ЛС сюда больше не носим, а текст честно говорит, куда они пойдут.
func TestDeliveryOffTellsWhereMessagesGo(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	now := time.Now()
	seedOwner(t, st, store.MessengerTelegram, user, "777", now)
	seedOwner(t, st, store.MessengerMax, 200, "777", now.Add(-time.Hour))

	l.HandleCallback(ctx, user, kbd.Callback{MessageID: "1", Payload: kbd.Pack(verbDelivSet, argDeliveryOff)})
	if got := tr.lastEdit().text; !strings.Contains(got, "MAX") {
		t.Fatalf("после отказа ЛС уходят в MAX, об этом и говорим: %q", got)
	}
}

// Вопрос от поллера — то же сообщение с теми же кнопками, что и /delivery.
func TestAskDeliveryOffersBothButtons(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.AskDelivery(ctx, 100, store.TalksOwner{Messenger: store.MessengerMax, UserID: 200})
	if got := tr.lastSent(); !strings.Contains(got, "MAX") {
		t.Fatalf("называем нынешнего получателя: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 2 || btns[0] != btnDeliverHere {
		t.Fatalf("две кнопки выбора: %v", btns)
	}
}

// Второй аккаунт в том же мессенджере: «приходят сюда» было бы неправдой —
// ЛС уходят в другой телеграм того же человека.
func TestAskDeliverySameMessengerOtherAccount(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.AskDelivery(ctx, 100, store.TalksOwner{Messenger: store.MessengerTelegram, UserID: 200})
	got := tr.lastSent()
	if !strings.Contains(got, "другой ваш аккаунт") || strings.Contains(got, "приходят сюда") {
		t.Fatalf("получатель — другой аккаунт того же мессенджера: %q", got)
	}
}

// Без входа на сайт настраивать нечего — зовём к /login, а не молчим.
func TestDeliveryWithoutSession(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})

	l.HandleText(ctx, 100, "1", "/delivery")
	if got := tr.lastSent(); !strings.Contains(got, "/login") {
		t.Errorf("ожидалось приглашение войти: %q", got)
	}
}

// У бота переписки настройка тоже есть: ЛС доставляет именно он.
func TestDeliveryAvailableInTalksBot(t *testing.T) {
	ctx := context.Background()
	const user = 100
	st := openTestStore(t)
	l, tr := newTestTalksLogic(t, st, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	seedOwner(t, st, store.MessengerTelegram, user, "777", time.Now())

	l.HandleText(ctx, user, "1", "/delivery")
	if got := tr.lastSent(); !strings.Contains(got, "Личные сообщения") {
		t.Fatalf("бот переписки должен отвечать на /delivery: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) == 0 {
		t.Error("кнопки выбора не показаны")
	}
}
