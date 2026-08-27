package archive

import (
	"testing"
	"time"
)

func TestLatencySecBounds(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
		want          int
		ok            bool
	}{
		{"обычный ответ", "2026-08-27T10:00:00Z", "2026-08-27T10:12:30Z", 750, true},
		{"мгновенный", "2026-08-27T10:00:00Z", "2026-08-27T10:00:00Z", 0, true},
		// Часы разошлись или строка пришла из чужого зеркала: отрицательная
		// задержка не «очень быстрый ответ», а испорченный замер.
		{"отрицательная", "2026-08-27T10:00:00Z", "2026-08-27T09:59:00Z", 0, false},
		// Позже суток — это возвращение к старому разговору, а не отклик.
		{"через неделю", "2026-08-20T10:00:00Z", "2026-08-27T10:00:00Z", 0, false},
		{"мусор", "не время", "2026-08-27T10:00:00Z", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := latencySec(tc.before, tc.after)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("latencySec = %d, %v; ожидалось %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestMeasureDecayHazard(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	at := func(mins ...int) []time.Time {
		out := make([]time.Time, 0, len(mins))
		for _, m := range mins {
			out = append(out, base.Add(time.Duration(m)*time.Minute))
		}
		return out
	}
	// Три живых треда (плотная переписка) и один, умерший после первой паузы.
	// Наблюдение у всех кончается через неделю после начала.
	seen := base.Add(7 * 24 * time.Hour)
	threads := []decayThread{
		{comments: at(0, 5, 12, 20, 400), seenTo: seen}, // одна пауза больше 6 ч
		{comments: at(0, 3, 9, 15), seenTo: seen},
		{comments: at(0, 7, 14, 30), seenTo: seen},
		{comments: at(0, 2), seenTo: seen},
	}
	c := measureDecay(threads)

	if c.Threads != 4 {
		t.Fatalf("тредов %d, ожидалось 4", c.Threads)
	}
	if c.Comments.Median <= 0 {
		t.Errorf("реплик в треде не посчитано: %+v", c.Comments)
	}
	byName := map[int]DecayHazard{}
	for _, h := range c.Hazard {
		byName[h.SilenceSec] = h
	}
	// Тишина 15 минут: продолжили дважды (паузы 380 и 16 минут), а кончились —
	// все четыре треда, ведь после последней реплики прошла неделя.
	h15 := byName[900]
	if h15.Continued != 2 || h15.Stopped != 4 {
		t.Errorf("после 15 минут тишины: продолжили %d, кончились %d; ожидалось 2 и 4",
			h15.Continued, h15.Stopped)
	}
	if h15.P <= 0 || h15.P >= 1 {
		t.Errorf("вероятность продолжения %v — вне (0,1)", h15.P)
	}
	// Чем дольше тишина, тем ниже шанс продолжения. Это и есть затухание.
	h24 := byName[24*3600]
	if h24.P > h15.P {
		t.Errorf("после суток тишины продолжают чаще (%v), чем после четверти часа (%v)", h24.P, h15.P)
	}
}

// Тред, за которым мы ещё смотрим, не считается умершим: иначе всякий свежий
// разговор попадал бы в знаменатель, и затухание вышло бы вдвое быстрее
// настоящего.
func TestMeasureDecayIgnoresOpenObservation(t *testing.T) {
	base := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	threads := []decayThread{{
		comments: []time.Time{base, base.Add(5 * time.Minute)},
		seenTo:   base.Add(6 * time.Minute), // досмотрели почти сразу
	}}
	for _, h := range measureDecay(threads).Hazard {
		if h.Stopped != 0 {
			t.Errorf("тишина %d с: тред засчитан умершим, хотя наблюдение оборвалось раньше", h.SilenceSec)
		}
	}
}

func TestContinueProbabilityTakesNearestMeasured(t *testing.T) {
	c := DecayCurve{Hazard: []DecayHazard{
		{SilenceSec: 900, Continued: 8, Stopped: 2, P: 0.8},
		{SilenceSec: 3600, Continued: 3, Stopped: 7, P: 0.3},
		{SilenceSec: 86400, Continued: 1, Stopped: 9, P: 0.1},
	}}
	cases := []struct {
		silence time.Duration
		want    float64
	}{
		{5 * time.Minute, 1.0},  // до первого порога затухания ещё нет
		{30 * time.Minute, 0.8}, // между порогами берётся ближайший снизу
		{2 * time.Hour, 0.3},
		{48 * time.Hour, 0.1},
	}
	for _, tc := range cases {
		if got := c.ContinueProbability(tc.silence); got != tc.want {
			t.Errorf("после %s: %v, ожидалось %v", tc.silence, got, tc.want)
		}
	}
}

func TestGapBucket(t *testing.T) {
	cases := map[int]int{1: 0, 5: 0, 6: 1, 10: 1, 11: 2, 20: 2, 21: 3, 50: 3, 51: 4, 900: 4}
	for pos, want := range cases {
		if got := gapBucket(pos); got != want {
			t.Errorf("позиция %d → корзина %d, ожидалась %d", pos, got, want)
		}
	}
	if gapBucket(0) != -1 {
		t.Error("нулевая позиция должна оставаться вне корзин")
	}
}
