package web

// Тесты приёма картинки. Перекодировщик здесь подделан целиком: настоящий
// ffmpeg проверяется в internal/imgconv, а тут важно другое — порядок проверок,
// цена отказа и то, что негодный файл не доходит до ядра.

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/imgconv"
	"lovegw/internal/platform"
)

// fakeShots — перекодировщик без ffmpeg.
type fakeShots struct {
	codec  string
	result imgconv.Result
	err    error
	calls  int
	// seen — что дали на вход последним. По нему проверяется, что до
	// перекодирования доезжают именно байты файла, а не части формы.
	seen []byte
}

func (f *fakeShots) Codec() string { return f.codec }

func (f *fakeShots) Convert(_ context.Context, in []byte) (imgconv.Result, error) {
	f.calls++
	f.seen = in
	if f.err != nil {
		return imgconv.Result{}, f.err
	}
	if f.result.Data == nil {
		return imgconv.Result{Data: []byte("перекодировано"), MIME: "image/webp", Width: 1600, Height: 900}, nil
	}
	return f.result, nil
}

func newShots() *fakeShots { return &fakeShots{codec: "webp"} }

// shotServer — площадка с вошедшим участником и подключённым перекодировщиком.
func shotServer(t *testing.T, conv imgconv.Converter) (http.Handler, *fakeWriter, *fakeStore, string) {
	h, wr, st, token, _ := shotServerFull(t, conv)
	return h, wr, st, token
}

func shotServerFull(t *testing.T, conv imgconv.Converter) (http.Handler, *fakeWriter, *fakeStore, string, *Server) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	st := noteStore()
	wr := &fakeWriter{}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, st, auth, wr, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })
	if conv != nil {
		srv.SetShots(conv)
	}
	return srv.routes(), wr, st, token, srv
}

// upload — форма с файлом, отправленная с нашей же страницы.
func upload(t *testing.T, token, body string, file []byte, opts ...func(*multipart.Writer)) *http.Request {
	return uploadTo(t, "/new", token, body, file, opts...)
}

// uploadTo — то же самое на заданный адрес: файл принимают ДВА маршрута —
// публикация заметки и правка, где картинку ставит администратор.
func uploadTo(t *testing.T, target, token, body string, file []byte, opts ...func(*multipart.Writer)) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	// CSRF первым полем — ровно как в разметке формы.
	if token != "" {
		_ = mw.WriteField(csrfField, csrfToken(token))
	}
	_ = mw.WriteField("body", body)
	for _, o := range opts {
		o(mw)
	}
	if file != nil {
		w, err := mw.CreateFormFile(shotField, "снимок.jpg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(file); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
	}
	return r
}

func TestNoteWithShotIsPublished(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	w := do(h, upload(t, token, "с картинкой", []byte("исходные байты")))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303: %s", w.Code, w.Body.String())
	}
	if wr.note.Body != "с картинкой" {
		t.Errorf("текст до ядра дошёл как %q", wr.note.Body)
	}
	if wr.shot == nil {
		t.Fatal("картинка до ядра не дошла")
	}
	// Морда отдаёт ядру ПЕРЕКОДИРОВАННЫЕ байты и размеры, которые задала сама:
	// stdlib не умеет читать webp, и посчитать их заново было бы нечем.
	if string(wr.shot.Data) != "перекодировано" {
		t.Errorf("до ядра дошли байты %q — похоже, исходник вместо результата", wr.shot.Data)
	}
	if wr.shot.Width != 1600 || wr.shot.Height != 900 {
		t.Errorf("размеры %d×%d", wr.shot.Width, wr.shot.Height)
	}
	if string(conv.seen) != "исходные байты" {
		t.Errorf("перекодировщику дали %q", conv.seen)
	}
}

// Прежняя дорога обязана работать: форма без файла ходит urlencoded, и так же
// приходят старые вкладки.
func TestPlainFormStillPublishes(t *testing.T) {
	h, wr, _, token := shotServer(t, newShots())

	w := do(h, postAs(t, "/new", url.Values{"body": {"без картинки"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if wr.shot != nil {
		t.Error("картинки не было, а до ядра что-то дошло")
	}
}

// Пустое поле файла — это «не выбрал», а не отказ.
func TestEmptyFileIsNotAnError(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	w := do(h, upload(t, token, "текст", []byte{}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303: %s", w.Code, w.Body.String())
	}
	if conv.calls != 0 {
		t.Error("пустой файл поехал в перекодировщик")
	}
	if wr.shot != nil {
		t.Error("пустой файл дошёл до ядра")
	}
}

func TestNotAnImageIsRefusedWithTheTextKept(t *testing.T) {
	conv := newShots()
	conv.err = imgconv.ErrNotImage
	h, wr, _, token := shotServer(t, conv)

	w := do(h, upload(t, token, "важный текст", []byte("<html>403</html>")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "важный текст") {
		t.Error("набранный текст потерян")
	}
	for _, want := range []string{"JPEG", "PNG", "GIF", "WebP"} {
		if !strings.Contains(body, want) {
			t.Errorf("в отказе не назван формат %s", want)
		}
	}
	// Заметка НЕ публикуется: опубликовать текст молча, без картинки, значит
	// решить за человека, который её прикладывал.
	if wr.note.Body != "" {
		t.Error("заметка без картинки всё-таки опубликована")
	}
	if !strings.Contains(body, "файл браузер не возвращает") {
		t.Error("про потерянный файл человеку не сказали")
	}
}

func TestTooManyPixelsNamesTheLimit(t *testing.T) {
	conv := newShots()
	conv.err = imgconv.ErrTooManyPx
	h, _, _, token := shotServer(t, conv)

	body := do(h, upload(t, token, "текст", []byte("огромная"))).Body.String()
	if !strings.Contains(body, "24 миллиона") {
		t.Errorf("предел не назван числом: %s", body)
	}
}

// Тело больше потолка режет http.MaxBytesReader, и отказ обязан назвать размер.
func TestTooBigBodyIsRefused(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	w := do(h, upload(t, token, "текст", bytes.Repeat([]byte{7}, uploadMaxBytes+1024)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("код %d, ожидался 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "10 МБ") {
		t.Errorf("потолок не назван: %s", w.Body.String())
	}
	if conv.calls != 0 {
		t.Error("слишком большое тело доехало до перекодировщика")
	}
	if wr.note.Body != "" {
		t.Error("заметка опубликована")
	}
}

func TestUploadWithoutCSRFIsRefused(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	// Токен не кладём вовсе — именно так выглядит форма с чужой страницы у
	// браузера, который не шлёт Sec-Fetch-Site.
	r := upload(t, "", "текст", []byte("байты"))
	r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
	w := do(h, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", w.Code)
	}
	if conv.calls != 0 {
		t.Error("файл без токена доехал до перекодировщика")
	}
	if wr.note.Body != "" {
		t.Error("заметка без токена опубликована")
	}
}

// Происхождение читается из заголовков и не стоит ничего — поэтому чужая
// страница получает отказ, не прислав ни байта тела.
func TestCrossSiteUploadRejectedBeforeBody(t *testing.T) {
	conv := newShots()
	h, _, _, token := shotServer(t, conv)

	r := upload(t, token, "текст", []byte("байты"))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	counted := &countingReader{r: r.Body}
	r.Body = counted

	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", got)
	}
	if counted.n != 0 {
		t.Errorf("прочитано %d байт тела: отказ обязан быть дешевле работы", counted.n)
	}
}

// Главный тест про осиротевшие файлы: отказ ПРАВА публиковать не должен стоить
// ни процессора на перекодирование, ни файла, который потом убрать некому.
func TestRefusedPublishSkipsConverter(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)
	wr.mayFail = platform.ErrRateLimited

	w := do(h, upload(t, token, "текст", []byte("байты")))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидался 429", w.Code)
	}
	if conv.calls != 0 {
		t.Fatal("перекодировщик звали при заведомом отказе — это файл на диске навсегда")
	}
}

func TestConverterFailureDoesNotPublish(t *testing.T) {
	conv := newShots()
	conv.err = errors.New("ffmpeg упал")
	h, wr, _, token := shotServer(t, conv)

	w := do(h, upload(t, token, "текст", []byte("байты")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	if wr.note.Body != "" {
		t.Error("заметка опубликована, хотя картинка не обработалась")
	}
	if !strings.Contains(w.Body.String(), "не обработалась") {
		t.Errorf("причина не названа: %s", w.Body.String())
	}
}

// Занятая очередь — не поломка, а честный отказ с Retry-After.
func TestShotSemaphoreBusy(t *testing.T) {
	conv := newShots()
	h, wr, _, token, srv := shotServerFull(t, conv)

	// Занимаем оба слота снаружи: ждать реального параллельного запроса в тесте
	// значило бы проверять планировщик, а не поведение.
	srvSem := srv.shotSem
	for i := 0; i < shotsInFlight; i++ {
		srvSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < shotsInFlight; i++ {
			<-srvSem
		}
	}()

	w := do(h, upload(t, token, "текст", []byte("байты")))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("нет Retry-After")
	}
	if wr.note.Body != "" {
		t.Error("заметка опубликована при занятой очереди")
	}
}

// Нет перекодировщика — нет и поля файла: кнопка, ведущая к отказу, хуже её
// отсутствия.
func TestFormHidesFileWithoutConverter(t *testing.T) {
	h, _, _, token := shotServer(t, nil)

	body := do(h, as(guest(t, "GET", "/new"), token)).Body.String()
	if strings.Contains(body, `type="file"`) {
		t.Error("поле файла показано, хотя принимать его нечем")
	}
	if strings.Contains(body, "multipart/form-data") {
		t.Error("форма объявлена multipart, хотя файлов не принимает")
	}
}

func TestFormOffersFileWithConverter(t *testing.T) {
	h, _, _, token := shotServer(t, newShots())

	body := do(h, as(guest(t, "GET", "/new"), token)).Body.String()
	if !strings.Contains(body, `type="file"`) {
		t.Fatal("поля файла нет")
	}
	if !strings.Contains(body, "multipart/form-data") {
		t.Error("форма не объявлена multipart — файл не доедет")
	}
	// Цена названа на самом экране, до отправки, а не в отказе после неё.
	for _, want := range []string{"10 МБ", "1600", "данные съёмки"} {
		if !strings.Contains(body, want) {
			t.Errorf("на форме не сказано про %q", want)
		}
	}
}

// Сборка ffmpeg без libwebp — рабочее состояние, а не отказ; но перекодировщик,
// не умеющий ничего, к приёму не подключается вовсе.
func TestConverterWithoutCodecIsNotAttached(t *testing.T) {
	h, _, _, token := shotServer(t, &fakeShots{codec: ""})

	if strings.Contains(do(h, as(guest(t, "GET", "/new"), token)).Body.String(), `type="file"`) {
		t.Error("поле файла показано при перекодировщике, который ничего не умеет")
	}
}

// ---------------------------------------------------------------- снятие картинки

func TestEditOffersDropOnlyWhenThereIsAShot(t *testing.T) {
	h, _, st, token := shotServer(t, newShots())
	target := makeEditable(st)

	if body := do(h, as(guest(t, "GET", target+"/edit"), token)).Body.String(); strings.Contains(body, "drop_shot") {
		t.Error("«снять картинку» предложено там, где картинки нет")
	}

	st.images = []platform.Media{{URL: "/media/ab/cd.webp", Width: 800, Height: 600}}
	body := do(h, as(guest(t, "GET", target+"/edit"), token)).Body.String()
	if !strings.Contains(body, "drop_shot") {
		t.Fatal("«снять картинку» не предложено")
	}
	if !strings.Contains(body, "Заменить картинку нельзя") {
		t.Error("про невозможность замены не сказано")
	}
}

func TestDropShotReachesCore(t *testing.T) {
	h, wr, st, token := shotServer(t, newShots())
	target := makeEditable(st)
	st.images = []platform.Media{{URL: "/media/ab/cd.webp"}}

	form := url.Values{"body": {"тот же текст"}, "drop_shot": {"1"}}
	if got := do(h, postAs(t, target+"/edit", form, token)).Code; got != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", got)
	}
	if !wr.dropShot {
		t.Fatal("просьба снять картинку до ядра не дошла")
	}
}

// makeEditable делает заметку хранилища своей, нативной и свежей — то есть
// такой, какую авторское окно править ещё даёт. Возвращает её адрес.
func makeEditable(st *fakeStore) string {
	st.note.ID = platform.NativeIDBase + 7
	st.note.Own = true
	st.note.CommentCount = 0
	st.note.EditedAt = nil
	st.note.PublishedAt = time.Now().Add(-time.Minute)
	st.notes = []platform.NoteView{st.note}
	st.thread = nil
	return "/n/" + strconv.FormatInt(st.note.ID, 10)
}

// countingReader считает, сколько байт тела действительно прочли.
type countingReader struct {
	r interface {
		Read([]byte) (int, error)
		Close() error
	}
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }
