package maxx

// Приём апдейтов MAX: long polling через GetUpdates(marker) и диспетчер
// «чат обсуждения → мост, диалог → ЛС-движок». Маркер не персистится:
// после рестарта сервер повторит неподтверждённые апдейты — от задвоения
// реплаев защищает processed_replies (как и в telegram-мосте).

import (
	"context"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// pollErrorPause — пауза перед повтором после ошибки long polling.
const pollErrorPause = 5 * time.Second

// UpdateHandler — обработчик одного апдейта MAX.
type UpdateHandler func(ctx context.Context, u model.Update)

// Start запускает long polling апдейтов; блокируется до отмены контекста.
// Ошибки не роняют цикл (ретрай с паузой), но полоса сбоев поднимает алерт
// админу через pollAlert: иначе умерший поллер (протухший токен, второй
// процесс на том же боте) оставлял бы демон молча полуживым.
func (m *Mirror) Start(ctx context.Context, onUpdate UpdateHandler) {
	var marker int64
	for ctx.Err() == nil {
		updates, next, err := m.api.Subscriptions.GetUpdates(ctx, marker)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.log.Warn("MAX getUpdates", "err", err)
			if m.pollAlert != nil {
				m.pollAlert.Fail(ctx, m.pollAlertKey, err.Error())
			}
			select {
			case <-time.After(pollErrorPause):
			case <-ctx.Done():
				return
			}
			continue
		}
		if m.pollAlert != nil {
			m.pollAlert.OK(ctx, m.pollAlertKey)
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
	HandleCallback(ctx context.Context, userID int64, cb kbd.Callback)
	Greet(ctx context.Context, userID int64)
	// AllowsOutsideDialog — можно ли исполнять этот payload вне диалога.
	// Белый список держит ядро: вне диалога живёт только кнопка под постом
	// канала. Иначе ЛС-глаголы (вход, заметки, переписка) стали бы доступны
	// нажатием на кнопку под чужим сообщением.
	AllowsOutsideDialog(payload string) bool
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
		case model.UpdateMessageCallback:
			m.dispatchCallback(ctx, u, dm)
		default:
			// Наблюдательность на debug-уровне: платформа развивается
			// (нативные комментарии каналов и т.п.), новые типы апдейтов
			// подсвечиваем, не мешая работе.
			m.log.Debug("апдейт MAX без обработчика",
				"type", u.UpdateType, "chat", u.ChatID, "user", u.UserID)
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
		// Реплай на доставленное ЛС talks → на сайт (до диалоговой логики).
		if m.talks != nil && msg.Link != nil && msg.Link.Type == model.LinkTypeReply && msg.Body.Text != "" {
			if m.talks.HandleReply(ctx, store.MessengerMax, msg.Body.Mid,
				msg.Sender.UserID, msg.Link.Message.Mid, msg.Body.Text) {
				return
			}
		}
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
	default:
		// Сообщение из незнакомого чата: работе не мешает, но подсвечивает
		// новые механики платформы (нативные комментарии каналов пишутся,
		// вероятно, в скрытый связанный чат). Текст не логируем — только
		// адресацию и связку.
		m.log.Debug("сообщение MAX вне известных чатов",
			"chat", msg.Recipient.ChatID, "chat_type", msg.Recipient.ChatType,
			"mid", msg.Body.Mid, "link", linkInfo(msg.Link))
	}
}

// dispatchCallback отдаёт нажатие кнопки ЛС-движку. Нажавшего берём из
// callback.user: для message_callback SDK кладёт в update.user_id ПОЛУЧАТЕЛЯ
// сообщения с клавиатурой, а update.user не заполняет вовсе. Пустой mid —
// рабочий случай: сообщение с кнопкой могли удалить.
//
// Маркер поллинга не персистится намеренно (см. шапку файла), так что после
// рестарта нажатие может приехать повторно — обработчики нажатий поэтому
// идемпотентны по dialog_states.
func (m *Mirror) dispatchCallback(ctx context.Context, u model.Update, dm DMLogic) {
	if dm == nil || u.Callback == nil {
		return
	}
	var mid string
	var chatType model.ChatType
	if u.Message != nil {
		mid, chatType = u.Message.Body.Mid, u.Message.Recipient.ChatType
	}
	// Вне диалога живут только публичные кнопки — «Подписаться» под постом
	// канала. Пустой тип считаем диалогом: он теряется вместе с удалённым
	// сообщением.
	if chatType != "" && chatType != model.ChatTypeDialog {
		if !dm.AllowsOutsideDialog(u.Callback.Payload) {
			m.log.Debug("нажатие MAX вне диалога отброшено",
				"chat", u.ChatID, "chat_type", chatType)
			return
		}
		// Сообщение с кнопкой — чужой пост канала. Правка его нажатием
		// недопустима, поэтому mid гасим здесь, на входе: ядро ответит новым
		// сообщением в ЛС, даже если обработчик об этом забудет.
		mid = ""
	}
	userID := u.Callback.User.UserID
	if userID == 0 {
		m.log.Warn("нажатие MAX без пользователя", "mid", mid)
		return
	}
	dm.HandleCallback(ctx, userID, kbd.Callback{
		AnswerID:  u.Callback.CallbackID,
		MessageID: mid,
		Payload:   u.Callback.Payload,
	})
}

// linkInfo — краткое описание связки сообщения для debug-лога.
func linkInfo(l *model.LinkedMessage) string {
	if l == nil {
		return ""
	}
	return string(l.Type) + "→" + l.Message.Mid
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
