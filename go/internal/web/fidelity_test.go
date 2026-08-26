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
	body := do(h, guest(t, "GET", "/?page=2")).Body.String()

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

// [Э] На телефоне постраничка держится ОДНОЙ строкой: 23.08.2026 «5867» и
// «След. »» уезжали второй строкой, отрываясь от номеров, — прыжок в конец
// ленты выглядел обломком чужого блока. Ширины экрана сервер не знает, поэтому
// прячет лишнее CSS, а разметка обязана назвать, ЧТО прятать: дальние номера
// (Far) и слова у стрелок. Пропуск при этом обязан быть виден — иначе «1 3 4 5»
// читается как поломка сортировки.
func TestPagerFitsOnePhoneLine(t *testing.T) {
	for _, c := range []struct {
		cur, total int
		want       string
	}{
		{1, 5867, "1 2 … 5867"}, // первая страница ленты, экран владельца
		{100, 5867, "99 100 101 … 5867"},
		{1, 8, "1 2 … 8"},   // своего многоточия у постранички нет — нужно своё
		{4, 5, "1 … 3 4 5"}, // дыра слева тоже дыра
	} {
		p := newPager(c.cur, c.total, func(n int) string { return "/?page=" + itoa(n) })
		var narrow []string
		for _, l := range p.Pages {
			switch {
			case l.Far:
			case l.Gap:
				narrow = append(narrow, "…")
			default:
				narrow = append(narrow, itoa(l.Num))
			}
		}
		if got := strings.Join(narrow, " "); got != c.want {
			t.Errorf("страница %d из %d: на телефоне постраничка «%s», ожидалась «%s»", c.cur, c.total, got, c.want)
		}
	}

	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	for _, want := range []string{".pages { white-space: nowrap", ".pg.arr .wd { display: none; }", ".pg.far { display: none; }"} {
		if !strings.Contains(mobile, want) {
			t.Errorf("в мобильных стилях нет %q — постраничка снова ляжет в две строки", want)
		}
	}
	// Многоточие узкого экрана на широком не показывается: там его место
	// занимают сами номера.
	if !strings.Contains(cssText(t), ".pg.gap.mob { display: none; }") {
		t.Error("многоточие для телефона видно и на широком экране")
	}
}

// [С] .lv-notes__note-author { width:100px } + аватар 100×100 с ником ПОД ним.
// Круглых аватарок на НГС нет нигде.
func TestAuthorColumnIsSquareAvatarWithNickBelow(t *testing.T) {
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}})
	body := do(h, guest(t, "GET", "/")).Body.String()

	img := regexp.MustCompile(`<img class="ava[^"]*"[^>]*>`).FindString(body)
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
	body := do(h, guest(t, "GET", "/")).Body.String()
	// Слово стоит в своей обёртке: на телефоне его заменяет значок, и разметка
	// обязана НАЗВАТЬ, что прятать (тот же приём, что у постранички). Порядок и
	// сами слова на десктопе от этого не изменились.
	if !strings.Contains(body, "<span class=\"lbl\">Комментарии</span> <span class=\"cnt\">3</span>") {
		t.Error("нет ссылки «Комментарии N»")
	}
	if !strings.Contains(body, "не актуальна") {
		t.Error("у закрытой заметки не та подпись")
	}
}

// [Р] Длинная заметка в ленте показывается НАЧАЛОМ — и это сознательный отход
// от оригинала (решение владельца 23.08.2026: «полный текст выглядит слишком
// громоздко»). На НГС лента текст не сворачивала, но там и заметки были короче:
// у нас в ленту попадают объявления площадки и выпуск дайджеста, а одна такая
// простыня съедает экран, за которым лежат ещё девятнадцать заметок.
//
// Проверяется не «обрезано», а ровно обратное: свёрнут ПОКАЗ, текст при этом
// отдан целиком — иначе поиск по странице и чтение без CSS теряли бы половину
// заметки.
func TestLongFeedNoteIsCollapsedButWhole(t *testing.T) {
	n := sampleNote()
	n.Body = strings.Repeat("длинная заметка. ", 200) // ~3400 знаков
	short := sampleNote()
	short.ID, short.Body = n.ID+1, "Коротко."
	h := openServer(t, &fakeStore{total: 2, notes: []platform.NoteView{n, short}})
	body := do(h, guest(t, "GET", "/")).Body.String()

	if strings.Count(body, `class="text clip"`) != 1 {
		t.Error("свёрнута не ровно одна заметка: порог считается не по той строке")
	}
	if !strings.Contains(body, "Показать полностью") || !strings.Contains(body, `for="exn`) {
		t.Error("длинную заметку нечем развернуть")
	}
	if strings.Count(body, "длинная заметка") < 200 {
		t.Error("текст свёрнутой заметки обрезан на сервере")
	}
	if strings.Contains(body, `<div class="text clip">Коротко.`) {
		t.Error("свёрнута короткая заметка")
	}
}

// [Ф] А на СВОЕЙ странице заметка не сворачивается никогда: туда пришли читать
// именно её, и лишнее нажатие там было бы издевательством.
func TestNotePageShowsWholeNote(t *testing.T) {
	n := sampleNote()
	n.Body = strings.Repeat("длинная заметка. ", 200)
	h := openServer(t, &fakeStore{note: n})
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, `class="text clip"`) || strings.Contains(body, "Показать полностью") {
		t.Error("заметка свёрнута на собственной странице")
	}
}

// [Э] Дерево показывается целиком, без постранички: ветка, обрезанная на
// середине, перестаёт быть веткой. Число ответов при этом видно — им сворачивают.
func TestTreeIsWholeAndCountsReplies(t *testing.T) {
	st := &fakeStore{note: sampleNote(), thread: sampleThread()}
	h := openServer(t, st)
	body := do(h, guest(t, "GET", "/n/312811?view=tree")).Body.String()

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
	body := do(h, guest(t, "GET", "/n/312811?view=linear&page=2")).Body.String()

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
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
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
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
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

// [С] .lv-comment__author-info .lv-people__nickname._female { color:#ef4e4f },
// ._male { color:#448dc8 } — ник покрашен по полу. В треде на четыре сотни
// реплик цвет первым отделяет собеседников друг от друга.
func TestNickCarriesGenderClass(t *testing.T) {
	n := sampleNote()
	n.Author.Gender = platform.GenderFemale
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{n}})
	if !strings.Contains(do(h, guest(t, "GET", "/")).Body.String(), `class="nick _female"`) {
		t.Error("ник не помечен полом")
	}
	// У анонима пола нет и быть не может: он приехал бы вместе с автором.
	n.Anonymous, n.Author = true, platform.Author{}
	h2 := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{n}})
	body := do(h2, guest(t, "GET", "/")).Body.String()
	if strings.Contains(body, "_female") || strings.Contains(body, "_male") {
		t.Error("у анонимной заметки в разметке оказался пол")
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

// [Ф] Шапка НГС: вход — в ПРАВОМ ВЕРХНЕМ углу верхней панели
// (`lv-top-menu__user-menu`, подпись «Вход на сайт»). Читать сайт можно и не
// входя, поэтому кнопка приглашает, а не преграждает: с 18.08.2026 площадка
// устроена так же.
func TestEnterSitsInTopRightCorner(t *testing.T) {
	if !strings.Contains(cssRule(t, cssText(t), ".acct {"), "margin-left: auto") {
		t.Error("угол участника не прижат к правому краю шапки")
	}
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}})
	head, _, ok := strings.Cut(do(h, guest(t, "GET", "/")).Body.String(), "</header>")
	if !ok {
		t.Fatal("на странице нет шапки")
	}
	if !strings.Contains(head, ">Вход<") {
		t.Fatal("в шапке нет входа")
	}
	if strings.Index(head, `class="acct"`) < strings.Index(head, `class="brand"`) {
		t.Error("вход стоит левее названия площадки")
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
	if got := do(h, guest(t, "GET", "/?page=вторая")).Code; got != http.StatusBadRequest {
		t.Errorf("мусор в номере: код %d, ожидался 400", got)
	}
	if got := do(h, guest(t, "GET", "/?page=99")).Code; got != http.StatusNotFound {
		t.Errorf("страница за краем ленты: код %d, ожидался 404", got)
	}
}

// cssRule вырезает тело правила из стилей по его началу: проверять надо
// именно то, что стоит В ЭТОМ правиле, а не то, что нужное слово где-то в
// файле есть. Годится и для @media, и для обычного селектора.
func cssRule(t *testing.T, css, header string) string {
	t.Helper()
	i := strings.Index(css, header)
	if i < 0 {
		t.Fatalf("в стилях нет правила %s", header)
	}
	depth, start := 0, -1
	for j := i; j < len(css); j++ {
		switch css[j] {
		case '{':
			if depth == 0 {
				start = j + 1
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				return css[start:j]
			}
		}
	}
	t.Fatalf("правило %s не закрыто", header)
	return ""
}

// [Э] Переключатель вида — ИКОНКИ, как на НГС: там это два пустых <a> с одним
// title, а картинку рисует CSS. Словами он резервировал 150 px, и на аппарате
// с крупным системным шрифтом (viewport 288 CSS-пикселей) под текст заметки
// оставалось два знака в строке — жалоба владельца 18.08.2026.
func TestViewSwitcherIsIcons(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote()})
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, ">Дерево<") || strings.Contains(body, ">Линейный<") {
		t.Error("переключатель остался словами — на узком экране он съедает колонку текста")
	}
	if !strings.Contains(body, `title="Древовидный вид комментариев"`) ||
		!strings.Contains(body, `aria-label="Линейный вид комментариев"`) {
		t.Error("у значка нет ни title, ни aria-label: назначение кнопки не узнать ни мышью, ни экранным диктором")
	}
}

// [Э] На телефоне блок заметки не резервирует ширину под переключатель: тот
// уходит в свою строку. Резерв — свойство ШИРОКОГО экрана, и это ровно тот
// случай, когда десктопное решение, дожившее до мобильной вёрстки, ломает
// страницу целиком.
func TestMobileFreesNoteTextWidth(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	if !strings.Contains(mobile, ".notebox .nbody { padding-right: 0; }") {
		t.Error("на телефоне под текстом заметки остался резерв под переключатель")
	}
	if !strings.Contains(mobile, ".switch { position: static") {
		t.Error("переключатель на телефоне остался в углу поверх текста")
	}
	// Ник шире своей колонки наезжал на текст: 100px при колонке в 64.
	if !strings.Contains(mobile, ".author .nick { max-width: 100%;") {
		t.Error("ник автора на телефоне шире своей колонки")
	}
}

// [Э] На телефоне шапка и подвал отбиты от края экрана так же, как колонка
// текста: в ленте текст стоит на 20px от края (10px у .main плюс 10px у
// .wrap), а название площадки и значок участника упирались в самый край —
// жалоба владельца 18.08.2026. Отступ добавлен ТОЛЬКО шапке и подвалу: у
// текста заметки на узком экране нельзя отнимать ширину.
func TestMobileHeaderKeepsEdgeMargin(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	if !strings.Contains(mobile, ".top .wrap, .foot .wrap { padding: 0 20px; }") {
		t.Error("на телефоне название площадки и значок участника прижаты к краю экрана")
	}
	if strings.Contains(mobile, "  .wrap {") {
		t.Error("отступ выдан всем .wrap разом — он отнимает ширину у текста заметки")
	}
}

// [Э] Шапка на узком экране ПЕРЕНОСИТСЯ, а не наезжает сама на себя. Правило
// было описано в комментарии к стилям («flex перенесёт его строкой ниже»), но
// flex-wrap не стоял, и вкладки шапки ложились поверх названия площадки.
func TestHeaderWrapsOnNarrowScreen(t *testing.T) {
	if !strings.Contains(cssText(t), "flex-wrap: wrap; gap: 8px 20px") {
		t.Error("шапка не переносится: на узком экране кнопки наедут на название")
	}
}

// [Э] Подвал прижат к низу окна. Самая частая короткая страница — заметка без
// комментариев; до 18.08.2026 подвал на ней повисал посередине, а под ним
// оставалось полэкрана пустоты, будто страница не догрузилась (экран
// владельца). Тянется при этом СОДЕРЖИМОЕ, а не пустая распорка: карточка с
// текстом доходит до подвала, и низ выглядит законченным.
func TestFooterSticksToTheBottom(t *testing.T) {
	css := cssText(t)
	body := cssRule(t, css, "body {")
	for _, want := range []string{"min-height: 100vh", "flex-direction: column"} {
		if !strings.Contains(body, want) {
			t.Errorf("в правиле body нет %q — подвал повиснет на короткой странице", want)
		}
	}
	if !strings.Contains(cssRule(t, css, ".main {"), "flex: 1 0 auto") {
		t.Error("содержимое не растягивается до подвала")
	}
}

// [Э] Ширина карточки не зависит от того, много ли на странице текста. Правило
// оплачено регрессией: после того как подвал прижали к низу (body стал
// flex-колонкой), у `.main` остались АВТОМАТИЧЕСКИЕ поля поперёк оси, а они
// отменяют растяжение flex-элемента — карточка стала считаться по содержимому.
// На ленте это незаметно, а страница новой заметки схлопнулась в колонку
// вокруг пустого поля ввода (экран владельца, 18.08.2026).
func TestCardWidthDoesNotFollowContent(t *testing.T) {
	main := cssRule(t, cssText(t), ".main {")
	if !strings.Contains(main, "margin: 10px auto") {
		t.Fatal("карточка перестала центроваться — правило ниже больше ничего не стережёт")
	}
	if !strings.Contains(main, "width: 100%") {
		t.Error("у карточки нет ширины: с автополями flex не растянет её, и она съёжится по содержимому")
	}
}

// До первого нажатия тему держит система, а сервер её не видит — значит назвать
// нажатую кнопку может только CSS. Иначе переключатель встречает человека двумя
// погашенными кнопками и врёт про то, что у него на экране.
// Источник: экран владельца, 18.08.2026 («не понял, чем Классика отличается от
// светлой» — набор сокращён, и подсветка обязана остаться честной).
func TestSwitcherMarksSystemTheme(t *testing.T) {
	css := cssText(t)
	for _, want := range []string{
		`:root:not([data-theme]) .tbtn.t-classic`,
		`:root:not([data-theme]) .tbtn.t-dark`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("в стилях нет правила %s: кнопка системной темы не подсветится", want)
		}
	}
}

// [Ф] Свёрнутая ветка ИСЧЕЗАЕТ. На НГС это класс `.lv-hidden{display:none}`, и
// ветки приходят с ним прямо в разметке (на живой странице заметки 312696 —
// 256 строк из 325); у нас ветка убирается атрибутом hidden, а его правило
// живёт в таблице БРАУЗЕРА и слабее любого авторского display. С `.c { display:
// flex }` сворачивание не работало вовсе: кнопка меняла подпись, тред не
// двигался (жалоба владельца 18.08.2026 — «не работает „Свернуть все ответы“»).
func TestCollapsedBranchIsHidden(t *testing.T) {
	css := cssText(t)
	if !strings.Contains(cssRule(t, css, ".c {"), "display: flex") {
		t.Fatal("у строки треда больше нет своего display — правило ниже стережёт уже не то")
	}
	if !strings.Contains(cssRule(t, css, ".c[hidden] {"), "display: none") {
		t.Error("свёрнутая ветка остаётся на виду: авторский display перебивает hidden")
	}
}

// [Э] На телефоне текст реплики крупнее, а служебное под ней — в одну строку.
// Жалоба владельца 23.08.2026: «слишком мелко и при этом слишком много
// свободного пространства». Одно вытекает из другого: 14px десктопа на телефоне
// читаются мелко, а реакции, «Ответить» и полоска модератора тремя отдельными
// строками превращали однострочную реплику в четырёхстрочную — экран уходил на
// пустоту вокруг двух фраз.
func TestMobileTextIsBiggerAndControlsFitOneLine(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	if !strings.Contains(mobile, ".text { font-size: 16px;") {
		t.Error("на телефоне текст остался кеглем десктопа")
	}
	// inline-flex, а не flex: только так три блока текут ОДНОЙ строкой и
	// переносятся сами, когда не помещаются.
	if !strings.Contains(mobile, ".rx, .cact, .modbar { display: inline-flex;") {
		t.Error("на телефоне служебное под репликой снова занимает три строки")
	}
}

// [Ж] Карточка заметки на телефоне сделана под ПАЛЕЦ (жалоба владельца
// 26.08.2026 по снимку вертикального экрана: «некоторые элементы слишком
// мелкие, какие-то съехали в асимметричную кучку»).
//
// Кучка была из двух разных мест. Служебная строка шла ТРЕМЯ этажами по 12px —
// «Комментарии N» и «Добавить комментарий» стояли display:block каждый со своей
// строки. А полоска модератора несла пять подписей словами, которые не
// помещались в ряд и переносились как придётся: две кнопки, потом кнопка и два
// серых слова.
//
// Лечится это по-разному, и тест держит оба лечения сразу, потому что порознь
// они бессмысленны: одна строка из мелких ссылок ничем не лучше трёх.
func TestMobileNoteCardIsForFingers(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")

	// Служебное — ОДИН ряд, а не три этажа.
	if !strings.Contains(mobile, ".nfoot { display: flex;") {
		t.Error("служебная строка под заметкой снова идёт этажами")
	}
	if strings.Contains(mobile, ".clink { margin-left: 0; display: block;") {
		t.Error("«Комментарии N» снова занимает свою строку")
	}
	// Нажимаемое — не меньше 40px: палец накрывает около 9 мм.
	if !strings.Contains(mobile, "min-width: 40px; min-height: 40px;") {
		t.Error("кнопки полоски действий на телефоне меньше пальца")
	}
	// Значки показываются только здесь; на десктопе их не видно вовсе.
	if !strings.Contains(mobile, ".modbar .ico, .clink .ico { display: inline-block; }") {
		t.Error("на телефоне значки не показываются — полоска снова словами")
	}
}

// [Ж] Подпись у кнопки со значком НЕ УДАЛЯЕТСЯ, а прячется показом.
//
// Разница не косметическая: display:none убирает текст и из дерева
// доступности, то есть у кнопки пропадает ИМЯ — читалка объявит её «кнопка».
// Поэтому подпись уводится тем же приёмом, что и .sr-only, а объяснение для
// глаза даёт title (правило Ш5з о собственных метках).
func TestMobileLabelsAreHiddenNotRemoved(t *testing.T) {
	mobile := cssRule(t, cssText(t), "@media (max-width: 700px)")
	lbl := ".modbar .lbl, .clink .lbl {"
	i := strings.Index(mobile, lbl)
	if i < 0 {
		t.Fatal("подписи кнопок на телефоне не прячутся вовсе")
	}
	rule := mobile[i : i+strings.Index(mobile[i:], "}")]
	if !strings.Contains(rule, "clip-path") {
		t.Error("подпись прячется не отводом за край — читалка её не прочтёт")
	}
	if strings.Contains(rule, "display: none") {
		t.Error("подпись удалена из дерева доступности: у кнопки пропало имя")
	}
}

// [Э] Шапка ЛИПКАЯ, и в ней стоит счётчик реплик читаемой заметки. Просьба
// владельца 23.08.2026: «из треда неудобно мотать наверх к меню, чтобы куда-то
// перейти». Тред показывается целиком — на самой длинной заметке зеркала это 891
// реплика, — и дорога до навигации из его середины измеряется экранами.
//
// Счётчик — спутник липкости, а не украшение: до заголовка «Комментарии N»
// оттуда же не домотать. Значком с числом, потому что ширина строки на телефоне
// кончается раньше всего.
func TestStickyHeaderCarriesCommentCount(t *testing.T) {
	css := cssText(t)
	if !strings.Contains(cssRule(t, css, ".top {"), "position: sticky") {
		t.Error("шапка не липкая: из середины треда до неё не домотать")
	}
	// Якорь под липкой шапкой обязан иметь отступ прокрутки, иначе реплика, на
	// которую пришли по ссылке «#», встаёт ПОД ней.
	if !strings.Contains(css, "scroll-margin-top") {
		t.Error("у якорей нет отступа под липкую шапку")
	}

	note := sampleNote()
	note.CommentCount = 158
	st := &fakeStore{note: note, thread: sampleThread(), total: 1, notes: []platform.NoteView{note}}
	h := openServer(t, st)

	page := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(page, `class="hcount"`) || !strings.Contains(page, `<span class="n">158</span>`) {
		t.Errorf("в шапке страницы заметки нет счётчика реплик:\n%s", page)
	}
	if !strings.Contains(page, `id="comments"`) {
		t.Error("счётчику некуда вести: у заголовка треда нет якоря")
	}
	// В ленте его нет вовсе: там читают не одну заметку, и счётчик показывал бы
	// число неизвестно чего.
	if feed := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(feed, "hcount") {
		t.Error("счётчик реплик оказался в шапке ленты")
	}
}

// [Э] Название площадки на узком экране — ЗНАК, а не слово. Мера прежняя:
// «человек не заметил переезда», — но шапка у нас своя (на НГС она не липкая и
// счётчика в ней нет), и цена слова считается по ней. На телефоне «Зазеркалье»
// занимает четверть строки, и угол «про меня» уезжал второй строкой: липкая
// шапка в две строки съедает пол-экрана треда (жалоба владельца 23.08.2026).
// Знак при этом обязан назвать себя сам — слово рядом скрыто ПОКАЗОМ, и без
// подписи на ссылке площадка осталась бы на телефоне безымянной.
func TestBrandBecomesMarkOnNarrowScreen(t *testing.T) {
	css := cssText(t)
	narrow := cssRule(t, css, "@media (max-width: 460px)")
	if !strings.Contains(narrow, ".brand .bname { display: none; }") {
		t.Error("на узком экране название площадки не уступает место знаку")
	}
	// Знак не должен уезжать вместе со словом: тогда от названия не осталось бы
	// ничего вовсе. Правил про него в этом блоке хватает (размер, поле под
	// палец) — запрещено ровно одно.
	if strings.Contains(narrow, ".mark { display: none") {
		t.Error("вместе со словом с узкого экрана уехал и знак площадки")
	}

	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}})
	page := do(h, guest(t, "GET", "/")).Body.String()
	if !strings.Contains(page, `class="mark"`) {
		t.Errorf("в шапке нет знака площадки: %s", page)
	}
	if !strings.Contains(page, `aria-label="`+SiteName+`"`) {
		t.Error("знак площадки не назван: с телефона имя площадки пропадёт вовсе")
	}
	if !strings.Contains(page, `<span class="bname">`+SiteName+`</span>`) {
		t.Error("слово «" + SiteName + "» пропало из шапки совсем")
	}
}
