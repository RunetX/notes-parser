package store

// Методы, обслуживающие мост «ответ в Telegram → комментарий на сайте»
// и машину состояний ЛС-бота.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"lovegw/internal/secret"
)

// TryMarkReplyProcessed атомарно помечает ответ обработанным. false — ответ
// уже обрабатывался (защита от повторной доставки апдейтов после рестарта).
// messageID — id сообщения-ответа в мессенджере (в Telegram — число строкой).
func (s *Store) TryMarkReplyProcessed(ctx context.Context, messenger, messageID string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO processed_replies (messenger, message_id, processed_at)
		VALUES (?, ?, ?)`, messenger, messageID, fmtTime(at))
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// SessionCookies возвращает JSON кук пользователя и флаг валидности.
// ErrNotFound — пользователь ни разу не логинился. Зашифрованная запись
// расшифровывается ключом хранилища; открытые записи (сделанные до включения
// шифрования) читаются как есть.
func (s *Store) SessionCookies(ctx context.Context, messenger string, userID int64) (cookiesJSON string, valid bool, err error) {
	var stored string
	var v int
	err = s.db.QueryRowContext(ctx, `
		SELECT cookies, valid FROM sessions WHERE messenger = ? AND user_id = ?`, messenger, userID).
		Scan(&stored, &v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("сессия пользователя %s/%d: %w", messenger, userID, ErrNotFound)
	}
	if err != nil {
		return "", false, err
	}
	cookiesJSON, err = s.key.Open(secret.SessionAAD(messenger, userID), stored)
	if err != nil {
		return "", false, fmt.Errorf("сессия пользователя %s/%d: %w", messenger, userID, err)
	}
	return cookiesJSON, v == 1, nil
}

// SetSessionValid помечает сессию (не)валидной; фиксирует успешное
// использование в last_ok_at.
func (s *Store) SetSessionValid(ctx context.Context, messenger string, userID int64, valid bool, now time.Time) error {
	if valid {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sessions SET valid = 1, last_ok_at = ? WHERE messenger = ? AND user_id = ?`,
			fmtTime(now), messenger, userID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET valid = 0 WHERE messenger = ? AND user_id = ?`, messenger, userID)
	return err
}

// DialogState возвращает состояние диалога ЛС-бота ("" — нет состояния).
func (s *Store) DialogState(ctx context.Context, messenger string, userID int64) (string, error) {
	var state string
	err := s.db.QueryRowContext(ctx, `
		SELECT state FROM dialog_states WHERE messenger = ? AND user_id = ?`, messenger, userID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
}

// SetDialogState сохраняет состояние диалога (переживает рестарт бота).
func (s *Store) SetDialogState(ctx context.Context, messenger string, userID int64, state string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dialog_states (messenger, user_id, state, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(messenger, user_id) DO UPDATE SET
			state = excluded.state, updated_at = excluded.updated_at`,
		messenger, userID, state, fmtTime(now))
	return err
}

// ClearDialogState сбрасывает состояние диалога.
func (s *Store) ClearDialogState(ctx context.Context, messenger string, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dialog_states WHERE messenger = ? AND user_id = ?`, messenger, userID)
	return err
}
