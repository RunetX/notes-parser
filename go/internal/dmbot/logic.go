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

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// Transport — отправка и удаление личных сообщений в мессенджере.
// Id сообщений строковые (Telegram — число строкой, MAX — mid).
type Transport interface {
	Send(ctx context.Context, userID int64, text string)
	DeleteMessage(ctx context.Context, userID int64, messageID string)
}

type Logic struct {
	st        *store.Store
	site      SiteAuth
	tr        Transport
	messenger string
	talks     TalkRouter // личная переписка сайта; nil — выключена
	log       *slog.Logger
}

// SetTalkRouter подключает роутер личной переписки (в runDaemon после сборки
// поллера talks). Для MAX ставится на maxDM-логику, для Telegram — через Bot.
func (l *Logic) SetTalkRouter(r TalkRouter) { l.talks = r }

// SiteIdentifier (опц.) снимает site-идентичность владельца сессии со страницы
// сайта. Реализуется love.Client в Ф4; до этого идентичность не заполняется.
type SiteIdentifier interface {
	SiteIdentity(ctx context.Context, cookies []*http.Cookie) (profileID, passportID, nick string, err error)
}

// NewLogic создаёт диалоговый движок для одного мессенджера.
func NewLogic(st *store.Store, site SiteAuth, tr Transport, messenger string, log *slog.Logger) *Logic {
	if log == nil {
		log = slog.Default()
	}
	return &Logic{st: st, site: site, tr: tr, messenger: messenger, log: log}
}

// Greet шлёт приветствие со списком команд (на /start и первое открытие
// диалога — bot_started в MAX).
func (l *Logic) Greet(ctx context.Context, userID int64) {
	l.tr.Send(ctx, userID, startMessage)
}

// HandleText обрабатывает входящее ЛС. messageID нужен для удаления
// сообщения с логином/паролем.
func (l *Logic) HandleText(ctx context.Context, userID int64, messageID, text string) {
	if text == "" {
		return
	}
	state, err := l.st.DialogState(ctx, l.messenger, userID)
	if err != nil {
		l.log.Error("чтение состояния диалога", "user", userID, "err", err)
		return
	}

	// Команда прерывает любой текущий диалог.
	if cmd := command(text); cmd != "" {
		if state != "" {
			_ = l.st.ClearDialogState(ctx, l.messenger, userID)
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
	switch cmd {
	case "/start":
		l.Greet(ctx, userID)
	case "/login":
		l.setState(ctx, userID, stateAwaitCredentials)
		l.tr.Send(ctx, userID, "Для входа на сайт отправьте логин и пароль через пробел")
	case "/add_note":
		l.setState(ctx, userID, stateAwaitNote)
		l.tr.Send(ctx, userID, "Отправьте текст заметки")
	case "/add_anonymous_note":
		l.setState(ctx, userID, stateAwaitAnonNote)
		l.tr.Send(ctx, userID, "Отправьте текст анонимной заметки")
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
	case "/cancel":
		l.handleCancel(ctx, userID)
	default:
		l.tr.Send(ctx, userID, "Неизвестная команда. Наберите /start")
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
		l.tr.Send(ctx, userID, "Пока нет диалогов. Как придёт ЛС с сайта — ответьте на него реплаем.")
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
	l.tr.Send(ctx, userID, "Пишу в диалог с "+nickOrPassport(peer)+". Следующие сообщения уйдут ему. /cancel — выйти.")
}

// handleCancel выходит из залипшего диалога (и любого другого состояния).
func (l *Logic) handleCancel(ctx context.Context, userID int64) {
	_ = l.st.ClearDialogState(ctx, l.messenger, userID)
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
		if l.talks == nil || !l.talks.SendToDialog(ctx, l.messenger, userID, peerID, messageID, text) {
			l.tr.Send(ctx, userID, "Не удалось отправить в диалог. Список — /talks")
		}
		return
	}
	switch state {
	case stateAwaitCredentials:
		l.tryLogin(ctx, userID, messageID, text)
	case stateAwaitNote:
		l.addNote(ctx, userID, text, false)
		_ = l.st.ClearDialogState(ctx, l.messenger, userID)
	case stateAwaitAnonNote:
		l.addNote(ctx, userID, text, true)
		_ = l.st.ClearDialogState(ctx, l.messenger, userID)
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
	_ = l.st.ClearDialogState(ctx, l.messenger, userID)
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
	if err := l.st.SetDialogState(ctx, l.messenger, userID, state, time.Now()); err != nil {
		l.log.Error("сохранение состояния диалога", "user", userID, "err", err)
	}
}
