package modwatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"lovegw/internal/love"
)

// fakeProfiles — подставной сайт: у каждой анкеты своя очередь отметок, чтобы
// проверить накопление следа между опросами.
type fakeProfiles struct {
	at    map[int64][]time.Time
	calls []int64
}

func (f *fakeProfiles) Activity(_ context.Context, id int64) (love.Activity, error) {
	f.calls = append(f.calls, id)
	a := love.Activity{UserID: id, Nick: "ник" + string(rune('a'+id%26))}
	if q := f.at[id]; len(q) > 0 {
		a.At = q[0]
		a.Raw = q[0].Format("02.01.2006 15:04")
		if len(q) > 1 {
			f.at[id] = q[1:]
		}
	}
	return a, nil
}

type fakeRoster []Commenter

func (r fakeRoster) Commenters(context.Context, time.Time, int) ([]Commenter, error) {
	return r, nil
}

func newActivityStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "modwatch.db"))
	if err != nil {
		t.Fatalf("открытие БД: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Сайт хранит только последнее действие человека — значит, ценность есть
// только у СВОЕГО следа: каждая новая отметка ложится рядом, повтор той же
// ничего не добавляет.
func TestActivityTrailAccumulates(t *testing.T) {
	ctx := context.Background()
	s := newActivityStore(t)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	first := time.Date(2026, 8, 13, 8, 50, 0, 0, time.UTC)
	second := time.Date(2026, 8, 13, 9, 40, 0, 0, time.UTC)

	for i, at := range []time.Time{first, first, second} {
		fresh, err := s.SaveActivity(ctx, now.Add(time.Duration(i)*time.Hour),
			love.Activity{UserID: 42, Nick: "Актриса", At: at})
		if err != nil {
			t.Fatalf("запись отметки: %v", err)
		}
		if want := i != 1; fresh != want { // вторая — повтор той же минуты
			fresh, want := fresh, want
			t.Errorf("отметка #%d: новизна %v, ожидалась %v", i, fresh, want)
		}
	}
	stamps, err := s.ActivityIn(ctx, 42, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("чтение следа: %v", err)
	}
	if len(stamps) != 2 || !stamps[0].At.Equal(first) || !stamps[1].At.Equal(second) {
		t.Fatalf("след = %+v, ожидались две отметки по возрастанию", stamps)
	}
	profiles, err := s.Profiles(ctx)
	if err != nil {
		t.Fatalf("чтение анкет: %v", err)
	}
	if p := profiles[42]; !p.LastAt.Equal(second) || p.Nick != "Актриса" || p.CheckedAt.IsZero() {
		t.Errorf("состояние анкеты = %+v, ожидалась последняя отметка %s", p, second)
	}
}

// Очередь обхода: сначала те, кого не смотрели ни разу, потом самые давние.
// Без этого свежезаведённые анкеты ждали бы полного круга.
func TestProfilesDueOrder(t *testing.T) {
	ctx := context.Background()
	s := newActivityStore(t)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if _, err := s.TrackProfiles(ctx, []int64{1, 2, 3}); err != nil {
		t.Fatalf("заведение анкет: %v", err)
	}
	if _, err := s.SaveActivity(ctx, now, love.Activity{UserID: 1}); err != nil {
		t.Fatalf("опрос анкеты 1: %v", err)
	}
	if _, err := s.SaveActivity(ctx, now.Add(-time.Hour), love.Activity{UserID: 2}); err != nil {
		t.Fatalf("опрос анкеты 2: %v", err)
	}
	due, err := s.ProfilesDue(ctx, 3)
	if err != nil {
		t.Fatalf("очередь: %v", err)
	}
	want := []int64{3, 2, 1} // 3 не опрошена, 2 давнее 1
	for i := range want {
		if i >= len(due) || due[i] != want[i] {
			t.Fatalf("очередь = %v, ожидалась %v", due, want)
		}
	}
}

// Такт обхода: круг пополняется из источника, порция берётся из очереди, а
// подозреваемые опрашиваются каждый раз — и не по второму разу, если они же
// стоят в очереди.
func TestActivityWatcherPoll(t *testing.T) {
	ctx := context.Background()
	s := newActivityStore(t)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	site := &fakeProfiles{at: map[int64][]time.Time{
		10: {now.Add(-10 * time.Minute)},
		11: {now.Add(-20 * time.Minute)},
		12: {now.Add(-30 * time.Minute)},
	}}
	w := &ActivityWatcher{
		Source: site,
		Roster: fakeRoster{{UserID: 10}, {UserID: 11}, {UserID: 12}},
		Store:  s, Batch: 2, Always: []int64{12},
		Now: func() time.Time { return now },
	}
	fresh, err := w.Poll(ctx)
	if err != nil {
		t.Fatalf("такт: %v", err)
	}
	if fresh != 3 {
		t.Errorf("новых отметок %d, ожидалось 3", fresh)
	}
	if len(site.calls) != 3 {
		t.Errorf("запросов к сайту %d (%v), ожидалось 3: порция 2 плюс подозреваемый", len(site.calls), site.calls)
	}
	if site.calls[0] != 12 {
		t.Errorf("первым опрошен %d, ожидался подозреваемый 12", site.calls[0])
	}

	// Второй такт: очередь двигается — теперь смотрим тех, кого пропустили.
	fresh, err = w.Poll(ctx)
	if err != nil {
		t.Fatalf("второй такт: %v", err)
	}
	if fresh != 0 {
		t.Errorf("на втором такте отметки те же, новых быть не должно, получено %d", fresh)
	}
}

// Круг наблюдения по своим комментариям — запасной источник, когда боевой БД
// рядом нет.
func TestStoreCommenters(t *testing.T) {
	ctx := context.Background()
	s := newActivityStore(t)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	save := func(id, author int64, at time.Time) {
		t.Helper()
		if err := s.SaveComment(ctx, now, CommentState{
			ID: id, NoteID: 1, AuthorID: author, AuthorName: "автор", PublishedAt: at,
		}); err != nil {
			t.Fatalf("запись реплики: %v", err)
		}
	}
	save(1, 100, now.Add(-72*time.Hour))
	save(2, 100, now.Add(-48*time.Hour))
	save(3, 200, now.Add(-time.Hour))

	people, err := s.Commenters(ctx, now.Add(-96*time.Hour), 2)
	if err != nil {
		t.Fatalf("круг: %v", err)
	}
	if len(people) != 1 || people[0].UserID != 100 || people[0].Comments != 2 {
		t.Fatalf("круг = %+v, ожидался один автор со двумя репликами", people)
	}
	if !people[0].LastComment.Equal(now.Add(-48 * time.Hour)) {
		t.Errorf("последняя реплика = %s, ожидалась %s", people[0].LastComment, now.Add(-48*time.Hour))
	}
}
