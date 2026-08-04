package store

// Методы message_targets — «id сообщения на пару (сущность, мессенджер)».
// Id сообщений хранятся как TEXT: Telegram отдаёт числа (пишем десятичной
// строкой), MAX — строковые mid. Пока жив v4, значения telegram дублируются
// write-through в старые tg_*-колонки (денормализованный кэш для отладочных
// команд и отката на прошлый релиз); в v5 колонки уйдут.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Мессенджеры-приёмники.
const (
	MessengerTelegram = "telegram"
	MessengerMax      = "max"
)

// Виды целей сообщений.
const (
	TargetNotePost   = "note_post"   // пост заметки в канале
	TargetNoteThread = "note_thread" // корень треда в группе/чате обсуждения
	TargetComment    = "comment"     // комментарий в треде
	TargetNoteImage  = "note_image"  // иллюстрация заметки в треде
	TargetPMMessage  = "pm_message"  // доставленное входящее ЛС talks (ref_id = talks_messages.id)
	TargetDigest     = "digest"      // выпуск дайджеста (ref_id = ISO-неделя или неделя#часть)
)

// SetTarget записывает id сообщения сущности в мессенджере. Пустые
// messageID/threadID хранятся как NULL; повторная запись дополняет строку
// (COALESCE), не затирая уже известные значения пустыми.
func (s *Store) SetTarget(ctx context.Context, messenger, kind, refID, messageID, threadID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_targets (messenger, kind, ref_id, message_id, thread_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(messenger, kind, ref_id) DO UPDATE SET
			message_id = COALESCE(excluded.message_id, message_id),
			thread_id  = COALESCE(excluded.thread_id, thread_id)`,
		messenger, kind, refID, nullStr(messageID), nullStr(threadID),
		fmtTime(time.Now())); err != nil {
		return fmt.Errorf("set target %s/%s/%s: %w", messenger, kind, refID, err)
	}
	if err := writeThroughLegacy(ctx, tx, messenger, kind, refID, messageID, threadID); err != nil {
		return err
	}
	return tx.Commit()
}

// writeThroughLegacy дублирует telegram-значения в старые tg_*-колонки.
func writeThroughLegacy(ctx context.Context, tx *sql.Tx, messenger, kind, refID, messageID, threadID string) error {
	if messenger != MessengerTelegram {
		return nil
	}
	var q string
	val := messageID
	switch kind {
	case TargetNotePost:
		q = `UPDATE notes SET tg_message_id = ? WHERE id = ?`
	case TargetNoteThread:
		q, val = `UPDATE notes SET tg_thread_id = ? WHERE id = ?`, threadID
	case TargetComment:
		q = `UPDATE comments SET tg_message_id = ? WHERE id = ?`
	case TargetNoteImage:
		q = `UPDATE note_images SET tg_message_id = ? WHERE id = ?`
	default:
		return nil
	}
	if val == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, q, val, refID); err != nil {
		return fmt.Errorf("write-through %s/%s: %w", kind, refID, err)
	}
	return nil
}

// Target возвращает записанные id сообщения сущности в мессенджере.
// found=false — сущность в этот мессенджер ещё не отправлялась.
func (s *Store) Target(ctx context.Context, messenger, kind, refID string) (messageID, threadID string, found bool, err error) {
	var msg, thread sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT message_id, thread_id FROM message_targets
		WHERE messenger = ? AND kind = ? AND ref_id = ?`,
		messenger, kind, refID).Scan(&msg, &thread)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return msg.String, thread.String, true, nil
}

// CaptureNoteThread связывает пост заметки с корнем её треда обсуждения:
// находит заметку по id поста и записывает note_thread, если он ещё не
// пойман. ok=false — пост не наш или тред уже записан.
func (s *Store) CaptureNoteThread(ctx context.Context, messenger, postMessageID, threadID string) (noteID string, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRowContext(ctx, `
		SELECT ref_id FROM message_targets
		WHERE messenger = ? AND kind = ? AND message_id = ?`,
		messenger, TargetNotePost, postMessageID).Scan(&noteID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO message_targets (messenger, kind, ref_id, thread_id, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		messenger, TargetNoteThread, noteID, threadID, fmtTime(time.Now()))
	if err != nil {
		return "", false, fmt.Errorf("capture thread %s/%s: %w", messenger, noteID, err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return "", false, nil // тред уже пойман
	}
	if err := writeThroughLegacy(ctx, tx, messenger, TargetNoteThread, noteID, "", threadID); err != nil {
		return "", false, err
	}
	return noteID, true, tx.Commit()
}

// NoteByThread находит заметку по корню её треда в мессенджере.
func (s *Store) NoteByThread(ctx context.Context, messenger, threadID string) (Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.author_id, n.author_name, n.text, n.author_avatar_url, n.status,
		       n.tg_message_id, n.tg_thread_id, n.first_seen_at, n.last_comment_at, n.comments_closed
		FROM notes n
		JOIN message_targets t ON t.ref_id = n.id
		WHERE t.messenger = ? AND t.kind = ? AND t.thread_id = ?`,
		messenger, TargetNoteThread, threadID)
	if err != nil {
		return Note{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Note{}, fmt.Errorf("заметка с тредом %s/%s: %w", messenger, threadID, ErrNotFound)
	}
	return scanNote(rows)
}

// CommentByTarget находит комментарий по id его сообщения в мессенджере.
func (s *Store) CommentByTarget(ctx context.Context, messenger, messageID string) (Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.note_id, c.author_name, c.author_age, c.author_link, c.avatar_url,
		       c.published_at, c.text, c.tg_message_id, c.created_at
		FROM comments c
		JOIN message_targets t ON t.ref_id = CAST(c.id AS TEXT)
		WHERE t.messenger = ? AND t.kind = ? AND t.message_id = ?`,
		messenger, TargetComment, messageID)
	if err != nil {
		return Comment{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Comment{}, fmt.Errorf("комментарий с id %s/%s: %w", messenger, messageID, ErrNotFound)
	}
	return scanComment(rows)
}

// AddresseeMessage ищет сообщение того комментария, которому адресована
// реплика: последний по времени комментарий заметки до beforeID, чей автор
// зовётся nick (ник уже в нижнем регистре — love.AddressPrefix). Пустая строка
// — адресат не найден; звать с ним PostComment безопасно, комментарий уйдёт
// реплаем на корень треда, как было до слоя адресатов.
//
// Регистр сводится в Go, а не в SQL: встроенный lower() в SQLite приводит
// только ASCII, а ники сплошь кириллические. Кандидатов даёт джойн с
// message_targets — комментарий без сообщения в этом мессенджере (запощен до
// его включения) адресатом всё равно быть не может.
func (s *Store) AddresseeMessage(ctx context.Context, messenger, noteID string,
	beforeID int64, nick string) (string, error) {
	if nick == "" {
		return "", nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.author_name, t.message_id
		FROM comments c
		JOIN message_targets t
			ON t.messenger = ? AND t.kind = ? AND t.ref_id = CAST(c.id AS TEXT)
		WHERE c.note_id = ? AND c.id < ? AND t.message_id IS NOT NULL
		ORDER BY c.id DESC`, messenger, TargetComment, noteID, beforeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var name, msgID string
		if err := rows.Scan(&name, &msgID); err != nil {
			return "", err
		}
		if strings.ToLower(name) == nick {
			return msgID, nil
		}
	}
	return "", rows.Err()
}

// UnsentCommentsFor возвращает комментарии заметки, ещё не отправленные в
// мессенджер (включая застрявшие: тред мог быть не пойман в прошлый цикл).
func (s *Store) UnsentCommentsFor(ctx context.Context, messenger, noteID string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.note_id, c.author_name, c.author_age, c.author_link, c.avatar_url,
		       c.published_at, c.text, c.tg_message_id, c.created_at
		FROM comments c
		LEFT JOIN message_targets t
			ON t.messenger = ? AND t.kind = ? AND t.ref_id = CAST(c.id AS TEXT)
		WHERE c.note_id = ? AND t.id IS NULL
		ORDER BY c.id`, messenger, TargetComment, noteID)
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

// UnsentNoteImagesFor возвращает иллюстрации заметки, ещё не отправленные
// в мессенджер.
func (s *Store) UnsentNoteImagesFor(ctx context.Context, messenger, noteID string) ([]NoteImage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.note_id, i.position, i.url, i.tg_message_id
		FROM note_images i
		LEFT JOIN message_targets t
			ON t.messenger = ? AND t.kind = ? AND t.ref_id = CAST(i.id AS TEXT)
		WHERE i.note_id = ? AND t.id IS NULL
		ORDER BY i.position, i.id`, messenger, TargetNoteImage, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var imgs []NoteImage
	for rows.Next() {
		var img NoteImage
		var tgMsg sql.NullInt64
		if err := rows.Scan(&img.ID, &img.NoteID, &img.Position, &img.URL, &tgMsg); err != nil {
			return nil, err
		}
		img.TGMessageID = tgMsg.Int64
		imgs = append(imgs, img)
	}
	return imgs, rows.Err()
}

// SetNoteStatusPosted помечает заметку запощенной (id постов — в
// message_targets, по одному на мессенджер).
func (s *Store) SetNoteStatusPosted(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE notes SET status = ? WHERE id = ?`, StatusPosted, id)
	return err
}

// nullStr: пустая строка хранится как NULL.
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}
