package narodsim

// Режим solo — «один слепок среди настоящих».
//
// Все реплики оригинала подаются по расписанию, кроме одного участника X: на
// его месте думает слепок. Смысл разделения в том, что у эмуляции ДВА разных
// умения, и мерить их вместе бессмысленно — плохой текст в верно выбранный
// момент и безупречный текст невпопад проваливают прогон одинаково, а чинятся
// по-разному.
//
// Поэтому мерок тоже две, и берутся они в разных точках:
//
//   - РЕШЕНИЕ («прийти или смолчать») меряется в КАЖДОЙ точке, где X мог бы
//     ответить, и не стоит ни копейки: кубик это формула, модель тут не
//     участвует. Правда известна точно — X ответил на реплику C тогда и только
//     тогда, когда в оригинале есть его реплика с ребром на C. Ребро настоящее
//     (замер по живому архиву: 82,9 % рёбер из мобильного дерева, угаданных
//     ноль), поэтому разметка не спорная.
//
//   - ГОЛОС меряется только там, где X ответил НА САМОМ ДЕЛЕ. Это и экономия
//     (обращений к модели ровно столько, сколько человек написал реплик, а не
//     сколько раз мог бы), и чистота: сравнивать наш текст с настоящим можно
//     лишь там, где настоящий есть.
//
// Своей мерки голоса пакет не заводит — считает её archive.GenerateVoice тем же
// циклом контроля, что и `personas voice`: ранг атрибуции, полоса held-out,
// пересечение с образцами. Второй набор порогов рядом с первым разошёлся бы с
// ним молча.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"lovegw/internal/archive"
)

// DecisionPoint — момент, когда житель мог бы заговорить.
type DecisionPoint struct {
	Now       time.Time
	Actor     int64 // чья очередь думать
	TriggerID int64 // реплика, на которую реагируем; 0 — сама заметка
	Trigger   string
	Author    int64 // кто её сказал; у заметки — её автор
	Nick      string
	// Addressed — обратились к самому жителю. Отдельным полем, а не разбором
	// текста: обращение уже срезано в ребро, и второй разбор рядом с первым
	// однажды разошёлся бы с ним.
	Addressed bool
	Seen      int // сколько реплик уже прозвучало
	Said      int // сколько раз житель уже говорил в этом треде
}

// Decision — что решил житель.
type Decision struct {
	Speak bool
	After time.Duration // через сколько ответил бы
}

// Decider — «прийти или смолчать». Модель здесь не участвует: это формула, и
// потому решение меряется в каждой точке, а не в выборочных.
type Decider interface {
	Decide(ctx context.Context, p DecisionPoint) (Decision, error)
}

// SpeechPoint — момент, когда X ответил на самом деле; здесь и спрашивают
// модель.
type SpeechPoint struct {
	Now    time.Time
	Actor  int64
	Script *archive.ThreadScript
	Upto   int                   // сколько реплик оригинала уже прозвучало
	Truth  archive.ScriptComment // что человек написал НА САМОМ ДЕЛЕ
}

// Speech — что вышло у жителя и насколько это похоже на правду.
type Speech struct {
	CommentID int64  `json:"comment_id"`
	At        string `json:"at"`
	Truth     string `json:"truth"`
	Got       string `json:"got"`
	Rejected  string `json:"rejected,omitempty"` // непустое — модель ничего не дала

	// Quantile — доля настоящих текстов автора, узнанных атрибуцией ХУЖЕ
	// нашего. Это и есть мерка голоса: 0,5 значит «неотличим от собственной
	// середины», а не «похоже на глаз».
	Quantile float64 `json:"quantile"`
	Rank     int     `json:"rank"`
	Of       int     `json:"of"`
	Copy     float64 `json:"copy"` // пересечение с образцами донора
}

// Speaker — что житель написал бы здесь. nil у прогона означает «без модели»:
// матрица решений считается всё равно и бесплатно.
type Speaker interface {
	Speak(ctx context.Context, p SpeechPoint) (Speech, error)
}

// Matrix — сошлось ли решение с фактом.
type Matrix struct {
	TP int `json:"tp"` // пришёл и должен был
	FP int `json:"fp"` // пришёл, а человек смолчал
	TN int `json:"tn"` // смолчал и правильно
	FN int `json:"fn"` // смолчал, а человек ответил
}

// Total — всего точек решения.
func (m Matrix) Total() int { return m.TP + m.FP + m.TN + m.FN }

// Accuracy — доля верных решений. Сама по себе она обманчива: в людном треде
// человек молчит почти всегда, и «никогда не приходить» даёт 95 %. Читать её
// нужно вместе с Recall.
func (m Matrix) Accuracy() float64 {
	if m.Total() == 0 {
		return 0
	}
	return float64(m.TP+m.TN) / float64(m.Total())
}

// Recall — какую долю настоящих ответов житель не проспал.
func (m Matrix) Recall() float64 {
	if m.TP+m.FN == 0 {
		return 0
	}
	return float64(m.TP) / float64(m.TP+m.FN)
}

// Precision — какая доля приходов была уместна.
func (m Matrix) Precision() float64 {
	if m.TP+m.FP == 0 {
		return 0
	}
	return float64(m.TP) / float64(m.TP+m.FP)
}

// Point — одна точка решения с ответом и правдой.
type Point struct {
	TriggerID int64         `json:"trigger_id"`
	At        string        `json:"at"`
	Spoke     bool          `json:"spoke"`
	Truth     bool          `json:"truth"`
	After     time.Duration `json:"after,omitempty"`      // наша задержка
	TrueAfter time.Duration `json:"true_after,omitempty"` // настоящая, если ответил
}

// SoloRun — итог прогона.
type SoloRun struct {
	NoteID  int64    `json:"note_id"`
	Actor   int64    `json:"actor"`
	Nick    string   `json:"nick"`
	Replies int      `json:"replies"` // реплик в оригинале
	Mine    int      `json:"mine"`    // из них сказанных X
	Points  []Point  `json:"points"`
	Speech  []Speech `json:"speech"`
	Matrix  Matrix   `json:"matrix"`
	Skipped int      `json:"skipped"` // точек голоса, не взятых из-за потолка
}

// SoloOpts — параметры прогона.
type SoloOpts struct {
	Actor   int64 // анкета, на месте которой играет слепок
	Decider Decider
	Speaker Speaker // nil — только матрица решений, без модели и без расхода
	// MaxSpeak — потолок обращений к модели за прогон. Ноль означает БЕЗ
	// потолка, и это осознанно опасно: тред на тысячу реплик с говорливым
	// участником — это сотни платных вызовов. Отчёт называет пропущенное.
	MaxSpeak int
}

// RunSolo прогоняет слепок на месте участника архивного треда.
func RunSolo(ctx context.Context, sc *archive.ThreadScript, o SoloOpts) (*SoloRun, error) {
	if o.Actor == 0 {
		return nil, fmt.Errorf("solo: не указан участник")
	}
	if o.Decider == nil {
		return nil, fmt.Errorf("solo: нечем решать «прийти или смолчать»")
	}
	run := &SoloRun{NoteID: sc.NoteID, Actor: o.Actor, Replies: len(sc.Comments)}

	answered := trueAnswers(sc, o.Actor)
	for _, c := range sc.Comments {
		if c.AuthorID == o.Actor {
			run.Mine++
			run.Nick = c.AuthorNick
		}
	}
	if run.Mine == 0 {
		return nil, fmt.Errorf("solo: анкета %d в треде %d не говорила — сравнивать не с чем",
			o.Actor, sc.NoteID)
	}

	// Заметка — тоже точка решения: ответ первого уровня приходит на неё.
	if err := step(ctx, run, o, sc, DecisionPoint{
		Now: sc.Note.PublishedAt, Actor: o.Actor, TriggerID: 0,
		Trigger: sc.Note.Text, Author: sc.Note.AuthorID, Nick: sc.Note.AuthorNick,
	}, answered, 0); err != nil {
		return nil, err
	}

	said := 0
	for i, c := range sc.Comments {
		// Своя реплика решением не считается: сам себя житель не подначивает.
		// Зато здесь спрашивают модель — это и есть точка, где правда известна.
		if c.AuthorID == o.Actor {
			said++
			if err := speak(ctx, run, o, sc, i, c); err != nil {
				return nil, err
			}
			continue
		}
		if err := step(ctx, run, o, sc, DecisionPoint{
			Now: c.PublishedAt, Actor: o.Actor, TriggerID: c.ID, Trigger: c.Text,
			Author: c.AuthorID, Nick: c.AuthorNick, Addressed: c.TargetID == o.Actor,
			Seen: i + 1, Said: said,
		}, answered, c.ID); err != nil {
			return nil, err
		}
	}
	return run, nil
}

// step — одна точка решения.
func step(ctx context.Context, run *SoloRun, o SoloOpts, sc *archive.ThreadScript,
	p DecisionPoint, answered map[int64]time.Duration, triggerID int64) error {

	d, err := o.Decider.Decide(ctx, p)
	if err != nil {
		return fmt.Errorf("решение на %d: %w", triggerID, err)
	}
	trueAfter, truth := answered[triggerID]
	pt := Point{
		TriggerID: triggerID, At: p.Now.UTC().Format(time.RFC3339),
		Spoke: d.Speak, Truth: truth,
	}
	if d.Speak {
		pt.After = d.After
	}
	if truth {
		pt.TrueAfter = trueAfter
	}
	run.Points = append(run.Points, pt)
	switch {
	case d.Speak && truth:
		run.Matrix.TP++
	case d.Speak && !truth:
		run.Matrix.FP++
	case !d.Speak && truth:
		run.Matrix.FN++
	default:
		run.Matrix.TN++
	}
	return nil
}

// speak — точка, где человек ответил на самом деле: здесь спрашивают модель.
func speak(ctx context.Context, run *SoloRun, o SoloOpts, sc *archive.ThreadScript,
	i int, c archive.ScriptComment) error {

	if o.Speaker == nil {
		return nil
	}
	if o.MaxSpeak > 0 && len(run.Speech) >= o.MaxSpeak {
		run.Skipped++
		return nil
	}
	sp, err := o.Speaker.Speak(ctx, SpeechPoint{
		Now: c.PublishedAt, Actor: o.Actor, Script: sc, Upto: i, Truth: c,
	})
	if err != nil {
		return fmt.Errorf("реплика на месте %d: %w", c.ID, err)
	}
	sp.CommentID = c.ID
	sp.At = c.PublishedAt.UTC().Format(time.RFC3339)
	sp.Truth = c.Text
	run.Speech = append(run.Speech, sp)
	return nil
}

// trueAnswers — на что человек ответил на самом деле: «реплика, на которую он
// откликнулся» → «через сколько». Ключ 0 — ответ самой заметке.
//
// Если человек отвечал на одну реплику дважды, остаётся ПЕРВЫЙ отклик: точка
// решения одна, и правда в ней — «ответил», а не «сколько раз».
func trueAnswers(sc *archive.ThreadScript, actor int64) map[int64]time.Duration {
	out := map[int64]time.Duration{}
	for _, c := range sc.Comments {
		if c.AuthorID != actor {
			continue
		}
		if _, seen := out[c.ReplyTo]; !seen {
			out[c.ReplyTo] = c.Delay
		}
	}
	return out
}

// MedianLatencyError — медианная ошибка задержки там, где житель пришёл верно.
// Считается по модулю в секундах: важно, насколько промахнулись, а не в какую
// сторону.
func (r *SoloRun) MedianLatencyError() time.Duration {
	var errs []time.Duration
	for _, p := range r.Points {
		if !p.Spoke || !p.Truth {
			continue
		}
		d := p.After - p.TrueAfter
		if d < 0 {
			d = -d
		}
		errs = append(errs, d)
	}
	if len(errs) == 0 {
		return 0
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i] < errs[j] })
	return errs[len(errs)/2]
}

// MedianQuantile — медианный квантиль полосы по сгенерированным репликам. Это
// главное число голоса: 0,5 значит «неотличим от собственной середины автора».
func (r *SoloRun) MedianQuantile() float64 {
	var qs []float64
	for _, s := range r.Speech {
		if s.Rejected != "" {
			continue
		}
		qs = append(qs, s.Quantile)
	}
	if len(qs) == 0 {
		return 0
	}
	sort.Float64s(qs)
	return qs[len(qs)/2]
}
