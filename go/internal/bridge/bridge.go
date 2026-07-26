// Пакет bridge обрабатывает обновления из группы обсуждения:
// захват автофорварда (связывание поста канала с корнем треда) и мост
// «ответ пользователя в треде → комментарий на сайте от его имени».
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// SitePoster — то, что мосту нужно от клиента сайта.
type SitePoster interface {
	PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error
}

// Notify шлёт пользователю личное сообщение (через ЛС-бота); nil — молча.
type Notify func(ctx context.Context, tgUserID int64, text string)

type Handler struct {
	st               *store.Store
	site             SitePoster
	notify           Notify
	channelID        int64
	discussionChatID int64
	log              *slog.Logger
}

func New(st *store.Store, site SitePoster, notify Notify,
	channelID, discussionChatID int64, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	if notify == nil {
		notify = func(context.Context, int64, string) {
			// ЛС-бот не подключён — уведомления пользователям некуда слать.
		}
	}
	return &Handler{st: st, site: site, notify: notify,
		channelID: channelID, discussionChatID: discussionChatID, log: log}
}

// Handle — обработчик обновлений постер-бота.
func (h *Handler) Handle(ctx context.Context, u *models.Update) {
	msg := u.Message
	if msg == nil || msg.Chat.ID != h.discussionChatID {
		return
	}
	if msg.IsAutomaticForward {
		h.captureForward(ctx, msg)
		return
	}
	if msg.ReplyToMessage != nil && msg.Text != "" &&
		msg.From != nil && !msg.From.IsBot {
		h.processReply(ctx, msg)
	}
}

// captureForward связывает пост канала с его автофорвардом в группе:
// id форварда становится корнем треда для комментариев.
func (h *Handler) captureForward(ctx context.Context, msg *models.Message) {
	if msg.ForwardOrigin == nil || msg.ForwardOrigin.MessageOriginChannel == nil {
		return
	}
	origin := msg.ForwardOrigin.MessageOriginChannel
	if origin.Chat.ID != h.channelID {
		return
	}
	noteID, ok, err := h.st.CaptureNoteThread(ctx, store.MessengerTelegram,
		strconv.Itoa(origin.MessageID), strconv.Itoa(msg.ID))
	if err != nil {
		h.log.Error("захват автофорварда", "channel_message", origin.MessageID, "err", err)
		return
	}
	if !ok {
		// Форвард чужого/старого поста — не наша заметка или тред уже пойман.
		return
	}
	h.log.Info("тред пойман", "note", noteID,
		"channel_message", origin.MessageID, "thread", msg.ID)
}

// processReply отправляет ответ пользователя на сайт как комментарий
// от его имени. At-most-once: сначала фиксация в processed_replies,
// потом POST — потерянный при сбое комментарий лучше задвоенного.
func (h *Handler) processReply(ctx context.Context, msg *models.Message) {
	noteID, comAPIID, text, ok := h.resolveTarget(ctx, msg)
	if !ok {
		return
	}
	userID := msg.From.ID

	fresh, err := h.st.TryMarkReplyProcessed(ctx, store.MessengerTelegram, strconv.Itoa(msg.ID), time.Now())
	if err != nil {
		h.log.Error("дедуп ответа", "message", msg.ID, "err", err)
		return
	}
	if !fresh {
		return // повторная доставка после рестарта
	}

	cookies, ok := h.userCookies(ctx, userID)
	if !ok {
		return
	}
	if err := h.site.PostComment(ctx, cookies, noteID, comAPIID, text); err != nil {
		h.log.Warn("комментарий не ушёл на сайт", "note", noteID, "user", userID, "err", err)
		if isAuthError(err) {
			h.invalidateSession(ctx, userID)
		}
		return
	}
	if err := h.st.SetSessionValid(ctx, store.MessengerTelegram, userID, true, time.Now()); err != nil {
		h.log.Error("отметка last_ok_at", "user", userID, "err", err)
	}
	h.log.Info("ответ отправлен на сайт", "note", noteID, "com_api_id", comAPIID, "user", userID)
}

// resolveTarget определяет, куда адресован ответ: в корень заметки
// (реплай на автофорвард) или на конкретный комментарий (реплай на
// сообщение бота с комментарием — текст получает префикс «Автор, ...»).
func (h *Handler) resolveTarget(ctx context.Context, msg *models.Message) (noteID, comAPIID, text string, ok bool) {
	replyTo := strconv.Itoa(msg.ReplyToMessage.ID)

	if n, err := h.st.NoteByThread(ctx, store.MessengerTelegram, replyTo); err == nil {
		return n.ID, "", msg.Text, true
	} else if !errors.Is(err, store.ErrNotFound) {
		h.log.Error("поиск заметки по треду", "thread", replyTo, "err", err)
		return "", "", "", false
	}

	c, err := h.st.CommentByTarget(ctx, store.MessengerTelegram, replyTo)
	if errors.Is(err, store.ErrNotFound) {
		return "", "", "", false // ответ не на наше сообщение
	}
	if err != nil {
		h.log.Error("поиск комментария", "message", replyTo, "err", err)
		return "", "", "", false
	}
	return c.NoteID, fmt.Sprintf("%d", c.ID),
		fmt.Sprintf("%s, %s", c.AuthorName, msg.Text), true
}

// userCookies достаёт живые куки пользователя; при их отсутствии
// подсказывает пользователю сделать /login.
func (h *Handler) userCookies(ctx context.Context, userID int64) ([]*http.Cookie, bool) {
	cookiesJSON, valid, err := h.st.SessionCookies(ctx, store.MessengerTelegram, userID)
	if errors.Is(err, store.ErrNotFound) {
		h.log.Info("ответ без сессии", "user", userID)
		h.notify(ctx, userID, "Чтобы ваши ответы попадали на сайт, войдите: /login")
		return nil, false
	}
	if err != nil {
		h.log.Error("чтение сессии", "user", userID, "err", err)
		return nil, false
	}
	if !valid {
		h.notify(ctx, userID, "Сессия сайта истекла. Сделайте /login ещё раз")
		return nil, false
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		h.log.Error("разбор кук", "user", userID, "err", err)
		return nil, false
	}
	if len(cookies) == 0 {
		h.invalidateSession(ctx, userID)
		return nil, false
	}
	return cookies, true
}

func (h *Handler) invalidateSession(ctx context.Context, userID int64) {
	if err := h.st.SetSessionValid(ctx, store.MessengerTelegram, userID, false, time.Now()); err != nil {
		h.log.Error("инвалидация сессии", "user", userID, "err", err)
	}
	h.notify(ctx, userID, "Сессия сайта истекла. Сделайте /login ещё раз")
}

// isAuthError — эвристика «сессия протухла»: сайт ответил 401/403 на POST.
// Точную форму неавторизованного ответа уточним по живым логам.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "статус 401") ||
		strings.Contains(err.Error(), "статус 403")
}
