package maxx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"golang.org/x/time/rate"

	"lovegw/internal/store"
)

// fakeMax — мок MAX API: фиксирует запросы /messages, обслуживает /uploads,
// умеет отдавать 429 первые failFirst раз.
type fakeMax struct {
	t *testing.T

	mu        sync.Mutex
	sent      []sentMessage
	uploads   int
	chatGets  int // обращений GET /chats/<id> (ссылка чата)
	failFirst int // столько первых /messages ответить 429
	seq       int

	markerSeen atomic.Int64 // последний маркер, переданный в GET /updates
}

type sentMessage struct {
	auth    string
	chatID  string
	userID  string
	noPrev  string
	body    model.NewMessageBody
	rawBody map[string]any
}

func (f *fakeMax) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failFirst > 0 {
			f.failFirst--
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"code":"too.many.requests","message":"Too Many Requests"}`)
			return
		}
		var body model.NewMessageBody
		raw := map[string]any{}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("чтение тела: %v", err)
		}
		if err := json.Unmarshal(data, &body); err != nil {
			f.t.Errorf("разбор NewMessageBody: %v", err)
		}
		_ = json.Unmarshal(data, &raw)
		f.seq++
		f.sent = append(f.sent, sentMessage{
			auth:    r.Header.Get("Authorization"),
			chatID:  r.URL.Query().Get("chat_id"),
			userID:  r.URL.Query().Get("user_id"),
			noPrev:  r.URL.Query().Get("disable_link_preview"),
			body:    body,
			rawBody: raw,
		})
		resp := model.SendMessageResult{}
		resp.Message.Body.Mid = fmt.Sprintf("mid.%06d", f.seq)
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	mux.HandleFunc("GET /updates", func(w http.ResponseWriter, r *http.Request) {
		marker, _ := strconv.ParseInt(r.URL.Query().Get("marker"), 10, 64)
		if marker > f.markerSeen.Load() {
			f.markerSeen.Store(marker)
		}
		if marker == 0 {
			// Первый вызов: один апдейт message_created, маркер = 2.
			_, _ = w.Write([]byte(`{"updates":[{"update_type":"message_created",` +
				`"timestamp":1,"message":{"recipient":{"chat_id":200,"chat_type":"chat"},` +
				`"body":{"mid":"mid.upd1","text":"привет"},` +
				`"sender":{"user_id":1,"is_bot":false}}}],"marker":2}`))
			return
		}
		// Дальше — пусто, с паузой (иначе тест закрутит горячий цикл).
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"updates":[],"marker":` + strconv.FormatInt(marker, 10) + `}`))
	})
	mux.HandleFunc("GET /chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.chatGets++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(model.Chat{
			ChatID: 200, Link: "https://max.ru/join/test-chat-link",
		})
	})
	mux.HandleFunc("POST /uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "image" {
			f.t.Errorf("тип загрузки: %q", r.URL.Query().Get("type"))
		}
		_ = json.NewEncoder(w).Encode(model.UploadEndpoint{Url: srv.URL + "/upload-image"})
	})
	mux.HandleFunc("POST /upload-image", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			f.t.Errorf("multipart: %v", err)
		}
		f.mu.Lock()
		f.uploads++
		n := f.uploads
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(model.PhotoTokens{
			Photos: map[string]model.PhotoToken{"orig": {Token: fmt.Sprintf("tok-%d", n)}},
		})
	})
	return srv
}

func (f *fakeMax) last() sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

func newTestMirror(t *testing.T, f *fakeMax) *Mirror {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)
	m, err := NewMirror(Params{
		Token:            "test-token",
		ChannelID:        100,
		DiscussionChatID: 200,
		Signature:        "@channel",
		BaseURL:          "https://love.ngs.ru",
		APIBaseURL:       srv.URL,
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	// В тестах лимитеры не задерживают.
	m.limiters[100] = rate.NewLimiter(rate.Inf, 1)
	m.limiters[200] = rate.NewLimiter(rate.Inf, 1)
	return m
}

func TestPostNoteRequest(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	mid, err := m.PostNote(context.Background(), store.Note{
		ID: "n1", AuthorID: "77", AuthorName: "Мария <3", Text: "текст & <тэг>",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mid != "mid.000001" {
		t.Errorf("mid: %q", mid)
	}
	sent := f.last()
	if sent.auth != "test-token" {
		t.Errorf("заголовок Authorization: %q", sent.auth)
	}
	if sent.chatID != "100" {
		t.Errorf("chat_id: %q", sent.chatID)
	}
	if sent.noPrev != "true" {
		t.Errorf("disable_link_preview: %q", sent.noPrev)
	}
	if sent.rawBody["format"] != "html" {
		t.Errorf("format: %v", sent.rawBody["format"])
	}
	text := sent.body.Text
	if !strings.Contains(text, "Мария &lt;3") || !strings.Contains(text, "текст &amp; &lt;тэг&gt;") {
		t.Errorf("HTML не экранирован: %q", text)
	}
	if !strings.Contains(text, `<a href="https://love.ngs.ru/profile/77">`) {
		t.Errorf("нет ссылки на профиль: %q", text)
	}
	if !strings.Contains(text, "@channel") {
		t.Errorf("нет подписи: %q", text)
	}
}

func TestPostNoteAvatarAttached(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	_, err := m.PostNote(context.Background(), store.Note{
		ID: "n1", AuthorName: "А", Text: "т",
		AuthorAvatarURL: "https://cdn/ava.jpg",
	}, []byte("jpeg-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	img := attachmentOf(t, f.last(), model.AttachImage)
	if img.Payload.Token != "tok-1" {
		t.Errorf("токен вложения: %+v", img.Payload)
	}

	// Повторная отправка того же URL не грузит медиа заново (кэш токенов).
	if _, err := m.PostNote(context.Background(), store.Note{
		ID: "n2", AuthorName: "А", Text: "т2",
		AuthorAvatarURL: "https://cdn/ava.jpg",
	}, []byte("jpeg-bytes")); err != nil {
		t.Fatal(err)
	}
	if f.uploads != 1 {
		t.Errorf("загрузок: %d, ожидалась 1 (кэш URL→токен)", f.uploads)
	}
	if attachmentOf(t, f.last(), model.AttachImage).Payload.Token != "tok-1" {
		t.Errorf("кэшированный токен: %+v", f.last().body.Attachments)
	}
}

// attachmentOf находит вложение нужного типа в отправленном сообщении.
func attachmentOf(t *testing.T, sent sentMessage, at model.AttachmentType) model.Attachment {
	t.Helper()
	for _, a := range sent.body.Attachments {
		if a.Type == at {
			return a
		}
	}
	t.Fatalf("вложение %s не найдено: %+v", at, sent.body.Attachments)
	return model.Attachment{}
}

// Кнопка «Обсудить» на посте канала: ссылка чата снимается один раз (кэш).
func TestPostNoteDiscussButton(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	for _, id := range []string{"n1", "n2"} {
		if _, err := m.PostNote(context.Background(),
			store.Note{ID: id, AuthorName: "А", Text: "т"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	kb := attachmentOf(t, f.last(), model.AttachInlineKeyboard)
	if len(kb.Payload.Buttons) != 1 || len(kb.Payload.Buttons[0]) != 1 {
		t.Fatalf("клавиатура: %+v", kb.Payload.Buttons)
	}
	btn := kb.Payload.Buttons[0][0]
	if btn.Type != model.ButtonLink || btn.URL != "https://max.ru/join/test-chat-link" {
		t.Errorf("кнопка: %+v", btn)
	}
	if f.chatGets != 1 {
		t.Errorf("GetChat должен вызываться один раз (кэш), было %d", f.chatGets)
	}
	// В копии заметки в чате обсуждения кнопки нет.
	if _, err := m.StartThread(context.Background(),
		store.Note{ID: "n3", AuthorName: "А", Text: "т"}, "mid.post"); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.last().body.Attachments {
		if a.Type == model.AttachInlineKeyboard {
			t.Errorf("у копии в чате не должно быть кнопки: %+v", a)
		}
	}
}

func TestPostCommentReply(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	mid, err := m.PostComment(context.Background(),
		store.Note{ID: "n1"}, "mid.root",
		store.Comment{ID: 5, AuthorName: "Пётр", AuthorAge: "44",
			AuthorLink: "https://love.ngs.ru/profile/5", Text: "ответ <б>"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mid == "" {
		t.Error("пустой mid")
	}
	sent := f.last()
	if sent.chatID != "200" {
		t.Errorf("комментарий должен идти в чат обсуждения: chat_id=%q", sent.chatID)
	}
	if sent.body.Link == nil || sent.body.Link.Type != model.LinkTypeReply || sent.body.Link.Mid != "mid.root" {
		t.Errorf("reply-link: %+v", sent.body.Link)
	}
	if !strings.Contains(sent.body.Text, "Пётр, 44:") || !strings.Contains(sent.body.Text, "ответ &lt;б&gt;") {
		t.Errorf("текст комментария: %q", sent.body.Text)
	}
}

func TestStartThreadPostsCopy(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	thread, err := m.StartThread(context.Background(),
		store.Note{ID: "n1", AuthorName: "А", Text: "т"}, "mid.post")
	if err != nil {
		t.Fatal(err)
	}
	if thread == "" {
		t.Fatal("пустой корень треда")
	}
	sent := f.last()
	if sent.chatID != "200" {
		t.Errorf("копия должна идти в чат обсуждения: chat_id=%q", sent.chatID)
	}
	if strings.Contains(sent.body.Text, "@channel") {
		t.Errorf("копия в чате без подписи канала: %q", sent.body.Text)
	}
}

func TestSendRetriesAfter429(t *testing.T) {
	f := &fakeMax{t: t, failFirst: 1}
	m := newTestMirror(t, f)
	old := retryAfter
	retryAfter = 50 * time.Millisecond
	t.Cleanup(func() { retryAfter = old })

	start := time.Now()
	mid, err := m.PostNote(context.Background(), store.Note{ID: "n1", AuthorName: "А", Text: "т"}, nil)
	if err != nil {
		t.Fatalf("после 429 и повтора отправка должна пройти: %v", err)
	}
	if mid == "" {
		t.Error("пустой mid после повтора")
	}
	if time.Since(start) < retryAfter {
		t.Errorf("повтор должен ждать %v", retryAfter)
	}
	if len(f.sent) != 1 {
		t.Errorf("успешных отправок: %d", len(f.sent))
	}
}

func TestPostNoteImageByToken(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	mid, err := m.PostNoteImage(context.Background(), "mid.root",
		"https://cdn/pic.jpg", []byte("jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if mid == "" {
		t.Error("пустой mid")
	}
	sent := f.last()
	if sent.body.Link == nil || sent.body.Link.Mid != "mid.root" {
		t.Errorf("иллюстрация должна быть ответом в тред: %+v", sent.body.Link)
	}
	if len(sent.body.Attachments) != 1 || sent.body.Attachments[0].Payload.Token != "tok-1" {
		t.Errorf("вложение: %+v", sent.body.Attachments)
	}
}

// Порт tgx.ComposeSubNotice не должен разъезжаться с оригиналом: подписчик в
// MAX видит тот же состав — слово, автора, заметку, выдержку и ссылку.
func TestComposeSubNotice(t *testing.T) {
	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "Ищу того,\nкто пьёт чай"}
	c := store.Comment{ID: 7, AuthorName: "Виктор <3", AuthorAge: "45 лет",
		AuthorLink:  "https://love.ngs.ru/profile/1",
		PublishedAt: time.Date(2026, 7, 30, 14, 5, 0, 0, time.UTC),
		Text:        "выпьем рюмку чая & закусим"}
	got := composeSubNotice("рюмк", n, c, "https://max.ru/c/200/AZ-t-FzlEyg")

	for _, want := range []string{
		"<b>рюмк</b>",
		`<a href="https://love.ngs.ru/profile/1">Виктор &lt;3, 45 лет</a>`,
		"Мария: Ищу того, кто пьёт чай",
		"(30.07 14:05)",
		"выпьем рюмку чая &amp; закусим",
		`<a href="https://max.ru/c/200/AZ-t-FzlEyg">Открыть комментарий</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q в:\n%s", want, got)
		}
	}
}

// Подписчику ссылка нужна в тред мессенджера, а не на сайт: mid комментария
// известен, значит уведомление ведёт прямо на сообщение чата обсуждения.
func TestNotifySubscriberDeepLink(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	m.limiters[7] = rate.NewLimiter(rate.Inf, 1)

	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "т"}
	c := store.Comment{ID: 7, AuthorName: "Виктор", Text: "выпьем рюмку чая"}
	err := m.NotifySubscriber(context.Background(), 7, "рюмк", n, c,
		"mid.ffffb9b4e305e2e5019fadf85ce51328", "mid.ffffb9b4e305e2e5019fadf85ce51329")
	if err != nil {
		t.Fatal(err)
	}
	sent := f.last()
	if sent.userID != "7" {
		t.Errorf("user_id: %q", sent.userID)
	}
	if want := `<a href="https://max.ru/c/200/AZ-t-FzlEyk">Открыть комментарий</a>`; !strings.Contains(sent.body.Text, want) {
		t.Errorf("нет ссылки на сообщение чата:\n%s", sent.body.Text)
	}
}

// Запасной вариант: mid непонятного вида (например, доехавший из старой
// записи) — ссылка на комментарий на сайте, уведомление всё равно уходит.
func TestNotifySubscriberFallsBackToSite(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	m.limiters[7] = rate.NewLimiter(rate.Inf, 1)

	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "т"}
	c := store.Comment{ID: 7, AuthorName: "Виктор", Text: "выпьем рюмку чая"}
	if err := m.NotifySubscriber(context.Background(), 7, "рюмк", n, c, "", "странный-mid"); err != nil {
		t.Fatal(err)
	}
	want := `<a href="https://love.ngs.ru/notes/312818/#anchor-7">Открыть комментарий</a>`
	if !strings.Contains(f.last().body.Text, want) {
		t.Errorf("нет запасной ссылки на сайт:\n%s", f.last().body.Text)
	}
}
