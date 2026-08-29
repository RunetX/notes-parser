package archive

import (
	"context"
	"testing"
	"time"
)

// Возможность повторного захода — очередная ЧУЖАЯ реплика после того, как
// человек в треде заговорил; успех — свой КОРЕНЬ до следующей чужой. Тред
// собран так, что счёт ведётся руками:
//
//	+1м  a2 корень            первый заход — возможности перед ним нет
//	+2м  a3 чужая             возможность 1 → следом свой корень      успех
//	+3м  a2 корень            повторный заход
//	+4м  a3 чужая             возможность 2 → следом свой ОТВЕТ       мимо
//	+5м  a2 ответ на 4м
//	+6м  a3 чужая             возможность 3 → своего больше нет       мимо
func TestMineRootRateCountsRepeatEntries(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Наш"}, {ID: 3, Name: "Чужой"}}
	note := Note{ID: 500, AuthorID: 1, Text: "заметка", PublishedAt: t0}
	comments := []Comment{
		{ID: 2001, NoteID: 500, AuthorID: 2, Text: "мысль", PublishedAt: at(1)},
		{ID: 2002, NoteID: 500, AuthorID: 3, Text: "а вот", PublishedAt: at(2)},
		{ID: 2003, NoteID: 500, AuthorID: 2, Text: "и ещё мысль", PublishedAt: at(3)},
		{ID: 2004, NoteID: 500, AuthorID: 3, Text: "гм", PublishedAt: at(4)},
		{ID: 2005, NoteID: 500, AuthorID: 2, Text: "Чужой, отвечаю", PublishedAt: at(5)},
		{ID: 2006, NoteID: 500, AuthorID: 3, Text: "ну ладно", PublishedAt: at(6)},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, t0); err != nil {
		t.Fatalf("SaveGrab: %v", err)
	}
	// Единственное настоящее ребро: 2005 отвечает 2004. Остальные реплики корни.
	if _, err := s.SaveReplyTree(ctx, 500, map[int64]int64{2005: 2004}); err != nil {
		t.Fatalf("SaveReplyTree: %v", err)
	}

	r, err := s.MineRootRate(ctx, []int64{2}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.Threads != 1 {
		t.Errorf("тредов %d, ожидался 1", r.Threads)
	}
	if r.Firsts != 1 || r.Repeats != 1 {
		t.Errorf("первых %d, повторных %d — ожидалось 1 и 1", r.Firsts, r.Repeats)
	}
	var chances, answers int
	for _, b := range r.Buckets {
		chances += b.Chances
		answers += b.Answers
	}
	if chances != 3 || answers != 1 {
		t.Errorf("возможностей %d, заходов %d — ожидалось 3 и 1", chances, answers)
	}
	// Накал считается по тем же возможностям, что и позиция: две меры одного
	// замера обязаны сойтись в сумме, иначе одна из них считает не то.
	var tc, ta int
	for _, b := range r.Tempo {
		tc += b.Chances
		ta += b.Answers
	}
	if tc != chances || ta != answers {
		t.Errorf("накал набрал %d/%d против %d/%d по позиции", tc, ta, chances, answers)
	}
}

// Тощая корзина замером не считается — тот же порог, что у отклика: доля по трём
// случаям это отсутствие данных, а не редкость события.
func TestRootRateRefusesThinBucket(t *testing.T) {
	r := RootRate{Buckets: []RateBucket{
		{Upto: 10, Chances: 5, Answers: 2},
		{Upto: 1 << 30, Chances: 1000, Answers: 100},
	}}
	if _, ok := r.Rate(3); ok {
		t.Error("тощая корзина принята за замер")
	}
	if p, ok := r.Rate(500); !ok || p != 0.1 {
		t.Errorf("набранная корзина: p=%v, ok=%v", p, ok)
	}
	if _, ok := (RootRate{}).Rate(1); ok {
		t.Error("пустой замер выдал себя за измерение")
	}
}
