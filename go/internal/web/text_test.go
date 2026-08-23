package web

import (
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// XSS у площадки убран структурно: хранимого HTML не существует, тексты лежат
// плоскими, а разметку добавляем мы. Тест сторожит именно это свойство — не
// «санитайзер работает», а «экранируется всё».
func TestBodyEscapesEverything(t *testing.T) {
	got := plainNote(`<script>alert("вжух")</script> & <b>жирный</b>`)
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
	got := plainNote("Первая строка\nвторая строка\n\nНовый абзац")
	want := "<p>Первая строка<br>вторая строка</p><p>Новый абзац</p>"
	if got != want {
		t.Errorf("получено %q, ожидалось %q", got, want)
	}
}

// Адреса в ЧУЖОМ тексте не кликабельны (решение владельца 18.08.2026:
// инструмента контроля нет, а в базе 10,7 млн чужих реплик за тринадцать лет).
// Текст адреса при этом остаётся целым — человек волен скопировать его сам.
func TestBodyDoesNotLinkForeignAddresses(t *testing.T) {
	defer withOwnLinks("https://t3h.ru")()
	got := plainNote("см. https://example.org/x, там всё")
	if strings.Contains(got, "<a ") {
		t.Fatalf("чужой адрес стал ссылкой: %s", got)
	}
	if !strings.Contains(got, "см. https://example.org/x, там всё") {
		t.Errorf("текст адреса потерян: %s", got)
	}
}

// А свой — кликается (22.08.2026): он никуда не уводит, а ведёт на соседнюю
// страницу, за которую мы отвечаем целиком. Понадобилось это с выпуском
// дайджеста: сводка недели состоит из ссылок на заметки.
func TestBodyLinksOwnAddresses(t *testing.T) {
	defer withOwnLinks("https://t3h.ru")()
	got := plainNote("см. https://t3h.ru/n/312811, там всё")
	if !strings.Contains(got, `<a href="https://t3h.ru/n/312811"`) {
		t.Errorf("свой адрес не стал ссылкой: %s", got)
	}
}

// Свой адрес опознаётся по границе, а не по началу строки: «t3h.ru.evil.example»
// начинается с «t3h.ru» по буквам, но это чужой хост.
func TestBodyDoesNotLinkLookalikeHost(t *testing.T) {
	defer withOwnLinks("https://t3h.ru")()
	got := plainNote("https://t3h.ru.evil.example/n/1")
	if strings.Contains(got, "<a ") {
		t.Fatalf("похожий адрес стал ссылкой: %s", got)
	}
}

// Без base_url своих ссылок не бывает — кликаться не должно ничего.
func TestBodyWithoutBaseURLLinksNothing(t *testing.T) {
	defer withOwnLinks("")()
	if got := plainNote("https://t3h.ru/n/1"); strings.Contains(got, "<a ") {
		t.Errorf("без base_url собрана ссылка: %s", got)
	}
}

// withOwnLinks подменяет адрес площадки на время теста.
func withOwnLinks(base string) func() {
	prev := ownLinkPrefix
	setOwnLinkPrefix(base)
	return func() { ownLinkPrefix = prev }
}

// А в тексте согласия ссылки живые: это НАШ документ, и адрес оператора в нём
// обязан работать.
func TestDocKeepsLinks(t *testing.T) {
	got := string(docHTML("Согласие\n\nОператор: https://t3h.ru/about"))
	if !strings.Contains(got, `<a href="https://t3h.ru/about"`) {
		t.Errorf("ссылка в документе не собралась: %s", got)
	}
}

// В href физически не может оказаться чужой схемы: распознаём только http и
// https, а не вычищаем опасное из всего подряд.
func TestBodyDoesNotLinkForeignSchemes(t *testing.T) {
	got := plainNote(`javascript:alert(1) data:text/html;base64,x`)
	if strings.Contains(got, "<a ") {
		t.Errorf("собрана ссылка на чужую схему: %s", got)
	}
}

// Обращение «Ник, » — ребро, а не текст. Значит рисуется оно из ТЕКУЩЕГО ника
// адресата, и внутри первого абзаца: на сайте оно выглядит началом фразы.
func TestCommentBodyDrawsAddressee(t *testing.T) {
	c := platform.CommentView{
		ID: 2, Body: "и правда",
		ReplyTo: &platform.ReplyRef{CommentID: 1, Nick: "Пух"},
	}
	got := string(commentBodyHTML(nil, c))
	if !strings.HasPrefix(got, `<p><a class="to" href="#c1">Пух</a>, и правда`) {
		t.Errorf("обращение не на месте: %s", got)
	}

	// Аноним подписывается «Аноним», а адресат без имени (снесён модерацией
	// или обезличен) обращения не получает вовсе — придумывать его нечем.
	c.ReplyTo.Anonymous = true
	if !strings.Contains(string(commentBodyHTML(nil, c)), ">Аноним</a>, ") {
		t.Error("анонимный адресат подписан не «Анонимом»")
	}
	c.ReplyTo = &platform.ReplyRef{CommentID: 1}
	if got := string(commentBodyHTML(nil, c)); got != "<p>и правда</p>" {
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

// Простыня показывается началом: реплика длиннее longBodyRunes сворачивается, а
// разворачивает её галочка с подписью — без единой строчки JS. Решение владельца
// 23.08.2026 («простыня выглядит не очень») сознательно принято про ПОКАЗ:
// запрет на длину не тронул бы ни одну из 10,7 млн уже лежащих реплик.
func TestLongCommentIsCollapsed(t *testing.T) {
	long := strings.Repeat("Психоаналитическая интерпретация лозунга. ", 60) // ~2500 знаков
	if !isLongBody(long) || isLongBody("Согласна.") {
		t.Fatalf("порог простыни считается неверно: %d знаков", len([]rune(long)))
	}
	st := &fakeStore{note: sampleNote(), thread: []platform.CommentView{
		{ID: 1, Author: platform.Author{ID: 1, Nick: "MatVei"}, Body: long, Depth: 1},
		{ID: 2, Author: platform.Author{ID: 2, Nick: "Пух"}, Body: "Согласна.", Depth: 1},
	}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	if strings.Count(body, `class="text clip"`) != 1 {
		t.Error("свёрнута не ровно одна реплика: порог считается не по той строке")
	}
	if !strings.Contains(body, "Показать полностью") || !strings.Contains(body, `for="ex1"`) {
		t.Error("простыню нечем развернуть")
	}
	// Текст при этом отдан ЦЕЛИКОМ: сворачивание — это показ, а не обрезка. Иначе
	// поиск по странице и чтение без CSS теряли бы половину реплики.
	if !strings.Contains(body, "лозунга. Психоаналитическая интерпретация лозунга. Психоаналитическая") {
		t.Error("текст свёрнутой реплики обрезан на сервере")
	}
	if strings.Contains(body, `<div class="text clip">Согласна.`) {
		t.Error("свёрнута короткая реплика")
	}
}
