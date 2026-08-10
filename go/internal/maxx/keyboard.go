package maxx

// Кнопки под сообщением и ответ на нажатие (dmbot.Transport). Клавиатура в MAX
// — вложение сообщения, поэтому «убрать кнопки» отдельной операцией не
// существует: и правка сообщения, и ответ на нажатие принимают тело целиком.

import (
	"context"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"lovegw/internal/kbd"
)

// SendKeyboard шлёт личное сообщение с кнопками (dmbot.Transport).
func (m *Mirror) SendKeyboard(ctx context.Context, userID int64, text string, kb *kbd.Keyboard) {
	msg := maxbot.NewMessage().SetUser(userID).SetText(text)
	if mk := maxKeyboard(kb); mk != nil {
		msg.AddKeyboard(mk)
	}
	if _, err := m.send(ctx, userID, msg); err != nil {
		m.log.Warn("отправка ЛС с кнопками не удалась", "user", userID, "err", err)
	}
}

// AnswerCallback гасит индикатор нажатия у пользователя (dmbot.Transport).
// Пустой toast — ответить молча.
func (m *Mirror) AnswerCallback(ctx context.Context, cb kbd.Callback, toast string) {
	answer := model.CallbackAnswer{}
	if toast != "" {
		answer.Notification = &toast
	}
	if _, err := m.api.Messages.AnswerOnCallback(ctx, cb.AnswerID, answer); err != nil {
		// Нажатие живёт недолго, протухший callback_id — обычное дело.
		m.log.Debug("ответ на нажатие MAX не прошёл", "err", err)
	}
}

// EditMessage переписывает уже отправленное сообщение (dmbot.Transport).
// kb == nil — снять кнопки: вложений в новом теле просто не будет.
func (m *Mirror) EditMessage(ctx context.Context, userID int64, messageID, text string, kb *kbd.Keyboard) {
	body := model.NewMessageBody{Text: text, Attachments: []model.Attachment{}}
	if mk := maxKeyboard(kb); mk != nil {
		body.Attachments = append(body.Attachments, mk.Build())
	}
	if _, err := m.api.Messages.EditMessage(ctx, messageID, body); err != nil {
		m.log.Debug("правка сообщения MAX не прошла", "user", userID, "mid", messageID, "err", err)
	}
}

// maxKeyboard переводит общую клавиатуру в максовскую; пустая — nil.
func maxKeyboard(kb *kbd.Keyboard) *model.Keyboard {
	if kb.Empty() {
		return nil
	}
	mk := model.NewKeyboard()
	for _, row := range kb.Rows {
		mr := mk.AddRow()
		for _, b := range row {
			mr.AddCallBack(b.Text, b.Payload)
		}
	}
	return mk
}
