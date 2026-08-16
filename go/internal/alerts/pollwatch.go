package alerts

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Пороги распознавания полосы сбоев поллинга (см. PollWatch).
const (
	// pollStreakGap — пауза между ошибками, разрывающая полосу: при мёртвом
	// поллинге go-telegram/bot выдаёт ошибку не реже чем раз в ~40 с
	// (long poll 30 с + backoff до 5 с), значит, более длинная пауза — в
	// промежутке были успешные опросы.
	pollStreakGap = 3 * time.Minute
	// pollStreakCount / pollStreakAge — сколько ошибок подряд или какой
	// длительности полоса поднимает алерт (что наступит раньше).
	pollStreakCount = 10
	pollStreakAge   = 2 * time.Minute
)

// PollWatch — здоровье long polling для транспорта БЕЗ сигнала об успехе:
// go-telegram/bot отдаёт наружу только ошибки (WithErrorsHandler), поэтому
// полоса сбоев распознаётся по плотности ошибок, а её конец — по паузе между
// ними. Отсюда честное ограничение: «восстановился» уходит только со
// СЛЕДУЮЩЕЙ ошибкой после паузы, а не в момент первого успеха. Транспорт с
// явным сигналом успеха (maxx.Start) использует обычный Alerter.
type PollWatch struct {
	name string
	send func(ctx context.Context, text string)
	now  func() time.Time // подменяется в тестах

	mu       sync.Mutex
	streak   int       // ошибок в текущей полосе
	streakAt time.Time // начало полосы
	lastErr  time.Time
	firing   bool
}

// NewPollWatch создаёт наблюдатель. name попадает в текст алерта («поллинг
// <name>: …»); send == nil — уведомлять некому, no-op.
func NewPollWatch(name string, send func(ctx context.Context, text string)) *PollWatch {
	if send == nil {
		send = func(context.Context, string) {}
	}
	return &PollWatch{name: name, send: send, now: time.Now}
}

// Error фиксирует очередную ошибку поллинга и решает, пора ли беспокоить
// админа. Демон при этом продолжает ретраить — алерт информационный:
// «наполовину живой» процесс лучше мёртвого, но молчать он не должен.
func (w *PollWatch) Error(ctx context.Context, err error) {
	now := w.now()
	w.mu.Lock()
	recovered := false
	if !w.lastErr.IsZero() && now.Sub(w.lastErr) > pollStreakGap {
		recovered = w.firing
		w.streak, w.firing = 0, false
	}
	if w.streak == 0 {
		w.streakAt = now
	}
	w.streak++
	w.lastErr = now
	fire := !w.firing && (w.streak >= pollStreakCount || now.Sub(w.streakAt) >= pollStreakAge)
	if fire {
		w.firing = true
	}
	w.mu.Unlock()

	if recovered {
		w.send(ctx, "поллинг "+w.name+": восстановился")
	}
	if fire {
		detail := err.Error()
		if strings.Contains(strings.ToLower(detail), "conflict") {
			detail += " — похоже, бота слушает другой процесс (409)"
		}
		w.send(ctx, "поллинг "+w.name+": ошибки идут подряд, апдейты не читаются: "+detail)
	}
}
