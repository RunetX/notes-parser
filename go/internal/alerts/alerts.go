// Пакет alerts — троттлер уведомлений админу: считает последовательные сбои по
// ключу и шлёт сообщение один раз при достижении порога, затем молчит до
// восстановления. Первый успех сбрасывает счётчик и (если алерт горел) шлёт
// «восстановилось». Потокобезопасен: воркеры зеркала и поллера talks работают
// в разных горутинах. Ключи — доменные, задаёт вызывающий пакет.
package alerts

import (
	"context"
	"sync"
)

// Alerter троттлит уведомления по ключу (см. док пакета).
type Alerter struct {
	send      func(ctx context.Context, text string)
	threshold int

	mu     sync.Mutex
	fails  map[string]int
	firing map[string]bool
}

// New создаёт троттлер. send=nil — админ не задан, уведомлять некому (no-op).
// threshold < 1 поднимается до 1.
func New(send func(ctx context.Context, text string), threshold int) *Alerter {
	if send == nil {
		send = func(context.Context, string) {}
	}
	if threshold < 1 {
		threshold = 1
	}
	return &Alerter{
		send:      send,
		threshold: threshold,
		fails:     make(map[string]int),
		firing:    make(map[string]bool),
	}
}

// Fail фиксирует сбой по ключу; при достижении порога один раз шлёт алерт.
func (a *Alerter) Fail(ctx context.Context, key, detail string) {
	a.mu.Lock()
	a.fails[key]++
	fire := a.fails[key] >= a.threshold && !a.firing[key]
	if fire {
		a.firing[key] = true
	}
	a.mu.Unlock()
	if fire {
		a.send(ctx, key+": "+detail)
	}
}

// OK фиксирует успех по ключу; если алерт горел — шлёт уведомление о восстановлении.
func (a *Alerter) OK(ctx context.Context, key string) {
	a.mu.Lock()
	wasFiring := a.firing[key]
	a.fails[key] = 0
	a.firing[key] = false
	a.mu.Unlock()
	if wasFiring {
		a.send(ctx, key+": восстановилось")
	}
}
