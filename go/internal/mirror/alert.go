package mirror

import (
	"context"
	"sync"
)

// Пороги и ключи уведомлений админу.
const alertThreshold = 3 // подряд неудач, прежде чем беспокоить админа

const (
	keyFeedDrift     = "дрейф вёрстки ленты"
	keyCommentsDrift = "дрейф вёрстки комментариев"
	keyForbidden     = "доступ к сайту (403)"
)

// alerter — троттлер уведомлений: считает последовательные сбои по ключу и
// шлёт сообщение один раз при достижении порога, затем молчит до восстановления.
// Первый успех сбрасывает счётчик и (если алерт горел) шлёт «восстановилось».
// Потокобезопасен: воркеры комментариев работают в разных горутинах.
type alerter struct {
	send      func(ctx context.Context, text string)
	threshold int

	mu     sync.Mutex
	fails  map[string]int
	firing map[string]bool
}

func newAlerter(send func(ctx context.Context, text string), threshold int) *alerter {
	if send == nil {
		send = func(context.Context, string) {
			// Админ не задан — уведомлять некому.
		}
	}
	if threshold < 1 {
		threshold = 1
	}
	return &alerter{
		send:      send,
		threshold: threshold,
		fails:     make(map[string]int),
		firing:    make(map[string]bool),
	}
}

// fail фиксирует сбой по ключу; при достижении порога один раз шлёт алерт.
func (a *alerter) fail(ctx context.Context, key, detail string) {
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

// ok фиксирует успех по ключу; если алерт горел — шлёт уведомление о восстановлении.
func (a *alerter) ok(ctx context.Context, key string) {
	a.mu.Lock()
	wasFiring := a.firing[key]
	a.fails[key] = 0
	a.firing[key] = false
	a.mu.Unlock()
	if wasFiring {
		a.send(ctx, key+": восстановилось")
	}
}
