package store

// Методы ASR: кэш расшифровок голосовых и суточная квота на пользователя.
// Кэш ключуется стабильным id файла (в telegram — file_unique_id): пересланное
// голосовое приходит с новым file_id, но тем же file_unique_id, поэтому за
// повтор провайдеру платить не нужно. Квота считается в секундах аудио, не в
// сообщениях, и живёт в БД — рестарт демона её не обнуляет.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Transcript возвращает кэшированную расшифровку файла. Второе значение —
// был ли кэш-хит.
func (s *Store) Transcript(ctx context.Context, messenger, fileKey string) (string, bool, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `
		SELECT text FROM asr_transcripts WHERE messenger = ? AND file_key = ?`,
		messenger, fileKey).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("чтение расшифровки %s/%s: %w", messenger, fileKey, err)
	}
	return text, true, nil
}

// SaveTranscript кладёт расшифровку в кэш (повторная запись перезаписывает).
func (s *Store) SaveTranscript(ctx context.Context, messenger, fileKey, text string, durationSec int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO asr_transcripts (messenger, file_key, text, duration_sec, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(messenger, file_key) DO UPDATE SET
			text = excluded.text, duration_sec = excluded.duration_sec`,
		messenger, fileKey, text, durationSec, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("сохранение расшифровки %s/%s: %w", messenger, fileKey, err)
	}
	return nil
}

// TryReserveASR атомарно списывает seconds из суточной квоты пользователя.
// false — квота исчерпана, распознавать нельзя. Проверка и списание идут одним
// UPDATE, поэтому конкурентные воркеры не могут вместе перебрать лимит.
func (s *Store) TryReserveASR(ctx context.Context, messenger string, userID int64, day string, seconds, limitSec int) (bool, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO asr_usage (messenger, user_id, day, seconds) VALUES (?, ?, ?, 0)`,
		messenger, userID, day); err != nil {
		return false, fmt.Errorf("квота asr %s/%d: %w", messenger, userID, err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE asr_usage SET seconds = seconds + ?
		WHERE messenger = ? AND user_id = ? AND day = ? AND seconds + ? <= ?`,
		seconds, messenger, userID, day, seconds, limitSec)
	if err != nil {
		return false, fmt.Errorf("квота asr %s/%d: %w", messenger, userID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ASRUsage — израсходованные за день секунды (диагностика и тесты).
func (s *Store) ASRUsage(ctx context.Context, messenger string, userID int64, day string) (int, error) {
	var seconds int
	err := s.db.QueryRowContext(ctx, `
		SELECT seconds FROM asr_usage WHERE messenger = ? AND user_id = ? AND day = ?`,
		messenger, userID, day).Scan(&seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("расход asr %s/%d: %w", messenger, userID, err)
	}
	return seconds, nil
}
