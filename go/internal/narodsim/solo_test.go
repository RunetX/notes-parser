package narodsim

import (
	"context"
	"fmt"
	"testing"
	"time"

	"lovegw/internal/archive"
)

const actorX = 9

// soloScript — тред, где X говорит дважды: отвечает соседу и самой заметке.
//
//	t0   заметка (автор 1)
//	+1м  2000 автор 2 → заметке
//	+2м  2001 X       → 2000      ← правда: на 2000 он ответил
//	+3м  2002 автор 3 → 2001
//	+5м  2003 X       → заметке   ← правда: на заметку он ответил
//	+6м  2004 автор 2 → 2003
func soloScript() *archive.ThreadScript {
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	m := time.Minute
	return &archive.ThreadScript{
		NoteID: 500,
		Note: archive.ScriptNote{
			AuthorID: 1, AuthorNick: "Хозяйка", Text: "заметка", PublishedAt: t0,
		},
		Comments: []archive.ScriptComment{
			{ID: 2000, AuthorID: 2, AuthorNick: "Ягода", Text: "раз", PublishedAt: at(1), ReplyTo: 0, Delay: m},
			{ID: 2001, AuthorID: actorX, AuthorNick: "ДВ", Text: "нафига", PublishedAt: at(2), ReplyTo: 2000, Delay: m},
			{ID: 2002, AuthorID: 3, AuthorNick: "Гость", Text: "три", PublishedAt: at(3), ReplyTo: 2001, Delay: m},
			{ID: 2003, AuthorID: actorX, AuthorNick: "ДВ", Text: "вечно так", PublishedAt: at(5), ReplyTo: 0, Delay: 5 * m},
			{ID: 2004, AuthorID: 2, AuthorNick: "Ягода", Text: "пять", PublishedAt: at(6), ReplyTo: 2003, Delay: m},
		},
	}
}

// fixedDecider — всегда одно и то же решение: так матрица проверяется без
// случайности.
type fixedDecider struct {
	speak bool
	after time.Duration
	seen  []int64
}

func (d *fixedDecider) Decide(_ context.Context, p DecisionPoint) (Decision, error) {
	d.seen = append(d.seen, p.TriggerID)
	return Decision{Speak: d.speak, After: d.after}, nil
}

// fakeSpeaker — «модель» без модели: возвращает предсказуемый текст.
type fakeSpeaker struct {
	calls []int64
	q     float64
}

func (s *fakeSpeaker) Speak(_ context.Context, p SpeechPoint) (Speech, error) {
	s.calls = append(s.calls, p.Truth.ID)
	return Speech{Got: "как бы сказал " + p.Truth.AuthorNick, Quantile: s.q}, nil
}

func TestRunSoloMatrix(t *testing.T) {
	sc := soloScript()
	d := &fixedDecider{speak: true, after: 90 * time.Second}
	run, err := RunSolo(context.Background(), sc, SoloOpts{Actor: actorX, Decider: d})
	if err != nil {
		t.Fatal(err)
	}
	if run.Replies != 5 || run.Mine != 2 || run.Nick != "ДВ" {
		t.Fatalf("реплик %d, своих %d, ник %q", run.Replies, run.Mine, run.Nick)
	}
	// Точки решения — заметка и три чужие реплики; свои две не в счёт.
	want := []int64{0, 2000, 2002, 2004}
	if fmt.Sprint(d.seen) != fmt.Sprint(want) {
		t.Errorf("точки решения %v, ожидались %v", d.seen, want)
	}
	// Приходил всегда: верно на заметке и на 2000, невпопад на 2002 и 2004.
	if run.Matrix != (Matrix{TP: 2, FP: 2}) {
		t.Errorf("матрица %+v", run.Matrix)
	}
	if got := run.Matrix.Recall(); got != 1 {
		t.Errorf("recall %v — все настоящие ответы должны быть пойманы", got)
	}
	if got := run.Matrix.Precision(); got != 0.5 {
		t.Errorf("precision %v", got)
	}
}

func TestRunSoloSilentDecider(t *testing.T) {
	sc := soloScript()
	run, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: actorX, Decider: &fixedDecider{speak: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Matrix != (Matrix{TN: 2, FN: 2}) {
		t.Errorf("матрица %+v", run.Matrix)
	}
	// Молчун набирает половину точности, ничего не умея. Ради этого Accuracy и
	// читается только вместе с Recall.
	if run.Matrix.Accuracy() != 0.5 || run.Matrix.Recall() != 0 {
		t.Errorf("accuracy %v, recall %v", run.Matrix.Accuracy(), run.Matrix.Recall())
	}
}

// Модель спрашивают РОВНО там, где человек ответил на самом деле: и дешевле, и
// сравнивать есть с чем.
func TestRunSoloSpeaksOnlyWhereTruthIs(t *testing.T) {
	sc := soloScript()
	sp := &fakeSpeaker{q: 0.42}
	run, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: actorX, Decider: &fixedDecider{speak: true}, Speaker: sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sp.calls) != fmt.Sprint([]int64{2001, 2003}) {
		t.Errorf("модель спрошена в точках %v, ожидались реплики самого X", sp.calls)
	}
	if len(run.Speech) != 2 {
		t.Fatalf("реплик от модели %d", len(run.Speech))
	}
	if run.Speech[0].Truth != "нафига" || run.Speech[0].Got == "" {
		t.Errorf("в отчёте нет пары «правда — наше»: %+v", run.Speech[0])
	}
	if run.MedianQuantile() != 0.42 {
		t.Errorf("медианный квантиль %v", run.MedianQuantile())
	}
}

// Без Speaker прогон идёт целиком и не стоит ничего: матрица решений считается
// формулой. Это рабочий режим, а не заглушка, — им отлаживают кубик.
func TestRunSoloWithoutSpeakerCostsNothing(t *testing.T) {
	sc := soloScript()
	run, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: actorX, Decider: &fixedDecider{speak: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Speech) != 0 || run.Matrix.Total() != 4 {
		t.Errorf("реплик %d, точек %d", len(run.Speech), run.Matrix.Total())
	}
}

// Потолок обращений к модели соблюдается, а пропущенное НАЗЫВАЕТСЯ: молча
// урезанный прогон читался бы как полный.
func TestRunSoloRespectsMaxSpeak(t *testing.T) {
	sc := soloScript()
	sp := &fakeSpeaker{}
	run, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: actorX, Decider: &fixedDecider{speak: true}, Speaker: sp, MaxSpeak: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Speech) != 1 || run.Skipped != 1 {
		t.Errorf("реплик %d, пропущено %d", len(run.Speech), run.Skipped)
	}
}

func TestSoloLatencyError(t *testing.T) {
	sc := soloScript()
	// Отвечаем всегда через 2 минуты: правда — 1 мин и 5 мин, ошибки 1 и 3.
	run, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: actorX, Decider: &fixedDecider{speak: true, after: 2 * time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := run.MedianLatencyError(); got != 3*time.Minute {
		t.Errorf("медианная ошибка задержки %v, ожидалось 3m (из 1m и 3m берётся верхняя)", got)
	}
}

// Тред, в котором человек молчал, сравнивать не с чем — это отказ, а не прогон
// с пустой матрицей.
func TestRunSoloRejectsSilentActor(t *testing.T) {
	sc := soloScript()
	if _, err := RunSolo(context.Background(), sc, SoloOpts{
		Actor: 777, Decider: &fixedDecider{speak: true},
	}); err == nil {
		t.Fatal("прогон по не участвовавшей анкете принят")
	}
}

func TestRunSoloNeedsDecider(t *testing.T) {
	if _, err := RunSolo(context.Background(), soloScript(), SoloOpts{Actor: actorX}); err == nil {
		t.Fatal("прогон без решателя принят")
	}
}
