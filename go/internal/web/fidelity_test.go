package web

// Соответствие оригиналу.
//
// Ставка площадки — преемственность: человек, читавший «Заметки» годами, должен
// узнать страницу, а не привыкать заново. Значит «похоже» — недостаточно, и
// спорить о памяти («кажется, там были круглые аватарки») тоже нельзя. Каждый
// тест здесь закрепляет ОДИН проверяемый факт об оригинале и называет источник:
//
//   [Ф] записанная страница сайта в internal/love/testdata (лента и оба вида
//       комментариев, снятые с боевого НГС);
//   [С] стили сайта index.css / main.css, снятые 18.08.2026;
//   [Э] экран оригинала, показанный владельцем 18.08.2026.
//
// Если сайт изменится, поменяются и эти числа — но поменяются осознанно, одной
// правкой с новым замером, а не «оно как-то само разъехалось».

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// [Ф] /notes/page~2/limit~20/ — двадцать заметок на страницу.
func TestFeedShowsTwentyNotesPerPage(t *testing.T) {
	if feedPageSize != 20 {
		t.Fatalf("в ленте %d заметок на страницу, на НГС 20", feedPageSize)
	}
}

// [Ф] /notes/comments/312696/page~2/limit~30/ — тридцать комментариев на
// страницу линейного вида.
func TestLinearShowsThirtyCommentsPerPage(t *testing.T) {
	if linearPageSize != 30 {
		t.Fatalf("в линейном виде %d комментариев на страницу, на НГС 30", linearPageSize)
	}
}

// [Ф][Э] Постраничка — номерами, со стрелками «Пред./След.», текущая страница
// выделена, последняя видна всегда. Кнопки «ещё» на НГС нет, и это не мелочь:
// по номеру человек знает, где он, и умеет вернуться назад.
func TestFeedHasNumberedPagerTopAndBottom(t *testing.T) {
	st := &fakeStore{total: 100, notes: []platform.NoteView{sampleNote()}}
	h := openServer(t, st)
	body := do(h, pass(t, "GET", "/?page=2")).Body.String()

	if n := strings.Count(body, `class="pages"`); n != 2 {
		t.Errorf("постраничка нарисована %d раз, ожидалось 2 (сверху и снизу)", n)
	}
	for _, want := range []string{`href="/?page=3"`, `href="/"`, "Пред.", "След.", `class="pg cur"`} {
		if !strings.Contains(body, want) {
			t.Errorf("в постраничке нет %q", want)
		}
	}
	// Последняя страница обязана быть видна: 100 заметок по 20 — это пять.
	if !strings.Contains(body, `href="/?page=5"`) {
		t.Error("в постраничке нет последней страницы")
	}
	if strings.Contains(body, "Ещё заметки") {
		t.Error("вернулась кнопка «ещё» вместо номеров страниц")
	}
}

// [Ф] Окно постранички — семь номеров, дальше многоточие и последняя.
func TestPagerWindowMatchesOriginal(t *testing.T) {
	p := newPager(1, 5922, func(n int) string { return "/?page=" + itoa(n) })
	var nums []int
	gaps := 0
	for _, l := range p.Pages {
		if l.Gap {
			gaps++
			continue
		}
		nums = append(nums, l.Num)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 5922}
	if len(nums) != len(want) {
		t.Fatalf("в постраничке номера %v, ожидались %v", nums, want)
	}
	for i := range want {
		if nums[i] != want[i] {
			t.Fatalf("в постраничке номера %v, ожидались %v", nums, want)
		}
	}
	if gaps != 1 {
		t.Errorf("многоточий %d, ожидалось 1", gaps)
	}
}

// [С] .lv-notes__note-author { width:100px } + аватар 100×100 с ником ПОД ним.
// Круглых аватарок на НГС нет нигде.
func TestAuthorColumnIsSquareAvatarWithNickBelow(t *testing.T) {
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}})
	body := do(h, pass(t, "GET", "/")).Body.String()

	img := regexp.MustCompile(`<img class="ava"[^>]*>`).FindString(body)
	if img == "" {
		t.Fatal("в ленте нет аватара")
	}
	if !strings.Contains(img, `width="100"`) || !strings.Contains(img, `height="100"`) {
		t.Errorf("аватар не 100×100: %s", img)
	}
	// Ник идёт ПОСЛЕ аватара внутри колонки автора — то есть под ним.
	block := regexp.MustCompile(`(?s)<div class="author">.*?</div>`).FindString(body)
	if !strings.Contains(block, "<img") || !strings.Contains(block, `class="nick`) {
		t.Fatalf("колонка автора собрана иначе: %s", block)
	}
	if strings.Index(block, "<img") > strings.Index(block, `class="nick`) {
		t.Error("ник стоит перед аватаром, а на НГС он под ним")
	}
}

// [Ф][Э] Дата вида 14.08.2026, 18:30:04 — с секундами и годом.
func TestDateLooksLikeOriginal(t *testing.T) {
	at := time.Date(2026, 8, 14, 11, 30, 4, 0, time.UTC) // 18:30:04 в Новосибирске
	got := string(whenHTML(at, true))
	if !strings.Contains(got, "14.08.2026, 18:30:04") {
		t.Fatalf("дата не в формате сайта: %s", got)
	}
	// Приблизительное время оригиналу неизвестно вовсе; наш след обязан быть
	// незаметным глазу, но должен существовать.
	appr := string(whenHTML(at, false))
	if strings.Contains(appr, "≈") {
		t.Error("знак приблизительности виден в тексте даты")
	}
	if !strings.Contains(appr, "_approx") || !strings.Contains(appr, "title=") {
		t.Errorf("след приблизительного времени пропал: %s", appr)
	}
}

// [Ф] В ленте под заметкой стоит ссылка «Комментарии N», а у неактуальной —
// «Заметка не актуальна, но вы можете ознакомиться с её обсуждением N».
func TestFeedCommentLinkWording(t *testing.T) {
	open := sampleNote()
	closed := sampleNote()
	closed.ID, closed.CommentsClosed = 312800, true

	h := openServer(t, &fakeStore{total: 2, notes: []platform.NoteView{open, closed}})
	body := do(h, pass(t, "GET", "/")).Body.String()
	if !strings.Contains(body, "Комментарии <span class=\"cnt\">3</span>") {
		t.Error("нет ссылки «Комментарии N»")
	}
	if !strings.Contains(body, "не актуальна") {
		t.Error("у закрытой заметки не та подпись")
	}
}

// [Ф] Лента показывает текст заметки ЦЕЛИКОМ: её листают, а не раскрывают.
func TestFeedShowsWholeNote(t *testing.T) {
	n := sampleNote()
	n.Body = strings.Repeat("длинная заметка. ", 200)
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{n}})
	body := do(h, pass(t, "GET", "/")).Body.String()
	if strings.Contains(body, "…") || strings.Contains(body, "Читать целиком") {
		t.Error("заметка в ленте обрезана, а на НГС она показана целиком")
	}
}

// [Э] Дерево показывается целиком, без постранички: ветка, обрезанная на
// середине, перестаёт быть веткой. Число ответов при этом видно — им сворачивают.
func TestTreeIsWholeAndCountsReplies(t *testing.T) {
	st := &fakeStore{note: sampleNote(), thread: sampleThread()}
	h := openServer(t, st)
	body := do(h, pass(t, "GET", "/n/312811?view=tree")).Body.String()

	if strings.Contains(body, `class="pages"`) {
		t.Error("у дерева появилась постраничка")
	}
	if n := strings.Count(body, `<li class="c `); n != len(st.thread) {
		t.Errorf("в дереве %d комментариев из %d", n, len(st.thread))
	}
	if !strings.Contains(body, "Ответы <b>2</b>") {
		t.Error("нет числа ответов в ветке")
	}
}

// [Ф] Линейный вид — от НОВЫХ к старым и по 30 на страницу с постраничкой.
// Порядок проверяем по тому, что морда просит у ядра со смещением: сортировку
// делает SQL, и подменять её на стороне показа нельзя.
func TestLinearIsPagedFromNewest(t *testing.T) {
	st := &fakeStore{note: sampleNote()}
	st.note.CommentCount = 95
	h := openServer(t, st)
	body := do(h, pass(t, "GET", "/n/312811?view=linear&page=2")).Body.String()

	if !st.flatUsed {
		t.Fatal("линейный вид не дошёл до хранилища")
	}
	if st.flatOffset != 30 {
		t.Errorf("вторая страница просит смещение %d, ожидалось 30", st.flatOffset)
	}
	if !strings.Contains(body, `class="pages"`) {
		t.Error("у линейного вида нет постранички")
	}
	// 95 комментариев по 30 — четыре страницы.
	if !strings.Contains(body, "page=4") {
		t.Error("в постраничке треда нет последней страницы")
	}
}

// [Ф] Переключатель «дерево / линейный» стоит в блоке заметки и работает
// ссылками: без JS он обязан работать так же.
func TestViewSwitcherIsLinks(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote()})
	body := do(h, pass(t, "GET", "/n/312811")).Body.String()
	for _, want := range []string{`href="/n/312811?view=tree"`, `href="/n/312811?view=linear"`, `class="sw tree on"`} {
		if !strings.Contains(body, want) {
			t.Errorf("в переключателе нет %q", want)
		}
	}
}

// [Ф][Э] Обращение «Ник, » на сайте выделено жирным и стоит началом первой
// фразы. У нас оно ещё и ссылка на адресата — единственное сознательное
// улучшение: в длинном треде иначе не найти, кому отвечали.
func TestAddressPrefixIsBoldAtStart(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote(), thread: sampleThread()})
	body := do(h, pass(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(body, `<p><a class="to" href="#c1">Пух</a>, `) {
		t.Error("обращение не стоит началом первой фразы")
	}
	if !strings.Contains(cssText(t), ".to { font-weight: 700") {
		t.Error("обращение не выделено жирным")
	}
}

// [С] .lv-note .lv-people__nickname { font-size:14px; font-weight:600 } —
// ник на сайте полужирный, и в ленте, и в треде.
func TestNickIsSemiBold(t *testing.T) {
	if !strings.Contains(cssText(t), "font-weight: 600") {
		t.Error("ник не полужирный, а на НГС он font-weight:600")
	}
}

// [С] .lv-comment__avatar { width:100px; height:100px } — аватар в
// комментарии такой же крупный, как у заметки, а не уменьшенный.
func TestCommentAvatarIsFullSize(t *testing.T) {
	css := cssText(t)
	i := strings.Index(css, ".cava .ava {")
	if i < 0 {
		t.Fatal("нет правила для аватара комментария")
	}
	rule := css[i : i+strings.Index(css[i:], "}")]
	if !strings.Contains(rule, "width: 100px") || !strings.Contains(rule, "height: 100px") {
		t.Errorf("аватар комментария не 100×100: %s", rule)
	}
}

// [С] Числа и цвета сайта: разделитель #c7d3d9, текст #3d4952, служебное #999,
// ник женский #ef4e4f, мужской #448dc8. Тест держит их от случайной правки.
func TestPaletteMatchesSite(t *testing.T) {
	css := cssText(t)
	for _, want := range []string{"#c7d3d9", "#3d4952", "#999", "#ef4e4f", "#448dc8"} {
		if !strings.Contains(css, want) {
			t.Errorf("в оформлении нет цвета сайта %s", want)
		}
	}
	if strings.Contains(css, "border-radius: 50%") {
		t.Error("вернулись круглые аватары — на НГС их нет нигде")
	}
}

func cssText(t *testing.T) string {
	t.Helper()
	a, ok := assets[strings.TrimPrefix(assetURL("style.css"), "/assets/")]
	if !ok {
		t.Fatal("style.css не найден")
	}
	return string(a.data)
}

// Мусор в номере страницы — 400, номер за пределами ленты — 404. Пустая
// страница вместо ответа хуже обоих.
func TestPageNumberErrors(t *testing.T) {
	h := openServer(t, &fakeStore{total: 5, notes: []platform.NoteView{sampleNote()}})
	if got := do(h, pass(t, "GET", "/?page=вторая")).Code; got != http.StatusBadRequest {
		t.Errorf("мусор в номере: код %d, ожидался 400", got)
	}
	if got := do(h, pass(t, "GET", "/?page=99")).Code; got != http.StatusNotFound {
		t.Errorf("страница за краем ленты: код %d, ожидался 404", got)
	}
}
