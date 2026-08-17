package web

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// XSS у площадки убран структурно: хранимого HTML не существует, тексты лежат
// плоскими, а разметку добавляем мы. Тест сторожит именно это свойство — не
// «санитайзер работает», а «экранируется всё».
func TestBodyEscapesEverything(t *testing.T) {
	got := string(bodyHTML(`<script>alert("вжух")</script> & <b>жирный</b>`))
	for _, bad := range []string{"<script", "<b>", `alert("`} {
		if strings.Contains(got, bad) {
			t.Errorf("в разметку прошло %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "&amp;") {
		t.Errorf("текст не экранирован: %s", got)
	}
}

func TestBodyKeepsParagraphsAndBreaks(t *testing.T) {
	got := string(bodyHTML("Первая строка\nвторая строка\n\nНовый абзац"))
	want := "<p>Первая строка<br>вторая строка</p><p>Новый абзац</p>"
	if got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

func TestBodyAutolinks(t *testing.T) {
	got := string(bodyHTML("см. https://t3h.ru/n/312811, там всё"))
	if !strings.Contains(got, `<a href="https://t3h.ru/n/312811" rel="nofollow noopener ugc">`) {
		t.Fatalf("ссылка не собралась: %s", got)
	}
	// Запятая принадлежит фразе, а не адресу.
	if !strings.Contains(got, "</a>, там всё") {
		t.Errorf("хвост фразы уехал в ссылку: %s", got)
	}
}

// В href физически не может оказаться чужой схемы: распознаём только http и
// https, а не вычищаем опасное из всего подряд.
func TestBodyDoesNotLinkForeignSchemes(t *testing.T) {
	got := string(bodyHTML(`javascript:alert(1) data:text/html;base64,x`))
	if strings.Contains(got, "<a ") {
		t.Errorf("собрана ссылка на чужую схему: %s", got)
	}
}

func TestExcerptCutsAtWordBoundary(t *testing.T) {
	text := strings.Repeat("слово ", 40)
	if !isLong(30, text) {
		t.Fatal("длинный текст не опознан")
	}
	got := string(excerptHTML(30, text))
	if !strings.Contains(got, "…") {
		t.Errorf("нет отбивки продолжения: %s", got)
	}
	if strings.Contains(got, "сло<") || strings.Contains(got, "сло…") {
		t.Errorf("обрыв на середине слова: %s", got)
	}
	// Короткий текст не трогаем вовсе.
	if got := string(excerptHTML(30, "коротко")); got != "<p>коротко</p>" {
		t.Errorf("короткий текст изменён: %s", got)
	}
}

// Обращение «Ник, » — ребро, а не текст. Значит рисуется оно из ТЕКУЩЕГО ника
// адресата, и внутри первого абзаца: на сайте оно выглядит началом фразы.
func TestCommentBodyDrawsAddressee(t *testing.T) {
	c := platform.CommentView{
		ID: 2, Body: "и правда",
		ReplyTo: &platform.ReplyRef{CommentID: 1, Nick: "Пух"},
	}
	got := string(commentBodyHTML(c))
	if !strings.HasPrefix(got, `<p><a class="to" href="#c1">Пух</a>, и правда`) {
		t.Errorf("обращение не на месте: %s", got)
	}

	// Аноним подписывается «Аноним», а адресат без имени (снесён модерацией
	// или обезличен) обращения не получает вовсе — придумывать его нечем.
	c.ReplyTo.Anonymous = true
	if !strings.Contains(string(commentBodyHTML(c)), ">Аноним</a>, ") {
		t.Error("анонимный адресат подписан не «Анонимом»")
	}
	c.ReplyTo = &platform.ReplyRef{CommentID: 1}
	if got := string(commentBodyHTML(c)); got != "<p>и правда</p>" {
		t.Errorf("безымянному адресату дорисовали обращение: %s", got)
	}
}

func TestPlural(t *testing.T) {
	cases := map[int]string{0: "комментариев", 1: "комментарий", 2: "комментария",
		5: "комментариев", 11: "комментариев", 21: "комментарий", 104: "комментария"}
	for n, want := range cases {
		if got := plural(n, "комментарий", "комментария", "комментариев"); got != want {
			t.Errorf("%d: %q, ожидалось %q", n, got, want)
		}
	}
}

// Неточное время — это момент, когда заметку увидело зеркало: настоящего сайт
// не отдаёт. Разница бывает в минуты, а на границе суток и в дату, поэтому оно
// помечено, а не подменено молча.
func TestWhenMarksInexactTime(t *testing.T) {
	at := time.Date(2026, 8, 17, 5, 0, 0, 0, time.UTC) // 12:00 в Новосибирске
	exact := string(whenHTML(at, true))
	if !strings.Contains(exact, "17 августа, 12:00") {
		t.Errorf("время не новосибирское: %s", exact)
	}
	if strings.Contains(exact, "≈") {
		t.Error("точное время помечено как приблизительное")
	}
	if got := string(whenHTML(at, false)); !strings.Contains(got, "≈") || !strings.Contains(got, "title=") {
		t.Errorf("неточное время не помечено: %s", got)
	}
	// Прошлый год печатается вместе с годом, нынешний — без: «2026» в каждой
	// строке ленты это шум.
	old := time.Date(2014, 3, 2, 5, 0, 0, 0, time.UTC)
	if !strings.Contains(string(whenHTML(old, true)), "2014") {
		t.Error("у старой записи не показан год")
	}
}

func TestDepthClassClamped(t *testing.T) {
	cases := map[int]string{0: "d1", 1: "d1", 5: "d5", 12: "d12", 40: "d12"}
	for d, want := range cases {
		if got := depthClass(d); got != want {
			t.Errorf("глубина %d: %q, ожидалось %q", d, got, want)
		}
	}
}

func TestLocalPath(t *testing.T) {
	ok := map[string]string{"/": "/", "/n/1?view=flat": "/n/1?view=flat"}
	for in, want := range ok {
		if got := localPath(in); got != want {
			t.Errorf("%q → %q, ожидалось %q", in, got, want)
		}
	}
	for _, in := range []string{"", "//evil", `/\evil`, "https://evil", "/x\nSet-Cookie: a=b"} {
		if got := localPath(in); got != "/" {
			t.Errorf("%q увёл на %q", in, got)
		}
	}
}

func TestValidPathCursor(t *testing.T) {
	if !validPathCursor("") || !validPathCursor("0000063207290.0000063207431") {
		t.Error("настоящий путь отвергнут")
	}
	for _, bad := range []string{"../etc", "1;2", strings.Repeat("1", maxPathLen+1)} {
		if validPathCursor(bad) {
			t.Errorf("мусор принят: %q", bad)
		}
	}
}
