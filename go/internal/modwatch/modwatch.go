// Пакет modwatch — наблюдатель за действиями модерации в заметках love.ngs.ru.
//
// Зачем: поля «модератор» на сайте нет, а по речи опознаются только болтливые
// модераторы. Действие же видно всегда — заметку снесли, комментарий исчез,
// картинку поставили, комментарии закрыли. Архив таких моментов не хранит (он
// снимок уцелевшего), поэтому их надо ловить онлайн: сборщик пишет ЧТО и КОГДА
// произошло, а отчёт считает, кто в эти минуты был на площадке — и сравнивает
// с контрольными минутами того же часа суток. Систематическое присутствие в
// момент действий — это улика, одиночное совпадение — нет.
package modwatch

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Виды событий.
const (
	KindNoteGone       = "note_gone"       // заметка исчезла из ленты, оставаясь внутри охвата
	KindNoteReturned   = "note_returned"   // исчезнувшая заметка снова в ленте — снимали на проверку
	KindNotePublished  = "note_published"  // заметка впервые увидена (для картиночных ≈ момент одобрения)
	KindImageAdded     = "image_added"     // у заметки появилась иллюстрация — её ставит модератор
	KindCommentsClosed = "comments_closed" // комментарии закрыли
	KindCommentsOpened = "comments_opened" // комментарии открыли обратно
	KindCommentGone    = "comment_gone"    // комментарий исчез внутри охвата треда
	KindNickChanged    = "nick_changed"    // ник сменился (смену подтверждает модерация)
)

// AllKinds — все виды событий в порядке убывания «модераторности».
var AllKinds = []string{
	KindNoteGone, KindCommentGone, KindImageAdded, KindNoteReturned,
	KindCommentsClosed, KindCommentsOpened, KindNotePublished, KindNickChanged,
}

// headLimit — сколько знаков текста хранить для читаемости события.
const headLimit = 160

// NoteState — состояние заметки на момент опроса.
type NoteState struct {
	ID             int64
	AuthorID       int64 // 0 — аноним
	AuthorName     string
	TextHead       string
	Images         int
	CommentsClosed bool
	PublishedAt    time.Time // нулевое — из ленты дата неизвестна
}

// NoteRow — то, что записано в БД.
type NoteRow struct {
	NoteState
	FirstSeen  time.Time
	LastSeen   time.Time
	LastPolled time.Time
	Gone       bool
}

// CommentState — комментарий на момент опроса.
type CommentState struct {
	ID          int64
	NoteID      int64
	AuthorID    int64
	AuthorName  string
	TextHead    string
	PublishedAt time.Time
}

// CommentRow — комментарий в БД.
type CommentRow struct {
	CommentState
	FirstSeen time.Time
	LastSeen  time.Time
	Gone      bool
}

// Event — зафиксированное действие модерации.
type Event struct {
	ID         int64
	Kind       string
	RefID      int64
	NoteID     int64
	PrevSeen   time.Time // объект точно был на месте
	DetectedAt time.Time // объекта точно уже нет
	Details    string
	Age        time.Duration // возраст объекта к моменту действия; Unknown — не знаем
	Idle       time.Duration // тишина в треде перед действием; Unknown — не знаем
	Size       int           // известных реплик в треде к моменту действия (снизу: глубже охвата мы не видели)
}

// Unknown — значение Age/Idle, когда исходная дата неизвестна (например,
// заметка исчезла раньше, чем мы дотянулись до её шапки с датой публикации).
const Unknown = time.Duration(-1)

// Presence — реплика как след присутствия человека в минуту X.
type Presence struct {
	UserID int64
	At     time.Time
}

// Store — соединение с modwatch.db.
type Store struct {
	db *sql.DB
}

// Open открывает (создавая при необходимости) БД наблюдателя и накатывает схему.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)"},
	}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close закрывает соединение.
func (s *Store) Close() error { return s.db.Close() }

// migrateV2SQL — контекст события: возраст объекта и «тишина» перед действием.
//
// Нужен, чтобы отличать руку от автоматики. Сайт сам метит заметку «не
// актуальна» — такие закрытия кучкуются на одном возрасте (таймер), а закрытое
// вручную выпадает из этой кучи. Тот же контекст полезен и для сносов: заметку
// убирают либо сразу, либо утренней зачисткой — это разные сюжеты.
const migrateV2SQL = `
ALTER TABLE events ADD COLUMN age_sec  INTEGER;  -- возраст объекта к моменту действия
ALTER TABLE events ADD COLUMN idle_sec INTEGER;  -- сколько в треде было тихо до действия
`

func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL,    // v1 — базовая схема
		migrateV2SQL, // v2 — возраст объекта и тишина перед действием
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
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migration); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция v%d: %w", target, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция v%d: установка версии: %w", target, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Head обрезает текст до headLimit знаков (рун), схлопывая пробелы.
func Head(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= headLimit {
		return s
	}
	return string(r[:headLimit]) + "…"
}

func ts(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return ts(t)
}

// KnownNotes возвращает заметки, которые попадались за последнее время, — и
// живые, и уже исчезнувшие (`Gone`). Исчезнувшие нужны потому, что заметка
// возвращается: сайт снимает её на проверку и кладёт обратно. Без них возврат
// читался бы как новая публикация каждый такт, а следующее — настоящее —
// удаление не фиксировалось бы вовсе (заметка ведь уже помечена исчезнувшей).
func (s *Store) KnownNotes(ctx context.Context, since time.Time) (map[int64]NoteRow, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, author_id, author_name, text_head, images, comments_closed,
               published_at, first_seen, last_seen, last_polled, gone_at
          FROM notes WHERE last_seen >= ?`, ts(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]NoteRow{}
	for rows.Next() {
		var (
			r                   NoteRow
			pub, polled, gone   sql.NullString
			firstSeen, lastSeen string
			closed              int
		)
		if err := rows.Scan(&r.ID, &r.AuthorID, &r.AuthorName, &r.TextHead, &r.Images, &closed,
			&pub, &firstSeen, &lastSeen, &polled, &gone); err != nil {
			return nil, err
		}
		r.CommentsClosed = closed != 0
		r.PublishedAt = parseTS(pub.String)
		r.FirstSeen = parseTS(firstSeen)
		r.LastSeen = parseTS(lastSeen)
		r.LastPolled = parseTS(polled.String)
		r.Gone = gone.Valid
		out[r.ID] = r
	}
	return out, rows.Err()
}

// UnmarkNoteGone возвращает заметку в наблюдение: она снова видна в ленте.
func (s *Store) UnmarkNoteGone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET gone_at = NULL WHERE id = ?`, id)
	return err
}

// SaveNote записывает состояние заметки (latest-wins) и двигает last_seen.
func (s *Store) SaveNote(ctx context.Context, now time.Time, n NoteState) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO notes (id, author_id, author_name, text_head, images, comments_closed,
                           published_at, first_seen, last_seen)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            author_id       = excluded.author_id,
            author_name     = CASE WHEN excluded.author_name <> '' THEN excluded.author_name ELSE notes.author_name END,
            text_head       = CASE WHEN excluded.text_head   <> '' THEN excluded.text_head   ELSE notes.text_head   END,
            images          = MAX(notes.images, excluded.images),
            comments_closed = excluded.comments_closed,
            published_at    = COALESCE(excluded.published_at, notes.published_at),
            last_seen       = excluded.last_seen`,
		n.ID, n.AuthorID, n.AuthorName, Head(n.TextHead), n.Images, boolInt(n.CommentsClosed),
		nullTS(n.PublishedAt), ts(now), ts(now))
	return err
}

// MarkNoteGone помечает заметку исчезнувшей.
func (s *Store) MarkNoteGone(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET gone_at = ? WHERE id = ? AND gone_at IS NULL`, ts(at), id)
	return err
}

// SetNotePolled отмечает время последнего опроса треда.
func (s *Store) SetNotePolled(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notes SET last_polled = ? WHERE id = ?`, ts(at), id)
	return err
}

// NotesDue возвращает живые заметки, которым пора обновить тред: не старше
// window от первой встречи и не опрошенные дольше interval. Свежие вперёд.
func (s *Store) NotesDue(ctx context.Context, now time.Time, interval, window time.Duration, limit int) ([]NoteRow, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, author_id, author_name, text_head, images, comments_closed,
               published_at, first_seen, last_seen, last_polled
          FROM notes
         WHERE gone_at IS NULL
           AND first_seen >= ?
           AND (last_polled IS NULL OR last_polled <= ?)
         ORDER BY id DESC
         LIMIT ?`, ts(now.Add(-window)), ts(now.Add(-interval)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteRow
	for rows.Next() {
		var (
			r                   NoteRow
			pub, polled         sql.NullString
			firstSeen, lastSeen string
			closed              int
		)
		if err := rows.Scan(&r.ID, &r.AuthorID, &r.AuthorName, &r.TextHead, &r.Images, &closed,
			&pub, &firstSeen, &lastSeen, &polled); err != nil {
			return nil, err
		}
		r.CommentsClosed = closed != 0
		r.PublishedAt = parseTS(pub.String)
		r.FirstSeen = parseTS(firstSeen)
		r.LastSeen = parseTS(lastSeen)
		r.LastPolled = parseTS(polled.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveComments возвращает комментарии заметки, которые на прошлом опросе были на месте.
func (s *Store) LiveComments(ctx context.Context, noteID int64) (map[int64]CommentRow, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, note_id, author_id, author_name, text_head, published_at, first_seen, last_seen
          FROM comments WHERE note_id = ? AND gone_at IS NULL`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]CommentRow{}
	for rows.Next() {
		var (
			r                        CommentRow
			pub, firstSeen, lastSeen string
		)
		if err := rows.Scan(&r.ID, &r.NoteID, &r.AuthorID, &r.AuthorName, &r.TextHead,
			&pub, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		r.PublishedAt = parseTS(pub)
		r.FirstSeen = parseTS(firstSeen)
		r.LastSeen = parseTS(lastSeen)
		out[r.ID] = r
	}
	return out, rows.Err()
}

// SaveComment записывает комментарий и двигает last_seen.
func (s *Store) SaveComment(ctx context.Context, now time.Time, c CommentState) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO comments (id, note_id, author_id, author_name, text_head, published_at, first_seen, last_seen)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            author_name = CASE WHEN excluded.author_name <> '' THEN excluded.author_name ELSE comments.author_name END,
            last_seen   = excluded.last_seen`,
		c.ID, c.NoteID, c.AuthorID, c.AuthorName, Head(c.TextHead), ts(c.PublishedAt), ts(now), ts(now))
	return err
}

// MarkCommentGone помечает комментарий исчезнувшим.
func (s *Store) MarkCommentGone(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE comments SET gone_at = ? WHERE id = ? AND gone_at IS NULL`, ts(at), id)
	return err
}

// SaveUser обновляет ник и возвращает предыдущий (пустой — увидели впервые).
func (s *Store) SaveUser(ctx context.Context, now time.Time, id int64, name string) (string, error) {
	if id == 0 {
		return "", nil
	}
	var prev string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, id).Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO users (id, name, first_seen, last_seen) VALUES (?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            name      = CASE WHEN excluded.name <> '' THEN excluded.name ELSE users.name END,
            last_seen = excluded.last_seen`, id, name, ts(now), ts(now))
	return prev, err
}

// AddEvent записывает событие; повтор того же события игнорируется.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT OR IGNORE INTO events (kind, ref_id, note_id, prev_seen_at, detected_at, details, age_sec, idle_sec)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Kind, e.RefID, e.NoteID, ts(e.PrevSeen), ts(e.DetectedAt), e.Details,
		secs(e.Age), secs(e.Idle))
	return err
}

// EventFilter — отбор событий. Нулевые поля — без ограничения.
type EventFilter struct {
	Since, Until   time.Time
	Kinds          []string
	MinAge, MaxAge time.Duration // возраст объекта к моменту действия
	Limit          int
}

// Events возвращает события по фильтру, от старых к новым.
//
// Фильтр по возрасту — это и есть проверка «таймером»: автоматика срабатывает
// на одном и том же возрасте, поэтому `-age-max` отсекает её и оставляет
// действия, сделанные раньше срока, то есть руками. События без известного
// возраста (age_sec IS NULL) при заданной границе не проходят: молча выдавать
// их за ручные нельзя.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	// age_sec/idle_sec появились в v2; у событий, записанных раньше, они пустые,
	// поэтому считаем их на лету из того, что и так лежит в таблицах.
	q := `SELECT * FROM (
            SELECT e.id, e.kind, e.ref_id, e.note_id, e.prev_seen_at, e.detected_at, e.details,
                   COALESCE(e.age_sec, CAST((julianday(e.detected_at) - julianday(
                       CASE WHEN e.kind = '` + KindCommentGone + `'
                            THEN (SELECT c.published_at FROM comments c WHERE c.id = e.ref_id)
                            ELSE (SELECT n.published_at FROM notes   n WHERE n.id = e.note_id)
                       END)) * 86400 AS INTEGER)) AS age_sec,
                   COALESCE(e.idle_sec, CAST((julianday(e.detected_at) - julianday(
                       (SELECT MAX(c2.published_at) FROM comments c2 WHERE c2.note_id = e.note_id)
                   )) * 86400 AS INTEGER)) AS idle_sec,
                   (SELECT COUNT(*) FROM comments c3
                     WHERE c3.note_id = e.note_id AND c3.published_at <= e.detected_at) AS size
              FROM events e
          ) WHERE 1=1`
	var args []any
	if !f.Since.IsZero() {
		q += " AND detected_at >= ?"
		args = append(args, ts(f.Since))
	}
	if !f.Until.IsZero() {
		q += " AND detected_at <= ?"
		args = append(args, ts(f.Until))
	}
	if len(f.Kinds) > 0 {
		q += " AND kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",") + ")"
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.MinAge > 0 {
		q += " AND age_sec >= ?"
		args = append(args, int64(f.MinAge.Seconds()))
	}
	if f.MaxAge > 0 {
		q += " AND age_sec <= ?"
		args = append(args, int64(f.MaxAge.Seconds()))
	}
	q += " ORDER BY detected_at"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var prev, det string
		var age, idle sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Kind, &e.RefID, &e.NoteID, &prev, &det, &e.Details, &age, &idle, &e.Size); err != nil {
			return nil, err
		}
		e.PrevSeen = parseTS(prev)
		e.DetectedAt = parseTS(det)
		e.Age, e.Idle = dur(age), dur(idle)
		out = append(out, e)
	}
	return out, rows.Err()
}

// NoteReturns — когда заметка появлялась в ленте, по каждой заметке.
//
// Нужно, чтобы отличить удаление от поездки на премодерацию: простой текст
// публикуется сразу, но стоит автору дописать картинку — заметка уходит на
// проверку, пропадая из ленты, и возвращается уже одобренной. Такое
// исчезновение сделал автор, а не модератор. Считаем и `note_published`: до
// правки от 12.08.2026 возврат записывался именно так.
func (s *Store) NoteReturns(ctx context.Context) (map[int64][]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT note_id, detected_at FROM events
         WHERE kind IN (?, ?) ORDER BY detected_at`, KindNoteReturned, KindNotePublished)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]time.Time{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = append(out[id], parseTS(at))
	}
	return out, rows.Err()
}

// LastCommentAt — время последней известной реплики в заметке.
func (s *Store) LastCommentAt(ctx context.Context, noteID int64) (time.Time, bool) {
	var at sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(published_at) FROM comments WHERE note_id = ?`, noteID).Scan(&at); err != nil {
		return time.Time{}, false
	}
	if !at.Valid {
		return time.Time{}, false
	}
	t := parseTS(at.String)
	return t, !t.IsZero()
}

// secs переводит длительность в секунды для БД; Unknown → NULL.
func secs(d time.Duration) any {
	if d < 0 {
		return nil
	}
	return int64(d.Seconds())
}

func dur(v sql.NullInt64) time.Duration {
	if !v.Valid {
		return Unknown
	}
	return time.Duration(v.Int64) * time.Second
}

// PresenceLog возвращает следы присутствия (все увиденные реплики) за период,
// отсортированные по времени публикации.
func (s *Store) PresenceLog(ctx context.Context, since, until time.Time) ([]Presence, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT author_id, published_at FROM comments
         WHERE author_id <> 0 AND published_at >= ? AND published_at <= ?
         ORDER BY published_at`, ts(since), ts(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Presence
	for rows.Next() {
		var p Presence
		var at string
		if err := rows.Scan(&p.UserID, &at); err != nil {
			return nil, err
		}
		p.At = parseTS(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Names возвращает известные ники по id анкет.
func (s *Store) Names(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// Counts — сводка по наполнению БД для статуса.
type Counts struct {
	Notes, NotesGone, Comments, CommentsGone, Events, Users int
	FirstSeen, LastSeen                                     time.Time
}

// Counts возвращает сводку наблюдения.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var c Counts
	var first, last sql.NullString
	err := s.db.QueryRowContext(ctx, `
        SELECT (SELECT COUNT(*) FROM notes),
               (SELECT COUNT(*) FROM notes WHERE gone_at IS NOT NULL),
               (SELECT COUNT(*) FROM comments),
               (SELECT COUNT(*) FROM comments WHERE gone_at IS NOT NULL),
               (SELECT COUNT(*) FROM events),
               (SELECT COUNT(*) FROM users),
               (SELECT MIN(first_seen) FROM notes),
               (SELECT MAX(last_seen) FROM notes)`).
		Scan(&c.Notes, &c.NotesGone, &c.Comments, &c.CommentsGone, &c.Events, &c.Users, &first, &last)
	if err != nil {
		return c, err
	}
	c.FirstSeen = parseTS(first.String)
	c.LastSeen = parseTS(last.String)
	return c, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
