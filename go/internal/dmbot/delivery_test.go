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

// allowScan — согласие читать переписку, как от нажатой кнопки «читать и
// присылать сюда»: она же выбирает мессенджер доставки.
func allowScan(t *testing.T, st *store.Store, messenger string, userID int64) {
	t.Helper()
	if _, err := st.SetTalksDelivery(context.Background(), messenger, userID, store.DeliveryOn, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// Пока согласия нет, экран говорит, что переписка не читается, и предлагает
// ровно одно — начать. Нажатие включает и чтение, и доставку сюда.
func TestDeliveryNoConsentOffersToRead(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	seedOwner(t, st, store.MessengerTelegram, user, "777", time.Now())

	l.HandleText(ctx, user, "1", "/delivery")
	if got := tr.lastSent(); !strings.Contains(got, "не читаю") {
		t.Fatalf("без согласия переписка не читается, об этом и говорим: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 1 || btns[0] != btnScanHere {
		t.Fatalf("единственная кнопка — начать читать: %v", btns)
	}

	l.HandleCallback(ctx, user, kbd.Callback{MessageID: "1", Payload: kbd.Pack(verbDelivSet, argDeliveryOn)})
	acc, err := st.TalksAccount(ctx, store.MessengerTelegram, user)
	if err != nil {
		t.Fatal(err)
	}
	if !store.ScanAllowed(acc) {
		t.Fatal("«читать и присылать сюда» — это и согласие читать")
	}
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "сюда") {
		t.Fatalf("итог выбора: %q", edit.text)
	}
	// Одна сессия: выбирать мессенджер не из чего, остаётся только отказ.
	if btns := buttonTexts(edit.kb); len(btns) != 1 || btns[0] != btnScanOff {
		t.Errorf("лишние кнопки при одной сессии: %v", btns)
	}
}

// Отказ от чтения гасит обход всему сайт-аккаунту, а не одной сессии: переписка
// на сайте одна, и второй мессенджер продолжал бы её вычитывать.
func TestScanOffStopsReadingForWholeAccount(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	now := time.Now()
	seedOwner(t, st, store.MessengerTelegram, user, "777", now)
	seedOwner(t, st, store.MessengerMax, 200, "777", now.Add(-time.Hour))
	allowScan(t, st, store.MessengerTelegram, user)

	l.HandleCallback(ctx, user, kbd.Callback{MessageID: "1", Payload: kbd.Pack(verbScanOff, "")})
	acc, err := st.TalksAccount(ctx, store.MessengerTelegram, user)
	if err != nil {
		t.Fatal(err)
	}
	if store.ScanAllowed(acc) {
		t.Fatal("после отказа переписку не читаем")
	}
	for _, o := range acc {
		if o.Scan != store.ScanOff {
			t.Errorf("отказ — настройка аккаунта, а не сессии: %+v", o)
		}
	}
	edit := tr.lastEdit()
	if !strings.Contains(edit.text, "не читаю") {
		t.Fatalf("итог отказа: %q", edit.text)
	}
	if btns := buttonTexts(edit.kb); len(btns) != 1 || btns[0] != btnScanHere {
		t.Errorf("после отказа остаётся только «начать читать»: %v", btns)
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
	allowScan(t, st, store.MessengerMax, 200)

	l.HandleText(ctx, user, "1", "/delivery")
	got := tr.lastSent()
	if !strings.Contains(got, "MAX") {
		t.Fatalf("показываем нынешнего получателя: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 2 || btns[0] != btnDeliverHere || btns[1] != btnScanOff {
		t.Fatalf("перенести доставку сюда или перестать читать: %v", btns)
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
	if btns := buttonTexts(edit.kb); len(btns) != 1 || btns[0] != btnScanOff {
		t.Errorf("лишняя кнопка в новом состоянии: %v", btns)
	}
}

// Кнопка «🔕 Не присылать сюда» из прошлых релизов ещё висит в чатах: нажатие
// обрабатываем, но носить ЛС становится некуда (второй мессенджер согласие уже
// выключило), и текст говорит именно это, а не обещает доставку в MAX.
func TestStaleDeliveryOffButtonStopsEverything(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	now := time.Now()
	seedOwner(t, st, store.MessengerTelegram, user, "777", now)
	seedOwner(t, st, store.MessengerMax, 200, "777", now.Add(-time.Hour))
	allowScan(t, st, store.MessengerTelegram, user)

	l.HandleCallback(ctx, user, kbd.Callback{MessageID: "1", Payload: kbd.Pack(verbDelivSet, argDeliveryOff)})
	got := tr.lastEdit().text
	if !strings.Contains(got, "носить их некуда") {
		t.Fatalf("носить ЛС стало некуда, об этом и говорим: %q", got)
	}
	if strings.Contains(got, "MAX") {
		t.Fatalf("доставку в MAX обещать нельзя — она выключена: %q", got)
	}
}

// Вопрос от поллера — то же сообщение с теми же кнопками, что и /delivery, и он
// называет цену: сайт пометит сообщения прочитанными и покажет человека в сети.
func TestAskTalksScanNamesThePrice(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.AskTalksScan(ctx, 100, false)
	got := tr.lastSent()
	if !strings.Contains(got, "прочитанным") || !strings.Contains(got, "в сети") {
		t.Fatalf("вопрос должен называть цену чтения: %q", got)
	}
	if strings.Contains(got, "не только здесь") {
		t.Fatalf("вход один — про второй мессенджер речи нет: %q", got)
	}
	btns := buttonTexts(tr.lastKB())
	if len(btns) != 2 || btns[0] != btnScanHere || btns[1] != btnScanOff {
		t.Fatalf("две кнопки — читать или нет: %v", btns)
	}
}

// Тот же вопрос человеку, вошедшему в двух мессенджерах: нажатая кнопка заодно
// выбирает получателя, поэтому про второй вход в тексте сказано.
func TestAskTalksScanMentionsSecondLogin(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.AskTalksScan(ctx, 100, true)
	if got := tr.lastSent(); !strings.Contains(got, "не только здесь") {
		t.Fatalf("про второй вход надо сказать: %q", got)
	}
}

// Второй аккаунт в том же мессенджере: «приходят сюда» было бы неправдой —
// ЛС уходят в другой телеграм того же человека.
func TestDeliverySameMessengerOtherAccount(t *testing.T) {
	ctx := context.Background()
	const user = 100
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})
	now := time.Now()
	seedOwner(t, st, store.MessengerTelegram, user, "777", now.Add(-time.Hour))
	seedOwner(t, st, store.MessengerTelegram, 200, "777", now)
	allowScan(t, st, store.MessengerTelegram, 200)

	l.HandleText(ctx, user, "1", "/delivery")
	got := tr.lastSent()
	if !strings.Contains(got, "другой ваш аккаунт") || strings.Contains(got, "приходят сюда") {
		t.Fatalf("получатель — другой аккаунт того же мессенджера: %q", got)
	}
}

// Вход на сайт — тот самый момент, когда уместно спросить про переписку: сразу
// после «Успешный вход», а не тактом поллера через несколько минут. Повторный
// вход уже согласившегося вопрос не повторяет.
func TestLoginAsksForConsent(t *testing.T) {
	ctx := context.Background()
	const uid = 42
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	l.SetTalkRouter(&fakeRouter{ret: true})

	l.HandleText(ctx, uid, "mid.1", "/login")
	l.HandleText(ctx, uid, "mid.2", "user secret")
	got := tr.lastSent()
	if !strings.Contains(got, "прочитанным") {
		t.Fatalf("после входа спрашиваем про чтение переписки: %q", got)
	}
	if btns := buttonTexts(tr.lastKB()); len(btns) != 2 || btns[0] != btnScanHere {
		t.Fatalf("кнопки согласия: %v", btns)
	}

	allowScan(t, st, store.MessengerTelegram, uid)
	l.HandleText(ctx, uid, "mid.3", "/login")
	l.HandleText(ctx, uid, "mid.4", "user secret")
	if got := tr.lastSent(); !strings.Contains(got, "Успешный вход") {
		t.Errorf("согласившегося переспрашивать нечего: %q", got)
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
