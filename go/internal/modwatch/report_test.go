package modwatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Отчёт должен отделять того, кто присутствует именно в моменты действий, от
// того, кто присутствует всегда: второй набирает столько же совпадений, но и
// контрольные окна закрывает целиком, поэтому z у него около нуля.
func TestAnalyzeSeparatesModeratorFromEverpresent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "modwatch.db"))
	if err != nil {
		t.Fatalf("открытие БД: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	const (
		moderID   = 11 // пишет редко, но в каждый момент действия
		alwaysID  = 22 // пишет постоянно
		observeD  = 10 // суток наблюдения
		eventsCnt = 24
	)
	var id int64
	save := func(user int64, at time.Time) {
		id++
		if err := store.SaveComment(ctx, at, CommentState{
			ID: id, NoteID: 1, AuthorID: user, AuthorName: "автор", PublishedAt: at,
		}); err != nil {
			t.Fatalf("запись реплики: %v", err)
		}
	}
	// Фон: «вездесущий» пишет каждые 10 минут все десять суток, «модератор» —
	// раз в два часа.
	for m := 0; m < observeD*24*60; m += 10 {
		save(alwaysID, base.Add(time.Duration(m)*time.Minute))
	}
	for m := 0; m < observeD*24*60; m += 120 {
		save(moderID, base.Add(time.Duration(m)*time.Minute))
	}
	// События на 4–7 сутки в разные часы; в каждый момент действия модератор
	// оставляет реплику.
	for i := 0; i < eventsCnt; i++ {
		at := base.AddDate(0, 0, 4).Add(time.Duration(i)*7*time.Hour + 17*time.Minute)
		save(moderID, at)
		if err := store.AddEvent(ctx, Event{
			Kind: KindNoteGone, RefID: int64(1000 + i), NoteID: int64(1000 + i),
			PrevSeen: at.Add(-time.Minute), DetectedAt: at.Add(time.Minute),
		}); err != nil {
			t.Fatalf("запись события: %v", err)
		}
	}
	if _, err := store.SaveUser(ctx, base, moderID, "Подозреваемый"); err != nil {
		t.Fatalf("запись анкеты: %v", err)
	}

	rep, err := store.Analyze(ctx, ReportOptions{
		Kinds: []string{KindNoteGone}, Window: 5 * time.Minute, Controls: 10, Seed: 7,
	})
	if err != nil {
		t.Fatalf("анализ: %v", err)
	}
	if rep.Events != eventsCnt {
		t.Fatalf("в расчёт вошло %d событий из %d", rep.Events, eventsCnt)
	}
	var moder, always *ReportRow
	for i := range rep.Rows {
		switch rep.Rows[i].UserID {
		case moderID:
			moder = &rep.Rows[i]
		case alwaysID:
			always = &rep.Rows[i]
		}
	}
	if moder == nil || always == nil {
		t.Fatalf("в отчёте нет обоих участников: %+v", rep.Rows)
	}
	if moder.Hits != eventsCnt {
		t.Fatalf("модератор совпал %d раз из %d", moder.Hits, eventsCnt)
	}
	if moder.Z < 3 {
		t.Fatalf("z модератора слишком мал: %.2f (ожидалось > 3)", moder.Z)
	}
	if always.Z > 1 {
		t.Fatalf("вездесущий получил z=%.2f — фон не вычтен", always.Z)
	}
	if rep.Rows[0].UserID != moderID {
		t.Fatalf("первым в отчёте должен идти модератор, а идёт u%d", rep.Rows[0].UserID)
	}
	if moder.Name != "Подозреваемый" {
		t.Fatalf("ник не подтянулся: %q", moder.Name)
	}
}

// Без контрольных окон (наблюдение короче суток) отчёт обязан честно сказать,
// что считать не на чем, а не выдать пустую таблицу как результат.
func TestAnalyzeWithoutControls(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "modwatch.db"))
	if err != nil {
		t.Fatalf("открытие БД: %v", err)
	}
	defer store.Close()

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for m := 0; m < 60; m += 5 {
		at := base.Add(time.Duration(m) * time.Minute)
		if err := store.SaveComment(ctx, at, CommentState{
			ID: int64(m + 1), NoteID: 1, AuthorID: 11, PublishedAt: at,
		}); err != nil {
			t.Fatalf("запись реплики: %v", err)
		}
	}
	if err := store.AddEvent(ctx, Event{
		Kind: KindNoteGone, RefID: 1, NoteID: 1,
		PrevSeen: base.Add(10 * time.Minute), DetectedAt: base.Add(11 * time.Minute),
	}); err != nil {
		t.Fatalf("запись события: %v", err)
	}
	rep, err := store.Analyze(ctx, ReportOptions{Kinds: []string{KindNoteGone}, Seed: 1})
	if err != nil {
		t.Fatalf("анализ: %v", err)
	}
	if rep.Events != 0 || rep.EventsSkipped != 1 {
		t.Fatalf("ожидалось 0 расчётных и 1 отброшенное событие, получено %d/%d", rep.Events, rep.EventsSkipped)
	}
	if len(rep.Rows) != 0 {
		t.Fatalf("без контроля таблица должна быть пустой: %+v", rep.Rows)
	}
}
