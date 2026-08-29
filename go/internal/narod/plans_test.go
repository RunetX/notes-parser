package narod

import (
	"errors"
	"testing"
	"time"
)

// Намерение заводится ОДИН раз на пару (актор, событие): такт, увидевший одно и
// то же событие дважды, не должен планировать два прихода.
func TestPlanReplyIsIdempotent(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	p := Plan{ActorID: "ivan", EventID: "c1", NoteID: 500, DueAt: now.Add(time.Hour)}

	id1, fresh, err := w.PlanReply(ctx, p, now)
	if err != nil || !fresh {
		t.Fatalf("первый план: fresh=%v err=%v", fresh, err)
	}
	id2, fresh, err := w.PlanReply(ctx, p, now)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Error("повтор засчитался как новый план")
	}
	if id1 != id2 {
		t.Errorf("повтор дал другой план: %d против %d", id2, id1)
	}
}

// Взять план может ТОЛЬКО ОДИН такт: это и есть защита от второй копии реплики.
// Проверка и переход — одним UPDATE, иначе между чтением и записью вклинится
// соседний такт.
func TestTakePlanIsExclusive(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	id, _, err := w.PlanReply(ctx, Plan{ActorID: "ivan", EventID: "c1",
		NoteID: 500, DueAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.TakePlan(ctx, id); err != nil {
		t.Fatalf("первый захват: %v", err)
	}
	if err := w.TakePlan(ctx, id); !errors.Is(err, ErrPlanTaken) {
		t.Errorf("второй захват дал %v, ожидалось ErrPlanTaken", err)
	}
}

// Взятый план из очереди пропадает: повторно его никто не увидит.
func TestDuePlansSkipsTaken(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	id, _, err := w.PlanReply(ctx, Plan{ActorID: "ivan", EventID: "c1",
		NoteID: 500, DueAt: now.Add(-time.Minute)}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Намерение на будущее в очередь не идёт вовсе.
	if _, _, err := w.PlanReply(ctx, Plan{ActorID: "olga", EventID: "c2",
		NoteID: 500, DueAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	due, err := w.DuePlans(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ActorID != "ivan" {
		t.Fatalf("в очереди %d планов: %+v", len(due), due)
	}
	if err := w.TakePlan(ctx, id); err != nil {
		t.Fatal(err)
	}
	if due, err = w.DuePlans(ctx, now, 10); err != nil || len(due) != 0 {
		t.Errorf("взятый план остался в очереди: %+v (%v)", due, err)
	}
}

// Брошенный план обязан назвать ПРИЧИНУ: «промолчал» и «сломалось» через месяц
// отличимы только по ней, а различать их требует DoD брифа.
func TestFinishPlanDemandsReason(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	id, _, err := w.PlanReply(ctx, Plan{ActorID: "ivan", EventID: "c1",
		NoteID: 500, DueAt: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.FinishPlan(ctx, id, PlanDropped, ""); err == nil {
		t.Error("план брошен без причины")
	}
	if err := w.FinishPlan(ctx, id, PlanDone, ""); err != nil {
		t.Errorf("сделанный план причины не требует, но: %v", err)
	}
	if err := w.FinishPlan(ctx, id, PlanQueued, "назад"); err == nil {
		t.Error("план закрыт обратно в очередь")
	}
}

// Круги считает ТРЕД, а не персонаж: затухание — свойство разговора, и
// спрашивают о нём все жители сразу.
func TestTouchThreadCountsRounds(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	for i := range 3 {
		if _, err := w.TouchThread(ctx, 500, i == 0, now); err != nil {
			t.Fatal(err)
		}
	}
	th, err := w.ThreadOf(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if th.Rounds != 3 {
		t.Errorf("кругов %d, ожидалось 3", th.Rounds)
	}
	if th.PersonaN != 1 {
		t.Errorf("реплик народа %d, ожидалась 1", th.PersonaN)
	}
}

// Незнакомый тред — не ошибка, а штатное начало разговора.
func TestThreadOfUnknownIsLive(t *testing.T) {
	w, ctx := testWorld(t)
	th, err := w.ThreadOf(ctx, 999)
	if err != nil {
		t.Fatalf("незнакомый тред дал ошибку: %v", err)
	}
	if th.State != ThreadLive || th.Rounds != 0 {
		t.Errorf("незнакомый тред пришёл как %+v", th)
	}
}

// Затихшие треды находятся по времени последнего движения — ими кормится
// обновление графа: разбирать разговор имеет смысл, когда он кончился.
func TestStaleThreads(t *testing.T) {
	w, ctx := testWorld(t)
	old := time.Now().Add(-2 * time.Hour)
	if _, err := w.TouchThread(ctx, 500, true, old); err != nil {
		t.Fatal(err)
	}
	if _, err := w.TouchThread(ctx, 501, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	stale, err := w.StaleThreads(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].NoteID != 500 {
		t.Fatalf("затихшими сочтены %+v", stale)
	}
	if err := w.CloseThread(ctx, 500); err != nil {
		t.Fatal(err)
	}
	if stale, err = w.StaleThreads(ctx, time.Now().Add(-time.Hour), 10); err != nil || len(stale) != 0 {
		t.Errorf("закрытый тред остался затихшим: %+v (%v)", stale, err)
	}
}

// Затихший тред возвращается в живые, и часы у него идут ЗАНОВО, а не
// подкручиваются назад: садовник зовёт жителей в настоящий момент, а не делает
// вид, что разговор не прерывался.
func TestReopenThreadStartsTheClockAnew(t *testing.T) {
	w, ctx := testWorld(t)
	vchera := time.Now().Add(-30 * time.Hour)
	if _, err := w.TouchThread(ctx, 700, false, vchera); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseThread(ctx, 700); err != nil {
		t.Fatal(err)
	}
	// Пока закрыт — обходу он не виден, даже если спрашивать про будущее.
	stale, err := w.StaleThreads(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, th := range stale {
		if th.NoteID == 700 {
			t.Fatal("закрытый тред попал в живые")
		}
	}

	now := time.Now()
	if err := w.ReopenThread(ctx, 700, now); err != nil {
		t.Fatalf("оживление: %v", err)
	}
	th, err := w.ThreadOf(ctx, 700)
	if err != nil {
		t.Fatal(err)
	}
	if th.State != ThreadLive {
		t.Errorf("состояние %q, а ждали живой", th.State)
	}
	// Часы заново: по вчерашней отметке тред тут же объявили бы затихшим снова.
	if th.LastActive.Before(now.Add(-time.Minute)) {
		t.Errorf("часы остались на %v — оживлённый тред умрёт следующим же тактом", th.LastActive)
	}
	// Треда, которого мир не видел, не оживить: молчаливое заведение строки
	// означало бы живой тред у заметки, которой, может быть, и нет.
	if err := w.ReopenThread(ctx, 701, now); err == nil {
		t.Error("оживлён тред, которого в мире нет")
	}
}
