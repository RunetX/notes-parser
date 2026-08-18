package web

// Тесты записи. Проверяют экраны и переходы; правила «можно ли» живут в ядре и
// проверяются против настоящего Postgres — здесь важно, что морда спрашивает то
// же самое и не показывает кнопок, ведущих к отказу.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"strconv"

	"lovegw/internal/platform"
)

// fakeWriter — ядро записи без Postgres. Помнит последний вызов: почти каждый
// тест ниже спрашивает именно «что дошло до ядра».
type fakeWriter struct {
	note     platform.NewNote
	comment  platform.NewComment
	edited   string
	nick     string
	reaction platform.NewReaction
	nextID   int64
	fail     error
}

func (f *fakeWriter) CreateNote(_ context.Context, in platform.NewNote) (int64, error) {
	f.note = in
	if f.fail != nil {
		return 0, f.fail
	}
	return f.id(), nil
}

func (f *fakeWriter) CreateComment(_ context.Context, in platform.NewComment) (int64, error) {
	f.comment = in
	if f.fail != nil {
		return 0, f.fail
	}
	return f.id(), nil
}

func (f *fakeWriter) React(_ context.Context, in platform.NewReaction) error {
	f.reaction = in
	return f.fail
}

func (f *fakeWriter) EditNote(_ context.Context, _, _ int64, body string) error {
	f.edited = body
	return f.fail
}

func (f *fakeWriter) SetOwnNick(_ context.Context, _ int64, nick string) error {
	if f.fail != nil {
		return f.fail
	}
	n := strings.TrimSpace(nick)
	if n == "" {
		return platform.ErrBadNick
	}
	f.nick = n
	return nil
}

func (f *fakeWriter) id() int64 {
	if f.nextID == 0 {
		f.nextID = platform.NativeIDBase + 1
	}
	return f.nextID
}

// writeServer — площадка с вошедшим участником и работающей записью.
func writeServer(t *testing.T, st *fakeStore) (http.Handler, *fakeWriter, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	wr := &fakeWriter{}
	return newFullServer(t, st, auth, wr, nil, Config{}), wr, token
}

func noteStore() *fakeStore {
	n := sampleNote()
	return &fakeStore{total: 1, notes: []platform.NoteView{n}, note: n, thread: sampleThread()}
}

// ---------------------------------------------------------------- кто может писать

// Читать может каждый, писать — только вошедший. Это единственное место, где
// «открыто всем» кончается.
func TestGuestCannotWrite(t *testing.T) {
	h, _, _ := writeServer(t, noteStore())

	if got := do(h, guest(t, "GET", "/new")).Header().Get("Location"); got != "/login" {
		t.Errorf("гостя с формы заметки ведёт на %q, ожидался /login", got)
	}
	for _, target := range []string{"/new", "/n/312811/reply", "/me/nick"} {
		w := do(h, post(t, target, url.Values{"body": {"текст"}}))
		if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
			t.Errorf("%s от гостя: код %d, ожидался отказ", target, w.Code)
		}
	}
}

// Гостю не показывают ни формы ответа, ни кнопки «написать»: кнопка, ведущая к
// отказу, хуже её отсутствия.
func TestGuestSeesInvitationInsteadOfForms(t *testing.T) {
	h, _, _ := writeServer(t, noteStore())

	feed := do(h, guest(t, "GET", "/")).Body.String()
	if strings.Contains(feed, "/new") {
		t.Error("гостю показали «Написать заметку»")
	}
	note := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(note, `name="body"`) {
		t.Error("гостю показали форму ответа")
	}
	if !strings.Contains(note, "войдите") {
		t.Error("гостю не предложили войти")
	}
}

// ---------------------------------------------------------------- CSRF

// Первый рубеж — заголовок происхождения, второй — скрытое поле. Проверяется
// именно второй: без него форма, отправленная с чужой страницы браузером, у
// которого нет Sec-Fetch-Site, прошла бы.
func TestWriteNeedsCSRFField(t *testing.T) {
	h, wr, token := writeServer(t, noteStore())

	w := do(h, as(post(t, "/new", url.Values{"body": {"текст"}}), token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("форма без токена: код %d, ожидался 403", w.Code)
	}
	if wr.note.Body != "" {
		t.Fatal("заметка без токена дошла до ядра")
	}

	// Чужой токен не годится: он выводится из сессии, а сессия у каждого своя.
	form := url.Values{"body": {"текст"}, csrfField: {csrfToken("чужая-сессия")}}
	if got := do(h, as(post(t, "/new", form), token)).Code; got != http.StatusForbidden {
		t.Errorf("чужой токен: код %d, ожидался 403", got)
	}
}

// ---------------------------------------------------------------- заметка

func TestPublishNote(t *testing.T) {
	h, wr, token := writeServer(t, noteStore())

	w := do(h, postAs(t, "/new", url.Values{
		"body": {"  Первая своя заметка.  "}, "anonymous": {"1"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if wr.note.AuthorID != testProfileID {
		t.Errorf("автор %d, ожидался %d", wr.note.AuthorID, testProfileID)
	}
	if !wr.note.Anonymous {
		t.Error("галочка «анонимно» до ядра не дошла")
	}
	// Заметка ведёт на свою страницу, а не обратно в ленту: автор хочет увидеть,
	// что получилось.
	if got := w.Header().Get("Location"); !strings.HasPrefix(got, "/n/") {
		t.Errorf("после публикации ведёт на %q", got)
	}
}

// Отказ не должен стоить человеку набранного текста.
func TestRefusalKeepsTheText(t *testing.T) {
	h, wr, token := writeServer(t, noteStore())
	wr.fail = platform.ErrRateLimited

	w := do(h, postAs(t, "/new", url.Values{"body": {"важный текст"}}, token))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидался 429", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "важный текст") {
		t.Error("набранный текст потерян")
	}
	if !strings.Contains(body, "Слишком часто") {
		t.Error("причина отказа не названа")
	}
}

// ---------------------------------------------------------------- ответ в тред

func TestReplyGoesToTheChosenComment(t *testing.T) {
	h, wr, token := writeServer(t, noteStore())

	w := do(h, postAs(t, "/n/312811/reply", url.Values{
		"body": {"а вот и нет"}, "reply_to": {"2"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if wr.comment.NoteID != 312811 || wr.comment.ReplyToID != 2 {
		t.Errorf("до ядра дошло note=%d reply_to=%d", wr.comment.NoteID, wr.comment.ReplyToID)
	}
	// Возврат — на якорь своей реплики: человек должен увидеть её на месте.
	if got := w.Header().Get("Location"); !strings.Contains(got, "#c") {
		t.Errorf("после ответа ведёт на %q, без якоря", got)
	}
}

// Ответ пишется в ЗЕРКАЛЬНУЮ заметку — в этом и смысл: своих заметок на старте
// ноль, а весь материал для разговора пришёл с НГС.
func TestReplyIntoMirroredNoteIsAllowed(t *testing.T) {
	st := noteStore()
	st.note.CommentsClosed = true // чужая отметка «не актуальна» — надпись, не запрет
	h, wr, token := writeServer(t, st)

	if got := do(h, postAs(t, "/n/312811/reply", url.Values{"body": {"ответ"}}, token)).Code; got != http.StatusSeeOther {
		t.Fatalf("код %d: чужая отметка запретила писать", got)
	}
	if wr.comment.Body != "ответ" {
		t.Error("комментарий до ядра не дошёл")
	}
}

// Наш замок — другое дело: он запирает и форму.
func TestOurLockHidesTheForm(t *testing.T) {
	st := noteStore()
	st.note.Locked = true
	h, _, token := writeServer(t, st)

	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Contains(body, `name="body"`) {
		t.Error("форма ответа показана в закрытом обсуждении")
	}
	if !strings.Contains(body, "закрыто модератором") {
		t.Error("не сказано, что обсуждение закрыто")
	}
}

// «Ответить» есть у каждой реплики, и адресат подставляется в форму.
func TestReplyFormNamesTheAddressee(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	page := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Count(page, `class="rep"`) != len(sampleThread()) {
		t.Errorf("«Ответить» не у каждой реплики: %d из %d",
			strings.Count(page, `class="rep"`), len(sampleThread()))
	}
	if !strings.Contains(page, "reply=2") {
		t.Error("ссылка «Ответить» не несёт номер комментария")
	}

	chosen := do(h, as(guest(t, "GET", "/n/312811?reply=2"), token)).Body.String()
	if !strings.Contains(chosen, `name="reply_to" value="2"`) {
		t.Error("адресат не подставлен в форму")
	}
	if !strings.Contains(chosen, "Ответ:") {
		t.Error("адресат не назван на экране")
	}
}

// Анонимной бывает ЗАМЕТКА, а реплика подписана всегда — так на НГС. В ядре это
// держится отсутствием поля у platform.NewComment, здесь проверяется вторая
// половина: на экране нет и предложения ответить анонимно.
func TestReplyIsAlwaysSigned(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	page := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	form := page[strings.Index(page, `class="replybox"`):]
	if strings.Contains(form, `name="anonymous"`) {
		t.Error("в форме ответа предложена анонимность")
	}
	// А у заметки она есть и остаётся.
	if !strings.Contains(do(h, as(guest(t, "GET", "/new"), token)).Body.String(), `name="anonymous"`) {
		t.Error("у заметки пропала анонимность, а она нужна")
	}
}

// Единственное место, где «своё» и «пришедшее с НГС» различаются на экране, — и
// различие показано на АВТОРЕ: у пишущего вопрос не «откуда текст», а «дойдёт
// ли мой ответ».
func TestReplyToShadowWarnsItWontReachNGS(t *testing.T) {
	st := noteStore()
	st.thread = sampleThread()
	st.thread[1].Author.Shadow = true
	h, _, token := writeServer(t, st)

	page := do(h, as(guest(t, "GET", "/n/312811?reply=2"), token)).Body.String()
	if !strings.Contains(page, "не переехал") {
		t.Error("не сказано, что адресат ещё не на площадке")
	}
	// А у участника такой строки быть не должно.
	st.thread[1].Author.Shadow = false
	page = do(h, as(guest(t, "GET", "/n/312811?reply=2"), token)).Body.String()
	if strings.Contains(page, "не переехал") {
		t.Error("участника объявили тенью")
	}
}

// ---------------------------------------------------------------- правка

// Окно правки закрывают три вещи, и любая — насовсем. Ссылка на правку обязана
// исчезать ровно тогда же, когда ядро перестаёт правку принимать: кнопка,
// отвечающая отказом, — худшее из возможного.
func TestEditLinkLivesExactlyAsLongAsTheWindow(t *testing.T) {
	fresh := time.Now().Add(-time.Minute)
	edited := fresh.Add(30 * time.Second)
	cases := []struct {
		name string
		note func(*platform.NoteView)
		want bool
	}{
		{"своя, свежая, без ответов", func(*platform.NoteView) {}, true},
		{"чужая", func(n *platform.NoteView) { n.Own = false }, false},
		{"под ней уже отвечают", func(n *platform.NoteView) { n.CommentCount = 1 }, false},
		{"уже правили", func(n *platform.NoteView) { n.EditedAt = &edited }, false},
		{"старше десяти минут", func(n *platform.NoteView) {
			n.PublishedAt = time.Now().Add(-platform.EditWindow - time.Second)
		}, false},
		{"зеркальная", func(n *platform.NoteView) { n.ID = 312811 }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := noteStore()
			st.note.ID = platform.NativeIDBase + 7
			st.note.Own = true
			st.note.CommentCount = 0
			st.note.PublishedAt = fresh
			st.thread = nil
			c.note(&st.note)
			h, _, token := writeServer(t, st)

			target := "/n/" + strconv.FormatInt(st.note.ID, 10)
			page := do(h, as(guest(t, "GET", target), token)).Body.String()
			if got := strings.Contains(page, "/edit"); got != c.want {
				t.Errorf("ссылка на правку: %v, ожидалось %v", got, c.want)
			}
			w := do(h, as(guest(t, "GET", target+"/edit"), token))
			okEdit := w.Code == http.StatusOK
			if okEdit != c.want {
				t.Errorf("экран правки: код %d, ожидалось разрешено=%v", w.Code, c.want)
			}
		})
	}
}

func TestEditSendsNewText(t *testing.T) {
	st := noteStore()
	st.note.ID = platform.NativeIDBase + 7
	st.note.Own = true
	st.note.CommentCount = 0
	st.note.PublishedAt = time.Now().Add(-time.Minute)
	h, wr, token := writeServer(t, st)

	target := "/n/" + strconv.FormatInt(st.note.ID, 10) + "/edit"
	w := do(h, postAs(t, target, url.Values{"body": {"поправленный текст"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if wr.edited != "поправленный текст" {
		t.Errorf("до ядра дошло %q", wr.edited)
	}
}

// ---------------------------------------------------------------- ник

// Смена ника под запрет «участник только пишет» не попадает: это не публикация,
// а собственное имя, и текст согласия обещает эту возможность прямо.
func TestNickChange(t *testing.T) {
	h, wr, token := writeServer(t, &fakeStore{})

	if got := do(h, postAs(t, "/me/nick", url.Values{"nick": {"Паноптикум"}}, token)).Code; got != http.StatusSeeOther {
		t.Fatalf("код %d", got)
	}
	if wr.nick != "Паноптикум" {
		t.Errorf("до ядра дошёл ник %q", wr.nick)
	}
	if got := do(h, postAs(t, "/me/nick", url.Values{"nick": {"   "}}, token)).Code; got != http.StatusBadRequest {
		t.Errorf("пустой ник: код %d, ожидался 400", got)
	}
}
