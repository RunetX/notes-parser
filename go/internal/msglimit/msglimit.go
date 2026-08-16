// Пакет msglimit — пер-чатовые лимитеры отправки мессенджера: свой темп у
// канала, у чата обсуждения и у личек. Общий для tgx и maxx: у обоих ключ —
// int64 (id чата/пользователя) и одинаковая семантика «один запрос за
// интервал». Сами интервалы и классификация 429 остаются у транспортов —
// у мессенджеров они разные.
package msglimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiters раздаёт лимитер по ключу, создавая его при первом обращении.
// Интервал выбирает переданное замыкание. Нулевое значение непригодно —
// пользуйтесь New.
type Limiters struct {
	interval func(key int64) time.Duration

	mu   sync.Mutex
	byID map[int64]*rate.Limiter
}

// New создаёт набор лимитеров. interval получает ключ и отдаёт минимальный
// промежуток между отправками для него.
func New(interval func(key int64) time.Duration) *Limiters {
	return &Limiters{interval: interval, byID: make(map[int64]*rate.Limiter)}
}

// For возвращает лимитер ключа. Burst равен 1: очередь важнее пиковой
// скорости — превышение темпа стоит флуд-контроля, а он останавливает
// доставку всем. Нулевой интервал — без ограничения (так тесты не ждут).
func (l *Limiters) For(key int64) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.byID[key]; ok {
		return lim
	}
	limit := rate.Inf
	if iv := l.interval(key); iv > 0 {
		limit = rate.Every(iv)
	}
	lim := rate.NewLimiter(limit, 1)
	l.byID[key] = lim
	return lim
}

// Unlimited — набор без задержек (тесты и диагностика).
func Unlimited() *Limiters {
	return New(func(int64) time.Duration { return 0 })
}
