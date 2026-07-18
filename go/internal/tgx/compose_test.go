package tgx

import (
	"strings"
	"testing"

	"lovegw/internal/store"
)

func TestComposeNoteMessage(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "376712", AuthorName: "Мария <3",
		Text: "Текст с <тегами> & амперсандом"}
	got := ComposeNoteMessage("https://love.ngs.ru", "@grfmn", n)
	want := `<b><a href="https://love.ngs.ru/profile/376712">Мария &lt;3:</a></b>` + "\n" +
		"Текст с &lt;тегами&gt; &amp; амперсандом\n\n@grfmn"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestComposeNoteMessageAnonymous(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "0", AuthorName: "Анонимно", Text: "т"}
	got := ComposeNoteMessage("https://love.ngs.ru", "", n)
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

func TestDeepLink(t *testing.T) {
	got := DeepLink(-1001418271018, 555, 42)
	want := "https://t.me/c/1418271018/555?thread=42"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
