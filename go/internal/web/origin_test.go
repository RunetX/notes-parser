package web

// Тесты метки происхождения (Ш5з) и превью картинки в ленте.

import (
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func TestOriginMarksBothSides(t *testing.T) {
	setNGSBase("https://love.ngs.ru")
	t.Cleanup(func() { setNGSBase("") })

	mirror := originOf(312811)
	if mirror.Label != "НГС" {
		t.Errorf("зеркальная помечена как %q", mirror.Label)
	}
	if mirror.URL != "https://love.ngs.ru/notes/312811/" {
		t.Errorf("адрес оригинала %q", mirror.URL)
	}

	own := originOf(platform.NativeIDBase + 7)
	if own.Label != SiteName {
		t.Errorf("нативная помечена как %q, ожидалось %q", own.Label, SiteName)
	}
	if own.URL != "" {
		t.Errorf("у своей заметки есть «оригинал» %q — вести туда некуда", own.URL)
	}
	// Обе метки обязаны объяснять себя: на НГС их не было (правило Ш5з).
	for _, o := range []noteOrigin{mirror, own} {
		if o.Title == "" {
			t.Errorf("метка %q не объясняет себя заголовком", o.Label)
		}
	}
}

// Значков ДВА, а не три: восстановленное из чужих зеркал читателю ничем не
// отличается от свежего зеркала. Но ссылки у него нет — страницы на сайте не
// существует, и вести туда значило бы обещать несуществующее.
func TestRestoredLooksLikeMirrorButHasNoOriginal(t *testing.T) {
	setNGSBase("https://love.ngs.ru")
	t.Cleanup(func() { setNGSBase("") })

	o := originOf(platform.RestoredIDBase + 5)
	if o.Label != "НГС" {
		t.Errorf("восстановленная помечена как %q: значков должно быть два, а не три", o.Label)
	}
	if o.URL != "" {
		t.Errorf("у восстановленной есть ссылка %q", o.URL)
	}
}

// Адрес чужого сайта не выдумывается: не задан — метка остаётся, ссылки нет.
func TestOriginWithoutBaseHasNoLink(t *testing.T) {
	setNGSBase("")
	if o := originOf(312811); o.URL != "" || o.Label != "НГС" {
		t.Errorf("без адреса НГС получили %+v", o)
	}
}

func TestOriginBadgeIsOnTheFeedAndOnThePage(t *testing.T) {
	st := noteStore()
	h := newTestServer(t, st, Config{SiteBaseURL: "https://love.ngs.ru"})
	t.Cleanup(func() { setNGSBase("") })

	feed := do(h, guest(t, "GET", "/")).Body.String()
	if !strings.Contains(feed, `class="orig"`) {
		t.Error("в ленте нет метки происхождения")
	}
	if !strings.Contains(feed, "love.ngs.ru/notes/312811/") {
		t.Error("метка зеркальной заметки не ведёт на оригинал")
	}
	page := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(page, `class="orig"`) {
		t.Error("на странице заметки нет метки происхождения")
	}
}

// Метка не должна вставать между словом «Комментарии» и числом: этот кусок
// разметки сверяется с оригиналом НГС в fidelity_test.
func TestOriginBadgeDoesNotSplitTheCommentLink(t *testing.T) {
	st := noteStore()
	h := newTestServer(t, st, Config{SiteBaseURL: "https://love.ngs.ru"})
	t.Cleanup(func() { setNGSBase("") })

	feed := do(h, guest(t, "GET", "/")).Body.String()
	if !strings.Contains(feed, `<span class="lbl">Комментарии</span> <span class="cnt">3</span>`) {
		t.Error("метка разорвала надпись «Комментарии N»")
	}
	// И не должна называться так, чтобы её принял за себя тест липкой шапки.
	if strings.Contains(feed, "hcount") {
		t.Error("в ленте появилось слово hcount")
	}
}

// ---------------------------------------------------------------- превью в ленте

func TestFeedShowsTheNoteImage(t *testing.T) {
	st := noteStore()
	st.thumbs = map[int64]platform.Media{
		312811: {URL: "/media/ab/cdef.webp", Width: 1600, Height: 900},
	}
	h := newTestServer(t, st, Config{})

	feed := do(h, guest(t, "GET", "/")).Body.String()
	if !strings.Contains(feed, `class="thumb"`) {
		t.Fatal("картинки заметки нет в ленте")
	}
	if !strings.Contains(feed, "/media/ab/cdef.webp") {
		t.Error("в ленте не тот адрес картинки")
	}
	// Размеры отдаются атрибутами: без них лента прыгает, пока картинки грузятся.
	if !strings.Contains(feed, `width="1600" height="900"`) {
		t.Error("размеры картинки не проставлены")
	}
	// В ленте нажатие означает «открыть заметку», а не «рассмотреть файл».
	if !strings.Contains(feed, `href="/n/312811"><img src="/media/ab/cdef.webp"`) {
		t.Error("картинка в ленте ведёт не на заметку")
	}
}

func TestFeedWithoutImageHasNoThumb(t *testing.T) {
	h := newTestServer(t, noteStore(), Config{})
	if strings.Contains(do(h, guest(t, "GET", "/")).Body.String(), `class="thumb"`) {
		t.Error("пустая карточка нарисовала картинку")
	}
}

// ---------------------------------------------------------------- потолки маршрута

// У публикации с файлом свои потолок тела, цена и срок. Список из трёх снятых
// защит обязан быть виден одним тестом: каждая из них снимается ради одного
// маршрута, и второй такой завести молча нельзя.
func TestUploadRouteHasItsOwnLimits(t *testing.T) {
	up := postReq("/new")
	other := postReq("/n/312811/reply")
	get := getReq("/new")

	if got := maxBodyOf(up); got != uploadMaxBytes {
		t.Errorf("потолок тела публикации %d, ожидался %d", got, uploadMaxBytes)
	}
	for _, r := range []*http.Request{other, get} {
		if got := maxBodyOf(r); got != maxFormBytes {
			t.Errorf("%s %s: потолок тела %d, ожидался прежний %d", r.Method, r.URL.Path, got, maxFormBytes)
		}
	}
	if got := costOf(up); got != costUpload {
		t.Errorf("цена публикации с файлом %v, ожидалась %v", got, costUpload)
	}
	if got := costOf(other); got != costWrite {
		t.Errorf("цена обычной записи %v, ожидалась %v", got, costWrite)
	}
	if got := budgetOf(up); got != uploadBudget {
		t.Errorf("срок публикации с файлом %v, ожидался %v", got, uploadBudget)
	}
	if got := budgetOf(other); got != requestBudget {
		t.Errorf("срок обычной записи %v, ожидался %v", got, requestBudget)
	}
	// GET на тот же адрес — это форма, а не приём файла.
	if isNoteUpload(get) {
		t.Error("GET /new принят за загрузку")
	}
}

func postReq(path string) *http.Request {
	r, _ := http.NewRequest("POST", path, strings.NewReader(""))
	return r
}

func getReq(path string) *http.Request {
	r, _ := http.NewRequest("GET", path, nil)
	return r
}
