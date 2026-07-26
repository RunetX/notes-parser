package maxx

// Приём апдейтов MAX: long polling через GetUpdates(marker) и диспетчер
// «чат обсуждения → мост, диалог → ЛС-движок». Маркер не персистится:
// после рестарта сервер повторит неподтверждённые апдейты — от задвоения
// реплаев защищает processed_replies (как и в telegram-мосте).

import (
	"context"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// pollErrorPause — пауза перед повтором после ошибки long polling.
const pollErrorPause = 5 * time.Second

// UpdateHandler — обработчик одного апдейта MAX.
type UpdateHandler func(ctx context.Context, u model.Update)

// Start запускает long polling апдейтов; блокируется до отмены контекста.
func (m *Mirror) Start(ctx context.Context, onUpdate UpdateHandler) {
	var marker int64
	for ctx.Err() == nil {
		updates, next, err := m.api.Subscriptions.GetUpdates(ctx, marker)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("MAX getUpdates", "err", err)
			select {
			case <-time.After(pollErrorPause):
			case <-ctx.Done():
				return
			}
			continue
		}
		marker = next
		for _, u := range updates {
			onUpdate(ctx, u)
		}
	}
}

// ReplyBridge — мост «ответ в чате обсуждения → комментарий на сайте»
// (реализуется bridge.Core).
type ReplyBridge interface {
	ProcessReply(ctx context.Context, replyMsgID string, userID int64, replyToID, text string)
}

// DMLogic — диалоговый ЛС-движок (реализуется dmbot.Logic).
type DMLogic interface {
	HandleText(ctx context.Context, userID int64, messageID, text string)
	Greet(ctx context.Context, userID int64)
}

// Dispatch собирает обработчик апдейтов: реплаи из чата обсуждения — мосту,
// личные сообщения — ЛС-движку, первое открытие диалога — приветствие.
// Любой из аргументов может быть nil — соответствующие апдейты игнорируются.
func (m *Mirror) Dispatch(bridge ReplyBridge, dm DMLogic) UpdateHandler {
	return func(ctx context.Context, u model.Update) {
		switch u.UpdateType {
		case model.UpdateBotStarted:
			if dm != nil && u.UserID != 0 {
				dm.Greet(ctx, u.UserID)
			}
		case model.UpdateMessageCreated:
			m.dispatchMessage(ctx, u, bridge, dm)
		}
	}
}

func (m *Mirror) dispatchMessage(ctx context.Context, u model.Update, bridge ReplyBridge, dm DMLogic) {
	msg := u.Message
	if msg == nil || msg.Sender.IsBot {
		return // эхо собственных постов моста не интересует
	}
	switch {
	case msg.Recipient.ChatType == model.ChatTypeDialog:
		if dm != nil {
			dm.HandleText(ctx, msg.Sender.UserID, msg.Body.Mid, msg.Body.Text)
		}
	case msg.Recipient.ChatID == m.discussionChatID:
		if bridge == nil || msg.Link == nil ||
			msg.Link.Type != model.LinkTypeReply || msg.Body.Text == "" {
			return // обычная болтовня в чате — не мосту
		}
		bridge.ProcessReply(ctx, msg.Body.Mid, msg.Sender.UserID,
			msg.Link.Message.Mid, msg.Body.Text)
	}
}

// Send шлёт личное сообщение, молча логируя ошибку (dmbot.Transport).
func (m *Mirror) Send(ctx context.Context, userID int64, text string) {
	if err := m.SendText(ctx, userID, text); err != nil {
		m.log.Warn("отправка ЛС не удалась", "user", userID, "err", err)
	}
}

// DeleteMessage удаляет сообщение по mid (dmbot.Transport: сообщение с
// логином/паролем не должно оставаться в истории диалога).
func (m *Mirror) DeleteMessage(ctx context.Context, userID int64, messageID string) {
	if _, err := m.api.Messages.DeleteMessage(ctx, messageID); err != nil {
		// Не критично: у бота может не быть права удалять чужое сообщение.
		m.log.Warn("не удалось удалить сообщение с логином", "user", userID, "err", err)
	}
}
