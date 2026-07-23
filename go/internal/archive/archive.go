// Пакет archive — отдельное SQLite-хранилище разового грабера: нормализованный
// дамп заметок с комментариями для восстановления социального графа «типажей».
// Независим от боевого пакета store (свои структуры, своя БД).
package archive

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// User — типаж (автор заметки или комментария), узел социального графа.
type User struct {
	ID         int64
	Name       string
	Age        string
	ProfileURL string
	AvatarURL  string
}

// Note — заметка в архиве. AuthorID 0 — аноним.
type Note struct {
	ID             int64
	AuthorID       int64
	Text           string
	Images         []string
	CommentsClosed bool
	PublishedAt    time.Time // zero — дата неизвестна
}

// Comment — комментарий. ParentID 0 — корень заметки, иначе id родителя.
type Comment struct {
	ID          int64
	NoteID      int64
	ParentID    int64
	AuthorID    int64
	Text        string
	PublishedAt time.Time
}

// Stats — итог одной выгрузки для отчёта пользователю.
type Stats struct {
	NewUsers         int // впервые увиденные типажи
	AvatarChanged    int // у скольких обновился аватар (несовпадение)
	NameChanged      int // у скольких обновилось имя
	CommentsInserted int // новых комментариев записано
	CommentsTotal    int // всего комментариев в выгрузке
}

// Store — соединение с archive.db.
type Store struct {
	db *sql.DB
}

// Open открывает (создавая при необходимости) архивную БД и накатывает схему.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие архива %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrateV2SQL — типовые VIEW поверх нормализованных таблиц: читаемое дерево
// комментариев, направленные рёбра соцграфа «кто кому отвечал», активность
// типажей и сводка по заметкам. Живут в миграции (а не в schema.sql), чтобы
// добавиться и в уже созданные архивы.
const migrateV2SQL = `
-- Комментарии с именами автора и родителя (у корней parent_* = NULL).
CREATE VIEW v_comment_tree AS
SELECT c.note_id, c.id, c.parent_id,
       c.author_id, ca.name AS author_name,
       p.author_id AS parent_author_id, pa.name AS parent_author_name,
       c.published_at, c.text
FROM comments c
JOIN users ca ON ca.id = c.author_id
LEFT JOIN comments p ON p.id = c.parent_id
LEFT JOIN users pa ON pa.id = p.author_id;

-- Направленные рёбра «кто кому отвечал» с весом (число ответов).
-- Само-ответы (from_id = to_id) не отфильтрованы — это реальные данные.
CREATE VIEW v_reply_edges AS
SELECT c.author_id AS from_id, ca.name AS from_name,
       p.author_id AS to_id,   pa.name AS to_name,
       COUNT(*) AS replies
FROM comments c
JOIN comments p ON p.id = c.parent_id
JOIN users ca ON ca.id = c.author_id
JOIN users pa ON pa.id = p.author_id
WHERE c.parent_id != 0
GROUP BY c.author_id, p.author_id;

-- Активность типажа: сколько комментариев и в скольких заметках. LEFT JOIN —
-- авторы, которые только писали заметки, тоже видны (0 комментариев).
CREATE VIEW v_user_activity AS
SELECT u.id, u.name, u.age,
       COUNT(c.id) AS comments,
       COUNT(DISTINCT c.note_id) AS notes,
       u.first_seen, u.last_seen, u.profile_url, u.avatar_url
FROM users u
LEFT JOIN comments c ON c.author_id = u.id
GROUP BY u.id;

-- Сводка по заметке: автор, число комментариев и уникальных участников.
CREATE VIEW v_note_overview AS
SELECT n.id, au.name AS author_name, n.published_at, n.comments_closed,
       COUNT(c.id) AS comments,
       COUNT(DISTINCT c.author_id) AS participants,
       n.grabbed_at
FROM notes n
LEFT JOIN users au ON au.id = n.author_id
LEFT JOIN comments c ON c.note_id = n.id
GROUP BY n.id;
`

// migrate накатывает недостающие миграции по PRAGMA user_version.
func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL,    // v1 — базовая схема (users/notes/comments)
		migrateV2SQL, // v2 — типовые VIEW (дерево, граф, активность, сводка)
		migrateV3SQL, // v3 — слой распознавания личностей (disclosure/alias/personas)
		migrateV4SQL, // v4 — перцептивные хэши аватаров (avatar_hashes)
		migrateV5SQL, // v5 — стилометрические профили (style_profiles)
		migrateV6SQL, // v6 — persona-aware граф (v_identity/v_persona_activity/v_persona_reply_edges)
		migrateV7SQL, // v7 — реальные даты активности из comments.published_at
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}
	for i, migration := range migrations {
		target := i + 1
		if version >= target {
			continue
		}
		if _, err := s.db.ExecContext(ctx, migration); err != nil {
			return fmt.Errorf("миграция v%d: %w", target, err)
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			return err
		}
	}
	return nil
}

// SaveGrab сохраняет одну выгрузку (заметка + комментарии + типажи) в одной
// транзакции. Порядок из-за внешних ключей: сначала users, потом note, потом
// comments. Пользователи обновляются по latest-wins (непустое новое значение
// перезаписывает старое; last_seen — время выгрузки, first_seen неизменно).
// Комментарии идемпотентны (INSERT OR IGNORE): повторная выгрузка не задваивает.
func (s *Store) SaveGrab(ctx context.Context, n Note, comments []Comment, users []User, now time.Time) (Stats, error) {
	var st Stats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck // после Commit — no-op

	nowStr := fmtTime(now)
	for _, u := range users {
		var oldAvatar, oldName string
		switch err := tx.QueryRowContext(ctx,
			`SELECT avatar_url, name FROM users WHERE id = ?`, u.ID).Scan(&oldAvatar, &oldName); {
		case errors.Is(err, sql.ErrNoRows):
			st.NewUsers++
		case err != nil:
			return st, err
		default:
			if u.AvatarURL != "" && u.AvatarURL != oldAvatar {
				st.AvatarChanged++
			}
			if u.Name != "" && u.Name != oldName {
				st.NameChanged++
			}
		}
		// latest-wins: непустое новое значение перезаписывает, иначе держим
		// старое (терпимо к разреженным данным автора заметки — у него нет
		// возраста и, возможно, ссылки).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (id, name, age, profile_url, avatar_url, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name        = CASE WHEN excluded.name        != '' THEN excluded.name        ELSE users.name        END,
				age         = CASE WHEN excluded.age         != '' THEN excluded.age         ELSE users.age         END,
				profile_url = CASE WHEN excluded.profile_url != '' THEN excluded.profile_url ELSE users.profile_url END,
				avatar_url  = CASE WHEN excluded.avatar_url  != '' THEN excluded.avatar_url  ELSE users.avatar_url  END,
				last_seen   = excluded.last_seen`,
			u.ID, u.Name, u.Age, u.ProfileURL, u.AvatarURL, nowStr, nowStr); err != nil {
			return st, fmt.Errorf("upsert user %d: %w", u.ID, err)
		}
	}

	imagesJSON, err := json.Marshal(nonNil(n.Images))
	if err != nil {
		return st, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notes (id, author_id, text, images, comments_closed, published_at, grabbed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			author_id       = excluded.author_id,
			text            = excluded.text,
			images          = excluded.images,
			comments_closed = excluded.comments_closed,
			published_at    = excluded.published_at,
			grabbed_at      = excluded.grabbed_at`,
		n.ID, nullID(n.AuthorID), n.Text, string(imagesJSON), boolInt(n.CommentsClosed),
		nullTime(n.PublishedAt), nowStr); err != nil {
		return st, fmt.Errorf("upsert note %d: %w", n.ID, err)
	}

	for _, c := range comments {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO comments (id, note_id, parent_id, author_id, text, published_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.NoteID, c.ParentID, c.AuthorID, c.Text, nullTime(c.PublishedAt))
		if err != nil {
			return st, fmt.Errorf("insert comment %d: %w", c.ID, err)
		}
		if aff, _ := res.RowsAffected(); aff > 0 {
			st.CommentsInserted++
		}
	}
	st.CommentsTotal = len(comments)

	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// StoredNote — заметка из архива с денормализованным автором (для чтения/экспорта).
type StoredNote struct {
	ID             int64
	Author         *User // nil — аноним
	Text           string
	Images         []string
	CommentsClosed bool
	PublishedAt    time.Time // zero — неизвестно
	GrabbedAt      time.Time
}

// StoredComment — комментарий из архива с денормализованным автором.
type StoredComment struct {
	ID          int64
	ParentID    int64
	Author      User
	Text        string
	PublishedAt time.Time
}

// LoadNote читает заметку с автором. Второй результат false — заметки нет.
func (s *Store) LoadNote(ctx context.Context, id int64) (StoredNote, bool, error) {
	var (
		n                          StoredNote
		authorID                   sql.NullInt64
		name, age, profile, avatar sql.NullString
		imagesJSON                 string
		closed                     int
		published, grabbed         sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT n.id, n.author_id, u.name, u.age, u.profile_url, u.avatar_url,
		       n.text, n.images, n.comments_closed, n.published_at, n.grabbed_at
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.id = ?`, id).Scan(
		&n.ID, &authorID, &name, &age, &profile, &avatar,
		&n.Text, &imagesJSON, &closed, &published, &grabbed)
	if errors.Is(err, sql.ErrNoRows) {
		return StoredNote{}, false, nil
	}
	if err != nil {
		return StoredNote{}, false, err
	}
	if authorID.Valid {
		n.Author = &User{ID: authorID.Int64, Name: name.String, Age: age.String,
			ProfileURL: profile.String, AvatarURL: avatar.String}
	}
	if err := json.Unmarshal([]byte(imagesJSON), &n.Images); err != nil {
		n.Images = nil // повреждённый JSON изображений — не критично
	}
	n.CommentsClosed = closed == 1
	n.PublishedAt = parseNullTime(published)
	n.GrabbedAt = parseNullTime(grabbed)
	return n, true, nil
}

// KnownNoteIDs возвращает id всех уже сохранённых заметок — для пропуска при
// массовой выгрузке (резюм после остановки/блока).
func (s *Store) KnownNoteIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// LoadComments читает комментарии заметки (плоско, по возрастанию id) с авторами.
func (s *Store) LoadComments(ctx context.Context, noteID int64) ([]StoredComment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.parent_id, c.author_id, u.name, u.age, u.profile_url, u.avatar_url,
		       c.text, c.published_at
		FROM comments c JOIN users u ON u.id = c.author_id
		WHERE c.note_id = ? ORDER BY c.id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredComment
	for rows.Next() {
		var c StoredComment
		var published sql.NullString
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Author.ID, &c.Author.Name, &c.Author.Age,
			&c.Author.ProfileURL, &c.Author.AvatarURL, &c.Text, &published); err != nil {
			return nil, err
		}
		c.PublishedAt = parseNullTime(published)
		out = append(out, c)
	}
	return out, rows.Err()
}

// nullID: 0 хранится как NULL (аноним/неизвестно).
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
	return t.UTC().Format(time.RFC3339)
}

// parseNullTime разбирает время из nullable-строки (RFC3339); NULL/пусто/мусор
// → нулевое время.
func parseNullTime(ns sql.NullString) time.Time {
	if !ns.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, ns.String)
	return t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
