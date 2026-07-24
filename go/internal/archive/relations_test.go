package archive

import (
	"context"
	"testing"
	"time"
)

func TestActiveIdentitiesSince(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "старый"}, {ID: 2, Name: "свежий"}}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "давно", PublishedAt: old},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: "недавно", PublishedAt: fresh},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveCountsSince(ctx, "2026-07-16T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if active["u1"] != 0 {
		t.Errorf("старый автор попал в активные за неделю: %d", active["u1"])
	}
	if active["u2"] != 1 {
		t.Errorf("свежий автор: активность %d, want 1", active["u2"])
	}
}

func TestReplyTone(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"спасибо тебе большое", 1},
		{"Спасибо!", 1},
		{"ну ты и дурак", -1},
		{"обычный ответ про погоду", 0},
		{":::flowers::: с праздником", 1},
		{":::mad::: опять ты", -1},
		{"смешно )))", 1},
		{"грустно (((", -1},
		{"дурак, но спасибо и :::flowers:::", 1}, // 2 позитива против 1 негатива
		{"скобка (одна) не смайл", 0},
		{"который час", 0}, // словограница: «дура» не ловится в «дурацкий»? отдельный кейс ниже
		{"дурацкий фильм", 0}, // «дурак»/«дура» — точные формы, «дурацкий» не оскорбление собеседника
	}
	for _, c := range cases {
		if got := replyTone(c.text); got != c.want {
			t.Errorf("replyTone(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

func TestReciprocity(t *testing.T) {
	if r := reciprocity(10, 5); r != 0.5 {
		t.Errorf("reciprocity(10,5)=%v, want 0.5", r)
	}
	if r := reciprocity(0, 5); r != 0 {
		t.Errorf("reciprocity(0,5)=%v, want 0", r)
	}
	if r := reciprocity(0, 0); r != 0 {
		t.Errorf("reciprocity(0,0)=%v, want 0", r)
	}
}

func TestStratify(t *testing.T) {
	mk := func(n int) []RelationExchange {
		out := make([]RelationExchange, n)
		for i := range out {
			out[i].NoteID = int64(i)
		}
		return out
	}
	all := mk(10)
	got := stratify(all, 3)
	if len(got) != 3 || got[0].NoteID != 0 || got[2].NoteID != 9 {
		t.Errorf("stratify(10,3): %+v — ожидались первый, средний и последний", got)
	}
	if got := stratify(mk(2), 5); len(got) != 2 {
		t.Errorf("stratify меньше лимита: len=%d, want 2", len(got))
	}
}

// seedToneFixture — заметка с перепиской: u1↔u2 дружат (позитив), u1↔u3
// конфликтуют (негатив). Возвращает store.
func seedToneFixture(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := openTemp(t)
	users := []User{{ID: 1, Name: "Ася"}, {ID: 2, Name: "Боря"}, {ID: 3, Name: "Вера"}}
	comments := []Comment{
		// корень u1
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "всем привет", PublishedAt: testNow},
		// u2 отвечает u1 позитивно ×3
		{ID: 11, NoteID: 1, ParentID: 10, AuthorID: 2, Text: "привет, солнышко :::flowers:::", PublishedAt: testNow},
		{ID: 12, NoteID: 1, ParentID: 10, AuthorID: 2, Text: "спасибо за заметку", PublishedAt: testNow},
		{ID: 13, NoteID: 1, ParentID: 10, AuthorID: 2, Text: "классно пишешь )))", PublishedAt: testNow},
		// u1 отвечает u2 позитивно ×2
		{ID: 14, NoteID: 1, ParentID: 11, AuthorID: 1, Text: "и тебе спасибо", PublishedAt: testNow},
		{ID: 15, NoteID: 1, ParentID: 12, AuthorID: 1, Text: "обнимаю :::agree:::", PublishedAt: testNow},
		// u3 отвечает u1 негативно ×3
		{ID: 16, NoteID: 1, ParentID: 10, AuthorID: 3, Text: "бред написала", PublishedAt: testNow},
		{ID: 17, NoteID: 1, ParentID: 14, AuthorID: 3, Text: "ну ты и дура", PublishedAt: testNow},
		{ID: 18, NoteID: 1, ParentID: 10, AuthorID: 3, Text: ":::mad::: чушь", PublishedAt: testNow},
		// u1 отвечает u3 нейтрально ×1
		{ID: 19, NoteID: 1, ParentID: 16, AuthorID: 1, Text: "думай что хочешь", PublishedAt: testNow},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScoreToneAndPairs(t *testing.T) {
	ctx := context.Background()
	s := seedToneFixture(t)

	st, err := s.ScoreTone(ctx, ToneParams{MinReplies: 1}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Replies != 9 {
		t.Errorf("просмотрено ответов %d, want 9", st.Replies)
	}

	rels, err := s.IdentityRelations(ctx, "u2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].To != "u1" {
		t.Fatalf("отношения u2: %+v, want одно ребро к u1", rels)
	}
	r := rels[0]
	if r.Source != RelSourceTone || r.Kind != KindTone {
		t.Errorf("источник/тип: %s/%s, want tone/tone", r.Source, r.Kind)
	}
	if r.Replies != 3 || r.Pos != 3 || r.Neg != 0 || r.Score != 1 {
		t.Errorf("u2→u1: replies=%d pos=%d neg=%d score=%v, want 3/3/0/1", r.Replies, r.Pos, r.Neg, r.Score)
	}
	// взаимность: u2→u1 3 реплики, u1→u2 2 → 2/3
	if r.Reciprocity < 0.66 || r.Reciprocity > 0.67 {
		t.Errorf("взаимность %v, want ≈0.67", r.Reciprocity)
	}

	// u3→u1 негатив
	rels, err = s.IdentityRelations(ctx, "u3", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].Score != -1 || rels[0].Neg != 3 {
		t.Fatalf("u3→u1: %+v, want score=-1 neg=3", rels)
	}

	// идемпотентность
	if _, err := s.ScoreTone(ctx, ToneParams{MinReplies: 1}, testNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM relation_edges WHERE source='tone'"); n != 4 {
		t.Errorf("тональных строк %d, want 4 (u2→u1, u1→u2, u3→u1, u1→u3)", n)
	}
}

func TestRelationCandidatesAndImport(t *testing.T) {
	ctx := context.Background()
	s := seedToneFixture(t)
	if _, err := s.ScoreTone(ctx, ToneParams{MinReplies: 1}, testNow); err != nil {
		t.Fatal(err)
	}

	// ядро: пары с суммой ≥5 (u1↔u2: 3+2=5, u1↔u3: 3+1=4 — мимо ядра),
	// полоса [2,5): u1↔u3 поляризована — добирается band-top'ом.
	cands, err := s.RelationCandidates(ctx, RelationCandidateParams{
		MinReplies: 5, BandMin: 2, BandTop: 1, Exchanges: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("кандидатов %d, want 2 (ядро u1↔u2 + полоса u1↔u3)", len(cands))
	}
	core := cands[0]
	if core.A != "u1" || core.B != "u2" || core.RepliesAB != 2 || core.RepliesBA != 3 {
		t.Errorf("ядро: %+v, want u1↔u2 2/3", core)
	}
	if len(core.Exchanges) == 0 || core.Exchanges[0].Parent == "" || core.Exchanges[0].Reply == "" {
		t.Errorf("обмены ядра пусты или без текста: %+v", core.Exchanges)
	}

	// импорт разметки: u1↔u2 дружба, кривой kind — пропуск
	st, err := s.ImportRelations(ctx, []RelationImport{
		{A: "u1", B: "u2", Kind: KindFriendship, Confidence: 0.9, Evidence: []string{"обнимаю"}},
		{A: "u1", B: "u3", Kind: "враги"}, // не из словаря
		{A: "uСтарый", AccountsA: []int64{3}, B: "u1", Kind: KindConflict, Confidence: 0.8}, // ремап
	}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Written != 2 || st.Skipped != 1 || st.Remapped != 1 {
		t.Errorf("итог импорта %+v, want written=2 skipped=1 remapped=1", st)
	}

	// v_relations: llm перекрывает tone, тон остаётся в колонке Tone
	rels, err := s.IdentityRelations(ctx, "u1", 0)
	if err != nil {
		t.Fatal(err)
	}
	byTo := map[string]RelationRow{}
	for _, r := range rels {
		byTo[r.To] = r
	}
	if r := byTo["u2"]; r.Source != RelSourceLLM || r.Kind != KindFriendship {
		t.Errorf("u1→u2: %s/%s, want llm/friendship", r.Source, r.Kind)
	}
	if r := byTo["u2"]; r.Replies != 2 {
		t.Errorf("u1→u2 replies=%d, want 2 (унаследовано от tone)", r.Replies)
	}
	if r := byTo["u3"]; r.Kind != KindConflict {
		t.Errorf("u1→u3 после ремапа: kind=%s, want conflict", r.Kind)
	}

	// метки для графа: kind + tone
	marks, err := s.AllRelationMarks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := marks["u2\x00u1"]
	if m.Kind != KindFriendship || m.Tone != 1 {
		t.Errorf("метка u2→u1: %+v, want friendship/1", m)
	}
}
