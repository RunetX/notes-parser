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

// plainNote — заметка, написанная после заката разметки: так показывается всё,
// что не застало разбор тегов на сайте, включая написанное на площадке.
func plainNote(text string) string {
	return string(noteBodyHTML(platform.NoteView{ID: 312811, Body: text, PublishedAt: now}))
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
// видел именно скобки. Переезд не место для улучшений оригинала.
func TestMarkupAfterSunsetStaysText(t *testing.T) {
	got := string(noteBodyHTML(platform.NoteView{ID: 312811, Body: "[b]жирный[/b]", PublishedAt: now}))
	if got != "<p>[b]жирный[/b]</p>" {
		t.Errorf("разметка разобрана там, где сайт её не разбирал: %s", got)
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
	got := string(commentBodyHTML(c))
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
	if got := string(commentBodyHTML(c)); got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}
