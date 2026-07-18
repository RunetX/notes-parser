// Пакет store — SQLite-хранилище lovegw, единственный источник правды.
// Все записи сквозные: состояние переживает kill -9.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Статусы заметки.
const (
	StatusPending  = "pending"  // увидели, ещё не запостили в канал
	StatusPosted   = "posted"   // запощена, отслеживаем комментарии
	StatusSeeded   = "seeded"   // была на сайте до перехода на Go — не постим
	StatusArchived = "archived" // комментарии больше не отслеживаем
)

type Note struct {
	ID            string
	AuthorID      string
	AuthorName    string
	Text          string
	Status        string
	TGMessageID   int64 // 0 — не запощена
	TGThreadID    int64 // 0 — автофорвард ещё не пойман
	FirstSeenAt   time.Time
	LastCommentAt time.Time // zero — комментариев не было
}

type Comment struct {
	ID          int64
	NoteID      string
	AuthorName  string
	AuthorAge   string
	AuthorLink  string
	AvatarURL   string
	PublishedAt time.Time // zero — дата неизвестна
	Text        string
	TGMessageID int64
	CreatedAt   time.Time
}

type Store struct {
	db *sql.DB
}

// Open открывает (при необходимости создавая) базу и накатывает миграции.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие БД %s: %w", path, err)
	}
	// Одно соединение снимает вопросы конкурентной записи при нашей нагрузке.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// InsertNote добавляет заметку, если её ещё нет. Возвращает true, если строка
// действительно вставлена (для идемпотентного импорта и seed).
func (s *Store) InsertNote(ctx context.Context, n Note) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO notes
			(id, author_id, author_name, text, status,
			 tg_message_id, tg_thread_id, first_seen_at, last_comment_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.AuthorID, n.AuthorName, n.Text, n.Status,
		nullID(n.TGMessageID), nullID(n.TGThreadID),
		fmtTime(n.FirstSeenAt), nullTime(n.LastCommentAt))
	if err != nil {
		return false, fmt.Errorf("insert note %s: %w", n.ID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// InsertComment добавляет комментарий, если его ещё нет.
func (s *Store) InsertComment(ctx context.Context, c Comment) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO comments
			(id, note_id, author_name, author_age, author_link, avatar_url,
			 published_at, text, tg_message_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.NoteID, c.AuthorName, c.AuthorAge, c.AuthorLink, c.AvatarURL,
		nullTime(c.PublishedAt), c.Text, nullID(c.TGMessageID), fmtTime(c.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("insert comment %d: %w", c.ID, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// UpsertSession сохраняет куки пользователя (JSON), помечая сессию валидной.
func (s *Store) UpsertSession(ctx context.Context, tgUserID int64, cookiesJSON string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (tg_user_id, cookies, valid, updated_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(tg_user_id) DO UPDATE SET
			cookies = excluded.cookies, valid = 1, updated_at = excluded.updated_at`,
		tgUserID, cookiesJSON, fmtTime(now))
	if err != nil {
		return fmt.Errorf("upsert session %d: %w", tgUserID, err)
	}
	return nil
}

// AddSubscription добавляет подписку на ключевое слово.
func (s *Store) AddSubscription(ctx context.Context, keyword string, tgUserID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO subscriptions (keyword, tg_user_id) VALUES (?, ?)`,
		keyword, tgUserID)
	if err != nil {
		return false, fmt.Errorf("insert subscription: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// KnownNoteIDs возвращает id всех известных заметок (для фильтра ленты).
func (s *Store) KnownNoteIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// NotesByStatus возвращает заметки в заданном статусе.
func (s *Store) NotesByStatus(ctx context.Context, status string) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, author_id, author_name, text, status,
		       tg_message_id, tg_thread_id, first_seen_at, last_comment_at
		FROM notes WHERE status = ? ORDER BY id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// CommentIDs возвращает id всех сохранённых комментариев заметки.
func (s *Store) CommentIDs(ctx context.Context, noteID string) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM comments WHERE note_id = ?`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

func scanNote(rows *sql.Rows) (Note, error) {
	var n Note
	var tgMsg, tgThread sql.NullInt64
	var firstSeen string
	var lastComment sql.NullString
	if err := rows.Scan(&n.ID, &n.AuthorID, &n.AuthorName, &n.Text, &n.Status,
		&tgMsg, &tgThread, &firstSeen, &lastComment); err != nil {
		return n, err
	}
	n.TGMessageID = tgMsg.Int64
	n.TGThreadID = tgThread.Int64
	n.FirstSeenAt = parseTime(firstSeen)
	if lastComment.Valid {
		n.LastCommentAt = parseTime(lastComment.String)
	}
	return n, nil
}

// nullID: 0 хранится как NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTime(t)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return time.Time{}.UTC().Format(time.RFC3339)
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
