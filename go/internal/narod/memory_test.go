package narod

import (
	"context"
	"testing"
	"time"
)

func testWorld(t *testing.T) (*World, context.Context) {
	t.Helper()
	ctx := context.Background()
	w, err := OpenWorld(ctx, MemoryWorld)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for _, a := range []Actor{
		{ID: "ivan", Kind: ActorPersona, Nick: "Иван"},
		{ID: "olga", Kind: ActorPersona, Nick: "Ольга"},
	} {
		if err := w.UpsertActor(ctx, a, now); err != nil {
			t.Fatal(err)
		}
	}
	return w, ctx
}

// Шкала отношений имеет ПОТОЛОК, и держит его хранилище, а не дисциплина
// зовущего: дельты приходят от модели, а модель однажды вернёт двадцать.
func TestNudgeClampsToScale(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	for range 5 {
		if _, err := w.Nudge(ctx, EdgeDelta{Src: "ivan", Dst: "olga",
			Sympathy: 4, Familiarity: 1}, now); err != nil {
			t.Fatal(err)
		}
	}
	e, err := w.EdgeOf(ctx, "ivan", "olga")
	if err != nil {
		t.Fatal(err)
	}
	if e.Sympathy != EdgeScale {
		t.Errorf("симпатия %v, ожидался потолок %v", e.Sympathy, EdgeScale)
	}
	// А знакомство потолка не имеет: это счётчик встреч, а не оценка.
	if e.Familiarity != 5 {
		t.Errorf("знакомство %v, ожидалось 5", e.Familiarity)
	}
}

// Ребро НАПРАВЛЕННОЕ: пара, где один прощает, а второй помнит, — как раз то,
// ради чего граф заводится.
func TestEdgesAreDirected(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	if _, err := w.Nudge(ctx, EdgeDelta{Src: "ivan", Dst: "olga", Sympathy: 3}, now); err != nil {
		t.Fatal(err)
	}
	back, err := w.EdgeOf(ctx, "olga", "ivan")
	if err != nil {
		t.Fatal(err)
	}
	if back.Sympathy != 0 {
		t.Errorf("обратное ребро завелось само: %v", back.Sympathy)
	}
}

// Незнакомые — это НОЛЬ, а не ошибка: в мире их большинство.
func TestEdgeOfStrangersIsZero(t *testing.T) {
	w, ctx := testWorld(t)
	e, err := w.EdgeOf(ctx, "ivan", "olga")
	if err != nil {
		t.Fatalf("незнакомая пара дала ошибку: %v", err)
	}
	if e.Sympathy != 0 || e.Familiarity != 0 {
		t.Errorf("у незнакомых уже есть отношение: %+v", e)
	}
}

func TestNudgeRejectsSelfEdge(t *testing.T) {
	w, ctx := testWorld(t)
	if _, err := w.Nudge(ctx, EdgeDelta{Src: "ivan", Dst: "ivan", Sympathy: 1}, time.Now()); err == nil {
		t.Error("ребро в себя записалось")
	}
}

// Вид эпизода — ЗАКРЫТЫЙ список, и проверяет его хранилище. Модель, которой
// позволено придумывать виды отношений, через десяток тредов заведёт «взаимное
// уважение с оттенком иронии», и сравнивать миры станет нечем.
func TestAddEpisodeRejectsUnknownKind(t *testing.T) {
	w, ctx := testWorld(t)
	_, err := w.AddEpisode(ctx, Episode{Src: "ivan", Dst: "olga",
		Kind: "взаимное уважение с оттенком иронии", Summary: "…", At: time.Now()})
	if err == nil {
		t.Error("эпизод неизвестного вида записался")
	}
}

// Длинный пересказ обрезается: эпизод обязан помещаться в промпт десятком штук.
func TestAddEpisodeTrimsSummary(t *testing.T) {
	w, ctx := testWorld(t)
	long := make([]rune, EpisodeSummaryRunes+100)
	for i := range long {
		long[i] = 'а'
	}
	if _, err := w.AddEpisode(ctx, Episode{Src: "ivan", Dst: "olga",
		Kind: EpisodeFight, Summary: string(long), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, err := w.EpisodesOf(ctx, "ivan", "olga", 5)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Summary)); n != EpisodeSummaryRunes {
		t.Errorf("пересказ длиной %d, потолок %d", n, EpisodeSummaryRunes)
	}
}

// Эпизоды отдаются от НОВЫХ к старым и несут ссылки на реплики: без них «а
// помнишь» не на что опереть.
func TestEpisodesNewestFirst(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	for i, kind := range []string{EpisodeTease, EpisodeFight, EpisodeAgree} {
		if _, err := w.AddEpisode(ctx, Episode{Src: "ivan", Dst: "olga", Kind: kind,
			Summary: kind, CommentIDs: []int64{int64(i + 1)}, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.EpisodesOf(ctx, "ivan", "olga", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != EpisodeAgree {
		t.Fatalf("эпизоды пришли не тем порядком: %+v", got)
	}
	if len(got[0].CommentIDs) != 1 || got[0].CommentIDs[0] != 3 {
		t.Errorf("ссылки на реплики потерялись: %+v", got[0].CommentIDs)
	}
}

// Монетка бросается РОВНО ОДИН раз на пару (актор, событие). Пятнадцать
// процентов, спрошенные десять раз за десять тактов, превращаются в восемьдесят
// — урок оплачен амвоном.
func TestRollHappensOnce(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	first, fresh, err := w.Roll(ctx, Dice{ActorID: "ivan", EventID: "c1",
		P: 0.15, Roll: 0.1, Verdict: DiceCome, At: now})
	if err != nil || !fresh {
		t.Fatalf("первый бросок: fresh=%v err=%v", fresh, err)
	}
	// Второй бросок с ДРУГИМ исходом обязан вернуть первый.
	again, fresh, err := w.Roll(ctx, Dice{ActorID: "ivan", EventID: "c1",
		P: 0.15, Roll: 0.9, Verdict: DiceSkip, At: now})
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Error("вторая монетка засчиталась как новая")
	}
	if again.Verdict != first.Verdict || again.Roll != first.Roll {
		t.Errorf("исход переписан: было %+v, стало %+v", first, again)
	}
}

// Журнал пишется до публикации и читается от новых к старым — это личная
// память персонажа, а не лог.
func TestRecallNewestFirst(t *testing.T) {
	w, ctx := testWorld(t)
	now := time.Now()
	for _, text := range []string{"первая", "вторая", "третья"} {
		if _, err := w.Remember(ctx, JournalEntry{ActorID: "ivan", At: now,
			Kind: JournalComment, NoteID: 500, Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.Recall(ctx, "ivan", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "третья" || got[1].Text != "вторая" {
		t.Errorf("память пришла не тем порядком: %+v", got)
	}
}
