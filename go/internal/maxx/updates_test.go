package maxx

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"lovegw/internal/kbd"
)

type fakeBridge struct {
	replies [][4]string // replyMsgID, userID(строкой не нужен — кладём в text), replyToID, text
}

func (f *fakeBridge) ProcessReply(_ context.Context, replyMsgID string, _ int64, replyToID, text string) {
	f.replies = append(f.replies, [4]string{replyMsgID, "", replyToID, text})
}

type fakeDM struct {
	texts     []string
	greets    int
	callbacks []dmCallback
	// public — payload'ы, разрешённые вне диалога (белый список держит ядро).
	public map[string]bool
}

func (f *fakeDM) AllowsOutsideDialog(payload string) bool { return f.public[payload] }

// dmCallback — нажатие, как его увидел ЛС-движок.
type dmCallback struct {
	userID int64
	cb     kbd.Callback
}

func (f *fakeDM) HandleText(_ context.Context, _ int64, _, text string) {
	f.texts = append(f.texts, text)
}
func (f *fakeDM) Greet(context.Context, int64) { f.greets++ }

func (f *fakeDM) HandleCallback(_ context.Context, userID int64, cb kbd.Callback) {
	f.callbacks = append(f.callbacks, dmCallback{userID: userID, cb: cb})
}

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

// Нажатие кнопки приезжает СЫРЫМ JSON через поллинг: именно разбор в SDK
// (FromRaw) кладёт в update.user_id получателя сообщения, а не нажавшего, и
// собранный руками model.Update этот путь не проверяет. В теле специально
// разные user_id у recipient и у callback.user.
func TestDispatchCallbackFromRawJSON(t *testing.T) {
	f := &fakeMax{t: t, updatesBody: `{"updates":[{"update_type":"message_callback",` +
		`"timestamp":1,"callback":{"callback_id":"cb-1","payload":"1:note:anon",` +
		`"user":{"user_id":777,"name":"Мария"}},` +
		`"message":{"recipient":{"chat_id":555,"chat_type":"dialog","user_id":555},` +
		`"body":{"mid":"mid.kb1","text":"Заметка от своего имени или анонимно?"},` +
		`"sender":{"user_id":1,"is_bot":true}}}],"marker":2}`}
	m := newTestMirror(t, f)
	dm := &fakeDM{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Start(ctx, m.Dispatch(nil, dm))
	}()
	deadline := time.After(2 * time.Second)
	for len(dm.callbacks) == 0 {
		select {
		case <-deadline:
			t.Fatal("нажатие не доехало до ЛС-движка")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	got := dm.callbacks[0]
	if got.userID != 777 {
		t.Errorf("нажавший должен браться из callback.user: %d", got.userID)
	}
	if got.cb.AnswerID != "cb-1" || got.cb.Payload != "1:note:anon" || got.cb.MessageID != "mid.kb1" {
		t.Errorf("нажатие: %+v", got.cb)
	}
}

// Вне диалога проходят только публичные кнопки — «Подписаться» под постом
// канала. И приезжает она с ПУСТЫМ mid: сообщение под кнопкой — чужой пост,
// править его нажатием нельзя, ответ должен уйти новым сообщением в ЛС.
func TestDispatchCallbackOutsideDialog(t *testing.T) {
	channelPress := func(payload string) model.Update {
		u := model.Update{UpdateType: model.UpdateMessageCallback, ChatID: 100}
		u.Callback = &model.Callback{CallbackID: "cb-2", Payload: payload, User: model.User{UserID: 777}}
		u.Message = &model.MessageUpdate{
			Recipient: model.Recipient{ChatID: 100, ChatType: model.ChatTypeChat},
			Body:      model.MessageBody{Mid: "mid.post"},
		}
		return u
	}

	m := &Mirror{discussionChatID: -200, log: slog.Default()}
	dm := &fakeDM{public: map[string]bool{"1:sub:312886": true}}
	m.Dispatch(nil, dm)(context.Background(), channelPress("1:sub:312886"))

	if len(dm.callbacks) != 1 {
		t.Fatalf("публичное нажатие должно дойти до ЛС-движка: %+v", dm.callbacks)
	}
	if got := dm.callbacks[0]; got.userID != 777 || got.cb.MessageID != "" {
		t.Errorf("mid поста канала обязан гаситься на входе: %+v", got)
	}

	// ЛС-глагол из канала по-прежнему отбрасывается.
	dm2 := &fakeDM{}
	m.Dispatch(nil, dm2)(context.Background(), channelPress("1:subs"))
	if len(dm2.callbacks) != 0 {
		t.Errorf("ЛС-глагол из канала не для ЛС-движка: %+v", dm2.callbacks)
	}
}

// Сообщение с кнопкой удалили: mid пустой, но нажатие всё равно обрабатывается —
// роутер ответит нажавшему, просто не станет править сообщение.
func TestDispatchCallbackDeletedMessage(t *testing.T) {
	m := &Mirror{log: slog.Default()}
	dm := &fakeDM{}
	u := model.Update{UpdateType: model.UpdateMessageCallback}
	u.Callback = &model.Callback{CallbackID: "cb-3", Payload: "1:cancel", User: model.User{UserID: 777}}

	m.Dispatch(nil, dm)(context.Background(), u)

	if len(dm.callbacks) != 1 {
		t.Fatalf("нажатий: %d", len(dm.callbacks))
	}
	if got := dm.callbacks[0]; got.userID != 777 || got.cb.MessageID != "" {
		t.Errorf("нажатие с удалённым сообщением: %+v", got)
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
