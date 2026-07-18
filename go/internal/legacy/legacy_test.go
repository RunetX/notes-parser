package legacy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

// Синтетический notes.json в точном формате poster.py: tg_message_id бывает
// и числом, и пустой строкой (начальное значение note_model).
const notesJSON = `[
 {"id": "301109", "author_id": "376712", "author_name": "Мария",
  "text": "Первая заметка\n\n@grfmn", "max_comment_id": 98770,
  "tg_message_id": 4567, "tg_discussion_id": 4568,
  "comments": [
   {"id": 98765, "author_name": "Кто-то", "author_age": "34",
    "author_link": "https://love.ngs.ru/anketa376712/",
    "avatar": "https://cdn.example.net/a.jpg",
    "date": "05.07.2026, 21:15:03", "text": "Отлично!", "tg_message_id": 999},
   {"id": 98770, "author_name": "Другой", "author_age": "25",
    "author_link": "https://love.ngs.ru/anketa981234/",
    "avatar": "https://cdn.example.net/b.jpg",
    "date": "битая дата", "text": "Согласен", "tg_message_id": ""}
  ]},
 {"id": "301110", "author_id": "0", "author_name": "Анонимно",
  "text": "Свежая заметка", "max_comment_id": 0,
  "tg_message_id": "", "tg_discussion_id": "", "comments": []}
]`

const subscribersJSON = `[
 {"key": "рюмк", "value": 1077374863},
 {"key": "", "value": 1}
]`

const sessionsJSON = `{
 "1077374863": [{"name": "sid", "value": "abc", "domain": ".ngs.ru",
                 "path": "/", "expires": 1790000000, "secure": true}],
 "не-число": [{"name": "x", "value": "y", "domain": "", "path": "/",
               "expires": 0, "secure": false}]
}`

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestImportNotesIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	now := time.Now()

	stats, err := ImportNotes(ctx, st, strings.NewReader(notesJSON), now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Notes != 2 || stats.Comments != 2 {
		t.Errorf("первый импорт: заметок %d (ждали 2), комментариев %d (ждали 2)",
			stats.Notes, stats.Comments)
	}
	// Битая дата — предупреждение, а не отказ.
	if len(stats.Warnings) != 1 {
		t.Errorf("предупреждения: %v", stats.Warnings)
	}

	// Повторный импорт ничего не добавляет.
	stats, err = ImportNotes(ctx, st, strings.NewReader(notesJSON), now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Notes != 0 || stats.Comments != 0 {
		t.Errorf("повторный импорт должен быть пустым: %+v", stats)
	}

	// Импортированные заметки получают статус posted и сохраняют tg id.
	notes, err := st.NotesByStatus(ctx, store.StatusPosted)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("posted-заметок: %d", len(notes))
	}
	if notes[0].ID != "301109" || notes[0].TGMessageID != 4567 || notes[0].TGThreadID != 4568 {
		t.Errorf("заметка 301109: %+v", notes[0])
	}
	// Пустые строки tg id превращаются в 0.
	if notes[1].TGMessageID != 0 || notes[1].TGThreadID != 0 {
		t.Errorf("заметка 301110 без tg id: %+v", notes[1])
	}

	ids, err := st.CommentIDs(ctx, "301109")
	if err != nil {
		t.Fatal(err)
	}
	if !ids[98765] || !ids[98770] {
		t.Errorf("комментарии не импортированы: %v", ids)
	}
}

func TestImportSubscribers(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	stats, err := ImportSubscribers(ctx, st, strings.NewReader(subscribersJSON))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Subscriptions != 1 {
		t.Errorf("подписок: %d (пустой ключ должен быть пропущен)", stats.Subscriptions)
	}
	if len(stats.Warnings) != 1 {
		t.Errorf("предупреждения: %v", stats.Warnings)
	}
}

func TestImportSessions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	stats, err := ImportSessions(ctx, st, strings.NewReader(sessionsJSON), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 1 {
		t.Errorf("сессий: %d (нечисловой id должен быть пропущен)", stats.Sessions)
	}
	if len(stats.Warnings) != 1 {
		t.Errorf("предупреждения: %v", stats.Warnings)
	}
}
