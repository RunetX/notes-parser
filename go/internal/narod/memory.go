package narod

// Память мира: журнал, граф отношений и эпизоды-«счёты».
//
// Разделение на три вещи не техническое, а смысловое, и держится оно на вопросе,
// на который каждая отвечает. ЖУРНАЛ — «что я сам говорил и делал»: без него
// персонаж противоречит себе через два треда. ГРАФ — «как я к тебе отношусь»:
// числа на паре, которые решают, отвечать ли и каким тоном. ЭПИЗОД — «а ПОЧЕМУ
// я к тебе так отношусь»: конкретный случай со ссылкой на реплики.
//
// Без третьего «а помнишь, как ты…» взяться неоткуда: шкала помнит итог, но не
// повод, а сообщество состоит как раз из поводов.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Сорта записи в журнале.
const (
	JournalComment = "comment"
	JournalNote    = "note"
	JournalInner   = "inner_event" // то, что случилось с персонажем вне площадки
)

// Сорта эпизодов. Список закрытый и живёт ЗДЕСЬ, а не в промпте: модель,
// которой позволено придумывать себе виды отношений, через десяток тредов
// заведёт «взаимное уважение с оттенком иронии», и сравнивать миры между собой
// станет нечем. Тот же довод, что у platform.AutoHideable.
const (
	EpisodeTease  = "поддел"
	EpisodeDefend = "вступился"
	EpisodeFight  = "сцепились"
	EpisodeAgree  = "согласились"
	EpisodeDigest = "digest" // сжатая выжимка старых эпизодов
)

// EpisodeKinds — все виды, кроме служебной выжимки: их и предлагают модели.
var EpisodeKinds = []string{EpisodeTease, EpisodeDefend, EpisodeFight, EpisodeAgree}

// EdgeScale — предел шкал отношений. Жёсткий потолок нужен затем, что пара,
// поговорившая двадцать раз, иначе уходит в бесконечность и перестаёт
// реагировать на новое: у отношения, где симпатия равна сорока, следующая ссора
// уже ничего не меняет.
const EdgeScale = 10.0

// JournalEntry — одна запись личной памяти.
type JournalEntry struct {
	ID        int64
	ActorID   string
	At        time.Time
	Kind      string
	NoteID    int64
	CommentID int64
	Text      string
	Meta      string // JSON, свободное поле службы
}

// Remember кладёт запись в журнал и возвращает её номер.
//
// Пишется ДО публикации: реплика, ушедшая на площадку и не попавшая в журнал,
// делает персонажа противоречащим самому себе, а это ровно то, чего эмуляция не
// прощает. Обратный порядок дал бы «сказал и забыл», и починить его потом нечем
// — на площадке текст уже стоит.
func (w *World) Remember(ctx context.Context, e JournalEntry) (int64, error) {
	res, err := w.db.ExecContext(ctx, `
		INSERT INTO journal (actor_id, at, kind, note_id, comment_id, text, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ActorID, fmtTime(e.At), e.Kind, nullID(e.NoteID), e.CommentID, e.Text, e.Meta)
	if err != nil {
		return 0, fmt.Errorf("журнал %s: %w", e.ActorID, err)
	}
	return res.LastInsertId()
}

// Recall — последние n записей персонажа, от новых к старым.
func (w *World) Recall(ctx context.Context, actorID string, n int) ([]JournalEntry, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, actor_id, at, kind, coalesce(note_id, 0), comment_id, text, meta
		  FROM journal WHERE actor_id = ? ORDER BY id DESC LIMIT ?`, actorID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JournalEntry
	for rows.Next() {
		var e JournalEntry
		var at string
		if err := rows.Scan(&e.ID, &e.ActorID, &at, &e.Kind, &e.NoteID, &e.CommentID,
			&e.Text, &e.Meta); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Edge — отношение одного к другому. НАПРАВЛЕННОЕ: симпатия не взаимна по своей
// природе, и пара, где один прощает, а второй помнит, — как раз то, ради чего
// граф и заводится.
type Edge struct {
	Src         string
	Dst         string
	Sympathy    float64 // [-EdgeScale..+EdgeScale]
	Irritation  float64
	Familiarity float64 // сколько раз вообще сталкивались
	UpdatedAt   time.Time
}

// EdgeDelta — на сколько подвинуть отношение.
type EdgeDelta struct {
	Src, Dst    string
	Sympathy    float64
	Irritation  float64
	Familiarity float64
}

// Nudge двигает отношение и возвращает, что получилось.
//
// Прибавка КЛАМПИТСЯ на границах шкалы прямо здесь, а не у зовущего: дельты
// приходят от модели, а модель однажды вернёт двадцать. Правило про пределы
// шкалы обязано жить там, где шкала, иначе оно живёт в дисциплине.
func (w *World) Nudge(ctx context.Context, d EdgeDelta, now time.Time) (Edge, error) {
	if d.Src == "" || d.Dst == "" {
		return Edge{}, fmt.Errorf("ребро без конца: %q → %q", d.Src, d.Dst)
	}
	if d.Src == d.Dst {
		return Edge{}, fmt.Errorf("ребро в себя: %s", d.Src)
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return Edge{}, err
	}
	defer tx.Rollback() //nolint:errcheck // откат после Commit — no-op

	e := Edge{Src: d.Src, Dst: d.Dst}
	var at string
	err = tx.QueryRowContext(ctx, `
		SELECT sympathy, irritation, familiarity, updated_at
		  FROM edges WHERE src = ? AND dst = ?`, d.Src, d.Dst).
		Scan(&e.Sympathy, &e.Irritation, &e.Familiarity, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Edge{}, err
	}
	e.Sympathy = clampScale(e.Sympathy + d.Sympathy)
	e.Irritation = clampScale(e.Irritation + d.Irritation)
	e.Familiarity += d.Familiarity
	e.UpdatedAt = now

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges (src, dst, sympathy, irritation, familiarity, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(src, dst) DO UPDATE SET
		    sympathy = excluded.sympathy, irritation = excluded.irritation,
		    familiarity = excluded.familiarity, updated_at = excluded.updated_at`,
		e.Src, e.Dst, e.Sympathy, e.Irritation, e.Familiarity, fmtTime(now)); err != nil {
		return Edge{}, fmt.Errorf("ребро %s→%s: %w", d.Src, d.Dst, err)
	}
	return e, tx.Commit()
}

// EdgeOf — как src относится к dst. Отсутствие ребра не ошибка, а нулевое
// отношение: незнакомых в мире большинство.
func (w *World) EdgeOf(ctx context.Context, src, dst string) (Edge, error) {
	e := Edge{Src: src, Dst: dst}
	var at string
	err := w.db.QueryRowContext(ctx, `
		SELECT sympathy, irritation, familiarity, updated_at
		  FROM edges WHERE src = ? AND dst = ?`, src, dst).
		Scan(&e.Sympathy, &e.Irritation, &e.Familiarity, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return e, nil
	}
	if err != nil {
		return Edge{}, err
	}
	e.UpdatedAt, _ = time.Parse(time.RFC3339, at)
	return e, nil
}

// Edges — весь граф: им отвечают на вопрос «ушёл ли мир от старта».
func (w *World) Edges(ctx context.Context) ([]Edge, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT src, dst, sympathy, irritation, familiarity, updated_at
		  FROM edges ORDER BY src, dst`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Edge
	for rows.Next() {
		var e Edge
		var at string
		if err := rows.Scan(&e.Src, &e.Dst, &e.Sympathy, &e.Irritation,
			&e.Familiarity, &at); err != nil {
			return nil, err
		}
		e.UpdatedAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Episode — конкретный случай между двумя.
type Episode struct {
	ID         int64
	Src, Dst   string
	At         time.Time
	Kind       string
	Summary    string
	CommentIDs []int64
	NoteID     int64
	Compressed bool
}

// EpisodeSummaryRunes — потолок пересказа. Эпизод обязан помещаться в промпт
// десятком штук, а не вытеснять оттуда сам разговор.
const EpisodeSummaryRunes = 200

// AddEpisode записывает случай. Append-only: эпизод — свидетельство, а
// переписанное свидетельство не свидетельство.
func (w *World) AddEpisode(ctx context.Context, e Episode) (int64, error) {
	if e.Src == "" || e.Dst == "" || e.Src == e.Dst {
		return 0, fmt.Errorf("эпизод без пары: %q → %q", e.Src, e.Dst)
	}
	if !knownEpisodeKind(e.Kind) {
		return 0, fmt.Errorf("эпизод неизвестного вида %q (список закрыт: %v)",
			e.Kind, EpisodeKinds)
	}
	if r := []rune(e.Summary); len(r) > EpisodeSummaryRunes {
		e.Summary = string(r[:EpisodeSummaryRunes])
	}
	ids, err := json.Marshal(e.CommentIDs)
	if err != nil {
		return 0, err
	}
	res, err := w.db.ExecContext(ctx, `
		INSERT INTO episodes (src, dst, at, kind, summary, comment_ids, note_id, compressed)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Src, e.Dst, fmtTime(e.At), e.Kind, e.Summary, string(ids),
		nullID(e.NoteID), boolInt(e.Compressed))
	if err != nil {
		return 0, fmt.Errorf("эпизод %s→%s: %w", e.Src, e.Dst, err)
	}
	return res.LastInsertId()
}

// EpisodesOf — последние случаи между парой, от новых к старым. Они и уходят в
// промпт как тон: шкала говорит «сколько», эпизод — «за что».
func (w *World) EpisodesOf(ctx context.Context, src, dst string, n int) ([]Episode, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, src, dst, at, kind, summary, comment_ids, coalesce(note_id, 0), compressed
		  FROM episodes WHERE src = ? AND dst = ? AND compressed = 0
		 ORDER BY id DESC LIMIT ?`, src, dst, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var at, ids string
		var compressed int
		if err := rows.Scan(&e.ID, &e.Src, &e.Dst, &at, &e.Kind, &e.Summary,
			&ids, &e.NoteID, &compressed); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339, at)
		e.Compressed = compressed != 0
		_ = json.Unmarshal([]byte(ids), &e.CommentIDs)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Dice — брошенная монетка.
type Dice struct {
	ActorID string
	EventID string
	P       float64
	Roll    float64
	Verdict string
	Reason  string
	At      time.Time
}

// Roll записывает монетку ОДИН раз на пару (актор, событие) и возвращает
// сохранённый исход. Второе значение — «записали только что».
//
// Однократность держится первичным ключом, а не проверкой перед вставкой:
// пятнадцать процентов, спрошенные десять раз за десять тактов, превращаются в
// восемьдесят, и урок этот оплачен амвоном (pulpit/reply.go). Гонку двух тактов
// ключ закрывает заодно — второй получит уже записанное, а не бросит свою.
func (w *World) Roll(ctx context.Context, d Dice) (Dice, bool, error) {
	res, err := w.db.ExecContext(ctx, `
		INSERT INTO dice (actor_id, event_id, p, roll, verdict, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(actor_id, event_id) DO NOTHING`,
		d.ActorID, d.EventID, d.P, d.Roll, d.Verdict, d.Reason, fmtTime(d.At))
	if err != nil {
		return Dice{}, false, fmt.Errorf("монетка %s/%s: %w", d.ActorID, d.EventID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return d, true, nil
	}
	got, err := w.DiceOf(ctx, d.ActorID, d.EventID)
	return got, false, err
}

// DiceOf — уже брошенная монетка.
func (w *World) DiceOf(ctx context.Context, actorID, eventID string) (Dice, error) {
	d := Dice{ActorID: actorID, EventID: eventID}
	var at string
	err := w.db.QueryRowContext(ctx, `
		SELECT p, roll, verdict, reason, at FROM dice
		 WHERE actor_id = ? AND event_id = ?`, actorID, eventID).
		Scan(&d.P, &d.Roll, &d.Verdict, &d.Reason, &at)
	if err != nil {
		return Dice{}, err
	}
	d.At, _ = time.Parse(time.RFC3339, at)
	return d, nil
}

func knownEpisodeKind(kind string) bool {
	for _, k := range EpisodeKinds {
		if k == kind {
			return true
		}
	}
	return kind == EpisodeDigest
}

func clampScale(x float64) float64 {
	switch {
	case x > EdgeScale:
		return EdgeScale
	case x < -EdgeScale:
		return -EdgeScale
	}
	return x
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EpisodeCap — сколько случаев пара помнит поимённо. Дальше старые
// сворачиваются в выжимку: в промпт уходят последние, а история пары длиной в
// год иначе вытеснила бы оттуда сам разговор.
const EpisodeCap = 12

// CompactEpisodes сворачивает старые случаи пары в одну выжимку.
//
// Сжатие НИЧЕГО НЕ СТИРАЕТ и ничего не переписывает — оно ПОМЕЧАЕТ: свёрнутые
// строки остаются в таблице как были, просто перестают идти в промпт. Иначе
// пришлось бы выбирать между двумя одинаково плохими вещами: либо промпт растёт
// без предела, либо свидетельство о том, что между людьми было, уничтожается
// ради экономии места.
//
// Выжимку пишет КОД, а не модель, и это тот же довод, по которому знакомство
// считается, а не оценивается: «сцепились четырежды с марта по май» — это
// арифметика, и в ней нельзя ни соврать, ни приукрасить. Модель, пересказавшая
// дюжину поводов одним абзацем, через год давала бы персонажу воспоминание,
// которого не было ни в одном треде.
func (w *World) CompactEpisodes(ctx context.Context, src, dst string, now time.Time) error {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, at, kind FROM episodes
		 WHERE src = ? AND dst = ? AND compressed = 0 AND kind <> ?
		 ORDER BY id DESC`, src, dst, EpisodeDigest)
	if err != nil {
		return err
	}
	type row struct {
		id   int64
		at   time.Time
		kind string
	}
	var all []row
	for rows.Next() {
		var r row
		var at string
		if err := rows.Scan(&r.id, &at, &r.kind); err != nil {
			rows.Close()
			return err
		}
		r.at, _ = time.Parse(time.RFC3339, at)
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(all) <= EpisodeCap {
		return nil
	}
	old := all[EpisodeCap:] // ORDER BY id DESC — значит здесь самые давние

	kinds := map[string]int{}
	first, last := old[len(old)-1].at, old[0].at
	ids := make([]any, 0, len(old))
	for _, r := range old {
		kinds[r.kind]++
		ids = append(ids, r.id)
	}
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", k, kinds[k])
	}
	summary := fmt.Sprintf("%s — с %s по %s",
		b.String(), first.Format("02.01.2006"), last.Format("02.01.2006"))

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // откат после Commit — no-op

	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	if _, err := tx.ExecContext(ctx,
		"UPDATE episodes SET compressed = 1 WHERE id IN ("+marks+")", ids...); err != nil {
		return fmt.Errorf("свёртка %s→%s: %w", src, dst, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO episodes (src, dst, at, kind, summary, comment_ids, note_id, compressed)
		VALUES (?, ?, ?, ?, ?, '[]', NULL, 0)`,
		src, dst, fmtTime(now), EpisodeDigest, summary); err != nil {
		return fmt.Errorf("выжимка %s→%s: %w", src, dst, err)
	}
	return tx.Commit()
}

// Tone — как одно число: [-1..+1], где минус это «раздражает», плюс «нравится».
//
// Одним числом потому, что кубик спрашивает у отношения ровно одно: тянет к
// этому человеку или отталкивает. Обе шкалы при этом остаются раздельными в
// базе — они и правда не концы одной, — но решение «влезать ли в его разговор»
// принимается по их разности, и складывать её каждый раз заново у зовущего
// значило бы завести второе место, где живёт смысл шкал.
func (e Edge) Tone() float64 {
	t := (e.Sympathy - e.Irritation) / EdgeScale
	switch {
	case t > 1:
		return 1
	case t < -1:
		return -1
	}
	return t
}
