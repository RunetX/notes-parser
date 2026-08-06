package modwatch

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"time"
)

// ReportOptions — параметры сверки «кто был на площадке в момент действия».
type ReportOptions struct {
	Since, Until   time.Time     // границы по времени обнаружения события
	Kinds          []string      // виды событий (пусто — все)
	MinAge, MaxAge time.Duration // возраст объекта к моменту действия (0 — без границы)
	Window         time.Duration // расширение окна события в обе стороны
	Controls       int           // сколько контрольных окон на событие
	Seed           int64         // зерно выбора контрольных окон (воспроизводимость)
	MinHits        int           // не показывать тех, кто совпал реже
	Top            int           // сколько строк вернуть (0 — все)
}

// Значения по умолчанию для отчёта.
const (
	DefaultPresenceWindow = 5 * time.Minute
	DefaultControls       = 24
	controlShiftDays      = 14   // на сколько суток вперёд/назад искать контрольные окна
	controlSmoothing      = 0.25 // сглаживание доли контрольных окон (см. accumulate)
)

// ReportRow — строка отчёта по одному человеку.
type ReportRow struct {
	UserID   int64
	Name     string
	Hits     int     // в скольких окнах событий он писал
	Expected float64 // сколько ожидалось по контрольным окнам того же часа суток
	Lift     float64 // Hits / Expected
	Z        float64 // (Hits − Expected) / σ, пуассон-биномиальное приближение
	Comments int     // всего реплик за период наблюдения — контекст объёма
}

// Report — итог сверки.
type Report struct {
	Rows           []ReportRow
	Events         int       // сколько событий вошло в расчёт
	EventsSkipped  int       // событий без пригодных контрольных окон
	Controls       int       // запрошено контрольных окон на событие
	From, To       time.Time // фактический период наблюдения (по репликам)
	AvgPresent     float64   // среднее число людей в окне события
	AvgPresentCtrl float64   // то же в контрольных окнах — база для сравнения
}

// tally — накопитель статистики по всем событиям.
type tally struct {
	hits             map[int64]int
	expected         map[int64]float64
	variance         map[int64]float64
	events           int
	skipped          int
	presentTotal     int
	ctrlPresentTotal int
	ctrlWindows      int
}

// Analyze считает, кто присутствует в моменты действий модерации чаще, чем в
// сопоставимые минуты без действий.
//
// Момент действия неизвестен точно: он лежит между prev_seen_at и detected_at,
// поэтому окном события считается [prev_seen − Window, detected + Window].
// Контроль подбирается сдвигом того же окна на целые сутки — так сохраняются
// час суток и длительность, а значит поправка на «ночью на сайте пусто» и на
// «активные всегда онлайн» встроена в саму конструкцию, а не в формулу.
func (s *Store) Analyze(ctx context.Context, opt ReportOptions) (Report, error) {
	if opt.Window <= 0 {
		opt.Window = DefaultPresenceWindow
	}
	if opt.Controls <= 0 {
		opt.Controls = DefaultControls
	}
	rep := Report{Controls: opt.Controls}

	events, err := s.Events(ctx, EventFilter{
		Since: opt.Since, Until: opt.Until, Kinds: opt.Kinds,
		MinAge: opt.MinAge, MaxAge: opt.MaxAge,
	})
	if err != nil || len(events) == 0 {
		return rep, err
	}
	// Присутствие берём за весь период наблюдения: контрольные окна лежат в
	// других сутках, значит нужны реплики далеко за границами событий.
	presence, err := s.PresenceLog(ctx, time.Unix(0, 0).UTC(), time.Now().UTC().AddDate(1, 0, 0))
	if err != nil || len(presence) == 0 {
		return rep, err
	}
	obsFrom, obsTo := presence[0].At, presence[len(presence)-1].At
	rep.From, rep.To = obsFrom, obsTo

	rng := rand.New(rand.NewPCG(uint64(opt.Seed), uint64(opt.Seed)^0x9e3779b97f4a7c15))
	acc := &tally{hits: map[int64]int{}, expected: map[int64]float64{}, variance: map[int64]float64{}}
	for _, e := range events {
		acc.add(e, presence, obsFrom, obsTo, opt, rng)
	}
	rep.Events, rep.EventsSkipped = acc.events, acc.skipped
	if rep.Events == 0 {
		return rep, nil
	}
	rep.AvgPresent = float64(acc.presentTotal) / float64(rep.Events)
	if acc.ctrlWindows > 0 {
		rep.AvgPresentCtrl = float64(acc.ctrlPresentTotal) / float64(acc.ctrlWindows)
	}

	names, err := s.Names(ctx)
	if err != nil {
		return rep, err
	}
	comments := map[int64]int{}
	for _, p := range presence {
		comments[p.UserID]++
	}
	rep.Rows = acc.rows(names, comments, opt)
	return rep, nil
}

// add обсчитывает одно событие: кто был в его окне и кто бывает в контрольных.
func (t *tally) add(e Event, presence []Presence, obsFrom, obsTo time.Time, opt ReportOptions, rng *rand.Rand) {
	from := e.PrevSeen.Add(-opt.Window)
	to := e.DetectedAt.Add(opt.Window)
	if !to.After(from) {
		to = from.Add(opt.Window)
	}
	dur := to.Sub(from)

	controls := controlWindows(from, dur, obsFrom, obsTo, opt.Controls, rng)
	if len(controls) == 0 {
		t.skipped++
		return
	}
	t.events++

	inEvent := authorsIn(presence, from, to)
	t.presentTotal += len(inEvent)
	for u := range inEvent {
		t.hits[u]++
	}
	seenCtrl := map[int64]int{}
	for _, c := range controls {
		set := authorsIn(presence, c, c.Add(dur))
		t.ctrlPresentTotal += len(set)
		t.ctrlWindows++
		for u := range set {
			seenCtrl[u]++
		}
	}
	n := float64(len(controls))
	for u := range inEvent {
		if _, ok := seenCtrl[u]; !ok {
			seenCtrl[u] = 0 // был при действии, но никогда в контроле — учесть надо
		}
	}
	for u, c := range seenCtrl {
		t.expected[u] += float64(c) / n
		// Дисперсию считаем по сглаженной доле: без сглаживания у человека,
		// которого не оказалось ни в одном контрольном окне, σ обнуляется — и
		// самый сильный случай («был только при действиях») получил бы z = 0.
		// Сглаживание задаёт нижнюю границу неопределённости.
		p := (float64(c) + controlSmoothing) / (n + 2*controlSmoothing)
		t.variance[u] += p * (1 - p)
	}
}

// rows собирает и сортирует итоговую таблицу.
func (t *tally) rows(names map[int64]string, comments map[int64]int, opt ReportOptions) []ReportRow {
	var out []ReportRow
	for u, h := range t.hits {
		if h < opt.MinHits {
			continue
		}
		row := ReportRow{
			UserID:   u,
			Name:     names[u],
			Hits:     h,
			Expected: t.expected[u],
			Comments: comments[u],
		}
		if row.Expected > 0 {
			row.Lift = float64(h) / row.Expected
		}
		if v := t.variance[u]; v > 0 {
			row.Z = (float64(h) - row.Expected) / math.Sqrt(v)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Z != out[j].Z {
			return out[i].Z > out[j].Z
		}
		return out[i].Hits > out[j].Hits
	})
	if opt.Top > 0 && len(out) > opt.Top {
		out = out[:opt.Top]
	}
	return out
}

// controlWindows подбирает начала контрольных окон: то же окно, сдвинутое на
// целое число суток, чтобы совпадал час суток. Возвращает не больше want штук.
func controlWindows(from time.Time, dur time.Duration, obsFrom, obsTo time.Time, want int, rng *rand.Rand) []time.Time {
	var candidates []time.Time
	for k := -controlShiftDays; k <= controlShiftDays; k++ {
		if k == 0 {
			continue
		}
		start := from.AddDate(0, 0, k)
		if start.Before(obsFrom) || start.Add(dur).After(obsTo) {
			continue
		}
		candidates = append(candidates, start)
	}
	if len(candidates) <= want {
		return candidates
	}
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	return candidates[:want]
}

// authorsIn возвращает множество авторов, писавших в интервале [from, to].
// presence отсортирован по времени — ищем границу двоичным поиском.
func authorsIn(presence []Presence, from, to time.Time) map[int64]bool {
	lo := sort.Search(len(presence), func(i int) bool { return !presence[i].At.Before(from) })
	out := map[int64]bool{}
	for i := lo; i < len(presence) && !presence[i].At.After(to); i++ {
		out[presence[i].UserID] = true
	}
	return out
}
