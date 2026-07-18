package love

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseNotes(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	// Третья заметка без .lv-notes__comment-link пропускается.
	if len(notes) != 2 {
		t.Fatalf("ожидалось 2 заметки, получено %d: %+v", len(notes), notes)
	}

	want0 := Note{ID: "301109", AuthorID: "376712", AuthorName: "Мария",
		Text: "Первая заметка — привет всем!"}
	if notes[0] != want0 {
		t.Errorf("заметка 0:\n got %+v\nwant %+v", notes[0], want0)
	}

	// Заметка без автора — анонимная.
	want1 := Note{ID: "301110", AuthorID: "0", AuthorName: "Анонимно",
		Text: "Анонимная заметка, автора не видно"}
	if notes[1] != want1 {
		t.Errorf("заметка 1:\n got %+v\nwant %+v", notes[1], want1)
	}
}

func TestParseNotesEmptyFeedIsMarkupError(t *testing.T) {
	_, err := ParseNotes(strings.NewReader("<html><body><p>ничего</p></body></html>"))
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError, получено: %v", err)
	}
}

func TestParseNotesMissingTextIsMarkupError(t *testing.T) {
	html := `<div class="lv-notes__note-item">
	           <a class="lv-notes__comment-link" name="1"></a>
	         </div>`
	_, err := ParseNotes(strings.NewReader(html))
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError про текст заметки, получено: %v", err)
	}
}

func TestParseComments(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 {
		t.Fatalf("ожидалось 2 комментария, получено %d", len(comments))
	}

	// Первый в документе — самый новый (страница desc).
	c := comments[0]
	if c.ID != 98770 {
		t.Errorf("id: got %d, want 98770", c.ID)
	}
	// Относительный аватар достраивается до абсолютного.
	if c.AvatarURL != "https://love.ngs.ru/static/i/new/profile/male300px.png" {
		t.Errorf("avatar: %q", c.AvatarURL)
	}
	if c.AuthorLink != "https://love.ngs.ru/anketa981234/" {
		t.Errorf("author link: %q", c.AuthorLink)
	}
	wantTime := time.Date(2026, 7, 5, 22, 0, 0, 0, nsk)
	if !c.PublishedAt.Equal(wantTime) {
		t.Errorf("published: got %v, want %v", c.PublishedAt, wantTime)
	}

	// Имя с запятыми: возраст отделяется по последней запятой.
	c = comments[1]
	if c.AuthorName != "Мария, свет, радость" || c.AuthorAge != "34" {
		t.Errorf("имя/возраст: %q / %q", c.AuthorName, c.AuthorAge)
	}
	// Абсолютный аватар не трогаем.
	if c.AvatarURL != "https://cdn.example.net/avatars/376712.jpg" {
		t.Errorf("avatar: %q", c.AvatarURL)
	}
	if c.Text != "Отличная заметка!" {
		t.Errorf("text: %q", c.Text)
	}
}

func TestParseCommentsEmptyIsOK(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_empty.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatalf("пустая страница комментариев не должна быть ошибкой: %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("ожидался пустой список, получено %d", len(comments))
	}
}

func TestParseCommentsBrokenDateIsMarkupError(t *testing.T) {
	html := `<div class="lv-note__comment-item">
	           <a id="comment5"></a>
	           <img class="avatar" src="/a.png" alt="Имя,30">
	           <a class="lv-people__nickname" href="/anketa1/">Имя</a>
	           <div class="lv-comment__pubdate">позавчера</div>
	           <div class="lv-comment__text">текст</div>
	         </div>`
	_, err := ParseComments(strings.NewReader(html), "https://love.ngs.ru")
	var me *MarkupError
	if !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError про дату, получено: %v", err)
	}
}

func TestSplitNameAge(t *testing.T) {
	for _, tc := range []struct{ alt, name, age string }{
		{"Мария,34", "Мария", "34"},
		{"Мария, свет, радость,34", "Мария, свет, радость", "34"},
		{"Безвозраста", "Безвозраста", ""},
	} {
		name, age := splitNameAge(tc.alt)
		if name != tc.name || age != tc.age {
			t.Errorf("splitNameAge(%q) = %q, %q; want %q, %q", tc.alt, name, age, tc.name, tc.age)
		}
	}
}

func TestDigitsOf(t *testing.T) {
	if got := digitsOf("/anketa376712/"); got != "376712" {
		t.Errorf("digitsOf: %q", got)
	}
	if got := digitsOf("/no-digits/"); got != "0" {
		t.Errorf("digitsOf без цифр должен вернуть \"0\", получено %q", got)
	}
}
