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
	"strconv"
	"strings"
	"sync"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// Глаголы нажатий. Аргумент — только идентификатор или перечисление: в payload
// влезает 64 байта, свободному пользовательскому тексту там не место.
const (
	verbLogin  = "login"
	verbNote   = "note" // без аргумента — спросить авторство; own/anon — выбор
	verbStatus = "status"
	verbSubs   = "subs"
	verbSubAdd = "subadd" // добавить подписку: спросить слово диалогом
	verbUnsub  = "unsub"  // аргумент — id строки подписки (цель в payload не влезет)
	verbTalks  = "talks"  // аргумент — номер страницы списка диалогов
	verbTalk   = "talk"   // аргумент — id собеседника
	verbCancel = "cancel"
	verbNews   = "news" // аргумент — id черновика новости
	// Подписка по заметке. Первый глагол приезжает с кнопки под постом канала —
	// он единственный публичный (см. поле public); остальные два уже из ЛС.
	verbSubscribe   = kbd.VerbSubscribe // аргумент — id заметки
	verbSubAuthor   = "suba"            // аргумент — id заметки: подписать на её автора
	verbSubComments = "subc"            // аргумент — id заметки: подписать на её комментарии
	verbUnsubOne    = "unsub1"          // аргумент — id подписки: снятие прямо из уведомления
)

const (
	argNoteOwn  = "own"
	argNoteAnon = "anon"
)

const (
	msgStaleButton = "Кнопка устарела, наберите /start"
	msgCancelled   = "Отменено."
	btnCancel      = "✖ Отмена"
	btnUnsubOne    = "🔕 Отписаться"
	// buttonTextRunes — потолок подписи кнопки: длинное слово подписки растянет
	// клавиатуру и в узком клиенте обрежется как попало.
	buttonTextRunes = 24
	// subLineRunes — потолок цитаты заметки в строке списка подписок.
	subLineRunes = 48
	// talksPageSize — диалогов на странице списка.
	talksPageSize = 8
)

// verbHandler — строка таблицы глаголов. ack показывается сразу, до работы:
// иначе публикация в каналы (секунды под лимитерами) успела бы просрочить
// нажатие, и «спиннер» у пользователя остался бы навсегда.
type verbHandler struct {
	ack   string // тост нажавшему ("" — ответить молча)
	talks bool   // доступен ли боту переписки
	// public — доступен ли вне диалога. Такую кнопку видят все читатели канала,
	// поэтому обработчик не вправе трогать сообщение, под которым она висит
	// (это чужой пост), и отвечает всегда новым сообщением в ЛС.
	public bool
	fn     func(l *Logic, ctx context.Context, userID int64, cb kbd.Callback, arg string)
}

// callbackVerbs — таблица, а не switch: у кнопки четыре свойства (что ответить,
// кому доступна, где доступна, что делает), и таблица не даёт забыть ни одного.
var callbackVerbs = map[string]verbHandler{
	verbLogin:       {fn: (*Logic).cbLogin},
	verbNote:        {fn: (*Logic).cbNote},
	verbStatus:      {fn: (*Logic).cbStatus},
	verbSubs:        {fn: (*Logic).cbSubs},
	verbSubAdd:      {fn: (*Logic).cbSubAdd},
	verbUnsub:       {ack: "Отписал", fn: (*Logic).cbUnsub},
	verbTalks:       {talks: true, fn: (*Logic).cbTalks},
	verbTalk:        {talks: true, fn: (*Logic).cbTalk},
	verbCancel:      {ack: "Отменил", talks: true, fn: (*Logic).cbCancel},
	verbNews:        {ack: "Публикую…", fn: (*Logic).cbNews},
	verbSubscribe:   {ack: "Открыл выбор в личке", public: true, fn: (*Logic).cbSubscribe},
	verbSubAuthor:   {ack: "Подписал", fn: (*Logic).cbSubAuthor},
	verbSubComments: {ack: "Подписал", fn: (*Logic).cbSubComments},
	verbUnsubOne:    {ack: "Отписал", fn: (*Logic).cbUnsubOne},
}

// AllowsOutsideDialog — разрешён ли payload вне диалога. Спрашивает транспорт
// (maxx) про нажатие из канала: иначе ЛС-глаголы — вход, заметки, переписка —
// стали бы доступны нажатием на кнопку под чужим сообщением.
func (l *Logic) AllowsOutsideDialog(payload string) bool {
	verb, _, ok := kbd.Parse(payload)
	return ok && callbackVerbs[verb].public
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
	l.handleMySubs(ctx, userID, nil)
}

// cbSubAdd — «Добавить подписку»: слово спрашиваем диалогом, в payload его
// класть нельзя (64 байта, кириллица по два на знак).
func (l *Logic) cbSubAdd(ctx context.Context, userID int64, _ kbd.Callback, _ string) {
	l.askSubscription(ctx, userID)
}

// cbUnsub снимает подписку по id строки и перерисовывает список на месте —
// снять несколько подряд удобнее, чем открывать список заново.
func (l *Logic) cbUnsub(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		l.log.Debug("нажатие отписки с нечисловым id", "user", userID, "arg", arg)
		return
	}
	// Результат не проверяем: подписки уже нет — список всё равно перерисуем,
	// и пользователь увидит верное состояние (повторный тап безвреден).
	if _, _, err := l.st.RemoveSubscriptionByID(ctx, l.messenger, userID, id); err != nil {
		l.log.Error("отписка кнопкой", "user", userID, "sub", id, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	l.handleMySubs(ctx, userID, &cb)
}

// cbSubscribe — кнопка «Подписаться» под постом канала: показываем выбор вида.
// Сообщение под кнопкой — пост канала, а не наше, поэтому ответ всегда уходит
// новым сообщением в ЛС (transport гасит и MessageID, но правило держим явно).
func (l *Logic) cbSubscribe(ctx context.Context, userID int64, _ kbd.Callback, arg string) {
	l.offerSubscribe(ctx, userID, arg)
}

// cbSubAuthor подписывает на заметки автора этой заметки.
func (l *Logic) cbSubAuthor(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	n, ok := l.noteForSubscription(ctx, userID, arg)
	if !ok {
		return
	}
	if n.AuthorID == "" || n.AuthorID == "0" {
		l.replace(ctx, userID, cb, "Заметка анонимная — на автора подписать не могу.", nil)
		return
	}
	l.addSubscription(ctx, userID, cb, store.SubAuthorNotes, n.AuthorID,
		"Подписал на заметки автора "+n.AuthorName+". Как появится новая — пришлю ссылку.",
		"Вы уже подписаны на заметки автора "+n.AuthorName+".")
}

// cbSubComments подписывает на комментарии этой заметки.
func (l *Logic) cbSubComments(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	n, ok := l.noteForSubscription(ctx, userID, arg)
	if !ok {
		return
	}
	title := shorten(oneLine(n.AuthorName+": "+n.Text), subLineRunes)
	l.addSubscription(ctx, userID, cb, store.SubNoteComments, n.ID,
		"Подписал на комментарии заметки «"+title+"». Пока заметка живая, буду "+
			"присылать новые; уйдёт в архив — сниму подписку сам.",
		"Вы уже подписаны на комментарии заметки «"+title+"».")
}

// cbUnsubOne снимает подписку прямо из уведомления. Сообщение не правим: текст
// со ссылкой на комментарий ещё пригодится, а в MAX снять одну клавиатуру, не
// переписав тело целиком, нечем. Повторное нажатие безвредно.
func (l *Logic) cbUnsubOne(ctx context.Context, userID int64, _ kbd.Callback, arg string) {
	id, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		l.log.Debug("нажатие отписки с нечисловым id", "user", userID, "arg", arg)
		return
	}
	if _, _, err := l.st.RemoveSubscriptionByID(ctx, l.messenger, userID, id); err != nil {
		l.log.Error("отписка из уведомления", "user", userID, "sub", id, "err", err)
	}
}

// cbTalks — список диалогов; аргумент — номер страницы. Без аргумента список
// открывают из главного меню, и он приходит новым сообщением: меню — пульт,
// его затирать нельзя. Перелистывание правит уже показанный список.
func (l *Logic) cbTalks(ctx context.Context, userID int64, cb kbd.Callback, arg string) {
	if arg == "" {
		l.showTalks(ctx, userID, 0, nil)
		return
	}
	page, _ := strconv.Atoi(arg) // мусор — первая страница
	l.showTalks(ctx, userID, page, &cb)
}

// cbTalk открывает диалог: дальше текст уходит собеседнику (как /talk N).
func (l *Logic) cbTalk(ctx context.Context, userID int64, _ kbd.Callback, arg string) {
	l.handleTalkOpen(ctx, userID, arg)
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

// PublishCommands публикует меню команд бота под его роль. Зовётся один раз на
// старте; сбой не фатален — команды просто не появятся в списке мессенджера.
func (l *Logic) PublishCommands(ctx context.Context) {
	l.tr.SetCommands(ctx, botCommands(l.talksOnly, l.talks != nil))
}

// botCommands — тот же набор, что и в приветствии: слэш-команды остаются
// рабочими, кнопки их не отменяют. Админская /news в меню не значится.
func botCommands(talksOnly, withTalks bool) []kbd.Command {
	if talksOnly {
		return []kbd.Command{
			{Name: "start", Description: "начать и показать меню"},
			{Name: "talks", Description: "мои диалоги на сайте"},
			{Name: "talk", Description: "писать в выбранный диалог"},
			{Name: "cancel", Description: "выйти из диалога"},
		}
	}
	cmds := []kbd.Command{
		{Name: "start", Description: "начать и показать меню"},
		{Name: "login", Description: "войти на сайт НГС.Лав"},
		{Name: "add_note", Description: "добавить заметку"},
		{Name: "add_anonymous_note", Description: "добавить анонимную заметку"},
		{Name: "status", Description: "проверить сессию сайта"},
		{Name: "subscribe", Description: "подписаться на слово"},
		{Name: "unsubscribe", Description: "отписаться от слова"},
		{Name: "mysubs", Description: "мои подписки"},
	}
	if withTalks {
		cmds = append(cmds,
			kbd.Command{Name: "talks", Description: "мои личные диалоги на сайте"},
			kbd.Command{Name: "talk", Description: "писать в выбранный диалог"})
	}
	return append(cmds, kbd.Command{Name: "cancel", Description: "отменить текущий шаг"})
}

// askSubscription спрашивает ключевое слово подписки.
func (l *Logic) askSubscription(ctx context.Context, userID int64) {
	l.setState(ctx, userID, stateAwaitSubscription)
	l.tr.SendKeyboard(ctx, userID, msgAskSubscription, cancelKeyboard())
}

// subIcon — вид подписки одним знаком: список из трёх видов иначе не читается.
func subIcon(kind string) string {
	switch kind {
	case store.SubAuthorNotes:
		return "✍️"
	case store.SubNoteComments:
		return "💬"
	default:
		return "🔔"
	}
}

// subTarget — как назвать цель подписки. Label считает стор одним запросом;
// пусто — заметка удалена или автор не виден, тогда честно показываем номер.
func subTarget(s store.Subscription) string {
	if s.Label != "" {
		return s.Label
	}
	switch s.Kind {
	case store.SubAuthorNotes:
		return "автор #" + s.Target
	case store.SubNoteComments:
		return "заметка #" + s.Target
	default:
		return s.Target
	}
}

// subLines — строки текстового списка подписок.
func subLines(subs []store.Subscription) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		switch s.Kind {
		case store.SubAuthorNotes:
			out = append(out, subIcon(s.Kind)+" заметки автора "+subTarget(s))
		case store.SubNoteComments:
			out = append(out, subIcon(s.Kind)+" комментарии к заметке «"+
				shorten(subTarget(s), subLineRunes)+"»")
		default:
			out = append(out, subIcon(s.Kind)+" слово «"+s.Target+"»")
		}
	}
	return out
}

// subsKeyboard — по кнопке «✖» на подписку и «Добавить» внизу. Подпись
// обрезаем: цель в кнопку целиком не влезет, а payload несёт id строки.
func subsKeyboard(subs []store.Subscription) *kbd.Keyboard {
	kb := kbd.New()
	for _, s := range subs {
		kb.Row(kbd.Button{
			Text:    "✖ " + subIcon(s.Kind) + " " + shorten(subTarget(s), buttonTextRunes),
			Payload: kbd.Pack(verbUnsub, strconv.FormatInt(s.ID, 10)),
		})
	}
	return kb.Row(kbd.Button{Text: "➕ Добавить", Payload: kbd.Pack(verbSubAdd, "")})
}

// subKindKeyboard — выбор вида подписки по заметке. У анонимной заметки
// подписывать не на кого: вариант «на автора» просто не появляется (пустая
// строка кнопок клавиатурой игнорируется).
func subKindKeyboard(n store.Note) *kbd.Keyboard {
	var row []kbd.Button
	if n.AuthorID != "" && n.AuthorID != "0" {
		row = append(row, kbd.Button{Text: "✍️ На автора", Payload: kbd.Pack(verbSubAuthor, n.ID)})
	}
	row = append(row, kbd.Button{Text: "💬 На эту заметку", Payload: kbd.Pack(verbSubComments, n.ID)})
	return kbd.New().Row(row...).Row(kbd.Button{Text: btnCancel, Payload: kbd.Pack(verbCancel, "")})
}

// UnsubKeyboard — кнопка «Отписаться» под уведомлением подписчика.
// Экспортируется для runDaemon: текст уведомления собирает композер
// мессенджера, а глагол кнопки — наш.
func UnsubKeyboard(subID int64) *kbd.Keyboard {
	return kbd.New().Row(kbd.Button{
		Text:    btnUnsubOne,
		Payload: kbd.Pack(verbUnsubOne, strconv.FormatInt(subID, 10)),
	})
}

// talksPage нарезает список диалогов на страницу. Возвращает саму страницу,
// исправленный номер (мусор из payload и выход за край чинятся молча) и общее
// число страниц.
func talksPage(peers []store.TalkPeer, page int) ([]store.TalkPeer, int, int) {
	pages := (len(peers) + talksPageSize - 1) / talksPageSize
	if page < 0 || page >= pages {
		page = 0
	}
	end := min((page+1)*talksPageSize, len(peers))
	return peers[page*talksPageSize : end], page, pages
}

// talksKeyboard — кнопка на диалог плюс перелистывание, когда страниц больше
// одной.
func talksKeyboard(peers []store.TalkPeer, page, pages int) *kbd.Keyboard {
	kb := kbd.New()
	for _, p := range peers {
		kb.Row(kbd.Button{
			Text:    "💬 " + shorten(nickOrPassport(p), buttonTextRunes),
			Payload: kbd.Pack(verbTalk, strconv.FormatInt(p.ID, 10)),
		})
	}
	if pages <= 1 {
		return kb
	}
	var nav []kbd.Button
	if page > 0 {
		nav = append(nav, kbd.Button{Text: "← Назад",
			Payload: kbd.Pack(verbTalks, strconv.Itoa(page-1))})
	}
	if page < pages-1 {
		nav = append(nav, kbd.Button{Text: "Вперёд →",
			Payload: kbd.Pack(verbTalks, strconv.Itoa(page+1))})
	}
	return kb.Row(nav...)
}

// shorten укорачивает подпись кнопки по рунам (кириллица — не байты).
func shorten(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

// oneLine сводит текст в одну строку: заметка в подписи и в списке подписок
// упоминается одной строкой, а в ней бывают абзацы.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
