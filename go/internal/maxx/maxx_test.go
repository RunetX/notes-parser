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

	"lovegw/internal/kbd"
	"lovegw/internal/msglimit"
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

	rejectReplyTo string // реплай на этот mid отвергать 400 (сообщение удалили)
	rejected      int

	answers []answeredCallback // POST /answers — ответы на нажатия
	edits   []editedMessage    // PUT /messages — правки сообщений

	markerSeen atomic.Int64 // последний маркер, переданный в GET /updates

	// updatesBody — что отдать первым ответом GET /updates вместо дефолтного
	// message_created (нажатия проверяем на сыром JSON, а не на model.Update).
	updatesBody string
}

type answeredCallback struct {
	callbackID string
	rawBody    map[string]any
}

type editedMessage struct {
	messageID string
	body      model.NewMessageBody
	rawBody   map[string]any
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
		if f.rejectReplyTo != "" && body.Link != nil && body.Link.Mid == f.rejectReplyTo {
			f.rejected++
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":"attachment.not.found","message":"Message to reply not found"}`)
			return
		}
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
	mux.HandleFunc("PUT /messages", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body model.NewMessageBody
		raw := map[string]any{}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			f.t.Errorf("разбор правки: %v", err)
		}
		_ = json.Unmarshal(data, &raw)
		f.edits = append(f.edits, editedMessage{
			messageID: r.URL.Query().Get("message_id"), body: body, rawBody: raw,
		})
		_ = json.NewEncoder(w).Encode(model.SimpleQueryResult{Success: true})
	})
	mux.HandleFunc("POST /answers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw := map[string]any{}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &raw)
		f.answers = append(f.answers, answeredCallback{
			callbackID: r.URL.Query().Get("callback_id"), rawBody: raw,
		})
		_ = json.NewEncoder(w).Encode(model.SimpleQueryResult{Success: true})
	})
	srv := httptest.NewServer(mux)
	mux.HandleFunc("GET /updates", func(w http.ResponseWriter, r *http.Request) {
		marker, _ := strconv.ParseInt(r.URL.Query().Get("marker"), 10, 64)
		if marker > f.markerSeen.Load() {
			f.markerSeen.Store(marker)
		}
		if marker == 0 {
			if f.updatesBody != "" {
				_, _ = w.Write([]byte(f.updatesBody))
				return
			}
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
	m.limiters = msglimit.Unlimited() // в тестах лимитеры не задерживают
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
	// Ссылок на анкету НГС проект не ставит нигде (27.08.2026): имя автора в
	// посте канала стоит текстом.
	if strings.Contains(text, "ngs.ru") {
		t.Errorf("в пост вернулась ссылка на анкету: %q", text)
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

// Кнопки на посте канала: «Обсудить» (ссылка чата снимается один раз, кэш) и
// «Подписаться» (нажатие приходит этому же боту — он и зеркало, и ЛС).
func TestPostNoteDiscussButton(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	// Идентификаторы настоящие: по их полосе решается, предлагать ли подписку.
	for _, id := range []string{"313027", "313028"} {
		if _, err := m.PostNote(context.Background(),
			store.Note{ID: id, AuthorName: "А", Text: "т"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	kb := attachmentOf(t, f.last(), model.AttachInlineKeyboard)
	if len(kb.Payload.Buttons) != 1 || len(kb.Payload.Buttons[0]) != 2 {
		t.Fatalf("клавиатура: %+v", kb.Payload.Buttons)
	}
	btn := kb.Payload.Buttons[0][0]
	if btn.Type != model.ButtonLink || btn.URL != "https://max.ru/join/test-chat-link" {
		t.Errorf("кнопка «Обсудить»: %+v", btn)
	}
	sub := kb.Payload.Buttons[0][1]
	if sub.Type != model.ButtonCallback || sub.Payload != "1:sub:313028" {
		t.Errorf("кнопка «Подписаться»: %+v", sub)
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
		store.Note{ID: "n1"}, "mid.root", "",
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

// Известный адресат — реплай на его сообщение, а не на корень треда: MAX сам
// покажет цитату исходного комментария.
func TestPostCommentRepliesToAddressee(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	if _, err := m.PostComment(context.Background(),
		store.Note{ID: "n1"}, "mid.root", "mid.parent",
		store.Comment{ID: 5, AuthorName: "Пётр", AuthorAge: "44", Text: "Аня, ответ"}, nil); err != nil {
		t.Fatal(err)
	}
	if link := f.last().body.Link; link == nil || link.Mid != "mid.parent" {
		t.Errorf("reply-link должен вести на адресата: %+v", link)
	}
}

// Сообщение адресата удалили — реплай на него MAX отвергает. Без запасного
// захода на корень треда комментарий остался бы неотправленным, а поскольку
// mirror не перескакивает через него, тред заметки встал бы навсегда.
func TestPostCommentFallsBackToThreadRoot(t *testing.T) {
	f := &fakeMax{t: t, rejectReplyTo: "mid.deleted"}
	m := newTestMirror(t, f)

	mid, err := m.PostComment(context.Background(),
		store.Note{ID: "n1"}, "mid.root", "mid.deleted",
		store.Comment{ID: 5, AuthorName: "Пётр", AuthorAge: "44", Text: "Аня, ответ"}, nil)
	if err != nil {
		t.Fatalf("комментарий должен уйти на корень треда: %v", err)
	}
	if mid == "" {
		t.Error("пустой mid")
	}
	if f.rejected != 1 {
		t.Errorf("отвергнутых попыток: %d, ожидалась 1", f.rejected)
	}
	if link := f.last().body.Link; link == nil || link.Mid != "mid.root" {
		t.Errorf("запасной reply-link должен вести на корень треда: %+v", link)
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

// Транспортная часть — подпись ссылки: в MAX она ведёт прямо на сообщение
// комментария. Состав уведомления проверяет пакет subnotice.
func TestComposeSubNoticeLinkLabel(t *testing.T) {
	got := ComposeSubNotice("к", store.Note{AuthorName: "А", Text: "т"},
		store.Comment{ID: 7, AuthorName: "Б", Text: "к"}, "https://max.ru/c/200/AZ-t-FzlEyg")
	if !strings.Contains(got, `<a href="https://max.ru/c/200/AZ-t-FzlEyg">Открыть комментарий</a>`) {
		t.Errorf("подпись ссылки MAX: %q", got)
	}
}

// Подписчику ссылка нужна в тред мессенджера, а не на сайт: mid комментария
// известен, значит уведомление ведёт прямо на сообщение чата обсуждения.
func TestNotifySubscriberDeepLink(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	m.limiters = msglimit.Unlimited()

	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "т"}
	c := store.Comment{ID: 7, AuthorName: "Виктор", Text: "выпьем рюмку чая"}
	link := m.SubCommentLink(n, c.ID, "mid.ffffb9b4e305e2e5019fadf85ce51329")
	err := m.NotifyHTML(context.Background(), 7,
		ComposeSubNotice("🔔 Ключевое слово «рюмк»", n, c, link), nil)
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
	m.limiters = msglimit.Unlimited()

	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "т"}
	c := store.Comment{ID: 7, AuthorName: "Виктор", Text: "выпьем рюмку чая"}
	link := m.SubCommentLink(n, c.ID, "странный-mid")
	if err := m.NotifyHTML(context.Background(), 7,
		ComposeSubNotice("🔔 Ключевое слово «рюмк»", n, c, link), nil); err != nil {
		t.Fatal(err)
	}
	want := `<a href="https://love.ngs.ru/notes/312818/#anchor-7">Открыть комментарий</a>`
	if !strings.Contains(f.last().body.Text, want) {
		t.Errorf("нет запасной ссылки на сайт:\n%s", f.last().body.Text)
	}
}

// Повод «новая заметка автора»: комментария нет, цитируем саму заметку, ссылка
// ведёт на пост канала, а под сообщением — кнопка «Отписаться».
func TestNotifyAuthorNoteWithUnsubButton(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)
	m.limiters = msglimit.Unlimited()

	n := store.Note{ID: "312818", AuthorName: "Мария", Text: "Ищу того,\nкто пьёт чай"}
	link := m.SubNoteLink(n, "mid.ffffb9b4e305e2e5019fadf85ce51329")
	kb := kbd.New().Row(kbd.Button{Text: "🔕 Отписаться", Payload: kbd.Pack("unsub1", "5")})
	if err := m.NotifyHTML(context.Background(), 7,
		ComposeSubNotice("✍️ Новая заметка автора Мария", n, store.Comment{}, link), kb); err != nil {
		t.Fatal(err)
	}
	sent := f.last()
	for _, want := range []string{
		"<b>✍️ Новая заметка автора Мария</b>",
		"Ищу того,\nкто пьёт чай",
		// Ссылка на пост — в канал (id 100), а не в чат обсуждения.
		`<a href="https://max.ru/c/100/AZ-t-FzlEyk">Открыть заметку</a>`,
	} {
		if !strings.Contains(sent.body.Text, want) {
			t.Errorf("нет %q в:\n%s", want, sent.body.Text)
		}
	}
	at := attachmentOf(t, sent, model.AttachInlineKeyboard)
	if len(at.Payload.Buttons) != 1 || at.Payload.Buttons[0][0].Payload != "1:unsub1:5" {
		t.Errorf("кнопка отписки: %+v", at.Payload.Buttons)
	}
}

// Заметка ещё не запощена (или mid непонятный) — ведём на сайт.
func TestSubNoteLinkFallsBackToSite(t *testing.T) {
	m := newTestMirror(t, &fakeMax{t: t})
	n := store.Note{ID: "312818"}
	if got := m.SubNoteLink(n, ""); got != "https://love.ngs.ru/notes/312818/" {
		t.Errorf("запасная ссылка на заметку: %q", got)
	}
}

// У заметки, написанной на площадке, кнопки «Подписаться» нет: подписки живут в
// SQLite и знают только заметки НГС, так что нажатие привело бы в «заметку не
// нашёл», а сработать подписка не смогла бы и потом. «Обсудить» при этом
// остаётся — тред у такой заметки самый настоящий.
func TestPostNativeNoteHasNoSubscribeButton(t *testing.T) {
	f := &fakeMax{t: t}
	m := newTestMirror(t, f)

	if _, err := m.PostNote(context.Background(),
		store.Note{ID: "100000000000", AuthorName: "А", Text: "т"}, nil); err != nil {
		t.Fatal(err)
	}
	kb := attachmentOf(t, f.last(), model.AttachInlineKeyboard)
	if len(kb.Payload.Buttons) != 1 || len(kb.Payload.Buttons[0]) != 1 {
		t.Fatalf("клавиатура: %+v", kb.Payload.Buttons)
	}
	if btn := kb.Payload.Buttons[0][0]; btn.Type != model.ButtonLink {
		t.Errorf("осталась не только «Обсудить»: %+v", btn)
	}
}
