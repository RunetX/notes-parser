package maxx

// Проверки предела длины сообщения MAX. Сервер меряет готовую строку вместе с
// разметкой (см. комментарий к messageLimit), а зеркало шлёт комментарии треда
// строго по порядку и встаёт на первой ошибке — поэтому непринятая длина здесь
// не косметика, а вечная пробка.

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"testing"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

// entityRe — текст, в котором каждый «&» начинает целую сущность.
var entityRe = regexp.MustCompile(`^([^&]|&(amp|lt|gt|#34|#39);)*$`)

// prodComment воспроизводит комментарий 63212222, на котором встала очередь
// MAX 06.08.2026: ссылка 35 знаков, имя 9, возраст 7, текст 4472.
func prodComment() store.Comment {
	return store.Comment{
		ID:         63212222,
		AuthorLink: "https://love.ngs.ru/profile/1234567", // 35
		AuthorName: "Собеседник",                          // 10 рун — близко к боевым 9
		AuthorAge:  "45, Нск",                             // 7
		Text:       strings.Repeat("я", 4472),
	}
}

func TestComposeCommentMessageFitsLimit(t *testing.T) {
	c := prodComment()

	// Прежняя сборка — без обрезки — предел превышала: ровно этот случай и
	// остановил очередь заметки 312886.
	old := fmt.Sprintf(`<b><a href="%s">%s, %s:</a></b>%s%s`,
		c.AuthorLink, html.EscapeString(c.AuthorName), html.EscapeString(c.AuthorAge),
		"\n", html.EscapeString(c.Text))
	if n := apiLen(old); n <= messageLimit {
		t.Fatalf("тест бессмысленен: прежняя сборка укладывалась в предел (%d)", n)
	}

	got := ComposeCommentMessage(c)
	if n := apiLen(got); n > messageLimit {
		t.Errorf("сообщение не влезает в предел MAX: %d > %d", n, messageLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("обрезанный комментарий должен кончаться многоточием")
	}
	if !strings.HasPrefix(got, `<b><a href="https://love.ngs.ru/profile/1234567">`) {
		t.Errorf("шапка автора потерялась: %.80q", got)
	}
}

// Ссылка автора приезжает атрибутом чужой вёрстки: кавычка в ней вырвалась бы
// из href, а непринятое сообщение в MAX — вечная пробка в очереди заметки.
func TestComposeCommentMessageEscapesAuthorLink(t *testing.T) {
	got := ComposeCommentMessage(store.Comment{
		AuthorLink: `https://love.ngs.ru/profile/1"><b>`,
		AuthorName: "Имя", AuthorAge: "40", Text: "т"})
	if strings.Contains(got, `1"><b>`) {
		t.Errorf("ссылка не экранирована: %s", got)
	}
	if !strings.Contains(got, "&#34;&gt;&lt;b&gt;") {
		t.Errorf("нет экранированной ссылки: %s", got)
	}
}

// Ссылки может не быть (parse.absolutize отбрасывает чужие схемы) — тогда шапка
// остаётся текстом, а не пустым <a href="">.
func TestComposeCommentMessageWithoutAuthorLink(t *testing.T) {
	got := ComposeCommentMessage(store.Comment{AuthorName: "Гость", AuthorAge: "40", Text: "т"})
	if strings.Contains(got, `<a href="">`) {
		t.Errorf("пустая ссылка протекла: %q", got)
	}
	if !strings.HasPrefix(got, "<b>Гость, 40:</b>") {
		t.Errorf("шапка без ссылки: %q", got)
	}
}

func TestComposeCommentMessageShortTextIntact(t *testing.T) {
	c := store.Comment{
		AuthorLink: "https://love.ngs.ru/profile/1",
		AuthorName: "Ягода",
		AuthorAge:  "40",
		Text:       "коротко и по делу",
	}
	got := ComposeCommentMessage(c)
	if !strings.HasSuffix(got, "коротко и по делу") {
		t.Errorf("короткий текст не должен меняться: %q", got)
	}
	if strings.Contains(got, "…") {
		t.Errorf("многоточие у неурезанного текста: %q", got)
	}
}

func TestComposeCommentMessageKeepsEntitiesWhole(t *testing.T) {
	// Каждый «&» раздувается в «&amp;» (5 единиц) — граница бюджета почти
	// наверняка попадёт внутрь сущности, если резать уже экранированную строку.
	c := prodComment()
	c.Text = strings.Repeat("&", 4000)

	got := ComposeCommentMessage(c)
	if n := apiLen(got); n > messageLimit {
		t.Errorf("не влезает: %d > %d", n, messageLimit)
	}
	body := strings.TrimSuffix(strings.SplitN(got, "\n", 2)[1], "…")
	if !entityRe.MatchString(body) {
		tail := body
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		t.Errorf("сущность разорвана, хвост: %q", tail)
	}
	if body == "" || strings.Count(body, "&amp;")*5 != len(body) {
		t.Errorf("тело должно состоять из целых «&amp;», длина %d", len(body))
	}
}

func TestComposeCommentMessageEmojiCountedAsUTF16(t *testing.T) {
	c := prodComment()
	c.Text = strings.Repeat("😀", 3000) // вне BMP: по две единицы UTF-16 на руну

	got := ComposeCommentMessage(c)
	if n := apiLen(got); n > messageLimit {
		t.Errorf("эмодзи посчитаны как руны, а не как единицы UTF-16: %d > %d", n, messageLimit)
	}
	if r := []rune(got); len(r) > messageLimit {
		t.Errorf("рун тоже не должно быть больше предела: %d", len(r))
	}
}

func TestComposeCommentMessageHugeAuthorFields(t *testing.T) {
	// Ник с сайта длиной в роман не должен съедать бюджет целиком.
	c := prodComment()
	c.AuthorName = strings.Repeat("Ы", 5000)
	c.AuthorAge = strings.Repeat("9", 5000)

	if n := apiLen(ComposeCommentMessage(c)); n > messageLimit {
		t.Errorf("шапка не ограничена: %d > %d", n, messageLimit)
	}
}

func TestComposeNoteMessageFitsAndKeepsSignature(t *testing.T) {
	n := store.Note{
		ID:         "312886",
		AuthorID:   "0",
		AuthorName: "Анонимно",
		Text:       strings.Repeat("текст заметки ", 500), // 7000 знаков
	}
	got := ComposeNoteMessage("https://love.ngs.ru", "Заметки 18+", n)
	if l := apiLen(got); l > messageLimit {
		t.Errorf("заметка не влезает: %d > %d", l, messageLimit)
	}
	if !strings.HasSuffix(got, "\n\nЗаметки 18+") {
		t.Errorf("подпись должна остаться в хвосте: %.60q", got[len(got)-60:])
	}
	if !strings.HasPrefix(got, "<b>Анонимно:</b>\n") {
		t.Errorf("шапка потерялась: %.40q", got)
	}
}

func TestComposeNoteMessageShortIntact(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "42", AuthorName: "Ivan", Text: "коротко"}
	got := ComposeNoteMessage("https://love.ngs.ru", "", n)
	want := `<b><a href="https://love.ngs.ru/profile/42">Ivan:</a></b>` + "\n" + "коротко"
	if got != want {
		t.Errorf("короткая заметка изменилась:\n получили %q\n ожидали  %q", got, want)
	}
}

func TestFitHTMLKeepsMarkupValid(t *testing.T) {
	// Дайджест: много ссылок, то есть много разметки при умеренном видимом
	// тексте — ровно тот случай, где бюджета chantext по видимой длине мало.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, `<b>Заметка %d</b>: <a href="https://love.ngs.ru/notes/31288%d/">обсуждение</a>`+"\n", i, i)
	}
	src := b.String()
	if apiLen(src) <= messageBudget {
		t.Fatalf("тест бессмысленен: исходник и так влезает (%d)", apiLen(src))
	}
	if err := chantext.ValidateHTML(src); err != nil {
		t.Fatalf("исходник теста невалиден: %v", err)
	}

	got, cut := fitHTML(src)
	if !cut {
		t.Error("обрезка должна быть отмечена признаком")
	}
	if n := apiLen(got); n > messageBudget {
		t.Errorf("не уложились в бюджет: %d > %d", n, messageBudget)
	}
	if err := chantext.ValidateHTML(got); err != nil {
		t.Errorf("разметка порвана: %v", err)
	}
	if chantext.VisibleLen(got) < 100 {
		t.Errorf("обрезано слишком жадно, видимых рун осталось %d", chantext.VisibleLen(got))
	}
}

func TestFitHTMLShortUnchanged(t *testing.T) {
	src := `<b>Новость</b>: <a href="https://love.ngs.ru/">сайт</a>`
	if got, cut := fitHTML(src); got != src || cut {
		t.Errorf("короткий HTML изменился:\n получили %q\n ожидали  %q", got, src)
	}
}

func TestEscapeFitEdges(t *testing.T) {
	if got := escapeFit("что угодно", 0); got != "" {
		t.Errorf("нулевой бюджет должен давать пустую строку, получили %q", got)
	}
	if got := escapeFit("", 100); got != "" {
		t.Errorf("пустой текст должен остаться пустым, получили %q", got)
	}
	// Ровно в бюджет — без многоточия.
	exact := strings.Repeat("я", 50)
	if got := escapeFit(exact, 50); got != exact {
		t.Errorf("текст ровно в бюджет обрезан: %q", got)
	}
	if got := escapeFit(strings.Repeat("я", 51), 50); apiLen(got) > 50 {
		t.Errorf("превышение бюджета на единицу: %d", apiLen(got))
	}
}

// То же для MAX: у реплики с площадки возраста нет, и запятая в шапке лишняя.
func TestComposeCommentWithoutAge(t *testing.T) {
	got := ComposeCommentMessage(store.Comment{AuthorName: "Паноптикум", Text: "текст"})
	if want := "<b>Паноптикум:</b>\nтекст"; got != want {
		t.Errorf("шапка без возраста:\n%q\nждали:\n%q", got, want)
	}
}
