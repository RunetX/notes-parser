package narod

// Отложенные намерения и ход разговора.
//
// Персонаж, решивший ответить через сорок минут, обязан ответить через сорок
// минут — и после рестарта демона тоже. Поэтому намерение лежит в базе, а не в
// памяти процесса: живёт оно дольше, чем такт службы, и потерянное намерение
// неотличимо от «передумал», хотя это разные вещи.
//
// Переходы состояния — CAS, тем же приёмом, что у амвона. Строка уходит в
// posting ДО генерации и отправки, поэтому застрявшая в posting не
// переотправляется никогда: сайт мог реплику и принять, ответив ошибкой, а
// вторая копия под чужим именем хуже пропавшей.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrPlanTaken — план уже взят другим тактом (или уже отработан). Не авария:
// именно так CAS и сообщает, что работать не надо.
var ErrPlanTaken = errors.New("план уже взят")

// Plan — намерение заговорить.
type Plan struct {
	ID        int64
	ActorID   string
	EventID   string // что вызвало намерение; ключ однократности вместе с актором
	NoteID    int64
	ReplyTo   int64  // 0 — реплика первого уровня, к самой заметке
	Target    string // актор, которому отвечаем; пусто — никому конкретно
	DueAt     time.Time
	State     string
	Reason    string
	CreatedAt time.Time
}

// Plan заводит намерение. Идемпотентна по (актор, событие): такт, увидевший
// одно и то же событие дважды, не должен планировать два прихода. Второе
// значение — «завели только что».
func (w *World) PlanReply(ctx context.Context, p Plan, now time.Time) (int64, bool, error) {
	if p.ActorID == "" || p.EventID == "" {
		return 0, false, fmt.Errorf("план без актора или события")
	}
	res, err := w.db.ExecContext(ctx, `
		INSERT INTO plans (actor_id, event_id, note_id, reply_to_comment_id,
		                   target_actor, due_at, state, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(actor_id, event_id) DO NOTHING`,
		p.ActorID, p.EventID, p.NoteID, p.ReplyTo, p.Target,
		fmtTime(p.DueAt), PlanQueued, p.Reason, fmtTime(now))
	if err != nil {
		return 0, false, fmt.Errorf("план %s/%s: %w", p.ActorID, p.EventID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		id, err := res.LastInsertId()
		return id, true, err
	}
	var id int64
	err = w.db.QueryRowContext(ctx, `
		SELECT id FROM plans WHERE actor_id = ? AND event_id = ?`,
		p.ActorID, p.EventID).Scan(&id)
	return id, false, err
}

// DuePlans — намерения, которым пора, от самых просроченных.
//
// Просрочку НЕ отсекаем здесь: решение «слишком поздно, уже неуместно» — это
// правило службы, а не хранилища, и принимать его должен тот, кто знает, что
// творится в треде.
func (w *World) DuePlans(ctx context.Context, now time.Time, limit int) ([]Plan, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, actor_id, event_id, note_id, reply_to_comment_id, target_actor,
		       due_at, state, reason, created_at
		  FROM plans WHERE state = ? AND due_at <= ?
		 ORDER BY due_at, id LIMIT ?`, PlanQueued, fmtTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TakePlan переводит намерение queued → posting и тем закрепляет его за собой.
//
// Возвращает ErrPlanTaken, если строку уже взяли: это и есть защита от второй
// копии реплики. Проверка и переход — одним UPDATE с условием по состоянию, а
// не «прочитали, решили, записали»: между чтением и записью успевает вклиниться
// соседний такт.
func (w *World) TakePlan(ctx context.Context, id int64) error {
	res, err := w.db.ExecContext(ctx, `
		UPDATE plans SET state = ? WHERE id = ? AND state = ?`,
		PlanPosting, id, PlanQueued)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPlanTaken
	}
	return nil
}

// FinishPlan закрывает намерение — сделанным либо брошенным с причиной.
//
// Причина обязательна у брошенного и хранится: «промолчал» и «сломалось» через
// месяц отличимы только по ней, а требование различать их стоит в DoD брифа.
func (w *World) FinishPlan(ctx context.Context, id int64, state, reason string) error {
	if state != PlanDone && state != PlanDropped {
		return fmt.Errorf("план %d: некуда закрывать в %q", id, state)
	}
	if state == PlanDropped && reason == "" {
		return fmt.Errorf("план %d брошен без причины", id)
	}
	_, err := w.db.ExecContext(ctx, `
		UPDATE plans SET state = ?, reason = ? WHERE id = ?`, state, reason, id)
	return err
}

// PlansOf — намерения актора, от новых к старым; для отчёта и разбора глазами.
func (w *World) PlansOf(ctx context.Context, actorID string, limit int) ([]Plan, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, actor_id, event_id, note_id, reply_to_comment_id, target_actor,
		       due_at, state, reason, created_at
		  FROM plans WHERE actor_id = ? ORDER BY id DESC LIMIT ?`, actorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Thread — что мир знает о ходе разговора.
type Thread struct {
	NoteID     int64
	State      string // live | closed
	Rounds     int
	PersonaN   int // сколько реплик народа в треде уже стоит
	LastActive time.Time
}

// Состояния треда.
const (
	ThreadLive   = "live"
	ThreadClosed = "closed"
)

// TouchThread отмечает движение в треде и возвращает, что вышло.
//
// Круг (rounds) считается ЗДЕСЬ, потому что затухание — свойство разговора, а не
// персонажа: каждому следующему кругу положено быть тише предыдущего, и
// спрашивать об этом будут все жители сразу.
func (w *World) TouchThread(ctx context.Context, noteID int64, byPersona bool, now time.Time) (Thread, error) {
	persona, rounds := 0, 1
	if byPersona {
		persona = 1
	}
	_, err := w.db.ExecContext(ctx, `
		INSERT INTO threads (note_id, state, rounds, persona_replies, last_activity_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(note_id) DO UPDATE SET
		    rounds = threads.rounds + ?,
		    persona_replies = threads.persona_replies + ?,
		    last_activity_at = excluded.last_activity_at`,
		noteID, ThreadLive, rounds, persona, fmtTime(now), rounds, persona)
	if err != nil {
		return Thread{}, fmt.Errorf("тред %d: %w", noteID, err)
	}
	return w.ThreadOf(ctx, noteID)
}

// ThreadOf — состояние треда. Незнакомый тред — не ошибка: мир видит его
// впервые, и это штатное начало разговора.
func (w *World) ThreadOf(ctx context.Context, noteID int64) (Thread, error) {
	t := Thread{NoteID: noteID, State: ThreadLive}
	var at string
	err := w.db.QueryRowContext(ctx, `
		SELECT state, rounds, persona_replies, last_activity_at
		  FROM threads WHERE note_id = ?`, noteID).
		Scan(&t.State, &t.Rounds, &t.PersonaN, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return t, nil
	}
	if err != nil {
		return Thread{}, err
	}
	t.LastActive, _ = time.Parse(time.RFC3339, at)
	return t, nil
}

// CloseThread объявляет разговор законченным: после этого приходить в него
// поздно, а графу пора обновиться по его итогам.
func (w *World) CloseThread(ctx context.Context, noteID int64) error {
	_, err := w.db.ExecContext(ctx, `
		UPDATE threads SET state = ? WHERE note_id = ?`, ThreadClosed, noteID)
	return err
}

// StaleThreads — живые треды, в которых давно тихо. Ими и кормится обновление
// графа: разбирать разговор имеет смысл, когда он кончился.
func (w *World) StaleThreads(ctx context.Context, before time.Time, limit int) ([]Thread, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT note_id, state, rounds, persona_replies, last_activity_at
		  FROM threads WHERE state = ? AND last_activity_at <= ?
		 ORDER BY last_activity_at LIMIT ?`, ThreadLive, fmtTime(before), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Thread
	for rows.Next() {
		var t Thread
		var at string
		if err := rows.Scan(&t.NoteID, &t.State, &t.Rounds, &t.PersonaN, &at); err != nil {
			return nil, err
		}
		t.LastActive, _ = time.Parse(time.RFC3339, at)
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanPlan(rows *sql.Rows) (Plan, error) {
	var p Plan
	var due, created string
	if err := rows.Scan(&p.ID, &p.ActorID, &p.EventID, &p.NoteID, &p.ReplyTo,
		&p.Target, &due, &p.State, &p.Reason, &created); err != nil {
		return Plan{}, err
	}
	p.DueAt, _ = time.Parse(time.RFC3339, due)
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return p, nil
}
