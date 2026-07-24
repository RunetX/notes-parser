package archive

import (
	"context"
	"testing"
	"time"
)

var testNow = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func TestFlagDisclosures(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 1, Name: "u1"}, {ID: 2, Name: "u2"}}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "Это моя вторая анкета, если что"}, // втор+анкет и это моя+анкет
		{ID: 11, NoteID: 1, AuthorID: 2, Text: "просто комментарий без признаков"}, // мимо
		{ID: 12, NoteID: 1, AuthorID: 2, Text: "пишу ПОД НИКОМ Аноним"},            // под ником (верхний регистр → ulower)
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}

	patterns := []string{"%втор%анкет%", "%это моя%анкет%", "%под ником%", "%фейк%анкет%"}
	st, err := s.FlagDisclosures(ctx, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.Inserted != 2 {
		t.Fatalf("пометок: inserted=%d total=%d, want 2/2", st.Inserted, st.Total)
	}
	// Коммент 10 матчит и «втор», и «это моя» — засчитан первому (PK first-wins).
	if st.PerPattern["%втор%анкет%"] != 1 || st.PerPattern["%это моя%анкет%"] != 0 {
		t.Errorf("пересечение шаблонов: втор=%d это_моя=%d, want 1/0",
			st.PerPattern["%втор%анкет%"], st.PerPattern["%это моя%анкет%"])
	}
	if st.PerPattern["%под ником%"] != 1 {
		t.Errorf("«под ником» (верхний регистр) не сработал: %d — ulower не применился?",
			st.PerPattern["%под ником%"])
	}

	// Идемпотентность: повторный прогон ничего не добавляет.
	st2, err := s.FlagDisclosures(ctx, patterns)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Inserted != 0 || st2.Total != 2 {
		t.Errorf("повторный flag: inserted=%d total=%d, want 0/2", st2.Inserted, st2.Total)
	}
}

func TestClusterPersonasUnionFind(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	// Пользователи 1..7. Активность: 2 — самый активный в своём кластере (для label).
	users := []User{
		{ID: 1, Name: "u1"}, {ID: 2, Name: "u2"}, {ID: 3, Name: "u3"}, {ID: 4, Name: "u4"},
		{ID: 5, Name: "u5"}, {ID: 6, Name: "u6"}, {ID: 7, Name: "u7"},
	}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: "a"},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: "b"},
		{ID: 12, NoteID: 1, AuthorID: 2, Text: "b2"},
		{ID: 13, NoteID: 1, AuthorID: 2, Text: "b3"},
		{ID: 14, NoteID: 1, AuthorID: 3, Text: "c"},
		{ID: 15, NoteID: 1, AuthorID: 4, Text: "d"},
		{ID: 16, NoteID: 1, AuthorID: 5, Text: "e"},
		{ID: 17, NoteID: 1, AuthorID: 6, Text: "f"},
		{ID: 18, NoteID: 1, AuthorID: 7, Text: "g"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}

	// Рёбра: 1-2, 2-3 (объединяются в {1,2,3}); 6-7 (отдельный кластер);
	// 4-5 ниже порога 0.7 — не склеиваются.
	links := []AliasLink{
		{UserA: 1, UserB: 2, Score: 0.9, Evidence: "1↔2"},
		{UserA: 2, UserB: 3, Score: 0.8, Evidence: "2↔3"},
		{UserA: 6, UserB: 7, Score: 0.75, Evidence: "6↔7"},
		{UserA: 4, UserB: 5, Score: 0.5, Evidence: "слабое"},
	}
	if _, err := s.ImportAliasLinks(ctx, links, nil, testNow); err != nil {
		t.Fatal(err)
	}

	clusters, _, err := s.ClusterPersonas(ctx, ClusterParams{MinScore: 0.7}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("кластеров: got %d, want 2", len(clusters))
	}

	big, small := splitBySize(clusters)
	if big.Confidence != 0.8 {
		t.Errorf("confidence большого кластера (слабейшее ребро): got %v, want 0.8", big.Confidence)
	}
	if big.Label != "u2" {
		t.Errorf("label = самый активный: got %q, want u2", big.Label)
	}
	ids := memberIDs(big)
	if !ids[1] || !ids[2] || !ids[3] || len(ids) != 3 {
		t.Errorf("состав большого кластера: %+v, want {1,2,3}", big.Members)
	}
	if len(big.Evidence) != 2 {
		t.Errorf("цитат в большом кластере: got %d, want 2", len(big.Evidence))
	}
	if len(small.Members) != 2 || small.Confidence != 0.75 {
		t.Errorf("малый кластер: %d участников, conf=%v, want 2/0.75", len(small.Members), small.Confidence)
	}

	// 4/5 (ниже порога) в личности не попали; всего 5 участников, все pending.
	if n := count(t, s, "SELECT COUNT(*) FROM user_personas WHERE user_id IN (4,5)"); n != 0 {
		t.Errorf("4/5 не должны склеиться: %d в user_personas", n)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM user_personas WHERE status='pending'"); n != 5 {
		t.Errorf("pending участников: got %d, want 5", n)
	}

	// Идемпотентность: пересчёт даёт те же 2 кластера без накопления personas.
	clusters2, _, err := s.ClusterPersonas(ctx, ClusterParams{MinScore: 0.7}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters2) != 2 {
		t.Errorf("повторный cluster: got %d, want 2", len(clusters2))
	}
	if n := count(t, s, "SELECT COUNT(*) FROM personas"); n != 2 {
		t.Errorf("personas после пересчёта: got %d, want 2 (без накопления)", n)
	}

	// set меняет статус всем участникам личности (id берём из последнего пересчёта).
	big2, _ := splitBySize(clusters2)
	n, err := s.SetPersonaStatus(ctx, big2.PersonaID, "confirmed")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("SetPersonaStatus затронул: got %d, want 3", n)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM user_personas WHERE status='confirmed'"); got != 3 {
		t.Errorf("confirmed участников: got %d, want 3", got)
	}
	if _, err := s.SetPersonaStatus(ctx, big2.PersonaID, "мусор"); err == nil {
		t.Error("ожидалась ошибка на недопустимый статус")
	}
	if got, _ := s.SetPersonaStatus(ctx, 99999, "rejected"); got != 0 {
		t.Errorf("несуществующая личность: got %d, want 0", got)
	}
}

func TestImportAliasLinksNormalizeAndResolve(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 5, Name: "five"}, {ID: 9, Name: "nine"}}
	comments := []Comment{
		{ID: 100, NoteID: 1, AuthorID: 5, Text: "это моя вторая анкета"},
		{ID: 101, NoteID: 1, AuthorID: 9, Text: "обычный комментарий"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 5, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FlagDisclosures(ctx, []string{"%втор%анкет%"}); err != nil {
		t.Fatal(err)
	}

	// До импорта: одна непроработанная пометка (коммент 100), с обогащением.
	cands0, err := s.DisclosureCandidates(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands0) != 1 {
		t.Fatalf("кандидатов до импорта: got %d, want 1", len(cands0))
	}
	if cands0[0].Author.ID != 5 || cands0[0].Pattern != "%втор%анкет%" {
		t.Errorf("обогащение пометки: author=%d pattern=%q", cands0[0].Author.ID, cands0[0].Pattern)
	}
	if !hasParticipant(cands0[0], 5) || !hasParticipant(cands0[0], 9) {
		t.Errorf("участники заметки не собраны: %+v", cands0[0].Participants)
	}

	// Импорт: user_a>user_b нормализуется (9,5→5,9); само-связь и ссылка на
	// несуществующего пропускаются; comment_id помечает пометку resolved.
	links := []AliasLink{
		{UserA: 9, UserB: 5, Score: 0.9, Evidence: "e", CommentID: 100},
		{UserA: 7, UserB: 7, Score: 1.0},   // само-связь → skip
		{UserA: 5, UserB: 999, Score: 0.9}, // 999 нет в users → skip
	}
	st, err := s.ImportAliasLinks(ctx, links, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Links != 1 || st.Skipped != 2 || st.HitsResolved != 1 {
		t.Fatalf("импорт: links=%d skipped=%d resolved=%d, want 1/2/1", st.Links, st.Skipped, st.HitsResolved)
	}
	var a, b int64
	if err := s.db.QueryRowContext(ctx, "SELECT user_a, user_b FROM alias_candidates").Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != 5 || b != 9 {
		t.Errorf("нормализация a<b: got %d,%d, want 5,9", a, b)
	}

	// Пометка 100 теперь resolved → исчезла из кандидатов.
	cands1, err := s.DisclosureCandidates(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands1) != 0 {
		t.Errorf("resolved-пометка не исключена: %d кандидатов", len(cands1))
	}

	// Повторный импорт той же пары — upsert (без дубля), score обновляется.
	if _, err := s.ImportAliasLinks(ctx, []AliasLink{{UserA: 5, UserB: 9, Score: 0.6, Evidence: "e2"}}, nil, testNow); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM alias_candidates"); n != 1 {
		t.Errorf("upsert задвоил связь: got %d, want 1", n)
	}
	var score float64
	if err := s.db.QueryRowContext(ctx, "SELECT score FROM alias_candidates").Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 0.6 {
		t.Errorf("score не обновился при повторном импорте: got %v, want 0.6", score)
	}
}

// --- вспомогательное для тестов ---

func splitBySize(clusters []PersonaCluster) (big, small PersonaCluster) {
	for _, c := range clusters {
		if len(c.Members) >= 3 {
			big = c
		} else {
			small = c
		}
	}
	return big, small
}

func memberIDs(c PersonaCluster) map[int64]bool {
	m := map[int64]bool{}
	for _, x := range c.Members {
		m[x.ID] = true
	}
	return m
}

func hasParticipant(h CandidateHit, id int64) bool {
	for _, p := range h.Participants {
		if p.ID == id {
			return true
		}
	}
	return false
}
