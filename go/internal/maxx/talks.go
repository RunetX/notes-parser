package maxx

// MAX-сторона личной переписки (talks): доставка входящего ЛС в диалог
// (talks.PMTransport) и маршрутизация ответа-реплая на сайт. Команды
// /talks//talk обслуживает общее ядро dmbot.Logic (MAX ходит через него),
// поэтому здесь нужен только приём реплаев и отправка.

import (
	"context"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// TalkReplyRouter — ответ пользователя (реплай на доставленное ЛС) на сайт.
// Реализуется talks.Watcher; nil — переписка выключена.
type TalkReplyRouter interface {
	HandleReply(ctx context.Context, messenger, replyMsgID string, userID int64, replyToID, text string) bool
}

// SetTalkRouter подключает роутер переписки (в runDaemon после сборки поллера).
func (m *Mirror) SetTalkRouter(r TalkReplyRouter) { m.talks = r }

// SendPM доставляет входящее ЛС в личный диалог пользователя (talks.PMTransport).
// Возвращает mid — по нему message_targets свяжет реплай с диалогом. Разметка
// HTML: сервер MAX разбирает format=html в text+markup (как в постах зеркала).
// Длину письма сайт не ограничивает, поэтому укладываем в бюджет MAX: иначе
// недоставленное ЛС встанет пробкой так же, как встал бы комментарий.
func (m *Mirror) SendPM(ctx context.Context, userID int64, html string) (string, error) {
	text, cut := fitHTML(html)
	if cut {
		m.log.Warn("входящее ЛС обрезано под предел сообщения MAX", "user", userID)
	}
	msg := maxbot.NewMessage().SetUser(userID).SetText(text).SetFormat(model.FormatHTML)
	return m.send(ctx, userID, msg)
}

// Confirm подтверждает исходящее реакцией. Реакции в MAX Bot API не
// задокументированы, поэтому при неудаче шлём короткую заметку, при успехе
// молчим (сообщение и так ушло собеседнику).
func (m *Mirror) Confirm(ctx context.Context, userID int64, _ string, ok bool) {
	if ok {
		return
	}
	m.Send(ctx, userID, "⚠️ Не удалось отправить на сайт. Повторите позже или /login.")
}
