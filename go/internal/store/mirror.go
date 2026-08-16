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
		if err := rows.Err(); err != nil {
			return Note{}, err
		}
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

// Виды подписок. Цель (Target) у каждого своя, отсюда и раздельные ветки
// срабатывания в зеркале.
const (
	SubKeyword      = "keyword"       // слово в тексте нового комментария
	SubAuthorNotes  = "author_notes"  // новые заметки автора (target — notes.author_id)
	SubNoteComments = "note_comments" // новые комментарии заметки (target — notes.id)
)

// SubscriptionLimit — потолок числа подписок на пользователя, все виды вместе.
const SubscriptionLimit = 50

// ErrSubscriptionLimit — подписка не заведена: у пользователя уже предел.
var ErrSubscriptionLimit = errors.New("превышен предел числа подписок")

// Subscription — подписка пользователя в конкретном мессенджере.
type Subscription struct {
	ID        int64
	Messenger string
	UserID    int64
	Kind      string // Sub*
	Target    string // слово / author_id / note_id
	// Label — человеческая подпись цели: само слово, имя автора, «автор: текст
	// заметки». Заполняет только SubscriptionsByUser (одним запросом, без
	// N+1); пусто — цель в notes не нашлась (заметка удалена, автор не виден).
	Label string
}

// Subscriptions возвращает все подписки (всех мессенджеров и видов). ID
// выбирается всегда: он нужен кнопке «Отписаться» в самом уведомлении.
func (s *Store) Subscriptions(ctx context.Context) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, messenger, user_id, kind, target FROM subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.Messenger, &sub.UserID, &sub.Kind, &sub.Target); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// SubscribersByTarget — подписчики конкретной цели в мессенджере. Новая заметка
// автора бьёт точечно по индексу, а не вычиткой всей таблицы: заметок в день
// единицы, а подписок со временем будут тысячи.
func (s *Store) SubscribersByTarget(ctx context.Context, messenger, kind, target string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id FROM subscriptions
		WHERE messenger = ? AND kind = ? AND target = ?
		ORDER BY user_id`, messenger, kind, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		sub := Subscription{Messenger: messenger, Kind: kind, Target: target}
		if err := rows.Scan(&sub.ID, &sub.UserID); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// RemoveNoteSubscriptions снимает подписки на комментарии заметки во всех
// мессенджерах: заметка ушла в архив, новых комментариев по ней не будет, а
// строка вечно ела бы предел и висела бы в /mysubs обманкой.
func (s *Store) RemoveNoteSubscriptions(ctx context.Context, noteID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM subscriptions WHERE kind = ? AND target = ?`, SubNoteComments, noteID)
	if err != nil {
		return 0, fmt.Errorf("снятие подписок на заметку %s: %w", noteID, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
