// Пакет bridge обрабатывает обновления из группы обсуждения:
// захват автофорварда (связывание поста канала с корнем треда) и —
// с этапа M4 — мост ответов пользователей обратно на сайт.
package bridge

import (
	"context"
	"log/slog"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/store"
)

type Handler struct {
	st               *store.Store
	channelID        int64
	discussionChatID int64
	log              *slog.Logger
}

func New(st *store.Store, channelID, discussionChatID int64, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{st: st, channelID: channelID, discussionChatID: discussionChatID, log: log}
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
	if msg.ReplyToMessage != nil && msg.Text != "" {
		// Мост «ответ в Telegram → комментарий на сайте» — этап M4.
		h.log.Info("получен ответ в треде (мост появится в M4)",
			"chat", msg.Chat.ID, "message", msg.ID)
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
	noteID, ok, err := h.st.SetNoteThreadIDByMessageID(ctx, int64(origin.MessageID), int64(msg.ID))
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
