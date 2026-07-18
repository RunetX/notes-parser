package store

// Методы, обслуживающие мост «ответ в Telegram → комментарий на сайте»
// и машину состояний ЛС-бота.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NoteByThreadID находит заметку по id корня её треда в группе обсуждения.
func (s *Store) NoteByThreadID(ctx context.Context, threadID int64) (Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, author_id, author_name, text, status,
		       tg_message_id, tg_thread_id, first_seen_at, last_comment_at
		FROM notes WHERE tg_thread_id = ?`, threadID)
	if err != nil {
		return Note{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Note{}, fmt.Errorf("заметка с тредом %d: %w", threadID, ErrNotFound)
	}
	return scanNote(rows)
}

// CommentByTGMessageID находит комментарий по id его сообщения в группе.
func (s *Store) CommentByTGMessageID(ctx context.Context, tgMessageID int64) (Comment, error) {
	var c Comment
	var published, createdAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, note_id, author_name, author_age, author_link, avatar_url,
		       published_at, text, tg_message_id, created_at
		FROM comments WHERE tg_message_id = ?`, tgMessageID).
		Scan(&c.ID, &c.NoteID, &c.AuthorName, &c.AuthorAge, &c.AuthorLink,
			&c.AvatarURL, &published, &c.Text, &c.TGMessageID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Comment{}, fmt.Errorf("комментарий с tg id %d: %w", tgMessageID, ErrNotFound)
	}
	if err != nil {
		return Comment{}, err
	}
	if published.Valid {
		c.PublishedAt = parseTime(published.String)
	}
	if createdAt.Valid {
		c.CreatedAt = parseTime(createdAt.String)
	}
	return c, nil
}

// TryMarkReplyProcessed атомарно помечает ответ обработанным. false — ответ
// уже обрабатывался (защита от повторной доставки getUpdates после рестарта).
func (s *Store) TryMarkReplyProcessed(ctx context.Context, tgMessageID int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO processed_replies (tg_message_id, processed_at)
		VALUES (?, ?)`, tgMessageID, fmtTime(at))
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SessionCookies возвращает JSON кук пользователя и флаг валидности.
// ErrNotFound — пользователь ни разу не логинился.
func (s *Store) SessionCookies(ctx context.Context, tgUserID int64) (cookiesJSON string, valid bool, err error) {
	var v int
	err = s.db.QueryRowContext(ctx, `
		SELECT cookies, valid FROM sessions WHERE tg_user_id = ?`, tgUserID).
		Scan(&cookiesJSON, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("сессия пользователя %d: %w", tgUserID, ErrNotFound)
	}
	if err != nil {
		return "", false, err
	}
	return cookiesJSON, v == 1, nil
}

// SetSessionValid помечает сессию (не)валидной; фиксирует успешное
// использование в last_ok_at.
func (s *Store) SetSessionValid(ctx context.Context, tgUserID int64, valid bool, now time.Time) error {
	if valid {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET valid = 1, last_ok_at = ? WHERE tg_user_id = ?`,
			fmtTime(now), tgUserID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET valid = 0 WHERE tg_user_id = ?`, tgUserID)
	return err
}

// DialogState возвращает состояние диалога ЛС-бота ("" — нет состояния).
func (s *Store) DialogState(ctx context.Context, tgUserID int64) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `
		SELECT state FROM dialog_states WHERE tg_user_id = ?`, tgUserID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

// SetDialogState сохраняет состояние диалога (переживает рестарт бота).
func (s *Store) SetDialogState(ctx context.Context, tgUserID int64, state string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dialog_states (tg_user_id, state, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(tg_user_id) DO UPDATE SET
			state = excluded.state, updated_at = excluded.updated_at`,
		tgUserID, state, fmtTime(now))
	return err
}

// ClearDialogState сбрасывает состояние диалога.
func (s *Store) ClearDialogState(ctx context.Context, tgUserID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dialog_states WHERE tg_user_id = ?`, tgUserID)
	return err
}
