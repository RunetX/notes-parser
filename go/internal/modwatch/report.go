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
	Events         int       // сколько событий отобрано фильтром
	Occasions      int       // сколько из них различных окказий — столько и наблюдений
	EventsSkipped  int       // окказий без пригодных контрольных окон
	Controls       int       // запрошено контрольных окон на событие
	From, To       time.Time // фактический период наблюдения (по репликам)
	AvgPresent     float64   // среднее число людей в окне события
	AvgPresentCtrl float64   // то же в контрольных окнах — база для сравнения
}

// occasion — один момент действия: всё, что модератор сделал в одном треде за
// один такт опроса. Пачка удалений — это ОДНО наблюдение присутствия, а не N:
// наблюдатель видит их разом и штампует общий detected_at, поэтому окно у всех
// одно и то же. Считать их независимыми — раздувать z примерно в √N раз
// (замер 10.08.2026: 46 событий на 25 моментов, один момент дал сразу 16).
type occasion struct {
	NoteID   int64
	From, To time.Time // объединение окон вошедших событий
	Objects  int       // сколько объектов исчезло/изменилось в этот момент
}

// occasionsOf схлопывает события в окказии по паре «заметка + такт опроса».
func occasionsOf(events []Event) []occasion {
	index := map[[2]any]int{}
	var out []occasion
	for _, e := range events {
		key := [2]any{e.NoteID, e.DetectedAt}
		i, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, occasion{NoteID: e.NoteID, From: e.PrevSeen, To: e.DetectedAt, Objects: 1})
			continue
		}
		if e.PrevSeen.Before(out[i].From) {
			out[i].From = e.PrevSeen
		}
		out[i].Objects++
	}
	return out
}

// tally — накопитель статистики по всем событиям.
type tally struct {
	hits             map[int64]int
	expected         map[int64]float64
	variance         map[int64]float64
	occasions        int
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
	occasions := occasionsOf(events)
	for _, o := range occasions {
		acc.add(o, presence, obsFrom, obsTo, opt, rng)
	}
	rep.Events, rep.Occasions, rep.EventsSkipped = len(events), acc.occasions, acc.skipped
	if rep.Occasions == 0 {
		return rep, nil
	}
	rep.AvgPresent = float64(acc.presentTotal) / float64(rep.Occasions)
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

// add обсчитывает одну окказию: кто был в её окне и кто бывает в контрольных.
func (t *tally) add(o occasion, presence []Presence, obsFrom, obsTo time.Time, opt ReportOptions, rng *rand.Rand) {
	from := o.From.Add(-opt.Window)
	to := o.To.Add(opt.Window)
	if !to.After(from) {
		to = from.Add(opt.Window)
	}
	dur := to.Sub(from)

	controls := controlWindows(from, dur, obsFrom, obsTo, opt.Controls, presence, rng)
	if len(controls) == 0 {
		t.skipped++
		return
	}
	t.occasions++

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

// controlJitter — на сколько разрешено сдвигать контрольное окно внутри часа.
// Одних суточных сдвигов мало: за неделю наблюдения их набирается ~7, выбирать
// не из чего, и подгонять под плотность нечем.
var controlJitter = []time.Duration{-time.Hour, -30 * time.Minute, 0, 30 * time.Minute, time.Hour}

// controlWindows подбирает начала контрольных окон: то же окно, сдвинутое на
// целое число суток (чтобы совпадал час суток) и на полчаса-час внутри часа.
// Из кандидатов берутся ближайшие ПО ЧИСЛУ РЕПЛИК к самому окну события.
//
// Выравнивание по плотности обязательно: комментарий удаляют в живом треде, и
// без него окна действий систематически люднее контрольных (замер 11.08.2026 —
// 11.6 человека против 6.1). Тогда нулевая гипотеза не «×1», а «×1.9», и любой
// разговорчивый попадает в верхушку просто потому, что действие случилось в
// шумную минуту.
func controlWindows(from time.Time, dur time.Duration, obsFrom, obsTo time.Time, want int, presence []Presence, rng *rand.Rand) []time.Time {
	target := countIn(presence, from, from.Add(dur))
	type candidate struct {
		start time.Time
		diff  int
	}
	var candidates []candidate
	for k := -controlShiftDays; k <= controlShiftDays; k++ {
		for _, j := range controlJitter {
			if k == 0 {
				continue
			}
			start := from.AddDate(0, 0, k).Add(j)
			if start.Before(obsFrom) || start.Add(dur).After(obsTo) {
				continue
			}
			d := countIn(presence, start, start.Add(dur)) - target
			if d < 0 {
				d = -d
			}
			candidates = append(candidates, candidate{start, d})
		}
	}
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].diff < candidates[j].diff })
	if len(candidates) > want {
		candidates = candidates[:want]
	}
	out := make([]time.Time, len(candidates))
	for i, c := range candidates {
		out[i] = c.start
	}
	return out
}

// countIn — сколько реплик в интервале [from, to].
func countIn(presence []Presence, from, to time.Time) int {
	lo := sort.Search(len(presence), func(i int) bool { return !presence[i].At.Before(from) })
	hi := sort.Search(len(presence), func(i int) bool { return presence[i].At.After(to) })
	return hi - lo
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
