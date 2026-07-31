package store

// Выборки для еженедельного дайджеста. Только чтение: окно недели по
// заметкам/комментариям и сравнительная история за всё время наблюдения.
// Окно везде (start, end]: start исключительно, end (слот выпуска)
// включительно. Время комментария — COALESCE(published_at, created_at):
// published_at (время сайта) может быть NULL, created_at пишется всегда.
// Строки RFC3339-UTC сравниваются лексикографически корректно.

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const noteColumns = `id, author_id, author_name, text, author_avatar_url, status,
       tg_message_id, tg_thread_id, first_seen_at, last_comment_at, comments_closed`

// CommentsBetween возвращает комментарии окна (start, end] по времени сайта
// с фолбэком на время вставки, сгруппированно по заметкам.
func (s *Store) CommentsBetween(ctx context.Context, start, end time.Time) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, note_id, author_name, author_age, author_link, avatar_url,
		       published_at, text, tg_message_id, created_at
		FROM comments
		WHERE COALESCE(published_at, created_at) > ?
		  AND COALESCE(published_at, created_at) <= ?
		ORDER BY note_id, id`, fmtTime(start), fmtTime(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// NotesSeenBetween возвращает заметки, впервые увиденные в окне (start, end].
// Seeded исключены: они существовали до перехода и нами не публиковались.
func (s *Store) NotesSeenBetween(ctx context.Context, start, end time.Time) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+noteColumns+`
		FROM notes
		WHERE first_seen_at > ? AND first_seen_at <= ? AND status != ?
		ORDER BY first_seen_at`, fmtTime(start), fmtTime(end), StatusSeeded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectNotes(rows)
}

// NotesByIDs возвращает шапки заметок по списку id (комментарии окна могут
// жить и на заметках старше окна).
func (s *Store) NotesByIDs(ctx context.Context, ids []string) (map[string]Note, error) {
	notes := make(map[string]Note, len(ids))
	if len(ids) == 0 {
		return notes, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+noteColumns+`
		FROM notes WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes[n.ID] = n
	}
	return notes, rows.Err()
}

// ActiveNotesSince возвращает отслеживаемые заметки с комментарием после
// since — «обсуждение ещё живо» к моменту выпуска.
func (s *Store) ActiveNotesSince(ctx context.Context, since time.Time) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+noteColumns+`
		FROM notes
		WHERE status = ? AND last_comment_at > ?
		ORDER BY last_comment_at DESC`, StatusPosted, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectNotes(rows)
}

// CommenterSeen — активность комментатора в окне и его прошлое до окна.
// Идентичность — author_link (URL анкеты): числового id у комментариев нет.
type CommenterSeen struct {
	Link          string
	Name          string
	InWindow      int       // комментариев в окне
	FirstInWindow time.Time // первый комментарий окна
	PrevSeenAt    time.Time // последний комментарий до окна; zero — новичок
}

// CommenterHistory возвращает комментаторов окна (start, end] с их прошлым.
// Комментарии с пустым author_link (без анкеты) не учитываются.
func (s *Store) CommenterHistory(ctx context.Context, start, end time.Time) ([]CommenterSeen, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH win AS (
			SELECT author_link AS link, MAX(author_name) AS name, COUNT(*) AS cnt,
			       MIN(COALESCE(published_at, created_at)) AS first_in_win
			FROM comments
			WHERE author_link != ''
			  AND COALESCE(published_at, created_at) > ?1
			  AND COALESCE(published_at, created_at) <= ?2
			GROUP BY author_link)
		SELECT w.link, w.name, w.cnt, w.first_in_win,
		       (SELECT MAX(COALESCE(published_at, created_at)) FROM comments c
		        WHERE c.author_link = w.link
		          AND COALESCE(published_at, created_at) <= ?1)
		FROM win w
		ORDER BY w.cnt DESC, w.link`, fmtTime(start), fmtTime(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seen []CommenterSeen
	for rows.Next() {
		var cs CommenterSeen
		var firstInWin string
		var prev sql.NullString
		if err := rows.Scan(&cs.Link, &cs.Name, &cs.InWindow, &firstInWin, &prev); err != nil {
			return nil, err
		}
		cs.FirstInWindow = parseTime(firstInWin)
		if prev.Valid {
			cs.PrevSeenAt = parseTime(prev.String)
		}
		seen = append(seen, cs)
	}
	return seen, rows.Err()
}

// AuthorSeen — активность автора заметок в окне и его прошлое до окна.
type AuthorSeen struct {
	AuthorID      string
	Name          string
	NotesInWindow int
	PrevNoteAt    time.Time // последняя заметка до окна; zero — раньше не писал
}

// NoteAuthorHistory возвращает авторов заметок окна (start, end] с их прошлым.
// Анонимы (author_id = '0') не учитываются. Прошлое считается по всем
// заметкам, включая seeded: до окна автор на сайте уже появлялся.
func (s *Store) NoteAuthorHistory(ctx context.Context, start, end time.Time) ([]AuthorSeen, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH win AS (
			SELECT author_id, MAX(author_name) AS name, COUNT(*) AS cnt
			FROM notes
			WHERE author_id != '0'
			  AND first_seen_at > ?1 AND first_seen_at <= ?2
			  AND status != ?3
			GROUP BY author_id)
		SELECT w.author_id, w.name, w.cnt,
		       (SELECT MAX(first_seen_at) FROM notes n
		        WHERE n.author_id = w.author_id AND n.first_seen_at <= ?1)
		FROM win w
		ORDER BY w.cnt DESC, w.author_id`, fmtTime(start), fmtTime(end), StatusSeeded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var seen []AuthorSeen
	for rows.Next() {
		var as AuthorSeen
		var prev sql.NullString
		if err := rows.Scan(&as.AuthorID, &as.Name, &as.NotesInWindow, &prev); err != nil {
			return nil, err
		}
		if prev.Valid {
			as.PrevNoteAt = parseTime(prev.String)
		}
		seen = append(seen, as)
	}
	return seen, rows.Err()
}

// NoteTotals — итоги обсуждения заметки за всю историю наблюдения
// (для сравнительных рекордов: «самый длинный тред с апреля»).
type NoteTotals struct {
	NoteID      string
	FirstSeenAt time.Time
	Comments    int
	Commenters  int // уникальных author_link; безанкетные не считаются
	FirstAt     time.Time
	LastAt      time.Time
}

// NoteCommentTotals возвращает итоги всех обсуждавшихся заметок,
// в порядке появления заметок.
func (s *Store) NoteCommentTotals(ctx context.Context) ([]NoteTotals, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.note_id, n.first_seen_at, COUNT(*),
		       COUNT(DISTINCT CASE WHEN c.author_link != '' THEN c.author_link END),
		       MIN(COALESCE(c.published_at, c.created_at)),
		       MAX(COALESCE(c.published_at, c.created_at))
		FROM comments c JOIN notes n ON n.id = c.note_id
		GROUP BY c.note_id
		ORDER BY n.first_seen_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var totals []NoteTotals
	for rows.Next() {
		var t NoteTotals
		var firstSeen, firstAt, lastAt string
		if err := rows.Scan(&t.NoteID, &firstSeen, &t.Comments, &t.Commenters,
			&firstAt, &lastAt); err != nil {
			return nil, err
		}
		t.FirstSeenAt = parseTime(firstSeen)
		t.FirstAt = parseTime(firstAt)
		t.LastAt = parseTime(lastAt)
		totals = append(totals, t)
	}
	return totals, rows.Err()
}

// PeakCommentHour возвращает самый плотный календарный час одного треда за
// всю историю: начало часа (UTC), заметку и число комментариев.
// n = 0 — комментариев в базе нет.
func (s *Store) PeakCommentHour(ctx context.Context) (hourStart time.Time, noteID string, n int, err error) {
	// Час — префикс "2006-01-02T15" строки RFC3339-UTC.
	row := s.db.QueryRowContext(ctx, `
		SELECT note_id, substr(COALESCE(published_at, created_at), 1, 13) AS h, COUNT(*) AS n
		FROM comments
		GROUP BY note_id, h
		ORDER BY n DESC, h DESC
		LIMIT 1`)
	var hour string
	if err = row.Scan(&noteID, &hour, &n); err != nil {
		if err == sql.ErrNoRows {
			return time.Time{}, "", 0, nil
		}
		return time.Time{}, "", 0, err
	}
	return parseTime(hour + ":00:00Z"), noteID, n, nil
}

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
