package store

// Методы, обслуживающие зеркалирование (mirror) и захват автофорварда.

import (
	"context"
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

// DeleteNote удаляет заметку со всем содержимым (иллюстрации, комментарии,
// цели сообщений). После этого демон при обходе ленты перепостит её как новую.
func (s *Store) DeleteNote(ctx context.Context, noteID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM message_targets WHERE kind = 'comment'
		   AND ref_id IN (SELECT CAST(id AS TEXT) FROM comments WHERE note_id = ?)`,
		`DELETE FROM message_targets WHERE kind = 'note_image'
		   AND ref_id IN (SELECT CAST(id AS TEXT) FROM note_images WHERE note_id = ?)`,
		`DELETE FROM message_targets WHERE kind IN ('note_post', 'note_thread') AND ref_id = ?`,
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

// Subscription — подписка на ключевое слово в конкретном мессенджере.
// ID заполняется только выборкой по пользователю (SubscriptionsByUser): по
// нему кнопка снимает подписку, не таща слово в payload.
type Subscription struct {
	ID        int64
	Messenger string
	Keyword   string
	UserID    int64
}

// Subscriptions возвращает все подписки (всех мессенджеров).
func (s *Store) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT messenger, keyword, user_id FROM subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.Messenger, &sub.Keyword, &sub.UserID); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}
