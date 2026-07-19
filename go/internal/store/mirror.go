package store

// Методы, обслуживающие зеркалирование (mirror) и захват автофорварда.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("запись не найдена")

// NoteByID возвращает заметку по id сайта.
func (s *Store) NoteByID(ctx context.Context, id string) (Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, author_id, author_name, text, author_avatar_url, status,
		       tg_message_id, tg_thread_id, first_seen_at, last_comment_at, comments_closed
		FROM notes WHERE id = ?`, id)
	if err != nil {
		return Note{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Note{}, fmt.Errorf("заметка %s: %w", id, ErrNotFound)
	}
	return scanNote(rows)
}

// SetNotePosted помечает заметку запощенной и сохраняет id поста в канале.
func (s *Store) SetNotePosted(ctx context.Context, id string, tgMessageID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notes SET status = ?, tg_message_id = ? WHERE id = ?`,
		StatusPosted, tgMessageID, id)
	return err
}

// SetNoteThreadIDByMessageID записывает id корня треда (автофорварда),
// найдя заметку по id её поста в канале. Возвращает id заметки.
func (s *Store) SetNoteThreadIDByMessageID(ctx context.Context, tgMessageID, threadID int64) (string, bool, error) {
	var noteID string
	err := s.db.QueryRowContext(ctx, `
		UPDATE notes SET tg_thread_id = ?
		WHERE tg_message_id = ? AND tg_thread_id IS NULL
		RETURNING id`, threadID, tgMessageID).Scan(&noteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return noteID, true, nil
}

// SetNoteArchived переводит заметку в архив: воркер комментариев завершается.
func (s *Store) SetNoteArchived(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notes SET status = ?, archived_at = ? WHERE id = ?`,
		StatusArchived, fmtTime(at), id)
	return err
}

// MarkNoteCommentsClosed помечает заметку закрытой для новых комментариев
// (сайт пометил её «не актуальна» в ленте). Возвращает true при первом
// переходе — чтобы залогировать событие один раз, а не каждый обход ленты.
func (s *Store) MarkNoteCommentsClosed(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE notes SET comments_closed = 1
		WHERE id = ? AND comments_closed = 0`, id)
	if err != nil {
		return false, fmt.Errorf("mark comments closed %s: %w", id, err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SetNoteLastCommentAt обновляет время последнего комментария (для
// адаптивного интервала опроса).
func (s *Store) SetNoteLastCommentAt(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notes SET last_comment_at = ? WHERE id = ?`, fmtTime(at), id)
	return err
}

// SetCommentTGMessageID сохраняет id сообщения комментария в группе.
func (s *Store) SetCommentTGMessageID(ctx context.Context, commentID, tgMessageID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE comments SET tg_message_id = ? WHERE id = ?`, tgMessageID, commentID)
	return err
}

// UnsentComments возвращает комментарии заметки, ещё не отправленные в
// Telegram (включая застрявшие: тред мог быть не пойман в прошлый цикл).
func (s *Store) UnsentComments(ctx context.Context, noteID string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, note_id, author_name, author_age, author_link, avatar_url,
		       published_at, text, tg_message_id, created_at
		FROM comments
		WHERE note_id = ? AND tg_message_id IS NULL
		ORDER BY id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []Comment
	for rows.Next() {
		var c Comment
		var published, createdAt sql.NullString
		var tgMsg sql.NullInt64
		if err := rows.Scan(&c.ID, &c.NoteID, &c.AuthorName, &c.AuthorAge,
			&c.AuthorLink, &c.AvatarURL, &published, &c.Text, &tgMsg, &createdAt); err != nil {
			return nil, err
		}
		if published.Valid {
			c.PublishedAt = parseTime(published.String)
		}
		if createdAt.Valid {
			c.CreatedAt = parseTime(createdAt.String)
		}
		c.TGMessageID = tgMsg.Int64
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// SentCommentTGMessageIDs — id отправленных в Telegram сообщений комментариев
// заметки (для удаления при перепосте).
func (s *Store) SentCommentTGMessageIDs(ctx context.Context, noteID string) ([]int64, error) {
	return s.collectIDs(ctx,
		`SELECT tg_message_id FROM comments WHERE note_id = ? AND tg_message_id IS NOT NULL`, noteID)
}

// SentNoteImageTGMessageIDs — id отправленных в Telegram иллюстраций заметки.
func (s *Store) SentNoteImageTGMessageIDs(ctx context.Context, noteID string) ([]int64, error) {
	return s.collectIDs(ctx,
		`SELECT tg_message_id FROM note_images WHERE note_id = ? AND tg_message_id IS NOT NULL`, noteID)
}

func (s *Store) collectIDs(ctx context.Context, query, noteID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteNote удаляет заметку со всем содержимым (иллюстрации, комментарии).
// После этого демон при обходе ленты перепостит её как новую.
func (s *Store) DeleteNote(ctx context.Context, noteID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM note_images WHERE note_id = ?`,
		`DELETE FROM comments WHERE note_id = ?`,
		`DELETE FROM notes WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, noteID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Subscription — подписка на ключевое слово.
type Subscription struct {
	Keyword  string
	TGUserID int64
}

// Subscriptions возвращает все подписки.
func (s *Store) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT keyword, tg_user_id FROM subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Keyword, &sub.TGUserID); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
