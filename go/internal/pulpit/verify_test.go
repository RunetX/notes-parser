package pulpit

import (
	"context"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// TestNeedsRecheck — подтверждённую реплику перечитываем ровно один раз: через
// полчаса после отправки, и больше никогда.
func TestNeedsRecheck(t *testing.T) {
	now := time.Now()
	posted := now.Add(-time.Hour)

	cases := []struct {
		name string
		row  store.PulpitComment
		want bool
	}{
		{
			name: "свежая — рано",
			row:  store.PulpitComment{PostedAt: now.Add(-time.Minute), CheckedAt: now.Add(-time.Minute)},
		},
		{
			name: "час назад, проверяли сразу после отправки",
			row:  store.PulpitComment{PostedAt: posted, CheckedAt: posted},
			want: true,
		},
		{
			name: "уже перечитывали после горизонта",
			row:  store.PulpitComment{PostedAt: posted, CheckedAt: now.Add(-time.Minute)},
		},
		{
			name: "не отправляли вовсе",
			row:  store.PulpitComment{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsRecheck(tc.row, now); got != tc.want {
				t.Errorf("needsRecheck = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestOwnComment — своя реплика первого уровня самая ранняя из наших: ответы
// на ответы приходят позже и имеют бо́льшие id.
func TestOwnComment(t *testing.T) {
	comments := []love.Comment{
		{ID: 700, AuthorID: "u9", Text: "чужая"},
		{ID: 701, AuthorID: ownID, Text: "своя реплика"},
		{ID: 705, AuthorID: ownID, Text: "Лампочка, ответ"},
	}
	c, ok := ownComment(comments, ownID)
	if !ok || c.ID != 701 {
		t.Fatalf("своя реплика: %+v (%v)", c, ok)
	}
	if _, ok := ownComment(comments, "12345"); ok {
		t.Error("чужой анкете своих реплик быть не должно")
	}
	if _, ok := ownComment(comments, ""); ok {
		t.Error("без своей анкеты искать нечего")
	}
}

// crowd дописывает в тред n чужих реплик — так тред перерастает окно страницы.
func crowd(site *fakeSite, noteID string, n int) {
	site.mu.Lock()
	defer site.mu.Unlock()
	next := site.nextID + 1000
	for i := 0; i < n; i++ {
		site.threads[noteID] = append(site.threads[noteID], love.Comment{
			ID: next + int64(i), AuthorID: "u9", AuthorName: "Прохожий", Text: "чужая реплика",
			PublishedAt: time.Now(),
		})
	}
}

// Ложное срабатывание, из-за которого амвон выключил себя сам 23.08.2026.
//
// Своя реплика в треде САМАЯ СТАРАЯ — амвон пишет первым, в этом весь смысл. А
// перепроверка смотрела обычную страницу, то есть тридцать ПОСЛЕДНИХ реплик:
// стоило заметке набрать тридцать чужих комментариев, и своя уезжала за край
// окна. Вердикт «её вычистили» выносился тем вернее, чем удачнее была реплика.
//
// Боевой случай: заметка 313058, реплика 63238855 на месте (и на странице треда
// она первой строкой из 88), а в БД — missing/deleted ровно через тридцать
// минут после отправки.
func TestConfirmedReplyBeyondNewestWindowIsNotADeletion(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)

	svc.cycle(ctx) // опубликовали и подтвердили
	rows, err := st.PulpitRecent(ctx, 10)
	if err != nil || len(rows) == 0 {
		t.Fatalf("строка не завелась: %v %v", rows, err)
	}
	row := rows[0]
	if row.State != store.PulpitConfirmed {
		t.Fatalf("реплика не подтвердилась: %s/%s", row.State, row.Reason)
	}

	crowd(site, "n1", 40) // тред ожил: своя реплика ушла за край окна «последних»

	svc.recheckConfirmed(ctx, row)

	rows, err = st.PulpitRecent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0]; got.State != store.PulpitConfirmed {
		t.Fatalf("реплика на месте, а помечена как %s/%s", got.State, got.Reason)
	}
}

// А настоящее удаление по-прежнему видно: реплики нет в НАЧАЛЕ треда, где она
// обязана быть. Иначе правка выше означала бы «предохранитель отключён».
func TestDeletedReplyIsStillDetected(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)

	svc.cycle(ctx)
	rows, _ := st.PulpitRecent(ctx, 10)
	row := rows[0]

	// Модерация вычистила нашу реплику, разговор продолжается без неё.
	site.mu.Lock()
	site.threads["n1"] = nil
	site.mu.Unlock()
	crowd(site, "n1", 5)

	svc.recheckConfirmed(ctx, row)

	rows, _ = st.PulpitRecent(ctx, 10)
	if got := rows[0]; got.State != store.PulpitMissing || got.Reason != reasonDeleted {
		t.Fatalf("удаление не замечено: %s/%s", got.State, got.Reason)
	}
}

// Правило охвата: страница, которая НЕ МОГЛА показать реплику, вердикта не даёт.
func TestPageCovers(t *testing.T) {
	page := func(total int, ids ...int64) love.CommentsPage {
		var cs []love.Comment
		for _, id := range ids {
			cs = append(cs, love.Comment{ID: id})
		}
		return love.CommentsPage{Comments: cs, Total: total}
	}
	cases := []struct {
		name string
		page love.CommentsPage
		id   int64
		want bool
	}{
		{"весь тред на странице", page(3, 10, 11, 12), 11, true},
		{"весь тред, реплики нет — значит удалена", page(3, 10, 11, 12), 99, true},
		{"окно частичное, реплика внутри", page(50, 10, 11, 12), 11, true},
		{"окно частичное, реплика дальше окна", page(50, 10, 11, 12), 99, false},
		{"счётчика нет вовсе — судить не о чем", page(0), 11, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageCovers(tc.page, tc.id); got != tc.want {
				t.Errorf("pageCovers = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}
