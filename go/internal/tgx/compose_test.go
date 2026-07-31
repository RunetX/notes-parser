package tgx

import (
	"strings"
	"testing"
	"time"

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

func TestComposeSubNotice(t *testing.T) {
	n := store.Note{ID: "312818", AuthorName: "Мария",
		Text: "Ищу того,\nкто пьёт чай\nиз рюмки"}
	c := store.Comment{ID: 7, AuthorName: "Виктор <3", AuthorAge: "45 лет",
		AuthorLink:  "https://love.ngs.ru/profile/1",
		PublishedAt: time.Date(2026, 7, 30, 14, 5, 0, 0, time.UTC),
		Text:        "выпьем рюмку чая & закусим"}
	got := ComposeSubNotice("рюмк", n, c, "https://t.me/c/1/2?thread=3")

	for _, want := range []string{
		"<b>рюмк</b>", // повод: сработавшее слово
		`<a href="https://love.ngs.ru/profile/1">Виктор &lt;3, 45 лет</a>`, // кто написал
		"Мария: Ищу того, кто пьёт чай из рюмки",                          // под какой заметкой, одной строкой
		"(30.07 14:05)",
		"выпьем рюмку чая &amp; закусим",
		`<a href="https://t.me/c/1/2?thread=3">Открыть в обсуждении</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q в:\n%s", want, got)
		}
	}
}

func TestComposeSubNoticeTruncatesComment(t *testing.T) {
	long := strings.Repeat("я", 600)
	got := ComposeSubNotice("к", store.Note{AuthorName: "А", Text: "т"},
		store.Comment{AuthorName: "Б", Text: long}, "https://t.me/c/1/2")
	if !strings.Contains(got, strings.Repeat("я", subNoticeCommentLimit)+"…") {
		t.Errorf("длинный комментарий не обрезан: %q", got)
	}
	if strings.Contains(got, strings.Repeat("я", subNoticeCommentLimit+1)) {
		t.Errorf("обрезка не сработала: %q", got)
	}
}

func TestComposeSubNoticeWithoutOptionalFields(t *testing.T) {
	// Аноним без возраста, ссылки на профиль и даты: в тексте не должно
	// появиться ни «Гость, :», ни пустого <a href="">.
	got := ComposeSubNotice("к", store.Note{AuthorName: "Анонимно", Text: "т"},
		store.Comment{AuthorName: "Гость", Text: "к"}, "https://t.me/c/1/2")
	if strings.Contains(got, `<a href="">`) || strings.Contains(got, "Гость, :") {
		t.Errorf("пустые поля протекли в текст: %q", got)
	}
	if !strings.Contains(got, "<b>Гость</b>") {
		t.Errorf("нет автора: %q", got)
	}
}

func TestDeepLink(t *testing.T) {
	got := DeepLink(-1001418271018, 555, 42)
	want := "https://t.me/c/1418271018/555?thread=42"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
