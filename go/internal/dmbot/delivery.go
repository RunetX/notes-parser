package dmbot

// Настройка «куда носить личные сообщения сайта» (/delivery). Один сайт-аккаунт
// получает ЛС ровно в одном мессенджере: сайт помечает сообщение прочитанным в
// тот момент, когда поллер забирает историю, и во втором мессенджере его уже не
// будет. Выбор исключающий — включение здесь гасит доставку там, это делает
// store.SetTalksDelivery одной транзакцией.
//
// Вопрос задаёт не только человек: поллер talks, увидев один сайт-аккаунт в
// двух мессенджерах, один раз зовёт AskDelivery — то же сообщение с теми же
// кнопками, где нажмут, туда и понесём.

import (
	"context"
	"errors"
	"strings"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

const (
	btnDelivery    = "📬 Куда слать ЛС"
	btnDeliverHere = "📬 Присылать сюда"
	btnDeliverOff  = "🔕 Не присылать сюда"
	// msgDeliveryOne — почему выбор вообще приходится делать. Текст один на все
	// входы: и на вопрос от поллера, и на показ настройки.
	msgDeliveryOne = "Вы вошли на сайт не только здесь, а носить личные сообщения " +
		"я могу только в один мессенджер: сайт помечает сообщение прочитанным, как " +
		"только я его забираю, — во втором его уже не будет."
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

// AskDelivery спрашивает человека, куда носить его личные сообщения: сайт-аккаунт
// залогинен в нескольких мессенджерах. Зовёт поллер talks (Config.AskDelivery)
// — один раз на человека, дальше настройка живёт под /delivery. current — та
// сессия, куда носим до ответа.
func (l *Logic) AskDelivery(ctx context.Context, userID int64, current store.TalksOwner) {
	l.tr.SendKeyboard(ctx, userID,
		"Про личные сообщения с сайта.\n"+msgDeliveryOne+
			"\nСейчас они приходят "+l.whereIs(userID, current)+". Где их получать?",
		kbd.New().Row(
			kbd.Button{Text: btnDeliverHere, Payload: kbd.Pack(verbDelivSet, argDeliveryOn)},
			kbd.Button{Text: btnDeliverOff, Payload: kbd.Pack(verbDelivSet, argDeliveryOff)},
		))
}

// deliveryText — куда идут ЛС сейчас и почему.
func (l *Logic) deliveryText(userID int64, acc []store.TalksOwner) string {
	win, ok := store.PickDelivery(acc)
	var b strings.Builder
	if !ok {
		b.WriteString("Личные сообщения с сайта я вам не приношу — доставка выключена везде.")
	} else {
		b.WriteString("Личные сообщения с сайта приходят " + l.whereIs(userID, win) + ".")
	}
	if len(acc) < 2 {
		return b.String()
	}
	b.WriteString("\n" + msgDeliveryOne)
	if ok && win.Delivery != store.DeliveryOn {
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
func (l *Logic) deliveryKeyboard(userID int64, acc []store.TalksOwner) *kbd.Keyboard {
	me := ownerIn(acc, l.messenger, userID)
	win, ok := store.PickDelivery(acc)
	var row []kbd.Button
	if !(ok && win.Messenger == l.messenger && win.UserID == userID && me.Delivery == store.DeliveryOn) {
		row = append(row, kbd.Button{Text: btnDeliverHere, Payload: kbd.Pack(verbDelivSet, argDeliveryOn)})
	}
	if me.Delivery != store.DeliveryOff {
		row = append(row, kbd.Button{Text: btnDeliverOff, Payload: kbd.Pack(verbDelivSet, argDeliveryOff)})
	}
	return kbd.New().Row(row...)
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
