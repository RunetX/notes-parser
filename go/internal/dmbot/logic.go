package dmbot

// Logic — мессенджер-агностичное ядро ЛС-диалогов РюмкинЪ: команды,
// машина состояний (в dialog_states), сессии сайта и подписки. Транспорт
// конкретного мессенджера (отправка/удаление ЛС) — за интерфейсом Transport;
// телеграм-обёртка в dmbot.go, MAX ходит через maxx.Mirror.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/love"
	"lovegw/internal/news"
	"lovegw/internal/store"
)

// Transport — отправка и удаление личных сообщений в мессенджере, плюс
// кнопки под сообщением. Id сообщений строковые (Telegram — число строкой,
// MAX — mid).
type Transport interface {
	Send(ctx context.Context, userID int64, text string)
	DeleteMessage(ctx context.Context, userID int64, messageID string)
	// SendKeyboard — сообщение с кнопками; текст без разметки, как у Send.
	SendKeyboard(ctx context.Context, userID int64, text string, kb *kbd.Keyboard)
	// AnswerCallback гасит «спиннер» у нажавшего. Зовётся ДО работы: публикация
	// в каналы идёт через лимитеры и занимает секунды, а нажатие к тому времени
	// протухнет. Пустой toast — ответить молча.
	AnswerCallback(ctx context.Context, cb kbd.Callback, toast string)
	// EditMessage переписывает уже отправленное сообщение; kb == nil — убрать
	// кнопки. Текст обязателен: в MAX клавиатура — вложение сообщения, и снять
	// её отдельно, как editMessageReplyMarkup, там нечем.
	EditMessage(ctx context.Context, userID int64, messageID, text string, kb *kbd.Keyboard)
}

type Logic struct {
	st        *store.Store
	site      SiteAuth
	tr        Transport
	messenger string
	talks     TalkRouter // личная переписка сайта; nil — выключена
	// news + adminID — публикация внутренних новостей проекта (/news).
	// nil/0 — команды нет: она админская и в списке команд не значится.
	news    *news.Service
	adminID int64
	log     *slog.Logger
	// talksOnly — движок бота переписки: из команд живут только /start,
	// /talks, /talk и /cancel, остальное отправляем к боту команд.
	talksOnly bool
	// stateNS — пространство ключей dialog_states. У бота переписки своё:
	// user_id в мессенджере общий на обоих ботов, и без разделения залипшее
	// «pm:<id>» ломало бы ввод логина/заметки у бота команд.
	stateNS string
}

// talksCommands — что умеет бот переписки; остальные команды он отфутболивает
// к боту команд, чтобы у пользователя не двоились /login и заметки.
var talksCommands = map[string]bool{"/start": true, "/talks": true, "/talk": true, "/cancel": true}

// SetTalkRouter подключает роутер личной переписки (в runDaemon после сборки
// поллера talks). Для MAX ставится на maxDM-логику, для Telegram — через Bot.
func (l *Logic) SetTalkRouter(r TalkRouter) { l.talks = r }

// SetNews подключает публикацию новостей проекта: /news станет доступна
// пользователю adminID. Пустая служба или нулевой admin — команды нет.
func (l *Logic) SetNews(svc *news.Service, adminID int64) {
	if !svc.Ready() || adminID == 0 {
		return
	}
	l.news, l.adminID = svc, adminID
}

// SiteIdentifier (опц.) снимает site-идентичность владельца сессии со страницы
// сайта. Реализуется love.Client в Ф4; до этого идентичность не заполняется.
type SiteIdentifier interface {
	SiteIdentity(ctx context.Context, cookies []*http.Cookie) (profileID, passportID, nick string, err error)
}

// NewLogic создаёт диалоговый движок бота команд для одного мессенджера.
func NewLogic(st *store.Store, site SiteAuth, tr Transport, messenger string, log *slog.Logger) *Logic {
	if log == nil {
		log = slog.Default()
	}
	return &Logic{st: st, site: site, tr: tr, messenger: messenger, log: log, stateNS: messenger}
}

// NewTalksLogic создаёт движок бота личной переписки: сайт ему не нужен
// (вход и заметки живут у бота команд), состояния — в своём пространстве.
// Сессии, подписки и диалоги читаются по messenger, а не по боту, поэтому
// переписка видит вход, сделанный пользователем у бота команд.
func NewTalksLogic(st *store.Store, tr Transport, messenger string, log *slog.Logger) *Logic {
	if log == nil {
		log = slog.Default()
	}
	return &Logic{
		st: st, tr: tr, messenger: messenger, log: log,
		talksOnly: true, stateNS: messenger + ":talks",
	}
}

// Greet шлёт приветствие со списком команд и главным меню кнопками (на /start
// и первое открытие диалога — bot_started в MAX). Список команд остаётся: он и
// справка, и обратная совместимость для тех, кто привык набирать.
func (l *Logic) Greet(ctx context.Context, userID int64) {
	l.tr.SendKeyboard(ctx, userID, startMessage(l.talksOnly, l.talks != nil),
		mainMenu(l.talksOnly, l.talks != nil))
}

// HandleText обрабатывает входящее ЛС. messageID нужен для удаления
// сообщения с логином/паролем.
func (l *Logic) HandleText(ctx context.Context, userID int64, messageID, text string) {
	if text == "" {
		return
	}
	state, err := l.st.DialogState(ctx, l.stateNS, userID)
	if err != nil {
		l.log.Error("чтение состояния диалога", "user", userID, "err", err)
		return
	}

	// Команда прерывает любой текущий диалог.
	if cmd := command(text); cmd != "" {
		if state != "" {
			_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
		}
		l.handleCommand(ctx, userID, cmd, messageID, text)
		return
	}
	if state == "" {
		l.tr.Send(ctx, userID, "Не понимаю. Наберите /start, чтобы увидеть команды")
		return
	}
	l.handleStateInput(ctx, userID, state, messageID, text)
}

func (l *Logic) handleCommand(ctx context.Context, userID int64, cmd, messageID, text string) {
	if l.talksOnly && !talksCommands[cmd] {
		l.tr.Send(ctx, userID, "Здесь только личная переписка сайта: /talks, /talk <номер>. "+
			"Вход, заметки и подписки — у основного бота.")
		return
	}
	switch cmd {
	case "/start":
		l.Greet(ctx, userID)
	case "/login":
		l.setState(ctx, userID, stateAwaitCredentials)
		l.tr.SendKeyboard(ctx, userID, msgAskCredentials, cancelKeyboard())
	case "/add_note":
		l.askNoteKind(ctx, userID)
	case "/add_anonymous_note":
		// Прямая дорога для тех, кто привык: вопрос об авторстве пропускаем.
		l.setState(ctx, userID, stateAwaitAnonNote)
		l.tr.SendKeyboard(ctx, userID, msgAskAnonNote, cancelKeyboard())
	case "/status":
		l.handleStatus(ctx, userID)
	case "/subscribe":
		l.handleSubscribe(ctx, userID, commandArg(text))
	case "/unsubscribe":
		l.handleUnsubscribe(ctx, userID, commandArg(text))
	case "/mysubs":
		l.handleMySubs(ctx, userID)
	case "/talks":
		l.handleTalks(ctx, userID)
	case "/talk":
		l.handleTalkOpen(ctx, userID, commandArg(text))
	case "/news":
		l.handleNews(ctx, userID)
	case "/cancel":
		l.handleCancel(ctx, userID)
	default:
		l.tr.Send(ctx, userID, msgUnknownCommand)
	}
}

// handleTalks показывает список диалогов личной переписки сайта.
func (l *Logic) handleTalks(ctx context.Context, userID int64) {
	if l.talks == nil {
		l.tr.Send(ctx, userID, "Личная переписка сайта не подключена.")
		return
	}
	peers, err := l.st.TalkPeers(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("список диалогов talks", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	if len(peers) == 0 {
		msg := "Пока нет диалогов. Как придёт ЛС с сайта — ответьте на него реплаем."
		if l.talksOnly {
			// Сессии заводит бот команд — без входа диалогов не будет вовсе.
			msg += "\nЕсли вы ещё не входили на сайт, сделайте /login у основного бота."
		}
		l.tr.Send(ctx, userID, msg)
		return
	}
	var b strings.Builder
	b.WriteString("Ваши диалоги (ответить: реплай на сообщение или /talk <номер>):\n")
	for _, p := range peers {
		fmt.Fprintf(&b, "#%d %s\n", p.ID, nickOrPassport(p))
	}
	l.tr.Send(ctx, userID, b.String())
}

// handleTalkOpen «залипает» на диалоге: следующие сообщения уйдут собеседнику.
func (l *Logic) handleTalkOpen(ctx context.Context, userID int64, arg string) {
	if l.talks == nil {
		l.tr.Send(ctx, userID, "Личная переписка сайта не подключена.")
		return
	}
	peerID, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil {
		l.tr.Send(ctx, userID, "Укажите номер диалога: /talk <номер> (список — /talks)")
		return
	}
	peer, err := l.st.TalkPeerByID(ctx, peerID)
	if err != nil || peer.Messenger != l.messenger || peer.OwnerUserID != userID {
		l.tr.Send(ctx, userID, "Диалог не найден. Список — /talks")
		return
	}
	l.setState(ctx, userID, statePMPrefix+strconv.FormatInt(peerID, 10))
	l.tr.SendKeyboard(ctx, userID,
		"Пишу в диалог с "+nickOrPassport(peer)+". Следующие сообщения уйдут ему. /cancel — выйти.",
		cancelKeyboard())
}

// handleCancel выходит из залипшего диалога (и любого другого состояния).
func (l *Logic) handleCancel(ctx context.Context, userID int64) {
	_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	l.tr.Send(ctx, userID, "Ок, вышел из диалога.")
}

// handleSubscribe подписывает пользователя на ключевое слово: как только в
// новом комментарии встретится это слово, придёт уведомление со ссылкой.
func (l *Logic) handleSubscribe(ctx context.Context, userID int64, keyword string) {
	if keyword == "" {
		l.tr.Send(ctx, userID, "Укажите слово: /subscribe <ключевое слово>")
		return
	}
	added, err := l.st.AddSubscription(ctx, l.messenger, keyword, userID)
	if err != nil {
		l.log.Error("подписка", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	if added {
		l.tr.Send(ctx, userID, "Подписал на «"+keyword+"». Уведомлю, когда слово встретится в комментарии.")
	} else {
		l.tr.Send(ctx, userID, "Вы уже подписаны на «"+keyword+"».")
	}
}

// handleUnsubscribe снимает подписку на ключевое слово.
func (l *Logic) handleUnsubscribe(ctx context.Context, userID int64, keyword string) {
	if keyword == "" {
		l.tr.Send(ctx, userID, "Укажите слово: /unsubscribe <ключевое слово>")
		return
	}
	removed, err := l.st.RemoveSubscription(ctx, l.messenger, keyword, userID)
	if err != nil {
		l.log.Error("отписка", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	if removed {
		l.tr.Send(ctx, userID, "Отписал от «"+keyword+"».")
	} else {
		l.tr.Send(ctx, userID, "Такой подписки не было.")
	}
}

// handleMySubs показывает список ключевых слов пользователя.
func (l *Logic) handleMySubs(ctx context.Context, userID int64) {
	keywords, err := l.st.SubscriptionsByUser(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("список подписок", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	if len(keywords) == 0 {
		l.tr.Send(ctx, userID, "У вас нет подписок. Добавить: /subscribe <слово>")
		return
	}
	l.tr.Send(ctx, userID, "Ваши подписки:\n• "+strings.Join(keywords, "\n• "))
}

func (l *Logic) handleStateInput(ctx context.Context, userID int64, state, messageID, text string) {
	// Залипание на диалоге talks: состояние сохраняется, пока не /cancel.
	if peerID, ok := parsePMState(state); ok {
		if l.talks == nil {
			// Переписку увёл к себе отдельный бот: чистим легаси-состояние,
			// иначе пользователь остался бы «залипшим» в диалоге навсегда.
			_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
			l.tr.Send(ctx, userID, "Личная переписка живёт у бота переписки — напишите ему.")
			return
		}
		if !l.talks.SendToDialog(ctx, l.messenger, userID, peerID, messageID, text) {
			l.tr.Send(ctx, userID, "Не удалось отправить в диалог. Список — /talks")
		}
		return
	}
	if l.talksOnly {
		return // прочие состояния — не наша роль
	}
	// Новость админа, ожидающая подтверждения: черновик лежит в состоянии.
	if id, html, ok := parseNewsState(state); ok {
		l.confirmNews(ctx, userID, id, html, text)
		return
	}
	switch state {
	case stateAwaitNoteKind:
		// Ждали нажатия, а пришёл текст: подсказываем, а не роняем «Не понимаю».
		l.tr.SendKeyboard(ctx, userID, "Сначала выберите: от своего имени или анонимно.",
			noteKindKeyboard())
	case stateAwaitCredentials:
		l.tryLogin(ctx, userID, messageID, text)
	case stateAwaitNote:
		l.addNote(ctx, userID, text, false)
		_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	case stateAwaitAnonNote:
		l.addNote(ctx, userID, text, true)
		_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	case stateAwaitNews:
		l.draftNews(ctx, userID, text)
	}
}

// parsePMState разбирает состояние «pm:<peer_id>» → id диалога.
func parsePMState(state string) (int64, bool) {
	if !strings.HasPrefix(state, statePMPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(state[len(statePMPrefix):], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// nickOrPassport — читабельная подпись собеседника.
func nickOrPassport(p store.TalkPeer) string {
	if strings.TrimSpace(p.Nick) != "" {
		return p.Nick
	}
	return "паспорт " + p.PassportID
}

// tryLogin входит на сайт и сохраняет куки. Сообщение с логином/паролем
// удаляется — в отличие от Python-версии, где пароль оставался в истории.
func (l *Logic) tryLogin(ctx context.Context, userID int64, messageID, text string) {
	l.tr.DeleteMessage(ctx, userID, messageID)

	parts := strings.Fields(text)
	if len(parts) != 2 {
		l.tr.Send(ctx, userID, "Нужны логин и пароль через пробел. Попробуйте ещё раз или /login")
		return
	}
	cookies, err := l.site.Login(ctx, parts[0], parts[1])
	if err != nil {
		var le *love.LoginError
		if errors.As(err, &le) {
			l.tr.Send(ctx, userID, "Вход не выполнен: "+le.Errors)
		} else {
			l.log.Warn("ошибка входа", "user", userID, "err", err)
			l.tr.Send(ctx, userID, "Не удалось войти, попробуйте позже. Начать заново: /login")
		}
		return
	}
	cookiesJSON, err := love.CookiesToJSON(cookies, time.Now())
	if err != nil {
		l.log.Error("сериализация кук", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	if err := l.st.UpsertSession(ctx, l.messenger, userID, cookiesJSON, time.Now()); err != nil {
		l.log.Error("сохранение сессии", "user", userID, "err", err)
		l.tr.Send(ctx, userID, "Не удалось сохранить сессию, попробуйте позже")
		return
	}
	l.captureIdentity(ctx, userID, cookies)
	_ = l.st.ClearDialogState(ctx, l.stateNS, userID)
	l.tr.Send(ctx, userID, "Успешный вход. Теперь ваши ответы в обсуждениях попадут на сайт")
}

// captureIdentity снимает site-идентичность владельца сессии (id анкеты,
// паспорт, ник) со страницы сайта, если клиент это умеет (Ф4). Без неё talks
// не связать анкету с паспортом; на ошибке просто не заполняем.
func (l *Logic) captureIdentity(ctx context.Context, userID int64, cookies []*http.Cookie) {
	ident, ok := l.site.(SiteIdentifier)
	if !ok {
		return
	}
	profileID, passportID, nick, err := ident.SiteIdentity(ctx, cookies)
	if err != nil {
		l.log.Warn("site-идентичность не снята", "user", userID, "err", err)
		return
	}
	if err := l.st.SetSessionIdentity(ctx, l.messenger, userID, profileID, passportID, nick); err != nil {
		l.log.Error("сохранение site-идентичности", "user", userID, "err", err)
	}
}

func (l *Logic) addNote(ctx context.Context, userID int64, text string, anonymous bool) {
	cookiesJSON, valid, err := l.st.SessionCookies(ctx, l.messenger, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !valid) {
		l.tr.Send(ctx, userID, "Сначала войдите на сайт: /login")
		return
	}
	if err != nil {
		l.log.Error("чтение сессии", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
		return
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil || len(cookies) == 0 {
		_ = l.st.SetSessionValid(ctx, l.messenger, userID, false, time.Now())
		l.tr.Send(ctx, userID, "Сессия истекла. Сделайте /login ещё раз")
		return
	}
	if err := l.site.PostNote(ctx, cookies, text, anonymous); err != nil {
		l.log.Warn("публикация заметки не удалась", "user", userID, "err", err)
		l.tr.Send(ctx, userID, "Не удалось опубликовать заметку, попробуйте позже")
		return
	}
	_ = l.st.SetSessionValid(ctx, l.messenger, userID, true, time.Now())
	l.tr.Send(ctx, userID, "Заметка отправлена на сайт")
}

func (l *Logic) handleStatus(ctx context.Context, userID int64) {
	_, valid, err := l.st.SessionCookies(ctx, l.messenger, userID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		l.tr.Send(ctx, userID, "Сессия не найдена. Для отправки на сайт сделайте /login")
	case err != nil:
		l.log.Error("чтение сессии", "user", userID, "err", err)
		l.tr.Send(ctx, userID, msgInternalError)
	case !valid:
		l.tr.Send(ctx, userID, "Сессия истекла. Сделайте /login ещё раз")
	default:
		l.tr.Send(ctx, userID, "Сессия активна. Можно отправлять заметки и ответы")
	}
}

func (l *Logic) setState(ctx context.Context, userID int64, state string) {
	if err := l.st.SetDialogState(ctx, l.stateNS, userID, state, time.Now()); err != nil {
		l.log.Error("сохранение состояния диалога", "user", userID, "err", err)
	}
}
