package love

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Фикстуры notes_feed.html и comments_312696.html — реальные страницы сайта,
// записанные через `lovegw crawl ... -save-html`. При дрейфе вёрстки
// перезаписать их той же командой и поправить селекторы в parse.go.

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseNotesRealFeed(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 5 {
		t.Fatalf("ожидалось 5 заметок, получено %d", len(notes))
	}
	first := notes[0]
	if first.ID != "312702" {
		t.Errorf("id первой заметки: %q", first.ID)
	}
	if first.AuthorID != "1511857" {
		t.Errorf("author_id: %q", first.AuthorID)
	}
	if first.AuthorName == "" || first.AuthorName == "Анонимно" {
		t.Errorf("автор не распознан: %q", first.AuthorName)
	}
	if len(first.Text) < 20 {
		t.Errorf("текст подозрительно короткий: %q", first.Text)
	}
	for _, n := range notes {
		if n.ID == "" {
			t.Errorf("заметка без id: %+v", n)
		}
	}
}

func TestParseNotesAvatarAndImages(t *testing.T) {
	notes, err := ParseNotes(openFixture(t, "notes_feed.html"))
	if err != nil {
		t.Fatal(err)
	}
	// Аватар автора первой заметки — дефолтный силуэт (/static/...).
	if !strings.Contains(notes[0].AuthorAvatarURL, "/static/") {
		t.Errorf("аватар автора не разобран: %q", notes[0].AuthorAvatarURL)
	}
	// Хотя бы у одной заметки в фикстуре есть иллюстрация с CDN.
	withImages := 0
	for _, n := range notes {
		for _, img := range n.Images {
			withImages++
			if !strings.HasPrefix(img, "http") {
				t.Errorf("URL иллюстрации не абсолютный: %q", img)
			}
		}
	}
	if withImages == 0 {
		t.Error("ни у одной заметки не разобрана иллюстрация, ожидалась хотя бы одна")
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

func TestParseCommentsRealPage(t *testing.T) {
	comments, err := ParseComments(openFixture(t, "comments_312696.html"), "https://love.ngs.ru")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 30 {
		t.Fatalf("ожидалось 30 комментариев (limit~30), получено %d", len(comments))
	}

	// Первый в документе — самый новый (страница desc).
	c := comments[0]
	if c.ID != 63167742 {
		t.Errorf("id: got %d, want 63167742", c.ID)
	}
	if c.AuthorLink != "https://love.ngs.ru/profile/981563/" {
		t.Errorf("ссылка автора не абсолютизирована: %q", c.AuthorLink)
	}
	if !strings.HasPrefix(c.AvatarURL, "https://") {
		t.Errorf("аватар: %q", c.AvatarURL)
	}
	if c.AuthorName == "" {
		t.Error("имя автора пустое")
	}
	// alt вида "Имя, 43 года": возраст — хвост после последней запятой.
	if !strings.Contains(c.AuthorAge, "года") && !strings.Contains(c.AuthorAge, "лет") {
		t.Errorf("возраст: %q", c.AuthorAge)
	}
	want := time.Date(2026, 7, 18, 17, 36, 34, 0, nsk)
	if !c.PublishedAt.Equal(want) {
		t.Errorf("дата: got %v, want %v", c.PublishedAt, want)
	}
	if c.Text == "" {
		t.Error("текст пуст")
	}

	// Все id уникальны и убывают (desc-порядок страницы).
	seen := map[int64]bool{}
	for i, cc := range comments {
		if seen[cc.ID] {
			t.Errorf("дубль id %d", cc.ID)
		}
		seen[cc.ID] = true
		if i > 0 && cc.ID >= comments[i-1].ID {
			t.Errorf("порядок не desc: %d после %d", cc.ID, comments[i-1].ID)
		}
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
	           <a id="anchor-5"></a>
	           <img class="avatar" src="/a.png" alt="Имя, 30 лет">
	           <a class="lv-people__nickname" href="/profile/1/">Имя</a>
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
		{"Яна, 43 года", "Яна", "43 года"},
		{"Мария, свет, радость, 34 года", "Мария, свет, радость", "34 года"},
		{"Безвозраста", "Безвозраста", ""},
	} {
		name, age := splitNameAge(tc.alt)
		if name != tc.name || age != tc.age {
			t.Errorf("splitNameAge(%q) = %q, %q; want %q, %q", tc.alt, name, age, tc.name, tc.age)
		}
	}
}

func TestDigitsOf(t *testing.T) {
	if got := digitsOf("/profile/981563/"); got != "981563" {
		t.Errorf("digitsOf: %q", got)
	}
	if got := digitsOf("/no-digits/"); got != "0" {
		t.Errorf("digitsOf без цифр должен вернуть \"0\", получено %q", got)
	}
}
