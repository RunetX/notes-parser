package web

// Тесты записи. Проверяют экраны и переходы; правила «можно ли» живут в ядре и
// проверяются против настоящего Postgres — здесь важно, что морда спрашивает то
// же самое и не показывает кнопок, ведущих к отказу.

import (
	"context"
	"mime/multipart"
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
	avatar   fakeAvatar
	// cleared — у кого сняли фото. Ноль означает «ядро об этом не просили», и
	// половина тестов аватара проверяет именно это.
	cleared int64
	nextID   int64
	fail     error
	// shot — что дошло до ядра вместе с заметкой; nil означает «картинки не
	// было». Половина тестов приёма проверяет именно это: перекодировщик не
	// должен звать ядро там, где файл негоден.
	shot *Shot
	// mayFail — чем отвечает предварительная проверка права публиковать.
	mayFail error
	// dropShot — просили ли снять картинку той же правкой.
	dropShot bool
}

// fakeAvatar — что дошло до ядра при обновлении фото: откуда взято и сами байты.
// Пустой url означает «до ядра не дошло вовсе», и половина тестов аватара
// проверяет именно это: фото не должно теряться там, где его не просили менять.
type fakeAvatar struct {
	url  string
	data []byte
}

func (f *fakeWriter) CreateNote(_ context.Context, in platform.NewNote, shot *Shot) (int64, error) {
	f.note = in
	f.shot = shot
	if f.fail != nil {
		return 0, f.fail
	}
	return f.id(), nil
}

func (f *fakeWriter) MayPublishNote(context.Context, int64) error { return f.mayFail }

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

func (f *fakeWriter) EditNote(_ context.Context, _ int64, in platform.NoteEdit) error {
	f.edited = in.Body
	f.dropShot = in.DropImage
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

func (f *fakeWriter) SetOwnAvatar(_ context.Context, _ int64, url string, data []byte) error {
	if f.fail != nil {
		return f.fail
	}
	f.avatar = fakeAvatar{url: url, data: data}
	return nil
}

func (f *fakeWriter) ClearOwnAvatar(_ context.Context, userID int64) error {
	if f.fail != nil {
		return f.fail
	}
	f.cleared = userID
	f.avatar = fakeAvatar{}
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
	return newFullServer(t, st, auth, wr, nil, nil, Config{}), wr, token
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

// Форма ответа встаёт ПОД той репликой, которой отвечают, а не в конце страницы:
// нажатие «Ответить» уводило в самый низ треда, и оттуда приходилось листать
// обратно к месту разговора (жалоба владельца 23.08.2026). Проверяется место в
// разметке, а не наличие формы: без JS его выбирает сервер, и выбрать он может
// только один раз.
func TestReplyFormStandsUnderTheAddressee(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	page := do(h, as(guest(t, "GET", "/n/312811?reply=2"), token)).Body.String()
	form := strings.Index(page, `id="reply"`)
	if form < 0 {
		t.Fatal("формы ответа нет вовсе")
	}
	if strings.Count(page, `id="reply"`) != 1 {
		t.Error("формы ответа две: одна под репликой, вторая внизу")
	}
	// Между адресатом и следующей репликой — значит внутри треда, на своём месте.
	if !(strings.Index(page, `id="c2"`) < form && form < strings.Index(page, `id="c3"`)) {
		t.Error("форма ответа стоит не под адресатом")
	}

	// Ссылка «Ответить» ведёт на САМУ реплику: иначе человек приезжает к форме, не
	// видя, кому отвечает, — а свёрнутая простыня рядом осталась бы свёрнутой.
	if !strings.Contains(page, "reply=2#c2") {
		t.Error("«Ответить» ведёт не на реплику")
	}

	// Ответ на ЗАМЕТКУ — по-прежнему внизу: он про весь тред, а не про чью-то
	// реплику, и места лучше конца страницы у него нет.
	plain := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Index(plain, `id="reply"`) < strings.LastIndex(plain, `id="c3"`) {
		t.Error("форма ответа на заметку уехала внутрь треда")
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

// ---------------------------------------------------------------- правка администратора

// foreignNativeNote — заготовка с ЧУЖОЙ нативной заметкой, старой и с ответами: ровно
// та, которую авторское окно править уже не даёт.
// shotModServer — площадка с вошедшим администратором, живой модерацией и
// подключённым перекодировщиком: без него поля файла в форме нет вовсе.
func shotModServer(t *testing.T, st *fakeStore, role platform.Role) (http.Handler, *fakeMod, string, *fakeShots) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: modUserID, Nick: "Хатуль мадан", Kind: platform.KindMember, Role: role,
	})
	mod := newFakeMod()
	conv := newShots()
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, st, auth, &fakeWriter{}, mod, nil)
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetShots(conv)
	return srv.routes(), mod, token, conv
}

func foreignNativeNote() *fakeStore {
	st := noteStore()
	st.note.Own = false
	st.note.CommentCount = 3
	st.note.PublishedAt = time.Now().Add(-48 * time.Hour)
	return st
}

// Единственное место площадки, где чужой текст меняется, а не скрывается, — и
// дверь у него выше модераторской. Модератор решает про слова: убрать их из
// разговора; переписать сказанное ближе к тому, чем ведает администратор.
func TestПравитьЧужуюЗаметкуМожетТолькоАдминистратор(t *testing.T) {
	native := "/n/" + strconv.FormatInt(platform.NativeIDBase+7, 10)
	for _, c := range []struct {
		name string
		role platform.Role
		want bool
	}{
		{"участник", platform.RoleUser, false},
		{"модератор", platform.RoleModerator, false},
		{"администратор", platform.RoleAdmin, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := modServerOn(t, foreignNativeNote(), c.role)

			page := do(h, as(guest(t, "GET", native), token)).Body.String()
			if got := strings.Contains(page, native+"/edit"); got != c.want {
				t.Errorf("ссылка на правку: %v, ожидалось %v", got, c.want)
			}
			w := do(h, as(guest(t, "GET", native+"/edit"), token))
			if got := w.Code == http.StatusOK; got != c.want {
				t.Errorf("экран правки: код %d, ожидалось разрешено=%v", w.Code, c.want)
			}
		})
	}
}

// Текст и «зачем» доходят до ядра, а автор и анонимность в правку не попадают
// вовсе: кто сказал — вопрос не редакторский.
func TestПравкаАдминистратораДоходитДоЯдра(t *testing.T) {
	h, mod, token := modServerOn(t, foreignNativeNote(), platform.RoleAdmin)
	target := "/n/" + strconv.FormatInt(platform.NativeIDBase+7, 10) + "/edit"

	w := do(h, postAs(t, target, url.Values{
		"body":   {"объявление без опечатки"},
		"reason": {"опечатка в дате"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if mod.edited != "объявление без опечатки" || mod.editReason != "опечатка в дате" {
		t.Errorf("до ядра дошло %q / %q", mod.edited, mod.editReason)
	}
	// Правка идёт СВОЕЙ дорогой, а не авторской: у той свои правила, и ядро
	// ответило бы на чужую заметку отказом.
	if len(mod.acts) != 1 || !strings.HasPrefix(mod.acts[0], "edit ") {
		t.Errorf("ядро позвали как %v", mod.acts)
	}
}

// Текст зеркальной заметки не правит НИКТО: здесь её копия, и молча разойтись с
// оригиналом значило бы соврать читателю о том, что он читает копию. А вот
// КАРТИНКУ администратор ставит и ей (27.08.2026): у копии иллюстрация живёт
// ссылкой на сервер НГС и в день, когда он перестанет отдавать файлы, обратится
// в пустое место — восполнить её иначе нечем.
func TestУЗеркальнойЗаметкиАдминистраторМеняетТолькоКартинку(t *testing.T) {
	h, mod, token, conv := shotModServer(t, foreignNativeNote(), platform.RoleAdmin)

	page := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if !strings.Contains(page, "/n/312811/edit") {
		t.Fatal("под зеркальной заметкой не предложена картинка")
	}
	// Подпись у ссылки другая: «Поправить» под текстом, который не правится,
	// обещало бы не то.
	if !strings.Contains(page, `<span class="lbl">Картинка</span>`) {
		t.Error("ссылка подписана так, будто правится текст")
	}

	form := do(h, as(guest(t, "GET", "/n/312811/edit"), token)).Body.String()
	if strings.Contains(form, "<textarea") {
		t.Error("на экране есть поле текста, которого править нельзя")
	}
	if !strings.Contains(form, `name="shot"`) || !strings.Contains(form, "multipart/form-data") {
		t.Error("на экране нет поля файла — ради него форма и открывается")
	}
	// Снять картинку у копии нельзя: сверка вернула бы её через пять минут.
	if strings.Contains(form, `name="drop_shot"`) {
		t.Error("предложено снять иллюстрацию у зеркальной заметки")
	}

	w := do(h, uploadTo(t, "/n/312811/edit", token, "переписал", []byte("файл"),
		func(mw *multipart.Writer) {
			_ = mw.WriteField("reason", "картинка с НГС отвалилась")
		}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, тело: %s", w.Code, w.Body.String())
	}
	if conv.calls != 1 {
		t.Errorf("перекодировщик позвали %d раз", conv.calls)
	}
	if mod.edited != "" {
		t.Errorf("до ядра дошёл ТЕКСТ зеркальной заметки: %q", mod.edited)
	}
	if !mod.shotSet || mod.shot == nil || mod.shotNote != 312811 {
		t.Fatalf("картинка до ядра не дошла: set=%v shot=%v note=%d", mod.shotSet, mod.shot, mod.shotNote)
	}
	if mod.editReason != "картинка с НГС отвалилась" {
		t.Errorf("«зачем» до журнала не дошло: %q", mod.editReason)
	}
	if len(mod.acts) != 1 || !strings.HasPrefix(mod.acts[0], "image ") {
		t.Errorf("ядро позвали как %v", mod.acts)
	}
}

// У НАТИВНОЙ заметки та же форма меняет и текст, и картинку — но это два разных
// действия ядра с разными дверями и разными записями в журнале.
func TestУНативнойЗаметкиАдминистраторМеняетИТекстИКартинку(t *testing.T) {
	native := sampleNote()
	native.ID = platform.NativeIDBase + 7
	native.Own = false
	native.CommentCount = 3
	native.PublishedAt = time.Now().Add(-48 * time.Hour)
	st := &fakeStore{total: 1, notes: []platform.NoteView{native}, note: native}
	h, mod, token, _ := shotModServer(t, st, platform.RoleAdmin)
	target := "/n/" + strconv.FormatInt(native.ID, 10) + "/edit"

	if w := do(h, uploadTo(t, target, token, "объявление без опечатки", []byte("файл"))); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, тело: %s", w.Code, w.Body.String())
	}
	if mod.edited != "объявление без опечатки" {
		t.Errorf("текст до ядра не дошёл: %q", mod.edited)
	}
	if !mod.shotSet || mod.shot == nil {
		t.Error("картинка до ядра не дошла")
	}
	if len(mod.acts) != 2 || !strings.HasPrefix(mod.acts[0], "edit ") ||
		!strings.HasPrefix(mod.acts[1], "image ") {
		t.Errorf("ядро позвали как %v, ожидались правка и картинка", mod.acts)
	}
}

// Отказ ядра на картинке не проваливается пятисоткой и не теряет набранного.
func TestОтказНаКартинкеПоказываетсяЧеловеком(t *testing.T) {
	h, mod, token, _ := shotModServer(t, foreignNativeNote(), platform.RoleAdmin)
	mod.fail = platform.ErrRateLimited

	w := do(h, uploadTo(t, "/n/312811/edit", token, "", []byte("файл")))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидался 429", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "Слишком часто") {
		t.Errorf("отказ не объяснён человеком: %s", body)
	}
}

// Правку видно на странице: текст под уже написанными ответами изменился, и
// узнать об этом читатель обязан со страницы, а не из сравнения с памятью.
func TestИсправленнаяЗаметкаПомечена(t *testing.T) {
	st := foreignNativeNote()
	h, _, token := modServerOn(t, st, platform.RoleUser)
	if strings.Contains(do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String(), "исправлено") {
		t.Error("метка стоит у заметки, которую не правили")
	}

	edited := time.Now()
	st.note.EditedAt = &edited
	h, _, token = modServerOn(t, st, platform.RoleUser)
	if !strings.Contains(do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String(), "исправлено") {
		t.Error("исправленная заметка не помечена")
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

// Разметка в своём тексте ожила (bbcode.go), и узнать о ней человеку неоткуда:
// на самом НГС теги умерли в 2014-м. Поэтому справочник стоит под ОБЕИМИ
// формами — и заметки, и ответа.
func TestFormsShowMarkupHelp(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	for _, target := range []string{"/new", "/n/312811"} {
		page := do(h, as(guest(t, "GET", target), token)).Body.String()
		if !strings.Contains(page, "<summary>Разметка</summary>") {
			t.Errorf("%s: справочника разметки под формой нет", target)
		}
		if !strings.Contains(page, "<code>[b]жирный[/b]</code>") {
			t.Errorf("%s: в справочнике нет кода, который набирают", target)
		}
	}
}
