package love

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Фикстуры notes_feed.html и comments_312696.html — реальные страницы сайта,
// записанные через `lovegw crawl ... -save-html`. При дрейфе вёрстки
// перезаписать их той же командой и поправить селекторы в parse.go.

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseNotesRealFeed(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 5 {
		t.Fatalf("ожидалось 5 заметок, получено %d", len(notes))
	}
	first := notes[0]
	if first.ID != "312702" {
		t.Errorf("id первой заметки: %q", first.ID)
	}
	if first.AuthorID != "1511857" {
		t.Errorf("author_id: %q", first.AuthorID)
	}
	if first.AuthorName == "" || first.AuthorName == "Анонимно" {
		t.Errorf("автор не распознан: %q", first.AuthorName)
	}
	if len(first.Text) < 20 {
		t.Errorf("текст подозрительно короткий: %q", first.Text)
	}
	for _, n := range notes {
		if n.ID == "" {
			t.Errorf("заметка без id: %+v", n)
		}
	}
}

func TestParseNotesAvatarAndImages(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	// Аватар автора первой заметки — дефолтный силуэт (/static/...).
	if !strings.Contains(notes[0].AuthorAvatarURL, "/static/") {
		t.Errorf("аватар автора не разобран: %q", notes[0].AuthorAvatarURL)
	}
	// Хотя бы у одной заметки в фикстуре есть иллюстрация с CDN.
	withImages := 0
	for _, n := range notes {
		for _, img := range n.Images {
			withImages++
			if !strings.HasPrefix(img, "http") {
				t.Errorf("URL иллюстрации не абсолютный: %q", img)
			}
		}
	}
	if withImages == 0 {
		t.Error("ни у одной заметки не разобрана иллюстрация, ожидалась хотя бы одна")
	}
}

func TestParseNotesCommentsClosed(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	// В фикстуре верхняя заметка ленты открыта (ссылка «Комментарии»),
	// остальные помечены сайтом «не актуальна» — комментарии закрыты.
	if notes[0].ID != "312702" || notes[0].CommentsClosed {
		t.Errorf("первая заметка должна быть открыта: id=%q closed=%v", notes[0].ID, notes[0].CommentsClosed)
	}
	closed := 0
	for _, n := range notes[1:] {
		if n.CommentsClosed {
			closed++
		}
	}
	if closed != len(notes)-1 {
		t.Errorf("все заметки кроме первой должны быть закрыты; закрыто %d из %d", closed, len(notes)-1)
	}
}

func TestParseNotesEmptyFeedIsMarkupError(t *testing.T) {
	_, err := ParseNotes(strings.NewReader("<html><body><p>ничего</p></body></html>"))
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError, получено: %v", err)
	}
}

func TestParseNotesMissingTextIsMarkupError(t *testing.T) {
	html := `<div class="lv-notes__note-item">
	           <a class="lv-notes__comment-link" name="1"></a>
	         </div>`
	_, err := ParseNotes(strings.NewReader(html))
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError про текст заметки, получено: %v", err)
	}
}

func TestParseCommentsRealPage(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 30 {
		t.Fatalf("ожидалось 30 комментариев (limit~30), получено %d", len(comments))
	}

	// Первый в документе — самый новый (страница desc).
	c := comments[0]
	if c.ID != 63167742 {
		t.Errorf("id: got %d, want 63167742", c.ID)
	}
	if c.AuthorLink != "https://love.ngs.ru/profile/981563/" {
		t.Errorf("ссылка автора не абсолютизирована: %q", c.AuthorLink)
	}
	if !strings.HasPrefix(c.AvatarURL, "https://") {
		t.Errorf("аватар: %q", c.AvatarURL)
	}
	if c.AuthorName == "" {
		t.Error("имя автора пустое")
	}
	// alt вида "Имя, 43 года": возраст — хвост после последней запятой.
	if !strings.Contains(c.AuthorAge, "года") && !strings.Contains(c.AuthorAge, "лет") {
		t.Errorf("возраст: %q", c.AuthorAge)
	}
	want := time.Date(2026, 7, 18, 17, 36, 34, 0, nsk)
	if !c.PublishedAt.Equal(want) {
		t.Errorf("дата: got %v, want %v", c.PublishedAt, want)
	}
	if c.Text == "" {
		t.Error("текст пуст")
	}

	// Все id уникальны и убывают (desc-порядок страницы).
	seen := map[int64]bool{}
	for i, cc := range comments {
		if seen[cc.ID] {
			t.Errorf("дубль id %d", cc.ID)
		}
		seen[cc.ID] = true
		if i > 0 && cc.ID >= comments[i-1].ID {
			t.Errorf("порядок не desc: %d после %d", cc.ID, comments[i-1].ID)
		}
	}
}

func TestParseCommentsAuthorIDAndLinearParent(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if comments[0].AuthorID != "981563" {
		t.Errorf("author_id первого комментария: got %q, want 981563", comments[0].AuthorID)
	}
	for _, c := range comments {
		if c.AuthorID == "" || c.AuthorID == "0" {
			t.Errorf("комментарий %d без числового author_id", c.ID)
		}
		// Фикстура записана в линейном виде: data-parent-comment-id всегда
		// пуст, значит parent_id обязан быть 0 (корень).
		if c.ParentID != 0 {
			t.Errorf("линейный вид: parent_id должен быть 0, у %d = %d", c.ID, c.ParentID)
		}
	}
}

// В древовидном виде сайт проставляет data-parent-comment-id — фикстуры такого
// вида пока нет, поэтому дерево проверяем на синтетической разметке.
func TestParseCommentsTreeParent(t *testing.T) {
	html := `<div class="lv-note__comment-item">
	           <a id="anchor-100" data-parent-comment-id="63167742"></a>
	           <img class="avatar" src="/a.png" alt="Имя, 30 лет">
	           <a class="lv-people__nickname" href="/profile/555/">Имя</a>
	           <div class="lv-comment__pubdate">18.07.2026, 17:00:00</div>
	           <div class="lv-comment__text">ответ</div>
	         </div>`
	comments, err := ParseComments(strings.NewReader(html), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("ожидался 1 комментарий, получено %d", len(comments))
	}
	if comments[0].ParentID != 63167742 {
		t.Errorf("parent_id: got %d, want 63167742", comments[0].ParentID)
	}
	if comments[0].AuthorID != "555" {
		t.Errorf("author_id: got %q, want 555", comments[0].AuthorID)
	}
}

// comments_tree_312696.html — реальная древовидная страница (?view=tree),
// обрезанная до шапки заметки и первых 15 комментариев. В отличие от линейной
// фикстуры здесь data-parent-comment-id заполнен — на нём и держится дерево.
func TestParseCommentsTreeRealPage(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_tree_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 15 {
		t.Fatalf("ожидалось 15 комментариев в обрезанной фикстуре, получено %d", len(comments))
	}

	byID := map[int64]Comment{}
	roots, replies := 0, 0
	for _, c := range comments {
		byID[c.ID] = c
		if c.ParentID == 0 {
			roots++
		} else {
			replies++
		}
		if c.AuthorID == "" || c.AuthorID == "0" {
			t.Errorf("комментарий %d без author_id", c.ID)
		}
	}
	// В обрезанной фикстуре 11 непустых data-parent-comment-id.
	if replies != 11 {
		t.Errorf("ответов (parent_id != 0): got %d, want 11", replies)
	}
	if roots != 4 {
		t.Errorf("корней (parent_id == 0): got %d, want 4", roots)
	}

	// Конкретное ребро дерева из реальной разметки: 63167045 → 63167023.
	if c, ok := byID[63167045]; !ok {
		t.Error("комментарий 63167045 не разобран")
	} else if c.ParentID != 63167023 {
		t.Errorf("parent_id 63167045: got %d, want 63167023", c.ParentID)
	}
	// Каждый parent_id указывает на комментарий, присутствующий на странице
	// (в этой обрезке — самодостаточное дерево, висячих ссылок нет).
	for _, c := range comments {
		if c.ParentID != 0 {
			if _, ok := byID[c.ParentID]; !ok {
				t.Errorf("висячий parent_id %d у комментария %d", c.ParentID, c.ID)
			}
		}
	}
}

func TestParseNoteFromCommentsPage(t *testing.T) {
	note, err := ParseNoteFromCommentsPage(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if note.AuthorID != "1472546" {
		t.Errorf("author_id заметки: got %q, want 1472546", note.AuthorID)
	}
	if note.AuthorName != "Рантье" {
		t.Errorf("author_name заметки: got %q, want Рантье", note.AuthorName)
	}
	if !strings.Contains(note.Text, "доход с капитала") {
		t.Errorf("текст заметки не разобран: %q", note.Text)
	}
	want := time.Date(2026, 7, 18, 13, 1, 12, 0, nsk)
	if !note.PublishedAt.Equal(want) {
		t.Errorf("дата заметки: got %v, want %v", note.PublishedAt, want)
	}
	// В фикстуре заметка помечена «Комментарии запрещены».
	if !note.CommentsClosed {
		t.Error("ожидался CommentsClosed=true для заметки 312696")
	}
}

func TestParseNoteFromCommentsPageMissingIsMarkupError(t *testing.T) {
	_, err := ParseNoteFromCommentsPage(strings.NewReader("<html><body><p>ничего</p></body></html>"), "https://love.ngs.ru")
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError, получено: %v", err)
	}
}

func TestParseCommentsEmptyIsOK(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_empty.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatalf("пустая страница комментариев не должна быть ошибкой: %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("ожидался пустой список, получено %d", len(comments))
	}
}

// Переезд комментариев на клиентский рендер выглядит как целая страница с
// пустым списком: ни ошибки, ни 403, ни сломанного элемента. Отличает такую
// страницу от честно пустого треда счётчик в шапке списка.
func TestParseCommentsEmptyWithCounterIsMarkupError(t *testing.T) {
	html := `<div class="lv-note__comments">
	           <div class="lv-note__comments-header">
	             Комментарии <span class="lv-note__comments-count">45</span>
	           </div>
	           <div class="lv-note__comments-list"></div>
	         </div>`
	_, err := ParseComments(strings.NewReader(html), "https://love.ngs.ru")
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError про молчащий источник, получено: %v", err)
	}
	if !strings.Contains(me.Context, "45") {
		t.Errorf("в тексте ошибки нет обещанного счётчиком числа: %q", me.Context)
	}
}

// Обратная сторона того же правила: счётчик в нуле — тред честно пуст.
func TestParseCommentsEmptyWithZeroCounterIsOK(t *testing.T) {
	html := `<div class="lv-note__comments">
	           <div class="lv-note__comments-header">
	             Комментарии <span class="lv-note__comments-count">0</span>
	           </div>
	         </div>`
	comments, err := ParseComments(strings.NewReader(html), "https://love.ngs.ru")
	if err != nil || len(comments) != 0 {
		t.Fatalf("пустой тред с нулевым счётчиком: %d комментариев, err %v", len(comments), err)
	}
}

// Счётчик — свидетель, а не обязательный селектор: увезут на клиент и его —
// молчаливый ноль снова станет законным, но ложной тревоги не будет.
func TestParseCommentsCounterOnRealPage(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(openFixture(t, "comments_312696.html"))
	if err != nil {
		t.Fatal(err)
	}
	n, ok := commentsCount(doc)
	if !ok || n != 325 {
		t.Errorf("счётчик записанной страницы: %d, ok=%v (ожидалось 325)", n, ok)
	}
}

// Снесённую заметку сайт отдаёт кодом 200 и целым каркасом, в котором вместо
// заметки одна фраза (снято с боевой страницы 313038 21.08.2026). Без этого
// признака удаление неотличимо от дрейфа вёрстки, и демон опрашивает мёртвый
// адрес до самого архива — неделю, дважды в минуту.
func TestParseCommentsPageDeletedNote(t *testing.T) {
	html := `<div class="lv-content-center-wrap"><div class="lv-content">
	           <div class="lv-people"> Заметка 313038 удалена. </div>
	         </div></div>`
	_, err := ParseCommentsPage(strings.NewReader(html), "https://love.ngs.ru")
	if !errors.Is(err, ErrNoteDeleted) {
		t.Fatalf("ожидалась ErrNoteDeleted, получено: %v", err)
	}
	if _, err := ParseNoteFromCommentsPage(strings.NewReader(html), "https://love.ngs.ru"); !errors.Is(err, ErrNoteDeleted) {
		t.Errorf("шапка снесённой заметки: %v", err)
	}
}

// Отсутствие шапки БЕЗ этой фразы остаётся дрейфом вёрстки: молчаливо считать
// удалением всё, что не разобралось, значило бы гасить опрос живых заметок.
func TestParseCommentsPageMissingHeaderIsNotDeleted(t *testing.T) {
	html := `<div class="lv-note__comments"><div class="lv-note__comments-count">0</div></div>`
	page, err := ParseCommentsPage(strings.NewReader(html), "https://love.ngs.ru")
	if err != nil {
		t.Fatalf("дрейф шапки не должен ронять страницу: %v", err)
	}
	if page.Note != nil {
		t.Errorf("шапки тут нет: %+v", page.Note)
	}
}

// Счётчик треда доезжает до вызывающего: по нему зеркало понимает, что окно
// limit~30 уехало вперёд и часть реплик надо добрать пейджером.
func TestParseCommentsPageTotal(t *testing.T) {
	page, err := ParseCommentsPage(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 325 || len(page.Comments) != 30 {
		t.Errorf("счётчик %d при %d разобранных (ожидалось 325 при 30)", page.Total, len(page.Comments))
	}
}

func TestParseCommentsBrokenDateIsMarkupError(t *testing.T) {
	html := `<div class="lv-note__comment-item">
	           <a id="anchor-5"></a>
	           <img class="avatar" src="/a.png" alt="Имя, 30 лет">
	           <a class="lv-people__nickname" href="/profile/1/">Имя</a>
	           <div class="lv-comment__pubdate">позавчера</div>
	           <div class="lv-comment__text">текст</div>
	         </div>`
	_, err := ParseComments(strings.NewReader(html), "https://love.ngs.ru")
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError про дату, получено: %v", err)
	}
}

func TestSplitNameAge(t *testing.T) {
	for _, tc := range []struct{ alt, name, age string }{
		{"Яна, 43 года", "Яна", "43 года"},
		{"Мария, свет, радость, 34 года", "Мария, свет, радость", "34 года"},
		{"Безвозраста", "Безвозраста", ""},
	} {
		name, age := splitNameAge(tc.alt)
		if name != tc.name || age != tc.age {
			t.Errorf("splitNameAge(%q) = %q, %q; want %q, %q", tc.alt, name, age, tc.name, tc.age)
		}
	}
}

func TestDigitsOf(t *testing.T) {
	if got := digitsOf("/profile/981563/"); got != "981563" {
		t.Errorf("digitsOf: %q", got)
	}
	if got := digitsOf("/no-digits/"); got != "0" {
		t.Errorf("digitsOf без цифр должен вернуть \"0\", получено %q", got)
	}
}

// Ссылка приезжает атрибутом чужой вёрстки и уходит в href поста и в загрузчик
// медиа, поэтому всё, кроме http(s) и пути от корня, должно отсеиваться здесь.
func TestAbsolutize(t *testing.T) {
	const base = "https://love.ngs.ru"
	for _, tc := range []struct{ name, link, want string }{
		{"путь от корня", "/profile/981563/", "https://love.ngs.ru/profile/981563/"},
		{"абсолютный https CDN", "https://cdn.hsmedia.ru/a.jpg", "https://cdn.hsmedia.ru/a.jpg"},
		{"абсолютный http", "http://cdn.hsmedia.ru/a.jpg", "http://cdn.hsmedia.ru/a.jpg"},
		{"схема-относительная остаётся на сайте", "//evil.example/x", "https://love.ngs.ru//evil.example/x"},
		{"javascript отбрасывается", "javascript:alert(1)", ""},
		{"data отбрасывается", "data:text/html;base64,PHNjcmlwdD4=", ""},
		{"mailto отбрасывается", "mailto:a@b.c", ""},
		{"относительный без слэша отбрасывается", "img/a.jpg", ""},
		{"пустая строка", "   ", ""},
	} {
		if got := absolutize(base, tc.link); got != tc.want {
			t.Errorf("%s: absolutize(%q) = %q, want %q", tc.name, tc.link, got, tc.want)
		}
	}
}

// Эмодзи сайт хранит текстом, а показывает картинкой — и разбор их терял.
//
// Жалоба владельца 23.08.2026 пришла с другого конца: на площадке ответ показал
// ник ДВАЖДЫ. Причина оказалась здесь — реплика состояла из одних эмодзи, разбор
// оставил от неё «Ник,», и обращение вышло и из тела, и из ребра. То есть
// терялись не «значки»: терялась вся реплика.
func TestCommentEmojiSurviveParsing(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, c := range comments {
		if strings.Contains(c.Text, "\U0001F609") {
			found = c.Text
			break
		}
	}
	if found == "" {
		t.Fatal("эмодзи потерялись при разборе страницы")
	}
	if !strings.HasSuffix(found, "не научит\U0001F609") {
		t.Errorf("эмодзи встал не на своё место: %q", found)
	}
}

// Тот самый случай: реплика ИЗ ОДНИХ эмодзи. До правки от неё оставалось
// обращение, а после — пустота, и «пустой комментарий» на странице неотличим от
// поломки.
func TestEmojiOnlyCommentIsNotEmpty(t *testing.T) {
	const page = `<div class="lv-note__comment-item">
	  <a id="anchor-1"></a>
	  <a class="lv-people__nickname" href="/profile/42/">Дракоша</a>
	  <img class="avatar" alt="Дракоша, 40 лет" src="/a.jpg"/>
	  <time class="lv-comment__pubdate">23.08.2026, 21:23:28</time>
	  <div class="lv-comment__text"><b>Анна</b>, <img class="emojione" alt="&#x1f60a;" src="/1F60A.png"/><img class="emojione" alt="&#x1f446;" src="/1F446.png"/></div>
	</div>`

	comments, err := ParseComments(strings.NewReader(page), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Fatalf("разобрано %d комментариев", len(comments))
	}
	if want := "Анна, \U0001F60A\U0001F446"; comments[0].Text != want {
		t.Errorf("текст %q, ожидался %q", comments[0].Text, want)
	}
}
