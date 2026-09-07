package web

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// old — момент, когда сайт разметку ещё показывал, new — когда уже нет.
// Рубеж снят замером по выгрузкам архива, см. bbSunset.
var (
	old = time.Date(2014, 3, 1, 12, 0, 0, 0, time.UTC)
	now = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
)

// plainNote — заметка НГС, написанная после заката разметки: сайт печатал теги
// буквально, и такими они остаются. Написанное на площадке сюда НЕ относится —
// у своего текста разметка живая, см. nativeNote.
func plainNote(text string) string {
	return string(noteBodyHTML(platform.NoteView{ID: 312811, Body: text, PublishedAt: now}))
}

// nativeNote — заметка, написанная ЗДЕСЬ: нативная полоса идентификаторов.
func nativeNote(text string) string {
	return string(noteBodyHTML(platform.NoteView{ID: platform.NativeIDBase + 7, Body: text, PublishedAt: now}))
}

func legacyNote(text string) string {
	return string(noteBodyHTML(platform.NoteView{ID: 240903, Body: text, PublishedAt: old}))
}

// До 02.06.2014 сайт разметку показывал — значит показываем и мы: заметка
// «Клуба пятничных неудачников» без этого читается как список тегов.
func TestLegacyMarkupRendered(t *testing.T) {
	got := legacyNote("сегодняшний [b][i][color=green]КПН[/color][/b][/i] объявляю открытым")
	want := `<p>сегодняшний <b><i><span class="bb-green">КПН</span></i></b> объявляю открытым</p>`
	if got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

// После рубежа сайт печатал теги буквально, и человек, читавший ту страницу,
// видел именно скобки. Переезд не место для улучшений оригинала — рубеж этот
// про ЧУЖОЙ текст и своего не касается (TestNativeMarkupRendered).
func TestMarkupAfterSunsetStaysText(t *testing.T) {
	got := string(noteBodyHTML(platform.NoteView{ID: 312811, Body: "[b]жирный[/b]", PublishedAt: now}))
	if got != "<p>[b]жирный[/b]</p>" {
		t.Errorf("разметка разобрана там, где сайт её не разбирал: %s", got)
	}
}

// СТАРАЯ ЗАПИСЬ ЦВЕТА — через двоеточие, и в архиве она главная: 1 196
// вхождений против 254 у знакомого «=» (замер 07.09.2026 по всему архиву).
// Пришедшая ей на смену запись застала лишь последние три года разметки.
func TestLegacyColorColonForm(t *testing.T) {
	got := legacyNote("[color:red]красный[/color] обычный")
	want := `<p><span class="bb-red">красный</span> обычный</p>`
	if got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
	// «[color:Red]» в архиве 53 раза — регистр значения роли не играет.
	if got := legacyNote("[color:Red]к[/color]"); got != `<p><span class="bb-red">к</span></p>` {
		t.Errorf("верхний регистр значения: %q", got)
	}
}

// Цвета, которые открыла старая запись: ими писали до 2011 года, и прежний
// список их не знал. Белый красится фоном карточки, а не белым: им прятали
// текст, и на тёмной теме буквальный белый показал бы спрятанное.
func TestLegacyColorsOfTheColonEra(t *testing.T) {
	for _, c := range []string{"cyan", "brown", "yellow", "grey", "white", "black"} {
		got := legacyNote("[color:" + c + "]x[/color]")
		if !strings.Contains(got, `<span class="bb-`+c+`">x</span>`) {
			t.Errorf("цвет %s не покрасился: %s", c, got)
		}
	}
	// Незнакомый цвет — не ошибка: тег снимается, текст остаётся. Инлайнового
	// style у нас нет, поэтому «#666666» покрасить нечем.
	if got := legacyNote("[color:#666666]серый[/color]"); got != "<p>серый</p>" {
		t.Errorf("незнакомый цвет: %q", got)
	}
}

// Тег, закрытый где-то ДАЛЬШЕ, строку переживает: в заметке 60950
// «[color:purple]» открыт на второй строке, а закрыт на четвёртой, и покрашены
// обязаны быть все три.
func TestUnclosedTagsDoNotBreakMultilineColor(t *testing.T) {
	got := legacyNote("[color:purple]первая\nвторая\nтретья[/color]")
	if n := strings.Count(got, `<span class="bb-purple">`); n != 1 {
		t.Errorf("цвет через строки разорван на %d кусков: %s", n, got)
	}
	if !strings.Contains(got, "третья</span>") {
		t.Errorf("цвет не дожил до последней строки: %s", got)
	}
}

// А тег, которого не закрыли ВОВСЕ, красит свою строку и дальше не идёт
// (решение владельца 07.09.2026: так это выглядело на НГС). Прежде он красил
// весь остаток заметки — по замеру до заката это 194 текста на 487 401.
func TestUnclosedTagEndsWithItsLine(t *testing.T) {
	got := legacyNote("[color:grey]серая строка\nобычная строка")
	if !strings.Contains(got, `<span class="bb-grey">серая строка</span><br>обычная строка`) {
		t.Errorf("незакрытый цвет не кончился строкой: %s", got)
	}
	if got := legacyNote("[b]жирная\nобычная"); !strings.Contains(got, "<b>жирная</b><br>обычная") {
		t.Errorf("незакрытый [b] не кончился строкой: %s", got)
	}
	// И через абзац он тоже не возвращается: возвращается только закрытое.
	if got := legacyNote("[b]жирная\n\nдругой абзац"); strings.Count(got, "<b>") != 1 {
		t.Errorf("незакрытый [b] вернулся в следующем абзаце: %s", got)
	}
}

// Живая разметка архива ПЕРЕКРЁСТНАЯ: «[b][i]…[/b][/i]» — обычное дело.
// Элементы HTML так пересекаться не умеют, поэтому порядок чинится, а смысл
// сохраняется; пустых пар при этом появляться не должно.
func TestLegacyMarkupCrossedNesting(t *testing.T) {
	got := legacyNote("[b][i]оба[/b][/i] дальше")
	if got != "<p><b><i>оба</i></b> дальше</p>" {
		t.Errorf("перекрёстная разметка собрана как %q", got)
	}
}

// Незакрытый тег — главная опасность: страницу мы собираем строкой, и утёкший
// <b> сделал бы жирным всё, что идёт после карточки.
func TestLegacyMarkupNeverLeaks(t *testing.T) {
	for _, in := range []string{"[b]начало без конца", "[i][u]совсем", "[color=red]красное"} {
		got := legacyNote(in)
		if op, cl := strings.Count(got, "<b>")+strings.Count(got, "<i>")+
			strings.Count(got, "<u>")+strings.Count(got, "<span"),
			strings.Count(got, "</b>")+strings.Count(got, "</i>")+
				strings.Count(got, "</u>")+strings.Count(got, "</span>"); op != cl {
			t.Errorf("%q: открыто %d, закрыто %d — %s", in, op, cl, got)
		}
	}
}

// Закрывающий без открывающего в архиве встречается чаще, чем парный:
// «[/color]» 379 против 224 «[color=…]». Он не разметка и не текст.
func TestLegacyOrphanCloserDropped(t *testing.T) {
	if got := legacyNote("текст[/b] дальше"); got != "<p>текст дальше</p>" {
		t.Errorf("осиротевший закрывающий тег дал %q", got)
	}
}

// «[b]» через пустую строку — обычное дело, а <p> в HTML пересекать нельзя:
// разметка закрывается на границе абзаца и возвращается в следующем.
func TestLegacyMarkupSpansParagraphs(t *testing.T) {
	got := legacyNote("[b]раз\n\nдва[/b] три")
	if got != "<p><b>раз</b></p><p><b>два</b> три</p>" {
		t.Errorf("разметка через абзац собралась как %q", got)
	}
}

// Цвет приезжает классом (CSP запрещает атрибут style), поэтому список цветов
// закрытый. Незнакомый не красит и НЕ печатается тегом: тег — не текст.
func TestLegacyUnknownColorJustDoesNotPaint(t *testing.T) {
	if got := legacyNote("[color=chartreuse]трава[/color]"); got != "<p>трава</p>" {
		t.Errorf("незнакомый цвет дал %q", got)
	}
	if got := legacyNote("[color=#ff0000]алое[/color]"); got != "<p>алое</p>" {
		t.Errorf("цвет числом дал %q", got)
	}
}

// Экранирование первично: разметку добавляем мы, а текст остаётся текстом
// целиком — в том числе внутри разобранных тегов.
func TestLegacyMarkupStillEscapes(t *testing.T) {
	got := legacyNote(`[b]<script>alert("вжух")</script>[/b]`)
	if strings.Contains(got, "<script") {
		t.Fatalf("в разметку прошёл скрипт: %s", got)
	}
	if !strings.HasPrefix(got, "<p><b>&lt;script&gt;") {
		t.Errorf("текст внутри тега собрался неверно: %s", got)
	}
}

// Осенью 2013-го обращение рисовал САМ сайт: «Для [b][i]Ник[/i][/b] текст».
// У нас адресат живёт ребром и подписывается текущим ником — оставленный в
// теле префикс дал бы обращение дважды.
func TestLegacyAddressDrawnOnce(t *testing.T) {
	c := platform.CommentView{
		ID: 5938231, PublishedAt: old,
		Body:    "Для [b][i]Сибирский кот[/i][/b] а вот и нет",
		ReplyTo: &platform.ReplyRef{CommentID: 5938221, Nick: "Кот"},
	}
	got := string(commentBodyHTML(nil, c))
	if strings.Contains(got, "Сибирский кот") {
		t.Errorf("обращение показано дважды: %s", got)
	}
	if got != `<p><a class="to" href="#c5938221">Кот</a>, а вот и нет</p>` {
		t.Errorf("получено %q", got)
	}
}

// Ребра нет — обращение единственный след адресата, и трогать его нельзя:
// сайт показывал его разметкой, показываем и мы.
func TestLegacyAddressKeptWithoutEdge(t *testing.T) {
	c := platform.CommentView{
		ID: 5938231, PublishedAt: old,
		Body: "Для [b][i]Сибирский кот[/i][/b] а вот и нет",
	}
	want := "<p>Для <b><i>Сибирский кот</i></b> а вот и нет</p>"
	if got := string(commentBodyHTML(nil, c)); got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

// Написанное ЗДЕСЬ разметку знает — решение владельца 21.08.2026. Случай из
// жизни: закреплённая заметка «[b]Хотелки[/b]» вышла на страницу скобками, и
// правило «своего синтаксиса не заводим» на этом кончилось. Знаки те же, что у
// сайта: у переехавших они в пальцах, а заводить рядом второй набор значило бы
// переучивать людей ради чистоты замысла.
func TestNativeMarkupRendered(t *testing.T) {
	got := nativeNote("[b]Хотелки[/b]\n\nА накидайте, чего хотелось бы.")
	want := "<p><b>Хотелки</b></p><p>А накидайте, чего хотелось бы.</p>"
	if got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

// Своя реплика — то же самое: разметку разбираем и в комментарии, иначе
// справочник под формой ответа обещал бы то, чего страница не делает.
func TestNativeCommentMarkupRendered(t *testing.T) {
	c := platform.CommentView{
		ID: platform.NativeIDBase + 12, PublishedAt: now,
		Body:    "[i]совсем[/i] другое дело",
		ReplyTo: &platform.ReplyRef{CommentID: 312811, Nick: "Кот"},
	}
	want := `<p><a class="to" href="#c312811">Кот</a>, <i>совсем</i> другое дело</p>`
	if got := string(commentBodyHTML(nil, c)); got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

// Обращение образца 2013 года — работа САЙТА, и в своём тексте его не снимают:
// человек написал эти слова сам, а ребро у реплики своё.
func TestNativeCommentKeepsLegacyLookingAddress(t *testing.T) {
	c := platform.CommentView{
		ID: platform.NativeIDBase + 13, PublishedAt: now,
		Body:    "Для [b][i]Сибирский кот[/i][/b] это цитата, а не обращение",
		ReplyTo: &platform.ReplyRef{CommentID: 312811, Nick: "Кот"},
	}
	if got := string(commentBodyHTML(nil, c)); !strings.Contains(got, "Сибирский кот") {
		t.Errorf("свой текст урезан по правилу сайта: %s", got)
	}
}

// Незакрытый тег в СВОЁМ тексте опаснее, чем в архивном: его пишут прямо
// сейчас, руками, и «[b]» без пары — обычная опечатка. Утёкший <b> сделал бы
// жирной всю страницу после карточки.
func TestNativeMarkupNeverLeaks(t *testing.T) {
	got := nativeNote("[b]начало без конца")
	if strings.Count(got, "<b>") != strings.Count(got, "</b>") {
		t.Errorf("разметка утекла: %s", got)
	}
}

// Справочник под формой — ПОДМНОЖЕСТВО того, что разбор красит: показать мы
// умеем и цвета 2008 года (белый, чёрный, cyan…), а предлагать их незачем.
// Проверяется поэтому включение, а не равенство: каждый предложенный цвет
// обязан краситься, иначе подсказка обещает то, чего не будет. Заодно — что
// образцы прогнаны РАЗБОРОМ, а не написаны руками: скобки в них не остаются.
func TestMarkupHelpCoversEveryOfferedColor(t *testing.T) {
	rows := markupHelp()
	if len(rows) == 0 {
		t.Fatal("справочник разметки пуст")
	}
	for _, r := range rows {
		if strings.Contains(string(r.Sample), "[") {
			t.Errorf("образец %q не разобран: %s", r.Code, r.Sample)
		}
	}
	colors := string(rows[len(rows)-1].Sample)
	if n := strings.Count(colors, `<span class="bb-`); n != len(bbColorNames) {
		t.Errorf("в справочнике %d цветов из %d предлагаемых", n, len(bbColorNames))
	}
	for _, c := range bbColorNames {
		if !bbColors[c.Code] {
			t.Errorf("цвет %s назван в справочнике, но разбором не красится", c.Code)
		}
		if !strings.Contains(colors, `class="bb-`+c.Code+`"`) {
			t.Errorf("цвет %s не показан образцом", c.Code)
		}
	}
}

// Заголовок вкладки — плоский текст: показать жирное или картинку он не умеет,
// поэтому знаки снимаются. Но ровно там, где страница их РАЗБИРАЕТ: у чужого
// текста после заката сайт печатал скобки буквально, и вкладка обязана
// совпадать с карточкой, а не улучшать её.
func TestNoteTitleDropsMarkupWhereThePageParsesIt(t *testing.T) {
	cases := []struct {
		name string
		note platform.NoteView
		want string
	}{
		{
			"своя заметка: знаки сняты",
			platform.NoteView{ID: platform.NativeIDBase + 15, Body: "[b]Хотелки[/b] площадки :::popcorn:::",
				PublishedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)},
			"Хотелки площадки",
		},
		{
			"НГС до заката: сайт их рисовал — снимаем",
			platform.NoteView{ID: 250000, Body: "[b]Объявление[/b] КПН",
				PublishedAt: time.Date(2013, 5, 1, 0, 0, 0, 0, time.UTC)},
			"Объявление КПН",
		},
		{
			"НГС после заката: сайт печатал буквально — оставляем",
			platform.NoteView{ID: 312811, Body: "[b]Объявление[/b] КПН",
				PublishedAt: time.Date(2020, 5, 1, 0, 0, 0, 0, time.UTC)},
			"[b]Объявление[/b] КПН",
		},
		{
			"НГС до заката: старая форма кода снимается так же",
			platform.NoteView{ID: 200000, Body: "Всем доброго утра ~flowers~",
				PublishedAt: time.Date(2011, 5, 1, 0, 0, 0, 0, time.UTC)},
			"Всем доброго утра",
		},
		{
			"незнакомый код смайла остаётся текстом",
			platform.NoteView{ID: platform.NativeIDBase + 16, Body: ":::такогонет::: и всё",
				PublishedAt: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)},
			":::такогонет::: и всё",
		},
	}
	for _, c := range cases {
		if got := noteTitle(c.note); got != c.want {
			t.Errorf("%s: заголовок %q, ожидался %q", c.name, got, c.want)
		}
	}
}
