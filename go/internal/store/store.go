// Пакет store — SQLite-хранилище lovegw, единственный источник правды.
// Все записи сквозные: состояние переживает kill -9.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lovegw/internal/secret"

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
	ID              string
	AuthorID        string
	AuthorName      string
	Text            string
	AuthorAvatarURL string // аватар автора; пусто/плейсхолдер — не показываем
	Status          string
	TGMessageID     int64 // 0 — не запощена
	TGThreadID      int64 // 0 — автофорвард ещё не пойман
	FirstSeenAt     time.Time
	LastCommentAt   time.Time // zero — комментариев не было
	CommentsClosed  bool      // сайт пометил заметку «не актуальна»: комментарии закрыты
}

// NoteImage — иллюстрация заметки, публикуемая первым сообщением в треде.
type NoteImage struct {
	ID          int64
	NoteID      string
	Position    int
	URL         string
	TGMessageID int64 // 0 — ещё не отправлена в тред
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
	// key шифрует куки сессий на диске. Нулевой ключ = шифрование выключено,
	// и тогда всё работает как раньше (см. пакет secret).
	key secret.Key
}

// Option — необязательная настройка хранилища.
type Option func(*Store)

// WithSecret включает шифрование сессионных кук. Ключ задан — новые записи
// ложатся шифротекстом, а старые открытые читаются по-прежнему.
func WithSecret(k secret.Key) Option {
	return func(s *Store) { s.key = k }
}

// Open открывает (при необходимости создавая) базу и накатывает миграции.
// Каталог заводится под 0700: в базе лежат сессионные куки пользователей, и
// шифрование (пакет secret) необязательно — без ключа они лежат открыто.
// Существующему каталогу режим не меняем: он мог быть заведён снаружи.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
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
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// InsertNote добавляет заметку, если её ещё нет. Возвращает true, если строка
// действительно вставлена (для идемпотентного импорта и seed). Ненулевые
// телеграм-id (legacy-импорт) дублируются в message_targets.
func (s *Store) InsertNote(ctx context.Context, n Note) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO notes
			(id, author_id, author_name, text, author_avatar_url, status,
			 tg_message_id, tg_thread_id, first_seen_at, last_comment_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.AuthorID, n.AuthorName, n.Text, n.AuthorAvatarURL, n.Status,
		nullID(n.TGMessageID), nullID(n.TGThreadID),
		fmtTime(n.FirstSeenAt), nullTime(n.LastCommentAt))
	if err != nil {
		return false, fmt.Errorf("insert note %s: %w", n.ID, err)
	}
	affected, _ := res.RowsAffected()
	if affected > 0 && n.TGMessageID != 0 {
		if err := s.SetTarget(ctx, MessengerTelegram, TargetNotePost, n.ID,
			strconv.FormatInt(n.TGMessageID, 10), ""); err != nil {
			return true, err
		}
	}
	if affected > 0 && n.TGThreadID != 0 {
		if err := s.SetTarget(ctx, MessengerTelegram, TargetNoteThread, n.ID,
			"", strconv.FormatInt(n.TGThreadID, 10)); err != nil {
			return true, err
		}
	}
	return affected > 0, nil
}

// InsertNoteImage добавляет иллюстрацию заметки, если её ещё нет.
func (s *Store) InsertNoteImage(ctx context.Context, noteID string, position int, url string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO note_images (note_id, position, url)
		VALUES (?, ?, ?)`, noteID, position, url)
	if err != nil {
		return fmt.Errorf("insert note image %s: %w", noteID, err)
	}
	return nil
}

// InsertComment добавляет комментарий, если его ещё нет. Ненулевой
// телеграм-id (legacy-импорт) дублируется в message_targets.
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
	if affected > 0 && c.TGMessageID != 0 {
		if err := s.SetTarget(ctx, MessengerTelegram, TargetComment,
			strconv.FormatInt(c.ID, 10), strconv.FormatInt(c.TGMessageID, 10), ""); err != nil {
			return true, err
		}
	}
	return affected > 0, nil
}

// UpsertSession сохраняет куки пользователя (JSON), помечая сессию валидной.
// Куки уходят на диск шифротекстом, если задан ключ (store.WithSecret).
func (s *Store) UpsertSession(ctx context.Context, messenger string, userID int64, cookiesJSON string, now time.Time) error {
	stored, err := s.key.Seal(secret.SessionAAD(messenger, userID), cookiesJSON)
	if err != nil {
		return fmt.Errorf("upsert session %s/%d: %w", messenger, userID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (messenger, user_id, cookies, valid, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(messenger, user_id) DO UPDATE SET
			cookies = excluded.cookies, valid = 1, updated_at = excluded.updated_at`,
		messenger, userID, stored, fmtTime(now))
	if err != nil {
		return fmt.Errorf("upsert session %s/%d: %w", messenger, userID, err)
	}
	return nil
}

// AddSubscription добавляет подписку с проверкой предела. Порядок «вставить →
// посчитать → откатить» держит предел честным при параллельных нажатиях и не
// отказывает на повторной подписке, когда предел уже выбран.
// false, nil — такая подписка уже была; ErrSubscriptionLimit — предел.
func (s *Store) AddSubscription(ctx context.Context, sub Subscription) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO subscriptions (messenger, user_id, kind, target)
		VALUES (?, ?, ?, ?)`, sub.Messenger, sub.UserID, sub.Kind, sub.Target)
	if err != nil {
		return false, fmt.Errorf("insert subscription: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return false, tx.Commit() // дубль: предел тут ни при чём
	}
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM subscriptions WHERE messenger = ? AND user_id = ?`,
		sub.Messenger, sub.UserID).Scan(&n); err != nil {
		return false, fmt.Errorf("подсчёт подписок: %w", err)
	}
	if n > SubscriptionLimit {
		return false, ErrSubscriptionLimit // откат делает defer
	}
	return true, tx.Commit()
}

// RemoveSubscription убирает подписку пользователя по цели (/unsubscribe
// <слово>). Возвращает true, если строка действительно была удалена.
func (s *Store) RemoveSubscription(ctx context.Context, messenger string, userID int64, kind, target string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM subscriptions
		WHERE messenger = ? AND user_id = ? AND kind = ? AND target = ?`,
		messenger, userID, kind, target)
	if err != nil {
		return false, fmt.Errorf("delete subscription: %w", err)
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

// subLabelSQL — человеческая подпись цели подписки. Считается в запросе, иначе
// список из полусотни строк превратился бы в N+1 обращений за именем автора и
// текстом заметки.
const subLabelSQL = `
	CASE s.kind
	  WHEN 'author_notes'  THEN COALESCE((SELECT n.author_name FROM notes n
	                                      WHERE n.author_id = s.target
	                                      ORDER BY n.first_seen_at DESC LIMIT 1), '')
	  WHEN 'note_comments' THEN COALESCE((SELECT n.author_name || ': ' || n.text
	                                      FROM notes n WHERE n.id = s.target), '')
	  ELSE s.target
	END`

// SubscriptionsByUser возвращает подписки пользователя вместе с id строк и
// готовой подписью цели: по id снимает подписку кнопка (цель в payload не
// влезает — там 64 байта, а кириллица стоит по два на знак).
func (s *Store) SubscriptionsByUser(ctx context.Context, messenger string, userID int64) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.kind, s.target, `+subLabelSQL+` AS label
		FROM subscriptions s
		WHERE s.messenger = ? AND s.user_id = ?
		ORDER BY CASE s.kind WHEN 'keyword' THEN 0 WHEN 'author_notes' THEN 1 ELSE 2 END,
		         label`, messenger, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []Subscription
	for rows.Next() {
		sub := Subscription{Messenger: messenger, UserID: userID}
		if err := rows.Scan(&sub.ID, &sub.Kind, &sub.Target, &sub.Label); err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// RemoveSubscriptionByID снимает подписку по id строки и возвращает снятую:
// подтверждение формулируется по виду и цели. Мессенджер и пользователь в
// условии обязательны: id приезжает из кнопки, и чужую подписку по нему снять
// быть не должно.
func (s *Store) RemoveSubscriptionByID(ctx context.Context, messenger string, userID, id int64) (Subscription, bool, error) {
	sub := Subscription{ID: id, Messenger: messenger, UserID: userID}
	err := s.db.QueryRowContext(ctx, `
		DELETE FROM subscriptions WHERE id = ? AND messenger = ? AND user_id = ?
		RETURNING kind, target`, id, messenger, userID).Scan(&sub.Kind, &sub.Target)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, false, nil
	}
	if err != nil {
		return Subscription{}, false, fmt.Errorf("delete subscription %d: %w", id, err)
	}
	return sub, true, nil
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
		SELECT id, author_id, author_name, text, author_avatar_url, status,
		       tg_message_id, tg_thread_id, first_seen_at, last_comment_at, comments_closed
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

func scanComment(rows *sql.Rows) (Comment, error) {
	var c Comment
	var published, createdAt sql.NullString
	var tgMsg sql.NullInt64
	if err := rows.Scan(&c.ID, &c.NoteID, &c.AuthorName, &c.AuthorAge,
		&c.AuthorLink, &c.AvatarURL, &published, &c.Text, &tgMsg, &createdAt); err != nil {
		return c, err
	}
	if published.Valid {
		c.PublishedAt = parseTime(published.String)
	}
	if createdAt.Valid {
		c.CreatedAt = parseTime(createdAt.String)
	}
	c.TGMessageID = tgMsg.Int64
	return c, nil
}

func scanNote(rows *sql.Rows) (Note, error) {
	var n Note
	var tgMsg, tgThread sql.NullInt64
	var firstSeen string
	var lastComment sql.NullString
	var commentsClosed int
	if err := rows.Scan(&n.ID, &n.AuthorID, &n.AuthorName, &n.Text, &n.AuthorAvatarURL,
		&n.Status, &tgMsg, &tgThread, &firstSeen, &lastComment, &commentsClosed); err != nil {
		return n, err
	}
	n.TGMessageID = tgMsg.Int64
	n.TGThreadID = tgThread.Int64
	n.FirstSeenAt = parseTime(firstSeen)
	if lastComment.Valid {
		n.LastCommentAt = parseTime(lastComment.String)
	}
	n.CommentsClosed = commentsClosed == 1
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
