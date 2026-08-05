package modwatch

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lovegw/internal/love"
)

// fakeSite — подставной сайт: лента и страницы комментариев задаются тестом.
type fakeSite struct {
	feed    []love.Note
	pages   map[string][][]love.Comment
	headers map[string]*love.Note
}

func (f *fakeSite) Feed(context.Context) ([]love.Note, error) { return f.feed, nil }

func (f *fakeSite) Thread(_ context.Context, noteID string, page int) ([]love.Comment, *love.Note, error) {
	pages := f.pages[noteID]
	if page > len(pages) {
		return nil, nil, errors.New("нет такой страницы")
	}
	return pages[page-1], f.headers[noteID], nil
}

func feedNote(id string, images int, closed bool) love.Note {
	n := love.Note{ID: id, AuthorID: "500" + id, AuthorName: "автор" + id, Text: "текст " + id, CommentsClosed: closed}
	for i := 0; i < images; i++ {
		n.Images = append(n.Images, "https://example/"+id+"/"+strconv.Itoa(i)+".jpg")
	}
	return n
}

func newTestWatcher(t *testing.T, site Site) (*Watcher, *Store, *time.Time) {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "modwatch.db"))
	if err != nil {
		t.Fatalf("открытие БД: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	w := &Watcher{
		Site:  site,
		Store: store,
		Now:   func() time.Time { return now },
	}
	return w, store, &now
}

func kindsOf(t *testing.T, s *Store, kind string) []Event {
	t.Helper()
	ev, err := s.Events(context.Background(), time.Time{}, time.Time{}, []string{kind}, 0)
	if err != nil {
		t.Fatalf("чтение событий: %v", err)
	}
	return ev
}

// Уход заметки за нижний край ленты — не удаление, а исчезновение внутри
// охвата (при живых заметках постарше) — удаление.
func TestFeedDeletionVsPagination(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{}
	w, store, now := newTestWatcher(t, site)

	site.feed = []love.Note{feedNote("102", 0, false), feedNote("101", 0, false), feedNote("100", 0, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("первый опрос: %v", err)
	}
	if got := len(kindsOf(t, store, KindNotePublished)); got != 3 {
		t.Fatalf("новых заметок: %d, ожидалось 3", got)
	}

	// 100 ушла за край страницы: старее неё в выдаче ничего нет — не удаление.
	*now = now.Add(2 * time.Minute)
	site.feed = []love.Note{feedNote("102", 0, false), feedNote("101", 0, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("второй опрос: %v", err)
	}
	if got := len(kindsOf(t, store, KindNoteGone)); got != 0 {
		t.Fatalf("пагинация принята за удаление: %d событий", got)
	}

	// 101 пропала, а более старая 100 на месте — значит удалена.
	*now = now.Add(2 * time.Minute)
	site.feed = []love.Note{feedNote("102", 0, false), feedNote("100", 0, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("третий опрос: %v", err)
	}
	gone := kindsOf(t, store, KindNoteGone)
	if len(gone) != 1 || gone[0].RefID != 101 {
		t.Fatalf("ожидалось удаление 101, получено %+v", gone)
	}
	if !gone[0].DetectedAt.After(gone[0].PrevSeen) {
		t.Fatalf("окно события пустое: %v … %v", gone[0].PrevSeen, gone[0].DetectedAt)
	}
}

// Картинка у заметки и закрытие комментариев — действия модератора.
func TestHeaderEvents(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{}
	w, store, now := newTestWatcher(t, site)

	site.feed = []love.Note{feedNote("200", 0, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос: %v", err)
	}
	*now = now.Add(2 * time.Minute)
	site.feed = []love.Note{feedNote("200", 1, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос: %v", err)
	}
	if got := kindsOf(t, store, KindImageAdded); len(got) != 1 || got[0].RefID != 200 {
		t.Fatalf("иллюстрация не поймана: %+v", got)
	}
	*now = now.Add(2 * time.Minute)
	site.feed = []love.Note{feedNote("200", 1, true)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос: %v", err)
	}
	if got := kindsOf(t, store, KindCommentsClosed); len(got) != 1 {
		t.Fatalf("закрытие комментариев не поймано: %+v", got)
	}
	// Повторный опрос с тем же состоянием не должен плодить события.
	*now = now.Add(2 * time.Minute)
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос: %v", err)
	}
	if got := kindsOf(t, store, KindImageAdded); len(got) != 1 {
		t.Fatalf("событие продублировалось: %+v", got)
	}
}

func comment(id int64, author string, at time.Time) love.Comment {
	return love.Comment{
		ID: id, AuthorID: author, AuthorName: "u" + author,
		Text: "реплика " + strconv.FormatInt(id, 10), PublishedAt: at,
	}
}

// Комментарий, ушедший за нижний край охвата, — не удаление; исчезнувший
// внутри охвата — удаление.
func TestCommentDeletionVsCoverage(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{pages: map[string][][]love.Comment{}, headers: map[string]*love.Note{}}
	w, store, now := newTestWatcher(t, site)
	w.ThreadInterval = time.Minute

	base := *now
	site.feed = []love.Note{feedNote("300", 0, false)}
	site.pages["300"] = [][]love.Comment{{
		comment(5, "11", base.Add(-3*time.Minute)),
		comment(4, "12", base.Add(-4*time.Minute)),
		comment(3, "13", base.Add(-5*time.Minute)),
	}}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("первый опрос: %v", err)
	}

	// Комментарий 3 вытеснен свежими — он ниже охвата, это не удаление.
	*now = now.Add(2 * time.Minute)
	site.pages["300"] = [][]love.Comment{{
		comment(6, "11", base.Add(time.Minute)),
		comment(5, "11", base.Add(-3*time.Minute)),
		comment(4, "12", base.Add(-4*time.Minute)),
	}}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("второй опрос: %v", err)
	}
	if got := len(kindsOf(t, store, KindCommentGone)); got != 0 {
		t.Fatalf("выход за охват принят за удаление: %d", got)
	}

	// Комментарий 5 пропал при живом более старом 4 — удалён.
	*now = now.Add(2 * time.Minute)
	site.pages["300"] = [][]love.Comment{{
		comment(6, "11", base.Add(time.Minute)),
		comment(4, "12", base.Add(-4*time.Minute)),
	}}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("третий опрос: %v", err)
	}
	gone := kindsOf(t, store, KindCommentGone)
	if len(gone) != 1 || gone[0].RefID != 5 || gone[0].NoteID != 300 {
		t.Fatalf("ожидалось удаление комментария 5 в заметке 300, получено %+v", gone)
	}
}

// Пустая лента (сбой разбора) не должна выглядеть как массовое удаление.
func TestEmptyFeedIsNotMassDeletion(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{}
	w, store, now := newTestWatcher(t, site)
	site.feed = []love.Note{feedNote("400", 0, false), feedNote("399", 0, false)}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос: %v", err)
	}
	*now = now.Add(2 * time.Minute)
	site.feed = nil
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("опрос на пустой ленте: %v", err)
	}
	if got := len(kindsOf(t, store, KindNoteGone)); got != 0 {
		t.Fatalf("пустая лента дала %d удалений", got)
	}
}
