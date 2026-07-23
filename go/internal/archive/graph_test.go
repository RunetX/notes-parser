package archive

import (
	"context"
	"strconv"
	"testing"
)

// Строим маленький тред и сливаем две анкеты (1,2) в личность, затем проверяем
// persona-aware граф и портрет: рёбра/активность агрегируются по личности.
func TestPersonaGraphAndPortrait(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}}
	comments := []Comment{
		{ID: 100, NoteID: 1, ParentID: 0, AuthorID: 3, Text: "корень C"},
		{ID: 101, NoteID: 1, ParentID: 100, AuthorID: 1, Text: "A→C"},
		{ID: 102, NoteID: 1, ParentID: 100, AuthorID: 2, Text: "B→C"},
		{ID: 103, NoteID: 1, ParentID: 101, AuthorID: 3, Text: "C→A"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 3, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	// Сливаем 1 и 2 в личность.
	if _, err := s.ImportAliasLinks(ctx, []AliasLink{{UserA: 1, UserB: 2, Score: 0.9, Evidence: "av"}}, nil, testNow); err != nil {
		t.Fatal(err)
	}
	clusters, err := s.ClusterPersonas(ctx, 0.7, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("ожидалась 1 личность, got %d", len(clusters))
	}
	pid := "p" + strconv.FormatInt(clusters[0].PersonaID, 10)

	// Узлы: личность p{1,2} — 2 анкеты, 2 комментария (c101+c102); C — 2 комм.
	nodes, err := s.GraphNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pn := nodes[pid]; !pn.IsPersona || pn.Accounts != 2 || pn.Comments != 2 {
		t.Errorf("узел личности: %+v (want is_persona, accounts=2, comments=2)", pn)
	}
	if cn := nodes["u3"]; cn.IsPersona || cn.Comments != 2 {
		t.Errorf("узел C(u3): %+v (want не персона, comments=2)", cn)
	}

	// Рёбра: личность→C вес 2 (A→C, B→C слиты), C→личность вес 1.
	edges, err := s.GraphEdges(ctx, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	w := map[string]int{}
	for _, e := range edges {
		w[e.From+">"+e.To] = e.Replies
	}
	if w[pid+">u3"] != 2 {
		t.Errorf("ребро личность→C: got %d, want 2 (A→C + B→C)", w[pid+">u3"])
	}
	if w["u3>"+pid] != 1 {
		t.Errorf("ребро C→личность: got %d, want 1", w["u3>"+pid])
	}

	// Портрет по голому user-id канонизируется в личность.
	p, err := s.Portrait(ctx, "u1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if p.Identity != pid || len(p.Accounts) != 2 || p.Comments != 2 {
		t.Errorf("портрет: identity=%s accounts=%d comments=%d (want %s/2/2)", p.Identity, len(p.Accounts), p.Comments, pid)
	}
	if len(p.RepliesTo) != 1 || p.RepliesTo[0].Identity != "u3" || p.RepliesTo[0].Replies != 2 {
		t.Errorf("кому отвечает: %+v (want u3 ×2)", p.RepliesTo)
	}
	if len(p.RepliedBy) != 1 || p.RepliedBy[0].Identity != "u3" || p.RepliedBy[0].Replies != 1 {
		t.Errorf("кто отвечает ему: %+v (want u3 ×1)", p.RepliedBy)
	}

	// drop-self исключает само-петли (у нас их нет → столько же рёбер).
	noSelf, err := s.GraphEdges(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(noSelf) != len(edges) {
		t.Errorf("само-петель не было, но drop-self изменил число рёбер: %d vs %d", len(noSelf), len(edges))
	}
}
