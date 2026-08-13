package modwatch

// Наблюдение за присутствием людей на сайте — вторая половина детектора
// запретов (первая, love.Activity, объясняет, откуда берётся отметка).
//
// Наблюдатель за модерацией видит только то, что ИСЧЕЗЛО: снесённую заметку,
// вычищенную реплику. Запрет писать в «Заметки» не убирает ничего, поэтому в
// его данных он невидим — а это самое частое наказание. Здесь оно и берётся:
// сайт печатает в анкете время последнего действия, и по нему видно, что
// замолчавший человек продолжает ходить. Молчит и ходит — закрыли; молчит и не
// заходит — ушёл сам.
//
// Побочно эта же таблица закрывает старую слепоту отчёта: присутствие мы умели
// считать только по репликам, то есть модератор, который не комментирует, для
// анализа не существовал. Отметки присутствия есть у всех, включая молчунов и
// тех, кто прячет присутствие «Приватностью».

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"lovegw/internal/love"
)

// migrateV4SQL — присутствие анкет.
//
// activity — след «человек был на сайте в такую-то минуту». Ключ «кто + когда»:
// та же отметка, снятая повторно, ничего не меняет, а новая добавляет строку.
// Сайт хранит только ПОСЛЕДНЕЕ действие, поэтому промежуточные заходы между
// нашими опросами теряются — отсюда и своя таблица, копящая их у себя.
//
// profiles — состояние анкеты на последний опрос и очередь обхода: кого
// смотрели давно, того смотрим первым. checked_at NULL — анкета в списке, но
// ещё ни разу не опрошена, такие идут вперёд всех.
const migrateV4SQL = `
CREATE TABLE activity (
    user_id INTEGER NOT NULL,
    at      TEXT    NOT NULL, -- last_activity со страницы (UTC)
    raw     TEXT    NOT NULL DEFAULT '',
    seen_at TEXT    NOT NULL, -- когда мы это увидели
    PRIMARY KEY (user_id, at)
);
CREATE INDEX idx_activity_at ON activity(at);

CREATE TABLE profiles (
    user_id    INTEGER PRIMARY KEY,
    nick       TEXT    NOT NULL DEFAULT '',
    hide_me    INTEGER NOT NULL DEFAULT 0, -- включена «Приватность»
    vip        INTEGER NOT NULL DEFAULT 0,
    missing    INTEGER NOT NULL DEFAULT 0, -- анкеты нет (404 или страница без данных)
    last_at    TEXT,                       -- последняя известная отметка
    checked_at TEXT                        -- когда опрашивали (NULL — ещё ни разу)
);
CREATE INDEX idx_profiles_checked ON profiles(checked_at);
`

// ActivityStamp — отметка присутствия.
type ActivityStamp struct {
	UserID int64
	At     time.Time // когда человек был на сайте
	Raw    string
	SeenAt time.Time // когда мы это сняли
}

// ProfileRow — состояние анкеты на последний опрос.
type ProfileRow struct {
	UserID    int64
	Nick      string
	HideMe    bool
	VIP       bool
	Missing   bool
	LastAt    time.Time
	CheckedAt time.Time // нулевое — ещё не опрашивали
}

// SaveActivity записывает снимок анкеты: отметку присутствия в след и само
// состояние в очередь обхода. true — отметка новая (прежде не видели).
func (s *Store) SaveActivity(ctx context.Context, now time.Time, a love.Activity) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	fresh := false
	if !a.At.IsZero() {
		res, err := tx.ExecContext(ctx, `
            INSERT INTO activity (user_id, at, raw, seen_at) VALUES (?, ?, ?, ?)
            ON CONFLICT(user_id, at) DO NOTHING`,
			a.UserID, ts(a.At), a.Raw, ts(now))
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		fresh = n > 0
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO profiles (user_id, nick, hide_me, vip, missing, last_at, checked_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(user_id) DO UPDATE SET
            nick       = CASE WHEN excluded.nick <> '' THEN excluded.nick ELSE profiles.nick END,
            hide_me    = excluded.hide_me,
            vip        = excluded.vip,
            missing    = excluded.missing,
            last_at    = COALESCE(excluded.last_at, profiles.last_at),
            checked_at = excluded.checked_at`,
		a.UserID, a.Nick, boolInt(a.HideMe), boolInt(a.VIP), boolInt(a.Missing),
		nullTS(a.At), ts(now)); err != nil {
		return false, err
	}
	return fresh, tx.Commit()
}

// TrackProfiles заводит анкеты в очередь обхода, не опрашивая их. Уже
// заведённых не трогает — иначе обход сбрасывал бы себе очередь на каждом такте.
func (s *Store) TrackProfiles(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	added := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO profiles (user_id) VALUES (?) ON CONFLICT(user_id) DO NOTHING`, id)
		if err != nil {
			return added, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			added++
		}
	}
	return added, tx.Commit()
}

// ProfilesDue возвращает анкеты, которые пора опросить: сначала ни разу не
// виденные, дальше самые давние. Удалённые (missing) из обхода не выпадают:
// анкету возвращают из блокировки, и это ровно то событие, которое интересно.
func (s *Store) ProfilesDue(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT user_id FROM profiles
         ORDER BY checked_at IS NOT NULL, checked_at
         LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Profiles возвращает состояние всех известных анкет.
func (s *Store) Profiles(ctx context.Context) (map[int64]ProfileRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, nick, hide_me, vip, missing, last_at, checked_at FROM profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]ProfileRow{}
	for rows.Next() {
		var (
			p                  ProfileRow
			hide, vip, missing int
			lastAt, checkedAt  sql.NullString
		)
		if err := rows.Scan(&p.UserID, &p.Nick, &hide, &vip, &missing, &lastAt, &checkedAt); err != nil {
			return nil, err
		}
		p.HideMe, p.VIP, p.Missing = hide != 0, vip != 0, missing != 0
		p.LastAt, p.CheckedAt = parseTS(lastAt.String), parseTS(checkedAt.String)
		out[p.UserID] = p
	}
	return out, rows.Err()
}

// ActivityIn возвращает отметки присутствия за период, от старых к новым.
// user > 0 — только по одной анкете. Это ответ на вопрос «кто был на сайте в
// минуту действия» для тех, кто в эту минуту ничего не писал.
func (s *Store) ActivityIn(ctx context.Context, user int64, since, until time.Time) ([]ActivityStamp, error) {
	q := `SELECT user_id, at, raw, seen_at FROM activity WHERE 1=1`
	var args []any
	if user > 0 {
		q += " AND user_id = ?"
		args = append(args, user)
	}
	if !since.IsZero() {
		q += " AND at >= ?"
		args = append(args, ts(since))
	}
	if !until.IsZero() {
		q += " AND at <= ?"
		args = append(args, ts(until))
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY at", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityStamp
	for rows.Next() {
		var (
			st       ActivityStamp
			at, seen string
		)
		if err := rows.Scan(&st.UserID, &at, &st.Raw, &seen); err != nil {
			return nil, err
		}
		st.At, st.SeenAt = parseTS(at), parseTS(seen)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ActivitySource — то, что обходу нужно от сайта: присутствие одной анкеты.
type ActivitySource interface {
	Activity(ctx context.Context, id int64) (love.Activity, error)
}

// RosterSource — откуда берётся круг наблюдения: кто писал за окно и когда
// замолчал. Реализуется поверх зеркала (боевая БД) либо поверх своих
// комментариев (Store.Commenters).
type RosterSource interface {
	Commenters(ctx context.Context, since time.Time, minComments int) ([]Commenter, error)
}

// Значения по умолчанию для обхода анкет.
//
// Такт мелкий, а порция небольшая, потому что дорог не объём, а ровный темп:
// DDoS-Guard режет серии. Сорок анкет за десять минут — это запрос раз в
// пятнадцать секунд; круг в двести человек обходится за час, то есть по
// каждому есть часовая сетка отметок. Точность самой отметки от этого не
// зависит: сайт печатает минуту, когда человек был на сайте, а не «был час
// назад».
const (
	DefaultActivityInterval = 10 * time.Minute
	DefaultActivityBatch    = 40
	DefaultActivityWindow   = 30 * 24 * time.Hour
	DefaultActivityMin      = 20 // реплик за окно, чтобы попасть в круг
)

// ActivityWatcher — обход анкет за присутствием.
type ActivityWatcher struct {
	Source ActivitySource
	Roster RosterSource // nil — круг не пополняется, ходим по уже заведённым
	Store  *Store
	Log    *slog.Logger

	Interval    time.Duration // такт
	Batch       int           // сколько анкет за такт (0 — все)
	Window      time.Duration // окно активности для набора круга
	MinComments int           // порог реплик для набора круга
	Always      []int64       // кого опрашивать каждый такт (подозреваемые)

	Now  func() time.Time
	Pace func(ctx context.Context) error // пауза между запросами; nil — без неё
}

func (w *ActivityWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *ActivityWatcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *ActivityWatcher) defaults() {
	if w.Interval <= 0 {
		w.Interval = DefaultActivityInterval
	}
	if w.Window <= 0 {
		w.Window = DefaultActivityWindow
	}
	if w.MinComments <= 0 {
		w.MinComments = DefaultActivityMin
	}
}

// Run крутит обход до отмены контекста. Ошибка такта его не роняет: 403
// DDoS-Guard отпускает сам, а пропущенный такт стоит недорого — очередь
// обхода помнит, кого не досмотрели.
func (w *ActivityWatcher) Run(ctx context.Context) error {
	w.defaults()
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		if _, err := w.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.log().Warn("обход анкет не доведён", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Poll — один такт: пополнить круг и опросить порцию анкет. Возвращает число
// новых отметок присутствия.
func (w *ActivityWatcher) Poll(ctx context.Context) (int, error) {
	w.defaults()
	if err := w.refreshRoster(ctx); err != nil {
		return 0, err
	}
	due, err := w.Store.ProfilesDue(ctx, w.Batch)
	if err != nil {
		return 0, err
	}
	// Подозреваемые идут первыми и каждый такт, остальные — по очереди.
	return w.sweep(ctx, dedupIDs(append(append([]int64{}, w.Always...), due...)))
}

// refreshRoster пополняет круг наблюдения активными комментаторами. Никого не
// выбрасывает: замолчавший выпадает из окна активности сам, а следить за ним
// как раз и надо — молчание и есть предмет наблюдения.
func (w *ActivityWatcher) refreshRoster(ctx context.Context) error {
	if len(w.Always) > 0 {
		if _, err := w.Store.TrackProfiles(ctx, w.Always); err != nil {
			return err
		}
	}
	if w.Roster == nil {
		return nil
	}
	people, err := w.Roster.Commenters(ctx, w.now().Add(-w.Window), w.MinComments)
	if err != nil {
		return err
	}
	ids := make([]int64, 0, len(people))
	for _, p := range people {
		ids = append(ids, p.UserID)
	}
	added, err := w.Store.TrackProfiles(ctx, ids)
	if err != nil {
		return err
	}
	if added > 0 {
		w.log().Info("в круг наблюдения добавлены анкеты", "сколько", added, "круг", len(ids))
	}
	return nil
}

// sweep опрашивает анкеты по списку. Сбой обрывает такт: 403 приходит волной,
// и упереться в неё всем списком — верный способ получить её надолго. Остаток
// доберётся следующим тактом, очередь помнит, кого не смотрели.
func (w *ActivityWatcher) sweep(ctx context.Context, ids []int64) (int, error) {
	fresh := 0
	for i, id := range ids {
		if i > 0 && i%25 == 0 {
			w.log().Info("обход анкет", "готово", i, "из", len(ids), "новых отметок", fresh)
		}
		if w.Pace != nil {
			if err := w.Pace(ctx); err != nil {
				return fresh, err
			}
		}
		a, err := w.Source.Activity(ctx, id)
		if err != nil {
			return fresh, err
		}
		isNew, err := w.Store.SaveActivity(ctx, w.now(), a)
		if err != nil {
			return fresh, err
		}
		if isNew {
			fresh++
		}
	}
	return fresh, nil
}

func dedupIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Commenters — круг наблюдения по своим же комментариям. Запасной источник:
// у наблюдателя коротка история (он копит с первого запуска), поэтому основной
// круг берётся из зеркала, где реплики лежат с самого начала мирроринга.
func (s *Store) Commenters(ctx context.Context, since time.Time, minComments int) ([]Commenter, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT c.author_id,
               COALESCE(u.name, MAX(c.author_name)),
               COUNT(*),
               MAX(c.published_at)
          FROM comments c
          LEFT JOIN users u ON u.id = c.author_id
         WHERE c.author_id <> 0 AND c.published_at >= ?
         GROUP BY c.author_id
        HAVING COUNT(*) >= ?`, ts(since), minComments)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commenter
	for rows.Next() {
		var (
			c    Commenter
			last string
		)
		if err := rows.Scan(&c.UserID, &c.Nick, &c.Comments, &last); err != nil {
			return nil, err
		}
		c.LastComment = parseTS(last)
		out = append(out, c)
	}
	return out, rows.Err()
}
