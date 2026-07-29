package maxx

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type fakeBridge struct {
	replies [][4]string // replyMsgID, userID(строкой не нужен — кладём в text), replyToID, text
}

func (f *fakeBridge) ProcessReply(_ context.Context, replyMsgID string, _ int64, replyToID, text string) {
	f.replies = append(f.replies, [4]string{replyMsgID, "", replyToID, text})
}

type fakeDM struct {
	texts  []string
	greets int
}

func (f *fakeDM) HandleText(_ context.Context, _ int64, _, text string) {
	f.texts = append(f.texts, text)
}
func (f *fakeDM) Greet(context.Context, int64) { f.greets++ }

func chatMsg(chatID int64, chatType model.ChatType, mid, text string, isBot bool) model.Update {
	u := model.Update{UpdateType: model.UpdateMessageCreated}
	u.Message = &model.MessageUpdate{
		Recipient: model.Recipient{ChatID: chatID, ChatType: chatType},
		Sender:    model.Sender{UserID: 25978651, IsBot: isBot},
		Body:      model.MessageBody{Mid: mid, Text: text},
	}
	return u
}

func TestDispatch(t *testing.T) {
	ctx := context.Background()
	m := &Mirror{discussionChatID: -200, log: slog.Default()}
	br := &fakeBridge{}
	dm := &fakeDM{}
	h := m.Dispatch(br, dm)

	// Первое открытие диалога — приветствие.
	h(ctx, model.Update{UpdateType: model.UpdateBotStarted, UserID: 25978651})

	// ЛС — в диалоговый движок.
	h(ctx, chatMsg(342358595, model.ChatTypeDialog, "mid.dm1", "/status", false))

	// Обычное сообщение в чате обсуждения — игнор.
	h(ctx, chatMsg(-200, model.ChatTypeChat, "mid.c1", "просто болтовня", false))

	// Эхо бота — игнор.
	h(ctx, chatMsg(-200, model.ChatTypeChat, "mid.c2", "пост бота", true))

	// Реплай на сообщение бота в чате обсуждения — мосту.
	reply := chatMsg(-200, model.ChatTypeChat, "mid.r1", "мой ответ", false)
	reply.Message.Link = &model.LinkedMessage{
		Type:    model.LinkTypeReply,
		Message: model.MessageBody{Mid: "mid.root"},
	}
	h(ctx, reply)

	// Реплай в чужом чате — игнор.
	other := chatMsg(-999, model.ChatTypeChat, "mid.r2", "ответ не там", false)
	other.Message.Link = &model.LinkedMessage{Type: model.LinkTypeReply,
		Message: model.MessageBody{Mid: "mid.x"}}
	h(ctx, other)

	if dm.greets != 1 {
		t.Errorf("приветствий: %d", dm.greets)
	}
	if len(dm.texts) != 1 || dm.texts[0] != "/status" {
		t.Errorf("ЛС в движок: %v", dm.texts)
	}
	if len(br.replies) != 1 {
		t.Fatalf("реплаев мосту: %v", br.replies)
	}
	if r := br.replies[0]; r[0] != "mid.r1" || r[2] != "mid.root" || r[3] != "мой ответ" {
		t.Errorf("разбор реплая: %v", r)
	}
}

// Бот переписки: моста у него нет (Dispatch с nil), поэтому чат обсуждения он
// не трогает, а ЛС и первое открытие диалога обрабатывает как обычно.
func TestDispatchTalksBot(t *testing.T) {
	ctx := context.Background()
	m := &Mirror{log: slog.Default()} // без discussionChatID: чат не его забота
	dm := &fakeDM{}
	h := m.Dispatch(nil, dm)

	h(ctx, model.Update{UpdateType: model.UpdateBotStarted, UserID: 25978651})
	h(ctx, chatMsg(342358595, model.ChatTypeDialog, "mid.dm1", "/talks", false))

	reply := chatMsg(-200, model.ChatTypeChat, "mid.r1", "ответ в чате", false)
	reply.Message.Link = &model.LinkedMessage{Type: model.LinkTypeReply,
		Message: model.MessageBody{Mid: "mid.root"}}
	h(ctx, reply) // без моста — просто игнор, паники быть не должно

	if dm.greets != 1 {
		t.Errorf("приветствий: %d", dm.greets)
	}
	if len(dm.texts) != 1 || dm.texts[0] != "/talks" {
		t.Errorf("ЛС в движок переписки: %v", dm.texts)
	}
}

// Start: батч апдейтов доставляется обработчику, маркер передаётся дальше,
// отмена контекста завершает цикл.
func TestStartDeliversUpdates(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	got := make(chan model.Update, 8)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Start(ctx, func(_ context.Context, u model.Update) { got <- u })
	}()

	u := <-got
	if u.UpdateType != model.UpdateMessageCreated || u.Message == nil || u.Message.Body.Mid != "mid.upd1" {
		t.Errorf("апдейт: %+v", u)
	}
	// Ждём, пока цикл сходит за следующей порцией с новым маркером.
	deadline := time.After(2 * time.Second)
	for f.markerSeen.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("маркер не продвинулся: %d", f.markerSeen.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
