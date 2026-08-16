package store

// Списки колонок доменных таблиц и общие хелперы выборок. Живут в одном месте
// намеренно: список колонок должен совпадать с порядком чтения в scanNote /
// scanComment, а раньше он был выписан в четырёх запросах — добавление
// колонки требовало правки всех и молча ломало scan при пропуске одного.

import (
	"context"
	"database/sql"
	"strings"
)

// noteColumns / commentColumns — порядок обязан совпадать со scanNote и
// scanComment. Префикс нужен запросам с JOIN (`c.` у comments).
const noteColumns = `id, author_id, author_name, text, author_avatar_url, status,
       tg_message_id, tg_thread_id, first_seen_at, last_comment_at, comments_closed`

const commentColumns = `id, note_id, author_name, author_age, author_link, avatar_url,
       published_at, text, tg_message_id, created_at`

// prefixed выдаёт список колонок с префиксом таблицы: `c.id, c.note_id, …`.
func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// placeholders — «?, ?, …» для IN-списка. n <= 0 даёт пустую строку: такой
// запрос звать нельзя, и вызывающий обязан отсечь пустой список раньше.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return "?" + strings.Repeat(", ?", n-1)
}

// collectNotes / collectComments / collectIDs — типовые циклы разбора выборки.
func collectNotes(rows *sql.Rows) ([]Note, error) {
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

func collectComments(rows *sql.Rows) ([]Comment, error) {
	var comments []Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// queryIDs — выборка одной скалярной колонки в множество.
func (s *Store) queryIDs(ctx context.Context, query string, args ...any) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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

// queryInt64Set — то же для числовых id.
func (s *Store) queryInt64Set(ctx context.Context, query string, args ...any) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
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
