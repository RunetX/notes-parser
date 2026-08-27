package archive

// Затухание треда: как разговор кончается.
//
// Замер под эмуляцию («народ»). Без него тред либо обрывается на второй реплике,
// либо не кончается никогда: вероятность новой реплики нужно откуда-то брать, а
// взятая с потолка она даёт мир, в котором разговоры устроены не как здесь.
//
// Главная величина — не «сколько реплик в треде», а ВЕРОЯТНОСТЬ ПРОДОЛЖЕНИЯ
// ПОСЛЕ ТИШИНЫ: тред, молчавший три часа, и тред, молчавший три минуты, — это
// разные положения, а по среднему числу реплик они неразличимы.
//
// Считается по выборке свежих заметок, а не по всему архиву: в comments 10,7 млн
// строк, а вопрос здесь про форму разговора, которая от объёма выборки не
// зависит. Размер выборки печатается рядом с ответом — молчаливое усечение
// читалось бы как «посчитано по всему».

import (
	"context"
	"fmt"
	"time"
)

// decaySilences — пороги тишины, на которых меряется продолжение. Шаг
// неравномерный: разговор живёт минутами, а умирает сутками.
var decaySilences = []time.Duration{
	15 * time.Minute, time.Hour, 3 * time.Hour, 6 * time.Hour,
	12 * time.Hour, 24 * time.Hour, 72 * time.Hour,
}

// decayGapBuckets — границы позиций, на которых меряется пауза между репликами.
// Начало треда и его хвост живут в разном темпе, и одна средняя пауза скрывает
// именно это.
var decayGapBuckets = [][2]int{{1, 5}, {6, 10}, {11, 20}, {21, 50}, {51, 0}}

// DecayGap — пауза между соседними репликами на участке треда.
type DecayGap struct {
	FromPos int  `json:"from_pos"`
	ToPos   int  `json:"to_pos"` // 0 — и дальше
	Sec     Dist `json:"sec"`
}

// DecayHazard — что бывает после тишины длиной Silence.
type DecayHazard struct {
	SilenceSec int     `json:"silence_sec"`
	Continued  int     `json:"continued"` // столько раз после такой тишины всё же написали
	Stopped    int     `json:"stopped"`   // а столько раз разговор на этом кончился
	P          float64 `json:"p"`         // доля продолжений
}

// DecayCurve — форма разговора.
type DecayCurve struct {
	Threads     int           `json:"threads"`      // тредов в замере
	SampleNotes int           `json:"sample_notes"` // из скольких заметок выбирали
	Comments    Dist          `json:"comments"`     // реплик в треде
	HalfSec     Dist          `json:"half_sec"`     // за сколько набирается половина реплик
	P90Sec      Dist          `json:"p90_sec"`      // и девять десятых
	Gaps        []DecayGap    `json:"gaps"`
	Hazard      []DecayHazard `json:"hazard"`
}

// decayThread — один тред для замера.
type decayThread struct {
	comments []time.Time // по возрастанию
	seenTo   time.Time   // до какого момента мы за тредом наблюдали
}

// MineDecay замеряет форму разговора по выборке последних sampleNotes заметок.
func (s *Store) MineDecay(ctx context.Context, sampleNotes, minComments int) (DecayCurve, error) {
	if sampleNotes <= 0 {
		sampleNotes = 3000
	}
	if minComments < 2 {
		minComments = 2 // тред из одной реплики о затухании не говорит ничего
	}

	// Наблюдение кончается тогда, когда мы заметку читали (grabbed_at): без
	// этой отметки «тред больше не продолжили» неотличимо от «мы перестали
	// смотреть», и вероятность продолжения вышла бы заниженной.
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.note_id, c.published_at, n.grabbed_at
		  FROM comments c
		  JOIN notes n ON n.id = c.note_id
		 WHERE c.note_id IN (SELECT id FROM notes ORDER BY id DESC LIMIT ?)
		   AND c.published_at IS NOT NULL
		 ORDER BY c.note_id, c.published_at`, sampleNotes)
	if err != nil {
		return DecayCurve{}, err
	}
	defer rows.Close()

	threads := map[string]*decayThread{}
	for rows.Next() {
		var noteID, at, grabbed string
		if err := rows.Scan(&noteID, &at, &grabbed); err != nil {
			return DecayCurve{}, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			continue
		}
		th := threads[noteID]
		if th == nil {
			th = &decayThread{}
			th.seenTo, _ = time.Parse(time.RFC3339, grabbed)
			threads[noteID] = th
		}
		th.comments = append(th.comments, t)
	}
	if err := rows.Err(); err != nil {
		return DecayCurve{}, err
	}

	list := make([]decayThread, 0, len(threads))
	for _, th := range threads {
		if len(th.comments) >= minComments {
			list = append(list, *th)
		}
	}
	if len(list) == 0 {
		return DecayCurve{}, fmt.Errorf("decay: в выборке из %d заметок нет тредов с %d+ репликами", sampleNotes, minComments)
	}
	curve := measureDecay(list)
	curve.SampleNotes = sampleNotes
	return curve, nil
}

// measureDecay — чистое ядро замера.
//
// Вероятность продолжения считается как доля дожитий: сколько раз после тишины
// длиной t кто-то всё же написал против того, сколько раз разговор на такой
// тишине кончился. Оборванные наблюдения (тред жив, но мы досмотрели до
// grabbed_at) в знаменатель НЕ идут — иначе всякий свежий тред считался бы
// умершим, и затухание вышло бы вдвое быстрее настоящего.
func measureDecay(threads []decayThread) DecayCurve {
	curve := DecayCurve{Threads: len(threads)}

	counts := make([]int, 0, len(threads))
	halves := make([]int, 0, len(threads))
	p90s := make([]int, 0, len(threads))
	gapsByBucket := make([][]int, len(decayGapBuckets))
	continued := make([]int, len(decaySilences))
	stopped := make([]int, len(decaySilences))

	for _, th := range threads {
		n := len(th.comments)
		counts = append(counts, n)
		start := th.comments[0]
		halves = append(halves, int(th.comments[quantileIndex(n, 0.5)].Sub(start).Seconds()))
		p90s = append(p90s, int(th.comments[quantileIndex(n, 0.9)].Sub(start).Seconds()))

		for i := 1; i < n; i++ {
			gap := th.comments[i].Sub(th.comments[i-1])
			if gap < 0 {
				continue
			}
			if b := gapBucket(i); b >= 0 {
				gapsByBucket[b] = append(gapsByBucket[b], int(gap.Seconds()))
			}
			// Пауза длиной gap означает, что тред пережил КАЖДЫЙ порог тишины
			// короче неё — и на каждом из них разговор продолжился.
			for k, sil := range decaySilences {
				if gap >= sil {
					continued[k]++
				}
			}
		}

		// Хвост: сколько тред молчал к концу наблюдения. Если мы досмотрели
		// недалеко, порог просто не засчитывается — ни в числитель, ни в
		// знаменатель.
		if !th.seenTo.IsZero() {
			tail := th.seenTo.Sub(th.comments[n-1])
			for k, sil := range decaySilences {
				if tail >= sil {
					stopped[k]++
				}
			}
		}
	}

	curve.Comments = distOf(counts)
	curve.HalfSec, curve.P90Sec = distOf(halves), distOf(p90s)
	for i, b := range decayGapBuckets {
		if len(gapsByBucket[i]) == 0 {
			continue
		}
		curve.Gaps = append(curve.Gaps, DecayGap{FromPos: b[0], ToPos: b[1], Sec: distOf(gapsByBucket[i])})
	}
	for k, sil := range decaySilences {
		h := DecayHazard{SilenceSec: int(sil.Seconds()), Continued: continued[k], Stopped: stopped[k]}
		if total := continued[k] + stopped[k]; total > 0 {
			h.P = round4(float64(continued[k]) / float64(total))
		}
		curve.Hazard = append(curve.Hazard, h)
	}
	return curve
}

// ContinueProbability — вероятность, что после тишины silence в треде напишут
// снова. Между замеренными порогами берётся ближайший снизу: мера здесь грубая
// по своей природе, и интерполяция сделала бы вид, что это не так.
func (c DecayCurve) ContinueProbability(silence time.Duration) float64 {
	p := 1.0
	for _, h := range c.Hazard {
		if silence >= time.Duration(h.SilenceSec)*time.Second && h.Continued+h.Stopped > 0 {
			p = h.P
		}
	}
	return p
}

// quantileIndex — индекс реплики, на которой набирается доля q от треда.
func quantileIndex(n int, q float64) int {
	i := int(float64(n)*q) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}

func gapBucket(pos int) int {
	for i, b := range decayGapBuckets {
		if pos >= b[0] && (b[1] == 0 || pos <= b[1]) {
			return i
		}
	}
	return -1
}
