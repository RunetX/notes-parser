package tgx

import (
	"strings"
	"testing"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

func TestComposeNoteMessage(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "376712", AuthorName: "Мария <3",
		Text: "Текст с <тегами> & амперсандом"}
	got := ComposeNoteMessage("@grfmn", n, "")
	// Имя автора — просто имя: ссылок на анкету НГС проект не ставит нигде
	// (решение владельца 27.08.2026).
	want := "<b>Мария &lt;3:</b>\n" +
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

	got := ComposeNoteMessage("@grfmn", n, link)
	if want := `@grfmn · <a href="` + link + `">` + linkSubscribe + `</a>`; !strings.HasSuffix(got, want) {
		t.Errorf("подвал с подписью:\n%s", got)
	}
	if n := chantext.VisibleUTF16Len(got); n != chantext.UTF16Len("Анонимно:\nт\n\n@grfmn · 🔔 Подписаться") {
		t.Errorf("видимая длина с подписью: %d", n)
	}

	got = ComposeNoteMessage("", n, link)
	if want := "т\n\n" + `<a href="` + link + `">` + linkSubscribe + `</a>`; !strings.HasSuffix(got, want) {
		t.Errorf("подвал без подписи:\n%s", got)
	}
	if n := chantext.VisibleUTF16Len(got); n != chantext.UTF16Len("Анонимно:\nт\n\n🔔 Подписаться") {
		t.Errorf("видимая длина без подписи: %d", n)
	}
}

// Длинная заметка не роняет отправку. Telegram отвечает на текст сверх 4096
// отказом, а приёмник идёт по заметкам строго по порядку и на отказе встаёт —
// то есть одна заметка площадки (потолок тела там 20 000 знаков) остановила бы
// канал насовсем. Подвал при этом обязан уцелеть: обрезанный текст без ссылки
// на оригинал — тупик для читателя.
func TestComposeNoteMessageFitsLimit(t *testing.T) {
	n := store.Note{ID: "100000000001", AuthorID: "1", AuthorName: "Паноптикум",
		Text:      strings.Repeat("ы", 20000),
		SourceURL: "https://t3h.ru/n/100000000001"}
	got := ComposeNoteMessage("@grfmn", n, "")
	if l := chantext.VisibleUTF16Len(got); l > messageLimit {
		t.Errorf("видимая длина %d > предела %d", l, messageLimit)
	}
	if !strings.Contains(got, chantext.SourceLinkLabel) {
		t.Error("ссылка на оригинал срезана вместе с телом")
	}
	if !strings.Contains(got, "@grfmn") {
		t.Error("подпись канала срезана вместе с телом")
	}
	if !strings.Contains(got, "…") {
		t.Error("обрезка не помечена многоточием")
	}
}

// Тело, размеченное отправителем (площадка), уходит разметкой, а не скобками и
// не экранированными тегами; обрезка такого тела не оставляет незакрытых тегов
// — непарный тег Telegram не принимает вовсе.
func TestComposeNoteMessageKeepsSenderMarkup(t *testing.T) {
	n := store.Note{ID: "100000000001", AuthorID: "1", AuthorName: "Паноптикум",
		Text: "[b]Хотелки[/b]", TextHTML: "<b>Хотелки</b>"}
	if got := ComposeNoteMessage("", n, ""); !strings.Contains(got, "<b>Хотелки</b>") {
		t.Errorf("разметка отправителя не доехала: %q", got)
	}

	long := store.Note{ID: "100000000002", AuthorID: "1", AuthorName: "Паноптикум",
		TextHTML: "<b>" + strings.Repeat("ы", 20000) + "</b>"}
	got := ComposeNoteMessage("", long, "")
	body := got[strings.Index(got, "</b>\n")+len("</b>\n"):]
	if err := chantext.ValidateHTML(body); err != nil {
		t.Errorf("обрезанное тело невалидно: %v", err)
	}
}

func TestComposeNoteMessageAnonymous(t *testing.T) {
	n := store.Note{ID: "1", AuthorID: "0", AuthorName: "Анонимно", Text: "т"}
	got := ComposeNoteMessage("", n, "")
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
	if !strings.Contains(got, "<b>Имя, 30 лет:</b>") {
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

// Ссылки на анкету НГС в шапке комментария больше нет (27.08.2026): поле
// AuthorLink живо — по нему дайджест зеркала опознаёт человека, — но в пост оно
// не идёт. Тест сторожит именно это: адрес чужого сайта в канал не попадает ни
// в каком виде.
func TestComposeCommentDoesNotLinkTheNGSProfile(t *testing.T) {
	c := store.Comment{AuthorName: "Имя", AuthorAge: "30 лет",
		AuthorLink: "https://love.ngs.ru/profile/1/", Text: "т"}
	got := ComposeCommentCaption(c)
	if strings.Contains(got, "ngs.ru") || strings.Contains(got, "<a href=") {
		t.Errorf("в шапку вернулась ссылка на анкету: %s", got)
	}
	if !strings.Contains(got, "<b>Имя, 30 лет:</b>") {
		t.Errorf("шапка без ссылки: %q", got)
	}
}

// Ссылки может не быть вовсе — шапка от этого не меняется.
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
