package web

// Тесты предзагрузки картинки. Перекодировщик здесь подделан, как и в
// shot_test.go: проверяется не ffmpeg, а то, ради чего затея сделана, — картинка
// уезжает раньше заметки, переживает отказ публикации и не достаётся чужому.

import (
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"lovegw/internal/imgconv"
	"lovegw/internal/platform"
)

// draftIDRe вытаскивает номер черновика из карточки, присланной сервером. Тесты
// смотрят на РАЗМЕТКУ, а не на внутренности склада, потому что именно её
// отправит обратно браузер.
var draftIDRe = regexp.MustCompile(`name="shot_id" value="([^"]+)"`)

// preload — предзагрузка файла со страницы формы.
func preload(t *testing.T, h http.Handler, token string, file []byte) (string, *httptest.ResponseRecorder) {
	t.Helper()
	w := do(h, uploadTo(t, "/shot", token, "", file))
	m := draftIDRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		return "", w
	}
	return m[1], w
}

// withDraft — поле с номером черновика, как его пришлёт форма.
func withDraft(id string) func(*multipart.Writer) {
	return func(mw *multipart.Writer) { _ = mw.WriteField(draftField, id) }
}

// Главный путь: картинка уходит по выбору, заметка — потом и без файла.
func TestPreloadedShotIsPublishedWithTheNote(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	id, w := preload(t, h, token, []byte("исходные байты"))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200: %s", w.Code, w.Body.String())
	}
	if id == "" {
		t.Fatalf("сервер не вернул номера черновика: %s", w.Body.String())
	}
	if conv.calls != 1 {
		t.Errorf("перекодировщика звали %d раз", conv.calls)
	}
	// Карточка обязана показать то, что ЛЯЖЕТ в заметку, — размеры после
	// перекодирования, а не размеры присланного файла.
	if body := w.Body.String(); !strings.Contains(body, "1600") || !strings.Contains(body, "900") {
		t.Errorf("в карточке нет размеров перекодированного: %s", body)
	}

	w = do(h, uploadTo(t, "/new", token, "с картинкой", nil, withDraft(id)))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("публикация: код %d, ожидался 303: %s", w.Code, w.Body.String())
	}
	if wr.shot == nil {
		t.Fatal("картинка до ядра не дошла")
	}
	if string(wr.shot.Data) != "перекодировано" {
		t.Errorf("до ядра дошли байты %q", wr.shot.Data)
	}
	if conv.calls != 1 {
		t.Errorf("перекодировщика позвали ещё раз на публикации (%d): черновик уже готов", conv.calls)
	}
}

// Ради этого всё и делалось: отказ ядра не должен стоить человеку картинки.
// Раньше файл терялся вместе с ответом браузера, и приложить его надо было
// заново — вместе с уже написанным текстом.
func TestDraftSurvivesRefusedPublish(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	id, _ := preload(t, h, token, []byte("байты"))
	if id == "" {
		t.Fatal("черновик не завёлся")
	}
	wr.fail = platform.ErrRateLimited

	w := do(h, uploadTo(t, "/new", token, "важный текст", nil, withDraft(id)))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидался 429", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "важный текст") {
		t.Error("набранный текст потерян")
	}
	if !strings.Contains(body, `value="`+id+`"`) {
		t.Errorf("картинка не вернулась на форму: %s", body)
	}
	if strings.Contains(body, "файл браузер не возвращает") {
		t.Error("человеку сказали заново выбрать файл, который лежит на сервере")
	}

	// И вторая попытка проходит той же картинкой, без второго выбора файла.
	wr.fail = nil
	w = do(h, uploadTo(t, "/new", token, "важный текст", nil, withDraft(id)))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("повторная публикация: код %d, ожидался 303: %s", w.Code, w.Body.String())
	}
	if wr.shot == nil {
		t.Fatal("картинка до ядра не дошла со второй попытки")
	}
}

// Ушедший в заметку черновик больше не нужен: держать чужие байты в памяти
// дольше, чем требуется, незачем.
func TestDraftIsForgottenAfterPublish(t *testing.T) {
	h, _, _, token, srv := shotServerFull(t, newShots())

	id, _ := preload(t, h, token, []byte("байты"))
	if srv.drafts.len() != 1 {
		t.Fatalf("черновиков %d, ожидался один", srv.drafts.len())
	}
	if w := do(h, uploadTo(t, "/new", token, "текст", nil, withDraft(id))); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if srv.drafts.len() != 0 {
		t.Errorf("черновик остался в памяти после публикации")
	}
}

// Файл, приложенный прямо сейчас, сильнее номера из скрытого поля: без скрипта
// он придёт телом формы, и это тот же выбор человека, только более поздний.
func TestUploadedFileBeatsDraft(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)

	id, _ := preload(t, h, token, []byte("старая"))
	conv.result = imgconv.Result{Data: []byte("новая"), MIME: "image/webp", Width: 800, Height: 600}

	w := do(h, uploadTo(t, "/new", token, "текст", []byte("новые байты"), withDraft(id)))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d: %s", w.Code, w.Body.String())
	}
	if wr.shot == nil || string(wr.shot.Data) != "новая" {
		t.Errorf("в заметку ушёл черновик вместо только что приложенного файла")
	}
}

// Черновик — вещь личная: чужой номер не подхватывается публикацией и не
// отдаётся показом. Отвечают оба одинаково, потому что существование чужого
// черновика само по себе сведения.
func TestDraftBelongsToItsOwner(t *testing.T) {
	h, wr, _, token, srv := shotServerFull(t, newShots())

	id, _ := preload(t, h, token, []byte("байты"))
	// Тот же склад, но владелец другой: так выглядит попытка подставить чужой
	// номер в свою форму.
	if _, ok := srv.drafts.get(testProfileID+1, id); ok {
		t.Fatal("черновик отдан не своему владельцу")
	}
	other := srv.drafts.put(testProfileID+1, Shot{Data: []byte("чужая"), MIME: "image/webp", Width: 10, Height: 10})

	w := do(h, uploadTo(t, "/new", token, "текст", nil, withDraft(other)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400: %s", w.Code, w.Body.String())
	}
	if wr.note.Body != "" {
		t.Error("заметка с чужой картинкой опубликована")
	}
	if !strings.Contains(w.Body.String(), "приложите её заново") {
		t.Errorf("человеку не сказали, что делать: %s", w.Body.String())
	}

	if got := do(h, as(guest(t, "GET", "/shot/"+other), token)).Code; got != http.StatusNotFound {
		t.Errorf("чужой черновик показан: код %d", got)
	}
}

// Превью — это ровно те байты, что лягут в заметку, и отдаются они своим типом:
// без него браузер угадывал бы, а nosniff на угадывание не оставляет права.
func TestDraftFileIsServedToItsOwner(t *testing.T) {
	h, _, _, token := shotServer(t, newShots())

	id, _ := preload(t, h, token, []byte("байты"))
	w := do(h, as(guest(t, "GET", "/shot/"+id), token))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", w.Code)
	}
	if w.Body.String() != "перекодировано" {
		t.Errorf("отдано %q", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "image/webp" {
		t.Errorf("тип %q", got)
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("превью кэшируется (%q), а файла ещё нет и может не быть вовсе", got)
	}
}

// Негодный файл отвергается ДО того, как человек написал заметку, — в этом
// половина смысла предзагрузки. Ответ 400 нужен скрипту: по нему он снимает
// выбор из поля, чтобы файл не поехал второй раз телом формы.
func TestPreloadRefusesBadFile(t *testing.T) {
	conv := newShots()
	conv.err = imgconv.ErrNotImage
	h, _, _, token, srv := shotServerFull(t, conv)

	_, w := preload(t, h, token, []byte("<html>403</html>"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Это не картинка") {
		t.Errorf("причина не названа: %s", w.Body.String())
	}
	if srv.drafts.len() != 0 {
		t.Error("негодный файл всё-таки лёг черновиком")
	}
}

// Тот же порядок проверок, что у публикации: право спрашивается ДО ffmpeg.
// Здесь это уже не про осиротевший файл (черновик лежит в памяти), а про
// процессор, которого у морды шесть десятых ядра.
func TestPreloadChecksRightBeforeConverting(t *testing.T) {
	conv := newShots()
	h, wr, _, token := shotServer(t, conv)
	wr.mayFail = platform.ErrRateLimited

	_, w := preload(t, h, token, []byte("байты"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("код %d, ожидался 429", w.Code)
	}
	if conv.calls != 0 {
		t.Error("перекодировщика позвали при заведомом отказе")
	}
}

// У администратора своя дверь: MayPublishNote считает и частоту заметок, а он
// картинку не публикует, а ставит её чужой — в том числе зеркальной.
func TestPreloadAllowsAdminOverRateLimit(t *testing.T) {
	conv := newShots()
	h, wr, token := adminShotServer(t, conv)
	wr.mayFail = platform.ErrRateLimited

	id, w := preload(t, h, token, []byte("байты"))
	if w.Code != http.StatusOK || id == "" {
		t.Fatalf("администратору отказано: код %d, %s", w.Code, w.Body.String())
	}
}

// adminShotServer — та же площадка, но вошедший в ней администратор.
func adminShotServer(t *testing.T, conv imgconv.Converter) (http.Handler, *fakeWriter, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: platform.RoleAdmin,
	}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	wr := &fakeWriter{}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, noteStore(), auth, wr, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetShots(conv)
	return srv.routes(), wr, token
}

// Без токена файл не принимается: сессия в куке — это ещё не «человек нажал
// кнопку на нашей форме».
func TestPreloadWithoutCSRFIsRefused(t *testing.T) {
	conv := newShots()
	h, _, _, token := shotServer(t, conv)

	r := uploadTo(t, "/shot", "", "", []byte("байты"))
	r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", got)
	}
	if conv.calls != 0 {
		t.Error("файл без токена доехал до перекодировщика")
	}
}

// Нет перекодировщика — нет и пути: поля файла на форме в этом случае тоже нет,
// и открытая дверь вела бы к гарантированному отказу.
func TestPreloadIsAbsentWithoutConverter(t *testing.T) {
	h, _, _, token := shotServer(t, nil)

	_, w := preload(t, h, token, []byte("байты"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404", w.Code)
	}
}

// ------------------------------------------------------------------ склад

// Картинка у заметки одна, поэтому перевыбор файла вытесняет прежний черновик:
// иначе склад рос бы ровно от того, что человек передумал.
func TestDraftReplacesPreviousOfSameOwner(t *testing.T) {
	d := newShotDrafts()
	first := d.put(7, Shot{Data: []byte("первая")})
	second := d.put(7, Shot{Data: []byte("вторая")})

	if _, ok := d.get(7, first); ok {
		t.Error("прежний черновик остался")
	}
	if _, ok := d.get(7, second); !ok {
		t.Error("новый черновик не лёг")
	}
	if d.len() != 1 {
		t.Errorf("черновиков %d, ожидался один", d.len())
	}
}

func TestDraftExpires(t *testing.T) {
	d := newShotDrafts()
	now := time.Now()
	d.now = func() time.Time { return now }

	id := d.put(7, Shot{Data: []byte("байты")})
	now = now.Add(draftTTL + time.Minute)
	if _, ok := d.get(7, id); ok {
		t.Error("протухший черновик отдан")
	}
	if d.len() != 0 {
		t.Error("протухший черновик остался в памяти")
	}
}

// Склад живёт в памяти контейнера, у которого её триста мегабайт, — значит у
// него обязан быть верхний край. Место освобождают самые старые: отказ пришёлся
// бы на того, кто прямо сейчас смотрит на форму.
func TestDraftsAreCapped(t *testing.T) {
	d := newShotDrafts()
	now := time.Now()
	d.now = func() time.Time { return now }

	var first string
	for i := range draftsMax + 5 {
		now = now.Add(time.Second)
		id := d.put(int64(i+1), Shot{Data: []byte("байты")})
		if i == 0 {
			first = id
		}
	}
	if d.len() > draftsMax {
		t.Errorf("черновиков %d при потолке %d", d.len(), draftsMax)
	}
	if _, ok := d.get(1, first); ok {
		t.Error("вытеснять надо было самый старый")
	}
}

func TestDraftsAreCappedByBytes(t *testing.T) {
	d := newShotDrafts()
	big := make([]byte, draftsBytes/2+1)
	a := d.put(1, Shot{Data: big})
	d.put(2, Shot{Data: big})

	if _, ok := d.get(1, a); ok {
		t.Error("вес черновиков не считается — два по половине потолка легли рядом")
	}
}

// Черновик — личное, и в поиске ему места нет: заголовок и robots.txt берут
// список из одного места (privateRoots), иначе путь оказался бы закрыт в одном
// и открыт в другом.
func TestDraftPathIsClosedToRobots(t *testing.T) {
	h, _, _, token := shotServer(t, newShots())

	id, _ := preload(t, h, token, []byte("байты"))
	w := do(h, as(guest(t, "GET", "/shot/"+id), token))
	if got := w.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("превью открыто роботам: %q", got)
	}
	if body := do(h, guest(t, "GET", "/robots.txt")).Body.String(); !strings.Contains(body, "Disallow: /shot") {
		t.Errorf("в robots.txt нет /shot: %s", body)
	}
}
