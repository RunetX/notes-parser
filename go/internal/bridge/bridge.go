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

// Core — ядро моста этого мессенджера. Нужно снаружи ровно затем, чтобы
// подключить площадку (SetPlatform): она поднимается позже ботов и может не
// подняться вовсе.
func (h *Handler) Core() *Core { return h.core }

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
	h.captureFromThread(ctx, msg)
	if msg.ReplyToMessage != nil && msg.Text != "" &&
		msg.From != nil && !msg.From.IsBot {
		h.core.ProcessReply(ctx, strconv.Itoa(msg.ID), msg.From.ID,
			strconv.Itoa(msg.ReplyToMessage.ID), msg.Text)
	}
}

// captureFromThread — запасной захват корня треда: обновление с автофорвардом
// приходит один раз и может потеряться (сбой сети, 502 от Bot API), после чего
// комментарии заметки навсегда копятся в очереди. Но у ответа на корневое
// сообщение ветки reply_to_message — это тот же автофорвард, с ForwardOrigin
// поста канала, так что связку можно восстановить из первого же ответа.
func (h *Handler) captureFromThread(ctx context.Context, msg *models.Message) {
	root := msg.ReplyToMessage
	if root == nil || msg.MessageThreadID == 0 || root.ID != msg.MessageThreadID {
		return
	}
	h.capture(ctx, root, "запасным путём")
}

// captureForward связывает пост канала с его автофорвардом в группе:
// id форварда становится корнем треда для комментариев.
func (h *Handler) captureForward(ctx context.Context, msg *models.Message) {
	h.capture(ctx, msg, "")
}

// capture — общая часть обоих входов: forward — сообщение-автофорвард (сам
// апдейт или корень ветки, в который ответили), его id и становится корнем
// треда. Не автофорвард нашего канала — молча выходим: это чужой пост.
func (h *Handler) capture(ctx context.Context, forward *models.Message, how string) {
	if forward.ForwardOrigin == nil || forward.ForwardOrigin.MessageOriginChannel == nil {
		return
	}
	origin := forward.ForwardOrigin.MessageOriginChannel
	if origin.Chat.ID != h.channelID {
		return
	}
	noteID, ok, err := h.st.CaptureNoteThread(ctx, store.MessengerTelegram,
		strconv.Itoa(origin.MessageID), strconv.Itoa(forward.ID))
	if err != nil {
		h.log.Error("захват треда", "как", how, "channel_message", origin.MessageID, "err", err)
		return
	}
	if !ok {
		// Тред уже пойман (обычный случай) или пост не наш.
		return
	}
	msg := "тред пойман"
	if how != "" {
		msg += " " + how
	}
	h.log.Info(msg, "note", noteID,
		"channel_message", origin.MessageID, "thread", forward.ID)
}
