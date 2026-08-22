// Пакет platdigest — площадка как источник еженедельного выпуска.
//
// Зачем он есть. Дайджест считался по SQLite зеркала, и это было верно ровно
// пока разговор шёл на НГС: туда пишет только зеркало, и сводка про сайт
// строилась по сайту. С 22.08.2026 выпуск публикуется НА ПЛОЩАДКЕ, а
// написанное здесь в SQLite не попадает вовсе — то есть сводка про сообщество
// приходила бы к сообществу, не видя ни одной его собственной реплики.
//
// Отдельный пакет, а не метод `platform`, по той же причине, по которой
// отдельно живут `platsink`, `platmod` и `platimport`: ядро площадки не обязано
// знать, что у кого-то есть еженедельная рубрика «цитата недели».
//
// ГРАНИЦА ПОКАЗА. Выпуск читает то же, что читатель: status = 0. Скрытое
// модератором или автором (отзыв согласия по 152-ФЗ) не попадает в сводку ни
// числом, ни цитатой — иначе спрятанная реплика вышла бы в канал недельной
// сводкой.
//
// АНОНИМ ОСТАЁТСЯ АНОНИМОМ. У анонимной заметки в базе лежит НАСТОЯЩИЙ автор
// (он нужен модерации и правам субъекта), поэтому маскирование стоит в самом
// SELECT, как в ядре: наружу уходит «Аноним» без идентичности, а в рубрики
// «новые лица» и «возвращение недели» анонимные заметки не идут вовсе — иначе
// выпуск деанонимизировал бы автора соседством чисел.
//
// ГОРИЗОНТ. Сравнительные рекорды считаются за год (digest.RecordHorizon), а не
// за всю историю: в базе архив с 2009-го, и «рекорд за всё время» — это тред
// 2013 года, который не побьют никогда.
package platdigest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lovegw/internal/digest"
	"lovegw/internal/platform"
)

// Source — площадка как источник выпуска (реализует digest.Source).
type Source struct {
	pool *pgxpool.Pool
	// ngsBase — адрес НГС для ссылок на анкеты. У участника с анкетой id строки
	// РАВЕН её номеру, поэтому ссылка собирается подстановкой; у нативного
	// (вошёл по приглашению) анкеты нет вовсе, и ссылки не будет.
	ngsBase string
}

// New создаёт источник. ngsBase — базовый адрес love.ngs.ru.
func New(p *platform.Platform, ngsBase string) *Source {
	return &Source{pool: p.Pool(), ngsBase: strings.TrimSuffix(ngsBase, "/")}
}

// ProfileURL — анкета автора на НГС; пусто у нативного участника и анонима.
func (s *Source) ProfileURL(author string) string {
	id, err := strconv.ParseInt(author, 10, 64)
	if err != nil || id <= 0 || id >= platform.NativeIDBase || s.ngsBase == "" {
		return ""
	}
	return fmt.Sprintf("%s/profile/%d/", s.ngsBase, id)
}

// authorKey — идентичность человека для слияния рубрик: номер строки. NULL
// (зеркальный комментатор без анкеты, аноним) даёт пустой ключ, и в «новые
// лица» такой человек не идёт — опознать его не по чему.
func authorKey(id *int64) string {
	if id == nil {
		return ""
	}
	return strconv.FormatInt(*id, 10)
}

// commentColumns — общая выборка комментария. Ник берётся ТЕКУЩИЙ из users, а
// author_display остаётся фолбэком: у зеркального комментатора без ссылки на
// анкету другого имени нет.
const commentColumns = `
	c.id, c.note_id, c.author_id,
	COALESCE(NULLIF(u.nick, ''), c.author_display) AS name,
	c.body, c.published_at`

// noteColumns — общая выборка заметки с маскированием анонима: настоящий автор
// не покидает базу (см. шапку пакета).
const noteColumns = `
	n.id,
	CASE WHEN n.anonymous THEN NULL ELSE n.author_id END,
	CASE WHEN n.anonymous THEN '` + platform.AnonNick + `'
	     ELSE COALESCE(NULLIF(u.nick, ''), '') END AS name,
	n.body, n.published_at`

// commentsWindowQuery — комментарии окна. Константой, а не строкой в вызове,
// потому что план ЭТОГО запроса проверяется тестом: без индекса по времени
// недельное окно превращается в чтение таблицы на 3 ГБ.
const commentsWindowQuery = `
	SELECT ` + commentColumns + `
	FROM comments c LEFT JOIN users u ON u.id = c.author_id
	WHERE c.status = 0 AND c.published_at > $1 AND c.published_at <= $2
	ORDER BY c.note_id, c.id`

// CommentsBetween — комментарии окна (start, end].
func (s *Source) CommentsBetween(ctx context.Context, start, end time.Time) ([]digest.Comment, error) {
	rows, err := s.pool.Query(ctx, commentsWindowQuery, start, end)
	if err != nil {
		return nil, fmt.Errorf("комментарии окна: %w", err)
	}
	return collectComments(rows)
}

func collectComments(rows pgx.Rows) ([]digest.Comment, error) {
	defer rows.Close()
	var out []digest.Comment
	for rows.Next() {
		var (
			c        digest.Comment
			noteID   int64
			authorID *int64
		)
		if err := rows.Scan(&c.ID, &noteID, &authorID, &c.AuthorName, &c.Text, &c.PublishedAt); err != nil {
			return nil, err
		}
		c.NoteID = strconv.FormatInt(noteID, 10)
		c.Author = authorKey(authorID)
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotesByIDs — шапки заметок по списку id.
func (s *Source) NotesByIDs(ctx context.Context, ids []string) (map[string]digest.Note, error) {
	out := make(map[string]digest.Note, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	nums := make([]int64, 0, len(ids))
	for _, id := range ids {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue // чужой формат id в выпуск не попадает
		}
		nums = append(nums, n)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+noteColumns+`
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.status = 0 AND n.id = ANY($1)`, nums)
	if err != nil {
		return nil, fmt.Errorf("шапки заметок: %w", err)
	}
	notes, err := collectNotes(rows)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		out[n.ID] = n
	}
	return out, nil
}

// NotesPublishedBetween — заметки, появившиеся в окне.
func (s *Source) NotesPublishedBetween(ctx context.Context, start, end time.Time) ([]digest.Note, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+noteColumns+`
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.status = 0 AND n.published_at > $1 AND n.published_at <= $2
		ORDER BY n.published_at`, start, end)
	if err != nil {
		return nil, fmt.Errorf("заметки окна: %w", err)
	}
	return collectNotes(rows)
}

// ActiveNotesSince — заметки, обсуждение которых ещё живо.
func (s *Source) ActiveNotesSince(ctx context.Context, since time.Time) ([]digest.Note, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+noteColumns+`
		FROM notes n LEFT JOIN users u ON u.id = n.author_id
		WHERE n.status = 0 AND n.last_comment_at > $1
		ORDER BY n.last_comment_at DESC`, since)
	if err != nil {
		return nil, fmt.Errorf("живые заметки: %w", err)
	}
	return collectNotes(rows)
}

func collectNotes(rows pgx.Rows) ([]digest.Note, error) {
	defer rows.Close()
	var out []digest.Note
	for rows.Next() {
		var (
			n        digest.Note
			id       int64
			authorID *int64
		)
		if err := rows.Scan(&id, &authorID, &n.AuthorName, &n.Text, &n.PublishedAt); err != nil {
			return nil, err
		}
		n.ID = strconv.FormatInt(id, 10)
		n.Author = authorKey(authorID)
		out = append(out, n)
	}
	return out, rows.Err()
}

// commenterHistoryQuery — комментаторы окна и их прошлое. Константой по той же
// причине, что и окно: план проверяется тестом, а LATERAL здесь обязан идти
// по индексу (author_id, published_at DESC) — у площадки есть участники со
// 138 тысячами реплик.
const commenterHistoryQuery = `
	WITH win AS (
		SELECT c.author_id, count(*) AS cnt, min(c.published_at) AS first_in_win
		FROM comments c
		WHERE c.status = 0 AND c.author_id IS NOT NULL
		  AND c.published_at > $1 AND c.published_at <= $2
		GROUP BY c.author_id)
	SELECT w.author_id, COALESCE(NULLIF(u.nick, ''), ''), w.cnt, w.first_in_win, prev.published_at
	FROM win w
	JOIN users u ON u.id = w.author_id
	LEFT JOIN LATERAL (
		SELECT c2.published_at FROM comments c2
		WHERE c2.author_id = w.author_id AND c2.status = 0 AND c2.published_at <= $1
		ORDER BY c2.published_at DESC LIMIT 1) prev ON true
	ORDER BY w.cnt DESC, w.author_id`

// CommenterHistory — комментаторы окна и их прошлое до окна.
//
// Прошлое берётся LATERAL-подзапросом с LIMIT 1 по индексу
// (author_id, published_at DESC): у площадки есть участники со 138 тысячами
// реплик, и агрегат max() по ним означал бы чтение всей истории каждого.
func (s *Source) CommenterHistory(ctx context.Context, start, end time.Time) ([]digest.CommenterSeen, error) {
	rows, err := s.pool.Query(ctx, commenterHistoryQuery, start, end)
	if err != nil {
		return nil, fmt.Errorf("история комментаторов: %w", err)
	}
	defer rows.Close()
	var out []digest.CommenterSeen
	for rows.Next() {
		var (
			cs   digest.CommenterSeen
			id   int64
			prev *time.Time
		)
		if err := rows.Scan(&id, &cs.Name, &cs.InWindow, &cs.FirstInWindow, &prev); err != nil {
			return nil, err
		}
		cs.Author = strconv.FormatInt(id, 10)
		if prev != nil {
			cs.PrevSeenAt = *prev
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// NoteAuthorHistory — авторы заметок окна и их прошлое. Анонимные заметки не
// участвуют: их автор не раскрывается никому, а «новое лицо» — это про лицо.
func (s *Source) NoteAuthorHistory(ctx context.Context, start, end time.Time) ([]digest.AuthorSeen, error) {
	rows, err := s.pool.Query(ctx, `
		WITH win AS (
			SELECT n.author_id, count(*) AS cnt
			FROM notes n
			WHERE n.status = 0 AND NOT n.anonymous AND n.author_id IS NOT NULL
			  AND n.published_at > $1 AND n.published_at <= $2
			GROUP BY n.author_id)
		SELECT w.author_id, COALESCE(NULLIF(u.nick, ''), ''), w.cnt, prev.published_at
		FROM win w
		JOIN users u ON u.id = w.author_id
		LEFT JOIN LATERAL (
			SELECT n2.published_at FROM notes n2
			WHERE n2.author_id = w.author_id AND n2.status = 0 AND NOT n2.anonymous
			  AND n2.published_at <= $1
			ORDER BY n2.published_at DESC LIMIT 1) prev ON true
		ORDER BY w.cnt DESC, w.author_id`, start, end)
	if err != nil {
		return nil, fmt.Errorf("история авторов: %w", err)
	}
	defer rows.Close()
	var out []digest.AuthorSeen
	for rows.Next() {
		var (
			as   digest.AuthorSeen
			id   int64
			prev *time.Time
		)
		if err := rows.Scan(&id, &as.Name, &as.NotesInWindow, &prev); err != nil {
			return nil, err
		}
		as.Author = strconv.FormatInt(id, 10)
		if prev != nil {
			as.PrevNoteAt = *prev
		}
		out = append(out, as)
	}
	return out, rows.Err()
}

// NoteTotals — итоги обсуждений заметок горизонта.
func (s *Source) NoteTotals(ctx context.Context, since time.Time) ([]digest.NoteTotals, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.note_id, n.published_at, count(*),
		       count(DISTINCT c.author_id),
		       min(c.published_at), max(c.published_at)
		FROM comments c JOIN notes n ON n.id = c.note_id
		WHERE c.status = 0 AND n.status = 0
		  AND c.published_at >= $1 AND n.published_at >= $1
		GROUP BY c.note_id, n.published_at
		ORDER BY n.published_at`, since)
	if err != nil {
		return nil, fmt.Errorf("итоги тредов: %w", err)
	}
	defer rows.Close()
	var out []digest.NoteTotals
	for rows.Next() {
		var (
			t      digest.NoteTotals
			noteID int64
		)
		if err := rows.Scan(&noteID, &t.PublishedAt, &t.Comments, &t.Commenters,
			&t.FirstAt, &t.LastAt); err != nil {
			return nil, err
		}
		t.NoteID = strconv.FormatInt(noteID, 10)
		out = append(out, t)
	}
	return out, rows.Err()
}

// PeakCommentHour — самый плотный календарный час одного треда за горизонт.
func (s *Source) PeakCommentHour(ctx context.Context, since time.Time) (time.Time, string, int, error) {
	var (
		noteID int64
		hour   time.Time
		n      int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT note_id, date_trunc('hour', published_at) AS h, count(*) AS n
		FROM comments
		WHERE status = 0 AND published_at >= $1
		GROUP BY note_id, h
		ORDER BY n DESC, h DESC
		LIMIT 1`, since).Scan(&noteID, &hour, &n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, "", 0, nil
		}
		return time.Time{}, "", 0, fmt.Errorf("пик-час: %w", err)
	}
	return hour.UTC(), strconv.FormatInt(noteID, 10), n, nil
}
