package modwatch

import (
	"context"
	"log/slog"
	"time"

	"lovegw/internal/love"
)

// GuestSource — то, что наблюдателю визитов нужно от сайта: страница списка
// гостей своей анкеты (page с единицы).
type GuestSource interface {
	Guests(ctx context.Context, page int) ([]love.Guest, error)
}

// Значения по умолчанию для наблюдения за визитами.
//
// Сайт отдаёт время визита сам, поэтому такт нужен не для точности, а чтобы
// успеть снять визит до того, как тот же человек придёт снова и затрёт запись.
// Пятнадцать минут — это два обращения в час при глубине списка в 124 записи.
const (
	DefaultGuestInterval = 15 * time.Minute
	DefaultGuestPages    = 6 // 24 записи на странице; глубина ~26 суток
)

// GuestWatcher — снимает список гостей и копит визиты.
type GuestWatcher struct {
	Source GuestSource
	Store  *Store
	Log    *slog.Logger

	Interval time.Duration // период снятия
	Pages    int           // сколько страниц обходить

	Now func() time.Time
}

func (w *GuestWatcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *GuestWatcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *GuestWatcher) defaults() {
	if w.Interval <= 0 {
		w.Interval = DefaultGuestInterval
	}
	if w.Pages <= 0 {
		w.Pages = DefaultGuestPages
	}
}

// Run крутит снятие до отмены контекста. Ошибка одного прохода цикл не роняет:
// сайт за DDoS-Guard иногда отвечает 403 или 5xx, пропуск такта не страшен —
// визит останется на странице до следующего прихода того же человека.
func (w *GuestWatcher) Run(ctx context.Context) error {
	w.defaults()
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		if _, err := w.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.log().Warn("список гостей не снят", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Poll — один проход по страницам списка. Возвращает число новых визитов.
//
// Обход останавливается, когда страница не принесла ни одного невиданного в
// этом проходе id: за концом списка сайт молча отдаёт последнюю страницу
// повторно, и без этой проверки цикл шёл бы до предела страниц.
func (w *GuestWatcher) Poll(ctx context.Context) (int, error) {
	w.defaults()
	now := w.now()
	seen := map[int64]bool{}
	fresh := 0
	for page := 1; page <= w.Pages; page++ {
		guests, err := w.Source.Guests(ctx, page)
		if err != nil {
			return fresh, err
		}
		newOnPage := 0
		for _, g := range guests {
			if seen[g.ID] {
				continue
			}
			seen[g.ID] = true
			newOnPage++
			at := g.VisitedAt
			if at.IsZero() {
				// Формат времени незнаком: визит всё равно записываем, но
				// временем ставим момент снятия, чтобы не потерять факт.
				at = now
			}
			isNew, err := w.Store.SaveVisit(ctx, now, Visit{
				UserID: g.ID, Nick: g.Nick, VisitedAt: at, Raw: g.Raw,
			})
			if err != nil {
				return fresh, err
			}
			if isNew {
				fresh++
				w.log().Info("визит в анкету", "гость", g.Nick, "анкета", g.ID, "когда", g.Raw)
			}
		}
		if newOnPage == 0 {
			break
		}
	}
	return fresh, nil
}
