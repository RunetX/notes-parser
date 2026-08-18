package web

// Обращение показывается ОДИН раз.
//
// Поломка, ради которой это заведено: у переименовавшегося человека приём не
// узнал обращение в теле (сверял с нынешним ником), и страница выдала
// «Паноптикум, Рантье, привычное…» — один и тот же человек старым ником и
// новым. Здесь проверяется правило, которое это закрывает, и его границы:
// зачин фразы обращением не считается, а текст автора не режется никогда.

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

func commentWith(body string) platform.CommentView {
	return platform.CommentView{
		ID:          200000000002,
		Body:        body,
		ReplyTo:     &platform.ReplyRef{CommentID: 200000000001, Nick: "Паноптикум"},
		PublishedAt: time.Date(2026, 7, 18, 6, 35, 22, 0, time.UTC),
	}
}

// Автор назвал адресата сам — второго обращения нет, а написанное им выделено
// жирным, как это делал сам НГС.
func TestExistingAddressIsNotDoubled(t *testing.T) {
	got := string(commentBodyHTML(commentWith("Рантье, привычное, далеко не всегда хорошее.")))

	if strings.Contains(got, "Паноптикум") {
		t.Errorf("обращение дорисовано поверх написанного: %s", got)
	}
	if !strings.Contains(got, `<b class="to">Рантье</b>, `) {
		t.Errorf("обращение автора не выделено жирным: %s", got)
	}
	if !strings.Contains(got, "привычное, далеко не всегда хорошее.") {
		t.Errorf("текст потерялся: %s", got)
	}
	// Ссылки здесь быть не должно: ник в тексте исторический, а ребро может
	// указывать на другого человека — ссылка врала бы.
	if strings.Contains(got, `href="#c200000000001"`) {
		t.Errorf("обращение из текста стало ссылкой: %s", got)
	}
}

// Там, где приём обращение срезал (большинство строк), всё как было: наш ник со
// ссылкой на реплику адресата, и он следует переименованиям.
func TestDrawnAddressStaysWhereBodyHasNone(t *testing.T) {
	got := string(commentBodyHTML(commentWith("адекватный чему?")))

	if !strings.Contains(got, `<a class="to" href="#c200000000001">Паноптикум</a>, `) {
		t.Errorf("обращение не дорисовано: %s", got)
	}
}

// Зачин фразы обращением не считается — иначе «Кстати» стало бы именем, а
// настоящий адресат исчез бы со страницы.
func TestOpenerIsNotAnAddress(t *testing.T) {
	got := string(commentBodyHTML(commentWith("Кстати, я тоже так думаю")))

	if !strings.Contains(got, `<a class="to"`) {
		t.Errorf("у реплики с зачином пропало обращение: %s", got)
	}
	if !strings.Contains(got, "Кстати, я тоже так думаю") {
		t.Errorf("зачин порезан: %s", got)
	}
}

// Ник с угловой скобкой не должен ломать страницу: он приезжает в HTML через
// экранирование, как и любой чужой текст.
func TestAddressFromBodyIsEscaped(t *testing.T) {
	got := string(commentBodyHTML(commentWith(`<b>Ник</b>, текст`)))

	if strings.Contains(got, "<b>Ник</b>,") {
		t.Errorf("разметка из тела уехала в страницу как есть: %s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;Ник&lt;/b&gt;") {
		t.Errorf("ник не экранирован: %s", got)
	}
}

// Границы разбора. Каждая строка — свой случай, и все они про одно: не принять
// за обращение то, что им не является.
func TestLeadingAddressBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
		nick string
		ok   bool
	}{
		{"обычное обращение", "Анюта, адекватный чему?", "Анюта", true},
		{"ник из двух слов", "Инженер Шурик 54, оне не понимают", "Инженер Шурик 54", true},
		{"ник без пробела после запятой", "Пух,согласна", "Пух", true},
		{"фраза с точкой", "Ну что ж. Ладно, поехали", "", false},
		{"перенос строки до запятой", "Ник\nещё строка, текст", "", false},
		{"слишком длинно", strings.Repeat("а", 41) + ", текст", "", false},
		{"после запятой пусто", "Ник,", "", false},
		{"запятая первой", ", текст", "", false},
		{"запятой нет вовсе", "просто текст", "", false},
		{"пробел перед запятой", "Ник , текст", "", false},
		{"зачин", "Спасибо, помогло", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nick, rest, ok := leadingAddress(c.body)
			if ok != c.ok {
				t.Fatalf("опознано как обращение: %v, ожидалось %v", ok, c.ok)
			}
			if ok && nick != c.nick {
				t.Errorf("ник %q, ожидался %q", nick, c.nick)
			}
			if ok && rest == "" {
				t.Error("текст после обращения пуст")
			}
		})
	}
}

// Реплика без ребра не меняется вовсе: дорисовывать там нечего, и трогать текст
// показ не вправе.
func TestBodyWithoutEdgeIsUntouched(t *testing.T) {
	c := commentWith("Рантье, привычное")
	c.ReplyTo = nil
	got := string(commentBodyHTML(c))

	if strings.Contains(got, `class="to"`) {
		t.Errorf("у реплики без ребра появилось обращение: %s", got)
	}
	if !strings.Contains(got, "Рантье, привычное") {
		t.Errorf("текст изменён: %s", got)
	}
}
