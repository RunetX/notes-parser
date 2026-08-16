package pulpit

import (
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
