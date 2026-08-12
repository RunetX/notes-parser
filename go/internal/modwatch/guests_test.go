package modwatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lovegw/internal/love"
)

// fakeGuests — подставной список гостей по страницам.
type fakeGuests struct {
	pages [][]love.Guest
	calls int
}

func (f *fakeGuests) Guests(_ context.Context, page int) ([]love.Guest, error) {
	f.calls++
	if page > len(f.pages) {
		return f.pages[len(f.pages)-1], nil // сайт повторяет последнюю страницу
	}
	return f.pages[page-1], nil
}

func newGuestWatcher(t *testing.T, src GuestSource) (*GuestWatcher, *Store, *time.Time) {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "modwatch.db"))
	if err != nil {
		t.Fatalf("открытие БД: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	now := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	return &GuestWatcher{Source: src, Store: store, Now: func() time.Time { return now }}, store, &now
}

func guest(id int64, nick string, at time.Time) love.Guest {
	return love.Guest{ID: id, Nick: nick, VisitedAt: at, Raw: at.Format("02.01 15:04")}
}

// Тот же визит, снятый повторно, дублем не считается; новый визит того же
// человека — считается: сайт затирает прежний, а у нас остаются оба.
func TestGuestPollKeepsHistoryOfRepeatVisits(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 12, 19, 38, 0, 0, time.UTC)
	src := &fakeGuests{pages: [][]love.Guest{{
		guest(175869, "Гадёныш", base),
		guest(1450213, "ЗАЯ в трениках", base.Add(-3*time.Hour)),
	}}}
	w, store, now := newGuestWatcher(t, src)

	fresh, err := w.Poll(ctx)
	if err != nil {
		t.Fatalf("снятие: %v", err)
	}
	if fresh != 2 {
		t.Fatalf("новых визитов %d, ожидалось 2", fresh)
	}
	// Повтор того же списка ничего не добавляет.
	*now = now.Add(15 * time.Minute)
	if fresh, err = w.Poll(ctx); err != nil || fresh != 0 {
		t.Fatalf("повторное снятие дало %d новых (%v)", fresh, err)
	}
	// Гадёныш пришёл снова — на сайте прежняя строка затёрта, у нас остаются обе.
	*now = now.Add(24 * time.Hour)
	src.pages[0][0] = guest(175869, "Гадёныш", base.Add(25*time.Hour))
	if fresh, err = w.Poll(ctx); err != nil || fresh != 1 {
		t.Fatalf("новый визит не пойман: %d (%v)", fresh, err)
	}
	visits, err := store.VisitsIn(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("чтение визитов: %v", err)
	}
	if len(visits) != 3 {
		t.Fatalf("в БД %d визитов, ожидалось 3 (история не должна теряться)", len(visits))
	}
	var gadfly int
	for _, v := range visits {
		if v.UserID == 175869 {
			gadfly++
		}
	}
	if gadfly != 2 {
		t.Fatalf("у Гадёныша %d визитов, ожидалось 2", gadfly)
	}
}

// За концом списка сайт повторяет последнюю страницу — обход обязан
// остановиться, а не листать до предела.
func TestGuestPollStopsOnRepeatedPage(t *testing.T) {
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	src := &fakeGuests{pages: [][]love.Guest{
		{guest(1, "Первый", base)},
		{guest(2, "Второй", base.Add(-time.Hour))},
	}}
	w, _, _ := newGuestWatcher(t, src)
	w.Pages = 6
	if _, err := w.Poll(context.Background()); err != nil {
		t.Fatalf("снятие: %v", err)
	}
	if src.calls != 3 { // две настоящие страницы плюс одна повторная
		t.Fatalf("страниц запрошено %d, ожидалось 3", src.calls)
	}
}

// Незнакомый формат времени не должен терять сам факт визита.
func TestGuestPollKeepsVisitWithUnknownTime(t *testing.T) {
	ctx := context.Background()
	src := &fakeGuests{pages: [][]love.Guest{{{ID: 42, Nick: "Кто-то", Raw: "только что"}}}}
	w, store, now := newGuestWatcher(t, src)
	if _, err := w.Poll(ctx); err != nil {
		t.Fatalf("снятие: %v", err)
	}
	visits, err := store.VisitsIn(ctx, time.Time{}, time.Time{})
	if err != nil || len(visits) != 1 {
		t.Fatalf("визит потерян: %d (%v)", len(visits), err)
	}
	if !visits[0].VisitedAt.Equal(*now) || visits[0].Raw != "только что" {
		t.Fatalf("нераспознанное время записано неверно: %+v", visits[0])
	}
}
