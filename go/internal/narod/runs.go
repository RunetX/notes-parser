package narod

// Журнал генерации, курсор и расход — три вещи, которые служба обязана помнить
// между тактами и между запусками.
//
// GEN_RUNS — не отладочный лог. DoD эпика требует, чтобы «промолчал» и
// «сломалось» различались на глаз: первое — поведение, второе — авария, и через
// месяц отличить их по одной пустой ленте будет нечем. Поэтому каждая попытка
// пишется со своим исходом и причиной, даже когда причина — «модель сказала,
// что ей нечего сказать».
//
// КУРСОР отделён от планов сознательно: план говорит «этот житель собирается
// ответить», курсор — «докуда служба вообще досмотрела». Потеряв первое, мы
// теряем одну реплику; потеряв второе, служба прочтёт заметку заново и бросит
// монетки повторно — а вот это уже не восстановимо, потому что монетка на пару
// (актор, событие) бросается ровно один раз.
//
// РАСХОД считается ПО ФАКТАМ, а не по намерениям: каскад «житель отвечает
// жителю» умеет разгоняться сам, и потолок, считающий планы, пропустил бы
// шторм, у которого половина планов сорвалась.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Исходы генерации.
const (
	GenPosted  = "posted"  // реплика ушла на площадку
	GenSkipped = "skipped" // житель промолчал: так решила модель или так вышло
	GenDropped = "dropped" // сломалось — сеть, модель, площадка
)

// GenRun — одна попытка заговорить.
type GenRun struct {
	ID       int64
	PlanID   int64
	ActorID  string
	At       time.Time
	Provider string
	Model    string
	Drafts   int
	Verdict  string
	Reason   string
	Text     string
	Rejects  []string
}

// RecordGenRun пишет попытку в журнал.
func (w *World) RecordGenRun(ctx context.Context, r GenRun) (int64, error) {
	evals, err := json.Marshal(r.Rejects)
	if err != nil {
		return 0, err
	}
	res, err := w.db.ExecContext(ctx, `
		INSERT INTO gen_runs (plan_id, actor_id, at, provider, model, drafts,
		                      verdict, drop_reason, text_final, evals)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullID(r.PlanID), r.ActorID, fmtTime(r.At), r.Provider, r.Model, r.Drafts,
		r.Verdict, r.Reason, r.Text, string(evals))
	if err != nil {
		return 0, fmt.Errorf("журнал генерации %s: %w", r.ActorID, err)
	}
	return res.LastInsertId()
}

// GenRuns — последние попытки, от новых к старым. Ими и отвечают на вопрос
// «почему в песочнице тихо».
func (w *World) GenRuns(ctx context.Context, limit int) ([]GenRun, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, coalesce(plan_id, 0), actor_id, at, provider, model, drafts,
		       verdict, drop_reason, text_final, evals
		  FROM gen_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GenRun
	for rows.Next() {
		var r GenRun
		var at, evals string
		if err := rows.Scan(&r.ID, &r.PlanID, &r.ActorID, &at, &r.Provider, &r.Model,
			&r.Drafts, &r.Verdict, &r.Reason, &r.Text, &evals); err != nil {
			return nil, err
		}
		r.At, _ = time.Parse(time.RFC3339, at)
		_ = json.Unmarshal([]byte(evals), &r.Rejects)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Cursor — докуда служба досмотрела по ключу k. Отсутствие ключа не ошибка, а
// ноль: мир, который только завели, не видел ещё ничего.
func (w *World) Cursor(ctx context.Context, k string) (int64, error) {
	var v int64
	err := w.db.QueryRowContext(ctx, `SELECT v FROM cursor WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// SetCursor двигает курсор. Только ВПЕРЁД: такт, отработавший не по порядку, не
// должен заставлять службу перечитывать уже прочитанное — второй раз она бросала
// бы монетки, которые бросаются один раз за жизнь события.
func (w *World) SetCursor(ctx context.Context, k string, v int64) error {
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO cursor (k, v) VALUES (?, ?)
		ON CONFLICT(k) DO UPDATE SET v = max(cursor.v, excluded.v)`, k, v)
	return err
}

// AddSpend прибавляет расход дня и возвращает, сколько вызовов за день вышло.
//
// День берётся в UTC, и это не мелочь: суточный потолок обязан обнуляться в один
// и тот же момент независимо от того, где стоит хост, — а «полночь по местному»
// у процесса, переехавшего на другую машину, сдвинулась бы молча.
func (w *World) AddSpend(ctx context.Context, at time.Time, calls, in, out int) (int, error) {
	day := at.UTC().Format("2006-01-02")
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO llm_spend (day, calls, in_tokens, out_tokens) VALUES (?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET
		    calls = llm_spend.calls + excluded.calls,
		    in_tokens = llm_spend.in_tokens + excluded.in_tokens,
		    out_tokens = llm_spend.out_tokens + excluded.out_tokens`,
		day, calls, in, out); err != nil {
		return 0, fmt.Errorf("расход за %s: %w", day, err)
	}
	return w.SpentOn(ctx, at)
}

// SpentOn — сколько вызовов к модели сделано в этот день.
func (w *World) SpentOn(ctx context.Context, at time.Time) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx,
		`SELECT calls FROM llm_spend WHERE day = ?`, at.UTC().Format("2006-01-02")).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// SaidInThread — сколько реплик житель уже сказал в этой заметке. Считается по
// ЖУРНАЛУ, а не по треду площадки: журнал пишется ДО публикации, поэтому в него
// попадает и то, что сейчас в пути, — а потолок, считающий только опубликованное,
// пропустил бы залп из трёх планов, созревших одновременно.
func (w *World) SaidInThread(ctx context.Context, actorID string, noteID int64) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `
		SELECT count(*) FROM journal
		 WHERE actor_id = ? AND note_id = ? AND kind = ?`,
		actorID, noteID, JournalComment).Scan(&n)
	return n, err
}

// SaidSince — сколько реплик житель сказал начиная с момента from. Ею считаются
// оба личных потолка, часовой и суточный: вопрос у них один и тот же, разное
// только окно.
func (w *World) SaidSince(ctx context.Context, actorID string, from time.Time) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `
		SELECT count(*) FROM journal
		 WHERE actor_id = ? AND kind = ? AND at >= ?`,
		actorID, JournalComment, fmtTime(from)).Scan(&n)
	return n, err
}

// SpokeInThread — номера реплик площадки, которые в этом треде поставили мы.
// Нужны каскаду: сказанное жителем — точка решения всем ОСТАЛЬНЫМ, и своё же
// эхо служба обязана узнавать, иначе она ответит сама себе.
func (w *World) SpokeInThread(ctx context.Context, noteID int64) (map[int64]string, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT comment_id, actor_id FROM journal
		 WHERE note_id = ? AND kind = ? AND comment_id <> 0`, noteID, JournalComment)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var actor string
		if err := rows.Scan(&id, &actor); err != nil {
			return nil, err
		}
		out[id] = actor
	}
	return out, rows.Err()
}

// eventKey — ключ монетки и плана. Строкой, а не числом, потому что событий два
// сорта («заметка вышла» и «прилетела реплика»), и склеивать их в одну числовую
// полосу значило бы однажды перепутать.
func eventKey(kind string, id int64) string {
	return kind + ":" + strings.TrimSpace(fmt.Sprint(id))
}
