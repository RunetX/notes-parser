package tgx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
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

// Вход в подписку — ссылка в подвале поста, а НЕ inline-кнопка: своя
// клавиатура вытесняет родную кнопку «Комментарии» под постом канала.
func TestPostNoteSubscribeLink(t *testing.T) {
	m, last := postMirror(t, "ryumkin_bot")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "312886", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok := last()["reply_markup"]; ok {
		t.Errorf("клавиатуры у поста быть не должно: %+v", last())
	}
	want := `<a href="https://t.me/ryumkin_bot?start=sub_312886">` + linkSubscribe + `</a>`
	if got := last()["text"]; !strings.Contains(got, want) {
		t.Errorf("нет ссылки подписки в тексте:\n%s", got)
	}
}

// Юзернейм не снялся (getMe не прошёл) — пост уходит как раньше, без ссылки.
func TestPostNoteWithoutSubscribeBot(t *testing.T) {
	m, last := postMirror(t, "")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "312886", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	if got := last()["text"]; strings.Contains(got, "t.me/") {
		t.Errorf("без юзернейма ссылки быть не должно:\n%s", got)
	}
}

// Нечисловой id заметки в payload не годится (Telegram разрешает в аргументе
// /start только [A-Za-z0-9_-]) — тогда ссылки тоже нет.
func TestPostNoteNonNumericIDHasNoLink(t *testing.T) {
	m, last := postMirror(t, "ryumkin_bot")

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "n1", AuthorName: "Ягода", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}

	if got := last()["text"]; strings.Contains(got, "t.me/") {
		t.Errorf("ссылки с негодным payload быть не должно:\n%s", got)
	}
}

// Ссылка на пост канала: внутренний id — полный без префикса -100.
func TestChannelDeepLink(t *testing.T) {
	if got := ChannelDeepLink(-1001234567890, 501); got != "https://t.me/c/1234567890/501" {
		t.Errorf("ссылка на пост: %q", got)
	}
}
