package subnotice

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

// Полный состав уведомления: повод, автор ссылкой, заметка одной строкой,
// дата, текст комментария и подпись ссылки от вызывающего.
func TestComposeFull(t *testing.T) {
	n := store.Note{ID: "312818", AuthorName: "Мария",
		Text: "Ищу того,\nкто пьёт чай\nиз рюмки"}
	c := store.Comment{ID: 7, AuthorName: "Виктор <3", AuthorAge: "45 лет",
		AuthorLink:  "https://love.ngs.ru/profile/1",
		PublishedAt: time.Date(2026, 7, 30, 14, 5, 0, 0, time.UTC),
		Text:        "выпьем рюмку чая & закусим"}
	got := Compose("🔔 Ключевое слово «рюмк»", n, c, "https://t.me/c/1/2?thread=3",
		"Открыть в обсуждении")

	for _, want := range []string{
		"<b>🔔 Ключевое слово «рюмк»</b>",
		`<a href="https://love.ngs.ru/profile/1">Виктор &lt;3, 45 лет</a>`,
		"Мария: Ищу того, кто пьёт чай из рюмки", // заметка — одной строкой
		"(30.07 14:05)",
		"выпьем рюмку чая &amp; закусим",
		`<a href="https://t.me/c/1/2?thread=3">Открыть в обсуждении</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q в:\n%s", want, got)
		}
	}
}

func TestComposeEscapesLinks(t *testing.T) {
	c := store.Comment{ID: 7, AuthorName: "Б", AuthorLink: `https://x/"onmouseover=`, Text: "к"}
	got := Compose("к", store.Note{AuthorName: "А", Text: "т"}, c, `https://t.me/c/1/2"`, "Открыть")
	if strings.Contains(got, `"onmouseover=`) || strings.Contains(got, `/2">Открыть`) {
		t.Errorf("ссылка не экранирована: %s", got)
	}
}

func TestComposeTruncatesComment(t *testing.T) {
	long := strings.Repeat("я", 600)
	got := Compose("к", store.Note{AuthorName: "А", Text: "т"},
		store.Comment{ID: 7, AuthorName: "Б", Text: long}, "https://t.me/c/1/2", "Открыть")
	if !strings.Contains(got, strings.Repeat("я", commentLimit)+"…") {
		t.Errorf("длинный комментарий не обрезан: %q", got)
	}
	if strings.Contains(got, strings.Repeat("я", commentLimit+1)) {
		t.Errorf("обрезка не сработала: %q", got)
	}
}

func TestComposeWithoutOptionalFields(t *testing.T) {
	// Аноним без возраста, ссылки на профиль и даты: в тексте не должно
	// появиться ни «Гость, :», ни пустого <a href="">.
	got := Compose("к", store.Note{AuthorName: "Анонимно", Text: "т"},
		store.Comment{ID: 7, AuthorName: "Гость", Text: "к"}, "https://t.me/c/1/2", "Открыть")
	if strings.Contains(got, `<a href="">`) || strings.Contains(got, "Гость, :") {
		t.Errorf("пустые поля протекли в текст: %q", got)
	}
	if !strings.Contains(got, "<b>Гость</b>") {
		t.Errorf("нет автора: %q", got)
	}
}

// Повод «новая заметка автора»: комментария ещё нет (ID == 0), поэтому вместо
// цитаты — сама заметка, а ссылка ведёт на пост канала.
func TestComposeAuthorEvent(t *testing.T) {
	n := store.Note{ID: "312818", AuthorName: "Ягода", Text: "Купила вчера кота & кофе"}
	got := Compose("✍️ Новая заметка автора Ягода", n, store.Comment{},
		"https://t.me/c/1234567890/501", "Открыть в обсуждении")

	for _, want := range []string{
		"<b>✍️ Новая заметка автора Ягода</b>",
		"<b>Ягода</b>:",
		"Купила вчера кота &amp; кофе",
		`<a href="https://t.me/c/1234567890/501">Открыть заметку</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("нет %q в:\n%s", want, got)
		}
	}
	if strings.Contains(got, "в заметке") {
		t.Errorf("блока автора комментария быть не должно:\n%s", got)
	}
}
