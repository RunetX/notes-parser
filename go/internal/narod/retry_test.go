package narod

// ВРЕМЕННЫЙ ОТКАЗ СЦЕНЫ — НЕ КОНЕЦ НАМЕРЕНИЯ.
//
// Тесты написаны по боевому случаю 31.08.2026 (смежное обсуждение 100000000031):
// у площадки свой потолок частоты — одна реплика в десять секунд на человека, —
// написанный для ЧЕЛОВЕКА и меряемый СТЕННЫМИ часами, а житель живёт во времени,
// сжатом LatencyScale. Четыре реплики из ста тридцати четырёх легли в одну
// десятисекундную щель, и служба хоронила план насовсем: модель уже вызвана,
// текст написан, запись в журнале стоит, — а на странице дыра и сожжённая
// монетка (бросок ключуется событием и второй раз не выпадет).
//
// Тесты здесь НА ПУТИ ДАННЫХ, а не на формуле, по тому же правилу, которым
// заведён wiring_test: различие «временный отказ против окончательного» видно
// только в поведении службы, и одним взглядом на код его не подтвердить.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// busyStage — сцена, отказывающая первые refuse раз по занятости.
type busyStage struct {
	fakeStage
	refuse int   // сколько раз ещё отказать
	err    error // чем отказывать; пусто — ErrStageBusy
	posts  int   // сколько реплик принято
}

func (b *busyStage) StagePost(_ context.Context, _, _, _ int64, _ string) (int64, error) {
	if b.refuse > 0 {
		b.refuse--
		if b.err != nil {
			return 0, b.err
		}
		return 0, fmt.Errorf("%w: слишком часто", ErrStageBusy)
	}
	b.posts++
	return 555, nil
}

// oneReply — модель, отвечающая одной и той же репликой. Считает вызовы: весь
// смысл придержанного черновика в том, чтобы повтор их не добавлял.
type oneReply struct{ calls int }

func (g *oneReply) GenerateJSON(context.Context, string, string, map[string]any) ([]byte, error) {
	g.calls++
	return []byte(`{"action":"reply","text":"у соседа тоже лает, и ничего"}`), nil
}

// stepClock — часы, которые двигает тест.
type stepClock struct{ t time.Time }

func (c *stepClock) Now() time.Time { return c.t }

// retryService собирает службу в ЖИВОМ режиме: в сухом реплика до сцены не
// доходит вовсе, а весь предмет тестов — отказ сцены.
func retryService(t *testing.T, stage Stage, gen JSONGenerator) (*Service, *World, *stepClock) {
	t.Helper()
	ctx := context.Background()
	svc, w := testService(t, stage)
	svc.cfg.Mode = ModeLive
	// Потолки подняты: они предохранители и к предмету теста отношения не имеют,
	// а на повторе журнальная запись уже стоит и в них засчитывается.
	svc.cfg.PerPersonaHour, svc.cfg.PerPersonaDay = 100, 100
	svc.cfg.PerThread, svc.cfg.DayCalls = 100, 100
	svc.gen = gen
	card := wiringCard("zhilets")
	svc.players = []Player{{Card: card, UserID: 42}}
	clock := &stepClock{t: time.Date(2026, 8, 31, 16, 0, 0, 0, time.UTC)}
	svc.SetClock(clock)
	if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
		PlatformUserID: 42, Nick: "Житель"}, clock.Now()); err != nil {
		t.Fatal(err)
	}
	return svc, w, clock
}

// queuePlan кладёт созревшее намерение и возвращает его номер.
func queuePlan(t *testing.T, w *World, now time.Time) int64 {
	t.Helper()
	id, fresh, err := w.PlanReply(context.Background(), Plan{
		ActorID: "zhilets", EventID: "reply:100", NoteID: wiringNote.ID,
		DueAt: now.Add(-time.Minute),
	}, now)
	if err != nil || !fresh {
		t.Fatalf("намерение не завелось: %v", err)
	}
	return id
}

// ЗАНЯТАЯ СЦЕНА ПЕРЕНОСИТ РЕПЛИКУ, А НЕ ХОРОНИТ ЕЁ.
//
// И переносит вместе с НАПИСАННЫМ: модель зовётся один раз на реплику, сколько
// бы попыток ни понадобилось. Иначе повтор платил бы за то, что уже оплачено, —
// вторая цена дефекта поверх потерянного разговора.
func TestЗанятаяСценаПереноситРеплику(t *testing.T) {
	ctx := context.Background()
	stage := &busyStage{fakeStage: fakeStage{notes: []StageNote{wiringNote}}, refuse: 1}
	gen := &oneReply{}
	svc, w, clock := retryService(t, stage, gen)
	id := queuePlan(t, w, clock.Now())

	// Первый заход: сцена занята.
	if err := svc.Work(ctx); err != nil {
		t.Fatalf("первый заход: %v", err)
	}
	if stage.posts != 0 {
		t.Fatalf("реплика встала на сцену, хотя та отказала")
	}
	pl := planByID(t, w, id)
	if pl.State != PlanQueued {
		t.Fatalf("намерение в состоянии %q, а должно вернуться в очередь", pl.State)
	}
	if !pl.DueAt.After(clock.Now()) {
		t.Errorf("срок повтора %v не позже отказа %v", pl.DueAt, clock.Now())
	}

	// Второй заход — после паузы.
	clock.t = clock.t.Add(stageRetryAfter)
	if err := svc.Work(ctx); err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if stage.posts != 1 {
		t.Fatalf("реплик на сцене %d, ожидалась одна", stage.posts)
	}
	if gen.calls != 1 {
		t.Errorf("модель звалась %d раз: повтор обязан идти уже написанным", gen.calls)
	}
	if pl := planByID(t, w, id); pl.State != PlanDone {
		t.Errorf("намерение закрыто как %q, а реплика встала", pl.State)
	}
}

// ПОВТОР НЕ ПЛОДИТ ВТОРУЮ ЗАПИСЬ В ЖУРНАЛЕ.
//
// Журнал пишется ДО публикации намеренно, поэтому у придержанной реплики запись
// уже есть. Заведи повтор вторую — житель помнил бы, что сказал одно и то же
// дважды, и «а помнишь» превратилось бы в бред. Номер реплики при этом обязан
// доехать до ТОЙ ЖЕ записи.
func TestПовторНеУдваиваетПамятьЖителя(t *testing.T) {
	ctx := context.Background()
	stage := &busyStage{fakeStage: fakeStage{notes: []StageNote{wiringNote}}, refuse: 2}
	svc, w, clock := retryService(t, stage, &oneReply{})
	queuePlan(t, w, clock.Now())

	for i := 0; i < 3; i++ {
		if err := svc.Work(ctx); err != nil {
			t.Fatalf("заход %d: %v", i, err)
		}
		clock.t = clock.t.Add(stageRetryAfter)
	}
	if stage.posts != 1 {
		t.Fatalf("реплик на сцене %d, ожидалась одна", stage.posts)
	}
	said, err := w.Recall(ctx, "zhilets", 10)
	if err != nil {
		t.Fatal(err)
	}
	var comments []JournalEntry
	for _, e := range said {
		if e.Kind == JournalComment {
			comments = append(comments, e)
		}
	}
	if len(comments) != 1 {
		t.Fatalf("в журнале %d записей о реплике, а сказана она один раз", len(comments))
	}
	if comments[0].CommentID != 555 {
		t.Errorf("запись помнит реплику под номером %d, а сцена дала 555", comments[0].CommentID)
	}
}

// ОКОНЧАТЕЛЬНЫЙ ОТКАЗ ПО-ПРЕЖНЕМУ ХОРОНИТ НАМЕРЕНИЕ.
//
// Это вторая половина правила, и она важнее первой. Общее устройство службы —
// «строка, застрявшая в posting, не переотправляется никогда»: сцена могла
// реплику и ПРИНЯТЬ, ответив ошибкой, а вторая копия под тем же именем хуже
// пропавшей. Повтор разрешён ровно на ErrStageBusy, потому что тот доказывает,
// что записи нет.
func TestНеВременныйОтказНеПовторяется(t *testing.T) {
	ctx := context.Background()
	stage := &busyStage{fakeStage: fakeStage{notes: []StageNote{wiringNote}},
		refuse: 1, err: errors.New("сцена ответила ошибкой, приняв реплику")}
	svc, w, clock := retryService(t, stage, &oneReply{})
	id := queuePlan(t, w, clock.Now())

	if err := svc.Work(ctx); err != nil {
		t.Fatalf("заход: %v", err)
	}
	pl := planByID(t, w, id)
	if pl.State != PlanDropped {
		t.Fatalf("намерение в состоянии %q, а отказ был окончательным", pl.State)
	}
	clock.t = clock.t.Add(stageRetryAfter)
	if err := svc.Work(ctx); err != nil {
		t.Fatalf("второй заход: %v", err)
	}
	if stage.posts != 0 {
		t.Errorf("реплика всё же ушла на сцену: это вторая копия")
	}
}

// ПОВТОРЫ КОНЧАЮТСЯ. Пятнадцать секунд на попытку, пять попыток — вчетверо
// больше десятисекундного окна площадки. Не помогло — дело не в частоте, и
// настоящую причину прячет повтор.
func TestПовторыНеБесконечны(t *testing.T) {
	ctx := context.Background()
	stage := &busyStage{fakeStage: fakeStage{notes: []StageNote{wiringNote}}, refuse: 100}
	svc, w, clock := retryService(t, stage, &oneReply{})
	id := queuePlan(t, w, clock.Now())

	for i := 0; i < stageRetries+3; i++ {
		if err := svc.Work(ctx); err != nil {
			t.Fatalf("заход %d: %v", i, err)
		}
		clock.t = clock.t.Add(stageRetryAfter)
	}
	pl := planByID(t, w, id)
	if pl.State != PlanDropped {
		t.Fatalf("намерение всё ещё %q после %d заходов", pl.State, stageRetries+3)
	}
	if _, held := svc.heldOf(id); held {
		t.Error("черновик остался в памяти у закрытого намерения")
	}
}

// planByID — намерение из базы.
func planByID(t *testing.T, w *World, id int64) Plan {
	t.Helper()
	plans, err := w.PlansOf(context.Background(), "zhilets", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("намерение %d в базе не найдено", id)
	return Plan{}
}
