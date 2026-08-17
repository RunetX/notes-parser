package dmbot

// Настройка личных сообщений сайта (/delivery): читать ли их вообще и куда
// носить.
//
// Читать — только с согласия: чтобы забрать ЛС, бот заходит на сайт под кукой
// человека, сайт помечает сообщения прочитанными и всё это время показывает его
// в сети. Без согласия обход сайт-аккаунт не трогает вовсе (store.ScanAllowed).
//
// Носить — ровно в один мессенджер, по той же причине: во втором сообщения уже
// не будет. Выбор исключающий — включение здесь гасит доставку там, это делает
// store.SetTalksDelivery одной транзакцией.
//
// Вопрос задаёт не только человек: поллер talks, увидев сайт-аккаунт без
// согласия, один раз зовёт AskTalksScan — то же сообщение с теми же кнопками.
// Кнопка «читать и присылать сюда» отвечает сразу на оба вопроса, поэтому
// вопрос один.

import (
	"context"
	"errors"
	"strings"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

const (
	btnDelivery    = "📬 Личные сообщения"
	btnDeliverHere = "📬 Присылать сюда"
	btnScanHere    = "📬 Читать и присылать сюда"
	btnScanOff     = "🚫 Не читать мою переписку"
	// msgScanPrice — чем человек платит за доставку ЛС. Текст один на все входы:
	// и на вопрос от поллера, и на показ настройки.
	msgScanPrice = "Чтобы принести личное сообщение, я захожу на сайт под вашей " +
		"сессией: сайт помечает сообщение прочитанным (собеседник видит «просмотрено») " +
		"и всё это время показывает вас в сети."
	// msgDeliveryOne — почему приходится выбирать ещё и мессенджер.
	msgDeliveryOne = "Вы вошли на сайт не только здесь, а носить личные сообщения " +
		"я могу только в один мессенджер: во втором сообщения уже не будет."
	msgDeliveryNoSession = "Вы ещё не входили на сайт — личных сообщений мне брать неоткуда."
)

// handleDelivery показывает, куда идут личные сообщения сайта, и даёт выбор.
// cb != nil — пришли кнопкой, тогда правим то же сообщение.
func (l *Logic) handleDelivery(ctx context.Context, userID int64, cb *kbd.Callback) {
	if l.talks == nil {
		l.tr.Send(ctx, userID, msgTalksOff)
		return
	}
	acc, err := l.st.TalksAccount(ctx, l.messenger, userID)
	if errors.Is(err, store.ErrNotFound) {
		l.tr.Send(ctx, userID, msgDeliveryNoSession+" "+l.loginHint())
		return
	}
	if err != nil {
		l.log.Error("состояние доставки ЛС", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	l.show(ctx, userID, cb, l.deliveryText(userID, acc), l.deliveryKeyboard(userID, acc))
}

// setDelivery записывает выбор и показывает получившееся состояние. Состояние
// перечитываем из БД, а не собираем на месте: так в сообщении ровно то, по чему
// дальше пойдёт поллер.
//
// argDeliveryOff кнопками больше не предлагается (см. deliveryKeyboard), но
// обрабатывается: «🔕 Не присылать сюда» прошлых релизов ещё висит в чатах.
func (l *Logic) setDelivery(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	choice := store.DeliveryOn
	if arg == argDeliveryOff {
		choice = store.DeliveryOff
	}
	off, err := l.st.SetTalksDelivery(ctx, l.messenger, userID, choice, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		l.tr.Send(ctx, userID, msgDeliveryNoSession+" "+l.loginHint())
		return
	}
	if err != nil {
		l.log.Error("выбор доставки ЛС", "user", userID, "choice", choice, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	acc, err := l.st.TalksAccount(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("состояние доставки ЛС", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	text := "Готово. " + l.deliveryText(userID, acc)
	if names := l.switchedOffText(off); names != "" {
		text += "\nДоставку " + names + " выключил."
	}
	l.replace(ctx, userID, cb, text, l.deliveryKeyboard(userID, acc))
}

// setScanOff — отказ от чтения переписки: обход перестаёт ходить на сайт под
// сессиями всего сайт-аккаунта, а не только этой. Подтверждения не спрашиваем
// (ничего не теряется, кнопка «читать» остаётся рядом), но состояние
// перечитываем — в сообщении ровно то, по чему дальше пойдёт поллер.
func (l *Logic) setScanOff(ctx context.Context, userID int64, cb kbd.Callback) {
	err := l.st.SetTalksScan(ctx, l.messenger, userID, store.ScanOff, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		l.tr.Send(ctx, userID, msgDeliveryNoSession+" "+l.loginHint())
		return
	}
	if err != nil {
		l.log.Error("отказ от чтения переписки", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	acc, err := l.st.TalksAccount(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("состояние личных сообщений", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	l.replace(ctx, userID, cb, "Готово. "+l.deliveryText(userID, acc), l.deliveryKeyboard(userID, acc))
}

// AskTalksScan спрашивает согласия читать личную переписку сайта. Зовут двое:
// поллер talks (Config.AskScan), увидев сайт-аккаунт без согласия, и сам вход
// (/login) — там это уместнее всего. Один раз на человека, дальше настройка
// живёт под /delivery. alsoElsewhere — этот сайт-аккаунт залогинен ещё где-то.
func (l *Logic) AskTalksScan(ctx context.Context, userID int64, alsoElsewhere bool) {
	text := "Про личные сообщения с сайта.\n" + msgScanPrice + "\nЧитать их и приносить сюда?"
	if alsoElsewhere {
		text += "\n" + msgDeliveryOne + " Нажмите «читать» там, где хотите их получать."
	}
	l.tr.SendKeyboard(ctx, userID, text, kbd.New().Row(
		kbd.Button{Text: btnScanHere, Payload: kbd.Pack(verbDelivSet, argDeliveryOn)},
		kbd.Button{Text: btnScanOff, Payload: kbd.Pack(verbScanOff, "")},
	))
}

// askScanOnLogin задаёт тот же вопрос сразу после входа: человек только что
// отдал боту доступ к аккаунту, и спросить про переписку правильнее здесь, а не
// тактом поллера через несколько минут. Уже согласившегося (повторный /login на
// истёкшей сессии) и уже спрошенного не трогаем.
func (l *Logic) askScanOnLogin(ctx context.Context, userID int64) {
	if l.talks == nil {
		return
	}
	acc, err := l.st.TalksAccount(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("состояние личных сообщений после входа", "user", userID, "err", err)
		return
	}
	if store.ScanAllowed(acc) || ownerIn(acc, l.messenger, userID).Asked {
		return
	}
	l.AskTalksScan(ctx, userID, len(acc) > 1)
	if err := l.st.MarkTalksAsked(ctx, l.messenger, userID, time.Now()); err != nil {
		l.log.Error("отметка вопроса о чтении переписки", "user", userID, "err", err)
	}
}

// deliveryText — что сейчас с личными сообщениями: читаем ли, куда носим и чем
// это оплачено.
func (l *Logic) deliveryText(userID int64, acc []store.TalksOwner) string {
	var b strings.Builder
	if !store.ScanAllowed(acc) {
		b.WriteString("Личные сообщения с сайта я не читаю и сюда не приношу.\n" + msgScanPrice)
		return b.String()
	}
	win, ok := store.PickDelivery(acc)
	if !ok {
		// Согласие есть, но носить некуда: читать переписку в пустоту поллер не
		// станет — так и говорим, иначе экран обещает доставку, которой нет.
		b.WriteString("Личные сообщения с сайта я вам не приношу — доставка выключена везде, " +
			"а значит и не читаю: носить их некуда.")
		return b.String()
	}
	b.WriteString("Личные сообщения с сайта приходят " + l.whereIs(userID, win) + ".\n" + msgScanPrice)
	if len(acc) < 2 {
		return b.String()
	}
	b.WriteString("\n" + msgDeliveryOne)
	if win.Delivery != store.DeliveryOn {
		// Явного «носи сюда» не было ни у кого: получателя выбрало правило, и
		// честнее сказать какое, чем делать вид, что это решение человека.
		b.WriteString("\nПока выбор не сделан, ношу туда, где вход свежее.")
	}
	return b.String()
}

// whereIs — как назвать нынешнего получателя ЛС. Второй аккаунт в ТОМ ЖЕ
// мессенджере — не выдумка: один сайт-логин под двумя телеграмами и был тем
// случаем, из-за которого настройка появилась, и «приходят сюда» там врёт.
func (l *Logic) whereIs(userID int64, win store.TalksOwner) string {
	switch {
	case win.Messenger == l.messenger && win.UserID == userID:
		return "сюда, в " + messengerName(l.messenger)
	case win.Messenger == l.messenger:
		return "в другой ваш аккаунт " + messengerName(win.Messenger)
	default:
		return "в " + messengerName(win.Messenger)
	}
}

// deliveryKeyboard — выбор кнопкой. Кнопку, которая описывает нынешнее
// состояние, не показываем: нажимать её незачем.
//
// Кнопки «не присылать сюда» здесь нет, хотя выбор доставки её предполагает:
// согласие всегда даётся кнопкой «читать и присылать сюда», а она гасит
// доставку остальным сессиям аккаунта, — значит «не присылать сюда» гасила бы
// последнюю живую и останавливала обход целиком. Ровно это и делает «не читать
// мою переписку», только называется честно. Сам глагол остаётся рабочим:
// кнопки прошлых релизов ещё висят в чатах.
func (l *Logic) deliveryKeyboard(userID int64, acc []store.TalksOwner) *kbd.Keyboard {
	if !store.ScanAllowed(acc) {
		return kbd.New().Row(kbd.Button{Text: btnScanHere, Payload: kbd.Pack(verbDelivSet, argDeliveryOn)})
	}
	me := ownerIn(acc, l.messenger, userID)
	win, ok := store.PickDelivery(acc)
	var row []kbd.Button
	if !(ok && win.Messenger == l.messenger && win.UserID == userID && me.Delivery == store.DeliveryOn) {
		row = append(row, kbd.Button{Text: btnDeliverHere, Payload: kbd.Pack(verbDelivSet, argDeliveryOn)})
	}
	// Отказ от чтения — своей строкой: он про другое, чем выбор мессенджера.
	return kbd.New().Row(row...).Row(kbd.Button{Text: btnScanOff, Payload: kbd.Pack(verbScanOff, "")})
}

// ownerIn — строка сессии самого спрашивающего. Пустая — сессии нет.
func ownerIn(acc []store.TalksOwner, messenger string, userID int64) store.TalksOwner {
	for _, o := range acc {
		if o.Messenger == messenger && o.UserID == userID {
			return o
		}
	}
	return store.TalksOwner{}
}

// switchedOffText перечисляет, кому выключили доставку. Сессии самого
// спрашивающего в списке нет и быть не может, поэтому свой же мессенджер здесь
// означает второй аккаунт человека — так его и называем.
func (l *Logic) switchedOffText(off []store.TalksOwner) string {
	seen := make(map[string]bool, len(off))
	names := make([]string, 0, len(off))
	for _, o := range off {
		name := "в " + messengerName(o.Messenger)
		if o.Messenger == l.messenger {
			name = "в другой ваш аккаунт " + messengerName(o.Messenger)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// messengerName — как называть мессенджер в тексте для человека.
func messengerName(messenger string) string {
	switch messenger {
	case store.MessengerTelegram:
		return "Telegram"
	case store.MessengerMax:
		return "MAX"
	}
	return messenger
}

// loginHint — где входить на сайт: у бота переписки своего /login нет.
func (l *Logic) loginHint() string {
	if l.talksOnly {
		return "Вход — у основного бота (/login)."
	}
	return "Войти: /login"
}
