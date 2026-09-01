package web

// Тесты метки происхождения (Ш5з) и превью картинки в ленте.

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

func TestOriginMarksBothSides(t *testing.T) {
	mirror := originOf(312811, false, false)
	if mirror.Label != "НГС" {
		t.Errorf("зеркальная помечена как %q", mirror.Label)
	}
	own := originOf(platform.NativeIDBase+7, false, false)
	if own.Label != SiteName {
		t.Errorf("нативная помечена как %q, ожидалось %q", own.Label, SiteName)
	}
	// Обе метки обязаны объяснять себя: на НГС их не было (правило Ш5з).
	for _, o := range []noteOrigin{mirror, own} {
		if o.Title == "" {
			t.Errorf("метка %q не объясняет себя заголовком", o.Label)
		}
	}
}

// Значков ДВА, а не три: восстановленное из чужих зеркал читателю ничем не
// отличается от свежего зеркала.
func TestRestoredLooksLikeMirror(t *testing.T) {
	if o := originOf(platform.RestoredIDBase+5, false, false); o.Label != "НГС" {
		t.Errorf("восстановленная помечена как %q: значков должно быть два, а не три", o.Label)
	}
}

// Песочница (эпик «народ») — ТРЕТИЙ значок, и он сильнее «своей»: обе написаны
// здесь, но у песочницы к этому добавлено единственное, что читателю нужно знать
// заранее, — ответить в ней он не сможет.
func TestStageOverridesOwnMark(t *testing.T) {
	id := platform.NativeIDBase + 7
	own, stage := originOf(id, false, false), originOf(id, true, false)
	if stage.Icon == own.Icon {
		t.Fatalf("у песочницы тот же значок, что у обычной своей заметки (%q)", stage.Icon)
	}
	if stage.Title == "" {
		t.Error("метка песочницы не объясняет себя заголовком (правило Ш5з)")
	}
	// Имён жителей метка не называет — решение эпика.
	if stage.Label == "" {
		t.Error("у метки песочницы нет имени для читалки")
	}
}

// Метка НИКУДА НЕ ВЕДЁТ (решение владельца 27.08.2026): ссылок на НГС площадка
// не ставит. Тест сторожит именно это — не отсутствие поля в структуре (его
// вернули бы первой же правкой), а отсутствие адреса чужого сайта на странице.
func TestNoNGSLinkAnywhereOnTheNotePages(t *testing.T) {
	st := noteStore()
	h := newTestServer(t, st, Config{})

	for _, path := range []string{"/", "/n/312811"} {
		body := do(h, guest(t, "GET", path)).Body.String()
		if !strings.Contains(body, `class="orig"`) {
			t.Errorf("%s: нет метки происхождения", path)
		}
		if strings.Contains(body, "ngs.ru") {
			t.Errorf("%s: на странице снова стоит адрес НГС", path)
		}
		if strings.Contains(body, `<a class="orig"`) {
			t.Errorf("%s: метка происхождения снова стала ссылкой", path)
		}
	}
}

// Метка не должна вставать между словом «Комментарии» и числом: этот кусок
// разметки сверяется с оригиналом НГС в fidelity_test.
// Метка показывается ЗНАЧКОМ, а не словом (решение владельца 26.08.2026): имя
// чужого сайта не печатается на каждой карточке ленты. Но и не пропадает совсем
// — уходит в .sr-only, оставаясь именем источника для читалки, — и объясняется
// заголовком, как велит Ш5з.
func TestOriginBadgeIsASignNotAWord(t *testing.T) {
	st := noteStore()
	h := newTestServer(t, st, Config{})

	feed := do(h, guest(t, "GET", "/")).Body.String()
	i := strings.Index(feed, `class="orig"`)
	if i < 0 {
		t.Fatal("метки происхождения в ленте нет вовсе")
	}
	const tail = "</span></span>" // закрывают sr-only и саму метку
	badge := feed[i : i+strings.Index(feed[i:], tail)+len(tail)]
	if !strings.Contains(badge, "<svg") {
		t.Error("метка нарисована не значком")
	}
	if !strings.Contains(badge, `<span class="sr-only">НГС</span>`) {
		t.Error("у метки пропало имя источника для читалки")
	}
	if !strings.Contains(badge, "title=") {
		t.Error("значок не объясняет себя заголовком (правило Ш5з)")
	}
	// А ВИДИМЫМ текстом источник больше не назван: вычёркиваем спрятанную
	// подпись и заголовок (он всплывает при наведении, а не печатается) и
	// смотрим, что от метки осталось для глаза.
	visible := strings.Replace(badge, `<span class="sr-only">НГС</span>`, "", 1)
	visible = visible[strings.Index(visible, ">"):]
	if strings.Contains(visible, "НГС") {
		t.Error("имя источника снова напечатано словом на карточке")
	}
}

func TestOriginBadgeDoesNotSplitTheCommentLink(t *testing.T) {
	st := noteStore()
	h := newTestServer(t, st, Config{})

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
	if isUpload(get) {
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

// ------------------------------------------------- унесённое на НГС (третье состояние)

// Метка реплики стои́т В СТРОКЕ ДАТЫ, а не соседкой (жалоба владельца
// 02.09.2026: «с рамкой перекос, значок источника ни к месту»). Дата — БЛОК,
// поэтому метка рядом с ним уезжала третьей строкой карточки и висела сама по
// себе. Проверяется именно вложенность, а не «метка есть»: разъехаться они
// могут молча, и видно это только глазом на странице.
func TestCommentOriginSitsInsideTheDateLine(t *testing.T) {
	st := noteStore()
	st.thread = sampleThread()
	h := newTestServer(t, st, Config{})

	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	i := strings.Index(body, `<div class="cdate">`)
	if i < 0 {
		t.Fatal("в треде нет строки даты")
	}
	line := body[i : i+strings.Index(body[i:], "</div>")]
	if !strings.Contains(line, `class="orig"`) {
		t.Error("метка происхождения выпала из строки даты — она снова встанет отдельной строкой")
	}
}

// Третье состояние: реплика написана здесь И её копия уехала на НГС (решение
// владельца 02.09.2026). Значок обязан отличаться от «своей» — иначе состояние
// есть, а показать его нечем.
func TestSentToNGSGetsItsOwnMark(t *testing.T) {
	sent := commentOriginOf(platform.NativeIDBase+7, true)
	own := commentOriginOf(platform.NativeIDBase+7, false)
	if sent.Icon == own.Icon {
		t.Fatalf("у унесённой реплики тот же значок, что у оставшейся здесь (%q)", sent.Icon)
	}
	if sent.Title == "" || sent.Label == "" {
		t.Error("метка унесённого не объясняет себя (правило Ш5з)")
	}
	// Зеркальную это состояние не касается вовсе: уносим мы СВОЁ, и «пришло с
	// НГС» плюс «ушло на НГС» вместе означали бы круг.
	if mirror := commentOriginOf(312811, true); mirror.Icon != commentOriginOf(312811, false).Icon {
		t.Error("зеркальная реплика помечена как унесённая")
	}
	// То же самое у заметки — она уезжает тем же путём и той же галочкой.
	if originOf(platform.NativeIDBase+7, false, true).Icon != sent.Icon {
		t.Error("у заметки и реплики разные значки для одного и того же состояния")
	}
}

// И метка эта доезжает ДО СТРАНИЦЫ, а не только считается: величина, которую
// показывают, обязана иметь тест на пути данных — тот же урок, что с полом
// собеседника в эпике «народ».
func TestSentMarkReachesThePage(t *testing.T) {
	st := noteStore()
	mine := platform.CommentView{
		ID: platform.NativeIDBase + 7, Author: platform.Author{ID: 1, Nick: "Пух"},
		Body: "Написано здесь.", Depth: 1,
		PublishedAt: time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC),
	}
	st.thread = []platform.CommentView{mine}
	st.ngsSent = map[string]bool{"comment:" + strconv.FormatInt(mine.ID, 10): true}
	h := newTestServer(t, st, Config{})

	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(body, commentOriginOf(mine.ID, true).Title) {
		t.Error("на странице нет метки «унесено на НГС»")
	}
	// А без отметки в очереди — прежняя метка «написано здесь».
	st.ngsSent = nil
	body = do(newTestServer(t, st, Config{}), guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, commentOriginOf(mine.ID, true).Title) {
		t.Error("метка «унесено» стои́т у реплики, которая никуда не уезжала")
	}
}
