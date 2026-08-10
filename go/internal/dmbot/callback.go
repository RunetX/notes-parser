package dmbot

// Кнопки под сообщениями: сборка клавиатур и роутер нажатий. Слой не добавляет
// бизнес-логики — он ставит состояния и правит сообщения, а работу делают те же
// обработчики, что и у слэш-команд.
//
// Идемпотентность держится на dialog_states: повторное нажатие перечитывает
// состояние и либо повторяет то же безвредное действие, либо отвечает «уже
// сделано». Отдельной таблицы обработанных нажатий нет и не нужно — у нажатия,
// в отличие от реплая на сайт, нет побочного эффекта, который нельзя вывести из
// состояния. Если такой глагол однажды появится (подписка кнопкой из эпика B),
// вход готов: store.TryMarkReplyProcessed с ключом «cb:<mid>:<payload>», как
// talks уже делает с префиксом «dm:».

import (
	"context"
	"sync"

	"lovegw/internal/kbd"
)

// Глаголы нажатий. Аргумент — только идентификатор или перечисление: в payload
// влезает 64 байта, свободному пользовательскому тексту там не место.
const (
	verbLogin  = "login"
	verbNote   = "note" // без аргумента — спросить авторство; own/anon — выбор
	verbStatus = "status"
	verbSubs   = "subs"
	verbTalks  = "talks"
	verbCancel = "cancel"
	verbNews   = "news" // аргумент — id черновика новости
)

const (
	argNoteOwn  = "own"
	argNoteAnon = "anon"
)

const (
	msgStaleButton = "Кнопка устарела, наберите /start"
	msgCancelled   = "Отменено."
	btnCancel      = "✖ Отмена"
)

// verbHandler — строка таблицы глаголов. ack показывается сразу, до работы:
// иначе публикация в каналы (секунды под лимитерами) успела бы просрочить
// нажатие, и «спиннер» у пользователя остался бы навсегда.
type verbHandler struct {
	ack   string // тост нажавшему ("" — ответить молча)
	talks bool   // доступен ли боту переписки
	fn    func(l *Logic, ctx context.Context, userID int64, cb kbd.Callback, arg string)
}

// callbackVerbs — таблица, а не switch: у кнопки три свойства (что ответить,
// кому доступна, что делает), и таблица не даёт забыть ни одного.
var callbackVerbs = map[string]verbHandler{
	verbLogin:  {fn: (*Logic).cbLogin},
	verbNote:   {fn: (*Logic).cbNote},
	verbStatus: {fn: (*Logic).cbStatus},
	verbSubs:   {fn: (*Logic).cbSubs},
	verbTalks:  {talks: true, fn: (*Logic).cbTalks},
	verbCancel: {ack: "Отменил", talks: true, fn: (*Logic).cbCancel},
	verbNews:   {ack: "Публикую…", fn: (*Logic).cbNews},
}

// HandleCallback обрабатывает нажатие кнопки — зеркало HandleText. Ответить
// мессенджеру нужно всегда, поэтому ответ живёт в роутере, а не в обработчиках.
func (l *Logic) HandleCallback(ctx context.Context, userID int64, cb kbd.Callback) {
	verb, arg, ok := kbd.Parse(cb.Payload)
	h, known := callbackVerbs[verb]
	if !ok || !known {
		// Кнопка прошлого релиза или мусор: состояние не трогаем, отвечаем.
		l.log.Debug("нажатие с неизвестным payload", "user", userID, "payload", cb.Payload)
		l.tr.AnswerCallback(ctx, cb, msgStaleButton)
		return
	}
	if l.talksOnly && !h.talks {
		l.tr.AnswerCallback(ctx, cb, "Здесь только личная переписка сайта")
		return
	}
	l.tr.AnswerCallback(ctx, cb, h.ack)
	defer lockUser(userID)()
	h.fn(l, ctx, userID, cb, arg)
}

// callbackLocks — сериализация нажатий одного пользователя. go-telegram/bot
// запускает обработчик каждого апдейта в своей горутине, и два быстрых тапа по
// «Опубликовать» успели бы оба прочитать черновик до того, как первый пометит
// новость отправленной в message_targets — в канале появилось бы два поста.
// Шардированный массив, а не map: чистить нечего и расти нечему.
var callbackLocks [64]sync.Mutex

func lockUser(userID int64) func() {
	mu := &callbackLocks[uint64(userID)%uint64(len(callbackLocks))]
	mu.Lock()
	return mu.Unlock
}

// replace переписывает сообщение с кнопками итогом нажатия. Сообщение бывает
// недоступно (удалено в MAX, слишком старое в Telegram) — тогда досылаем
// обычным сообщением, чтобы диалог не оборвался молчанием.
func (l *Logic) replace(ctx context.Context, userID int64, cb kbd.Callback, text string, kb *kbd.Keyboard) {
	if cb.MessageID == "" {
		l.tr.SendKeyboard(ctx, userID, text, kb)
		return
	}
	l.tr.EditMessage(ctx, userID, cb.MessageID, text, kb)
}

func (l *Logic) cbLogin(ctx context.Context, userID int64, _ kbd.Callback, _ string) {
	l.setState(ctx, userID, stateAwaitCredentials)
	l.tr.SendKeyboard(ctx, userID, msgAskCredentials, cancelKeyboard())
}

// cbNote: без аргумента — спрашиваем авторство, с аргументом — выбор сделан, и
// вопрос превращается в приглашение (вторая ветка исчезает, чтобы её не нажали
// неделю спустя).
func (l *Logic) cbNote(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	switch arg {
	case argNoteOwn:
		l.setState(ctx, userID, stateAwaitNote)
		l.replace(ctx, userID, cb, msgAskNote, cancelKeyboard())
	case argNoteAnon:
		l.setState(ctx, userID, stateAwaitAnonNote)
		l.replace(ctx, userID, cb, msgAskAnonNote, cancelKeyboard())
	default:
		l.askNoteKind(ctx, userID)
	}
}

func (l *Logic) cbStatus(ctx context.Context, userID int64, _ kbd.Callback, _ string) {
	l.handleStatus(ctx, userID)
}

func (l *Logic) cbSubs(ctx context.Context, userID int64, _ kbd.Callback, _ string) {
	l.handleMySubs(ctx, userID)
}

func (l *Logic) cbTalks(ctx context.Context, userID int64, _ kbd.Callback, _ string) {
	l.handleTalks(ctx, userID)
}

func (l *Logic) cbCancel(ctx context.Context, userID int64, cb kbd.Callback, _ string) {
	if err := l.st.ClearDialogState(ctx, l.stateNS, userID); err != nil {
		l.log.Error("снятие состояния диалога", "user", userID, "err", err)
	}
	l.replace(ctx, userID, cb, msgCancelled, nil)
}

// askNoteKind спрашивает авторство заметки. Состояние ставим до вопроса:
// набранный вместо нажатия текст должен получить подсказку, а не «Не понимаю».
func (l *Logic) askNoteKind(ctx context.Context, userID int64) {
	l.setState(ctx, userID, stateAwaitNoteKind)
	l.tr.SendKeyboard(ctx, userID, msgAskNoteKind, noteKindKeyboard())
}

// mainMenu — главное меню бота. У бота переписки своё, короткое: вход, заметки
// и подписки живут у бота команд.
func mainMenu(talksOnly, withTalks bool) *kbd.Keyboard {
	if talksOnly {
		return kbd.New().Row(
			kbd.Button{Text: "💬 Мои диалоги", Payload: kbd.Pack(verbTalks, "")},
			kbd.Button{Text: "✖ Выйти из диалога", Payload: kbd.Pack(verbCancel, "")},
		)
	}
	kb := kbd.New().Row(
		kbd.Button{Text: "🔑 Войти", Payload: kbd.Pack(verbLogin, "")},
		kbd.Button{Text: "ℹ️ Статус", Payload: kbd.Pack(verbStatus, "")},
	).Row(
		kbd.Button{Text: "✍️ Написать заметку", Payload: kbd.Pack(verbNote, "")},
	)
	last := []kbd.Button{{Text: "🔔 Подписки", Payload: kbd.Pack(verbSubs, "")}}
	if withTalks {
		last = append(last, kbd.Button{Text: "💬 Переписка", Payload: kbd.Pack(verbTalks, "")})
	}
	return kb.Row(last...)
}

func cancelKeyboard() *kbd.Keyboard {
	return kbd.New().Row(kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")})
}

func noteKindKeyboard() *kbd.Keyboard {
	return kbd.New().Row(
		kbd.Button{Text: "От своего имени", Payload: kbd.Pack(verbNote, argNoteOwn)},
		kbd.Button{Text: "Анонимно", Payload: kbd.Pack(verbNote, argNoteAnon)},
	).Row(kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")})
}

// newsKeyboard — подтверждение публикации новости. id черновика едет в payload:
// нажатие на кнопку от старого черновика не должно опубликовать новый.
func newsKeyboard(id string) *kbd.Keyboard {
	return kbd.New().Row(
		kbd.Button{Text: "📣 Опубликовать", Payload: kbd.Pack(verbNews, id)},
		kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")},
	)
}
