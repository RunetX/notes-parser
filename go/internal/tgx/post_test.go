package tgx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/time/rate"

	"lovegw/internal/store"
)

// postMirror собирает Mirror поверх тестового сервера Bot API и отдаёт функцию
// доступа к полям последнего запроса. Bot API принимает multipart/form-data —
// go-telegram/bot шлёт именно его, поэтому смотрим на поля формы.
func postMirror(t *testing.T, subBot string) (*Mirror, func() map[string]string) {
	t.Helper()
	last := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("разбор запроса %s: %v", r.URL.Path, err)
		}
		clear(last)
		for name, values := range r.MultipartForm.Value {
			last[name] = values[0]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":501}}`)
	}))
	t.Cleanup(srv.Close)

	b, err := bot.New(secretToken, bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	m := &Mirror{
		b: b, hc: srv.Client(), log: testLogger(), channelID: -1001234567890,
		limiters: map[int64]*rate.Limiter{-1001234567890: rate.NewLimiter(rate.Inf, 1)},
	}
	m.SetSubscribeBot(subBot)
	return m, func() map[string]string { return last }
}

// Под постом канала — URL-кнопка в ЛС-бота: постер писать в личку не может, а
// переход по deep-link'у заодно стартует бота у того, кто его не запускал.
func TestPostNoteSubscribeButton(t *testing.T) {
	m, last := postMirror(t, "ryumkin_bot")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "312886", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	raw, ok := last()["reply_markup"]
	if !ok {
		t.Fatalf("нет клавиатуры в запросе: %+v", last())
	}
	var markup models.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &markup); err != nil {
		t.Fatalf("клавиатура %q: %v", raw, err)
	}
	if len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("клавиатура: %+v", markup.InlineKeyboard)
	}
	btn := markup.InlineKeyboard[0][0]
	if btn.Text != btnSubscribe {
		t.Errorf("подпись кнопки: %q", btn.Text)
	}
	if want := "https://t.me/ryumkin_bot?start=sub_312886"; btn.URL != want {
		t.Errorf("ссылка кнопки: %q", btn.URL)
	}
	// Кнопка ведёт в ЛС, а не в канал: callback_data у неё быть не должно.
	if btn.CallbackData != "" {
		t.Errorf("кнопка должна быть ссылочной: %q", btn.CallbackData)
	}
}

// Юзернейм не снялся (getMe не прошёл) — пост уходит как раньше, без кнопки.
func TestPostNoteWithoutSubscribeBot(t *testing.T) {
	m, last := postMirror(t, "")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "312886", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok := last()["reply_markup"]; ok {
		t.Errorf("без юзернейма клавиатуры быть не должно: %+v", last())
	}
}

// Нечисловой id заметки в payload не годится (Telegram разрешает в аргументе
// /start только [A-Za-z0-9_-]) — тогда кнопки тоже нет.
func TestPostNoteNonNumericIDHasNoButton(t *testing.T) {
	m, last := postMirror(t, "ryumkin_bot")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "n1", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok := last()["reply_markup"]; ok {
		t.Errorf("кнопки с негодным payload быть не должно: %+v", last())
	}
}

// Ссылка на пост канала: внутренний id — полный без префикса -100.
func TestChannelDeepLink(t *testing.T) {
	if got := ChannelDeepLink(-1001234567890, 501); got != "https://t.me/c/1234567890/501" {
		t.Errorf("ссылка на пост: %q", got)
	}
}
