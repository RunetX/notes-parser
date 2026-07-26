// Пакет bridge обрабатывает ответы из обсуждений: мессенджер-агностичное
// ядро «ответ пользователя в треде → комментарий на сайте от его имени»
// (core.go) и телеграм-обработчик — захват автофорварда (связывание поста
// канала с корнем треда) + разбор reply-обновлений.
package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/store"
)

// SitePoster — то, что мосту нужно от клиента сайта.
type SitePoster interface {
	PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error
}

// Notify шлёт пользователю личное сообщение (через ЛС-бота); nil — молча.
type Notify func(ctx context.Context, userID int64, text string)

// Handler — телеграм-сторона моста.
type Handler struct {
	core             *Core
	st               *store.Store
	channelID        int64
	discussionChatID int64
	log              *slog.Logger
}

func New(st *store.Store, site SitePoster, notify Notify,
	channelID, discussionChatID int64, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		core:             NewCore(st, site, notify, store.MessengerTelegram, log),
		st:               st,
		channelID:        channelID,
		discussionChatID: discussionChatID,
		log:              log,
	}
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
		h.core.ProcessReply(ctx, strconv.Itoa(msg.ID), msg.From.ID,
			strconv.Itoa(msg.ReplyToMessage.ID), msg.Text)
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
