package tgx

import (
	"strings"
	"testing"

	"lovegw/internal/store"
)

func TestComposeNoteMessage(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "376712", AuthorName: "Мария <3",
		Text: "Текст с <тегами> & амперсандом"}
	got := ComposeNoteMessage("https://love.ngs.ru", "@grfmn", n, "")
	want := `<b><a href="https://love.ngs.ru/profile/376712">Мария &lt;3:</a></b>` + "\n" +
		"Текст с &lt;тегами&gt; &amp; амперсандом\n\n@grfmn"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Подвал: подпись канала и ссылка подписки живут одной строкой, а без подписи
// ссылка стоит одна. Видимая длина считается по тем же правилам — иначе пост
// с аватаром уедет в подпись к фото и превысит лимит 1024.
func TestComposeNoteMessageFooter(t *testing.T) {
	n := store.Note{ID: "312818", AuthorID: "0", AuthorName: "Анонимно", Text: "т"}
	link := "https://t.me/ryumkinbot?start=sub_312818"

	got := ComposeNoteMessage("https://love.ngs.ru", "@grfmn", n, link)
	if want := `@grfmn · <a href="` + link + `">` + linkSubscribe + `</a>`; !strings.HasSuffix(got, want) {
		t.Errorf("подвал с подписью:\n%s", got)
	}
	if n := visibleNoteLen("@grfmn", link, n); n != len([]rune("Анонимно:\nт\n\n@grfmn · 🔔 Подписаться")) {
		t.Errorf("видимая длина с подписью: %d", n)
	}

	got = ComposeNoteMessage("https://love.ngs.ru", "", n, link)
	if want := "т\n\n" + `<a href="` + link + `">` + linkSubscribe + `</a>`; !strings.HasSuffix(got, want) {
		t.Errorf("подвал без подписи:\n%s", got)
	}
	if n := visibleNoteLen("", link, n); n != len([]rune("Анонимно:\nт\n\n🔔 Подписаться")) {
		t.Errorf("видимая длина без подписи: %d", n)
	}
}

func TestComposeNoteMessageAnonymous(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "0", AuthorName: "Анонимно", Text: "т"}
	got := ComposeNoteMessage("https://love.ngs.ru", "", n, "")
	if got != "<b>Анонимно:</b>\nт" {
		t.Errorf("got: %q", got)
	}
}

func TestComposeCommentCaptionTruncates(t *testing.T) {
	c := store.Comment{
		AuthorName: "Имя", AuthorAge: "30 лет",
		AuthorLink: "https://love.ngs.ru/profile/1/",
		Text:       strings.Repeat("ы", 3000),
	}
	got := ComposeCommentCaption(c)
	// Видимая часть (шапка + текст без тегов) должна влезать в лимит 1024.
	head := "Имя, 30 лет:\n"
	if !strings.Contains(got, ">"+"Имя, 30 лет:"+"</a>") {
		t.Errorf("шапка с пробелом после запятой не найдена: %.80s", got)
	}
	body := got[strings.Index(got, "</b>\n")+len("</b>\n"):]
	if n := len([]rune(head)) + len([]rune(body)); n > 1024 {
		t.Errorf("видимая длина %d > 1024", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("обрезанный текст должен оканчиваться троеточием: ...%s", got[len(got)-20:])
	}
}

// Длинный комментарий в текстовом сообщении (лимит 4096) не режется, а в
// подписи к документу (1024) — режется. Так длинный комментарий уходит в TG
// целиком, а не усечённым.
func TestComposeCommentMessageKeepsLongText(t *testing.T) {
	c := store.Comment{
		AuthorName: "Имя", AuthorAge: "30 лет",
		AuthorLink: "https://love.ngs.ru/profile/1/",
		Text:       strings.Repeat("ы", 3000), // > 1024, но < 4096
	}
	msg := composeComment(c, messageLimit)
	if strings.HasSuffix(msg, "…") {
		t.Error("текст 3000 < 4096 не должен обрезаться в сообщении")
	}
	body := msg[strings.Index(msg, "</b>\n")+len("</b>\n"):]
	if n := len([]rune(body)); n < 3000 {
		t.Errorf("тело обрезано: %d рун вместо 3000", n)
	}
	if cap := composeComment(c, captionLimit); !strings.HasSuffix(cap, "…") {
		t.Error("в подписи 1024 длинный текст должен обрезаться")
	}
}

func TestComposeCommentCaptionEscapes(t *testing.T) {
	c := store.Comment{AuthorName: "A<b>", AuthorAge: "3&3",
		AuthorLink: "https://x/", Text: "x<y"}
	got := ComposeCommentCaption(c)
	if strings.Contains(got, "A<b>") || !strings.Contains(got, "A&lt;b&gt;") {
		t.Errorf("имя не экранировано: %s", got)
	}
	if !strings.Contains(got, "x&lt;y") {
		t.Errorf("текст не экранирован: %s", got)
	}
}

// Ссылка автора приезжает атрибутом чужой вёрстки. Кавычка в ней вырвалась бы
// из href, а сообщение с битой разметкой Telegram отвергает — очередь
// комментариев заметки встала бы навсегда.
func TestComposeCommentEscapesAuthorLink(t *testing.T) {
	c := store.Comment{AuthorName: "Имя", AuthorAge: "30 лет",
		AuthorLink: `https://love.ngs.ru/profile/1/"><b>`, Text: "т"}
	got := ComposeCommentCaption(c)
	if strings.Contains(got, `"><b>`) {
		t.Errorf("ссылка не экранирована: %s", got)
	}
	if !strings.Contains(got, "&#34;&gt;&lt;b&gt;") {
		t.Errorf("нет экранированной ссылки: %s", got)
	}
}

// Ссылки может не быть (parse.absolutize отбрасывает чужие схемы) — тогда шапка
// остаётся текстом, а не пустым <a href="">.
func TestComposeCommentWithoutAuthorLink(t *testing.T) {
	got := ComposeCommentCaption(store.Comment{AuthorName: "Гость", AuthorAge: "30 лет", Text: "т"})
	if strings.Contains(got, `<a href="">`) {
		t.Errorf("пустая ссылка протекла: %q", got)
	}
	if !strings.Contains(got, "<b>Гость, 30 лет:</b>") {
		t.Errorf("шапка без ссылки: %q", got)
	}
}

// Транспортная часть уведомления — подпись ссылки; сам композер и его состав
// проверяет пакет subnotice.
func TestComposeSubNoticeLinkLabel(t *testing.T) {
	got := ComposeSubNotice("к", store.Note{AuthorName: "А", Text: "т"},
		store.Comment{ID: 7, AuthorName: "Б", Text: "к"}, "https://t.me/c/1/2?thread=3")
	if !strings.Contains(got, `<a href="https://t.me/c/1/2?thread=3">Открыть в обсуждении</a>`) {
		t.Errorf("подпись ссылки Telegram: %q", got)
	}
}

func TestDeepLink(t *testing.T) {
	got := DeepLink(-1001418271018, 555, 42)
	want := "https://t.me/c/1418271018/555?thread=42"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// У реплики, написанной на площадке, возраста нет: анкетных полей она не
// заводит вовсе. Шапка обязана обойтись без запятой, иначе выйдет «Ник, :».
func TestCommentHeadWithoutAge(t *testing.T) {
	if got := commentHead("Паноптикум", ""); got != "Паноптикум:" {
		t.Errorf("без возраста: %q", got)
	}
	if got := commentHead("Паноптикум", "48"); got != "Паноптикум, 48:" {
		t.Errorf("с возрастом: %q", got)
	}
	if got := commentHead("<b>", ""); got != "&lt;b&gt;:" {
		t.Errorf("экранирование: %q", got)
	}
}
