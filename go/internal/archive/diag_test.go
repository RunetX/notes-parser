package archive

import (
	"context"
	"testing"
	"time"
)

func TestTemporalRel(t *testing.T) {
	mk := func(from, to string) DiagAccount { return DiagAccount{ActiveFrom: from, ActiveTo: to} }
	cases := []struct {
		name string
		a, b DiagAccount
		rel  string
	}{
		{"overlap", mk("2015-01-01T00:00:00Z", "2016-01-01T00:00:00Z"), mk("2015-06-01T00:00:00Z", "2017-01-01T00:00:00Z"), "overlap"},
		{"handoff", mk("2015-01-01T00:00:00Z", "2015-03-01T00:00:00Z"), mk("2015-04-01T00:00:00Z", "2015-06-01T00:00:00Z"), "handoff"},
		{"disjoint", mk("2013-01-01T00:00:00Z", "2013-03-01T00:00:00Z"), mk("2016-01-01T00:00:00Z", "2016-06-01T00:00:00Z"), "disjoint"},
		{"unknown", mk("", ""), mk("2016-01-01T00:00:00Z", "2016-06-01T00:00:00Z"), "unknown"},
	}
	for _, c := range cases {
		if rel, _ := temporalRel(c.a, c.b); rel != c.rel {
			t.Errorf("%s: got %s want %s", c.name, rel, c.rel)
		}
	}
}

func TestPartnerSet(t *testing.T) {
	rt := map[int64]int{10: 1, 20: 2, 0: 5}    // 0 (аноним/корень) — игнор
	rb := map[int64]int{20: 1, 30: 1, 99: 1}   // 99 входит в набор — игнор
	set := partnerSet(rt, rb, map[int64]bool{99: true})
	for _, id := range []int64{10, 20, 30} {
		if !set[id] {
			t.Errorf("собеседник %d потерян", id)
		}
	}
	if set[0] || set[99] || len(set) != 3 {
		t.Errorf("0/known не должны попасть, размер %d: %v", len(set), set)
	}
}

// styleNeighbors работает с уже центрированными/нормированными векторами (dot =
// косинус). Проверяем, что ближайший всплывает первым и ранг сиблинга = #1.
func TestStyleNeighbors(t *testing.T) {
	pids := []int64{100, 200, 300}
	vecs := [][]float32{
		{1, 0, 0},
		{0.9, 0.1, 0}, // почти совпадает со 100
		{0, 0, 1},     // ортогонален
	}
	known := map[int64]bool{100: true, 200: true}
	nb, sib := styleNeighbors(0, pids, vecs, known)
	if len(nb) == 0 || nb[0].ID != 200 || !nb[0].Known {
		t.Errorf("ближайший к 100 должен быть свой 200, got %+v", nb)
	}
	if r, ok := sib[200]; !ok || r.Rank != 1 {
		t.Errorf("ранг 200 у 100 должен быть #1, got %+v (ok=%v)", r, ok)
	}
}

// DiagPersonas на маленьком треде: A и B общаются с общим C, B отвечает A.
func TestDiagPersonasIntegration(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	pub := func(y int) time.Time { return time.Date(y, 6, 15, 12, 0, 0, 0, time.UTC) }
	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}}
	comments := []Comment{
		{ID: 100, NoteID: 1, ParentID: 0, AuthorID: 3, Text: "корень", PublishedAt: pub(2015)},
		{ID: 101, NoteID: 1, ParentID: 100, AuthorID: 1, Text: "A→C", PublishedAt: pub(2015)},
		{ID: 102, NoteID: 1, ParentID: 100, AuthorID: 2, Text: "B→C", PublishedAt: pub(2016)},
		{ID: 103, NoteID: 1, ParentID: 101, AuthorID: 2, Text: "B→A", PublishedAt: pub(2016)},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 3, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}
	d, err := s.DiagPersonas(ctx, []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 2 || d.Accounts[0].Comments != 1 || d.Accounts[1].Comments != 2 {
		t.Fatalf("активность анкет: %+v", d.Accounts)
	}
	if len(d.Pairs) != 1 {
		t.Fatalf("пар %d, want 1", len(d.Pairs))
	}
	p := d.Pairs[0]
	if p.CrossRepliesBA != 1 { // B(2) ответил A(1)
		t.Errorf("B→A ответов %d, want 1", p.CrossRepliesBA)
	}
	if p.CrossRepliesAB != 0 {
		t.Errorf("A→B ответов %d, want 0", p.CrossRepliesAB)
	}
	if p.SharedPartners != 1 { // общий собеседник C(3)
		t.Errorf("общих собеседников %d, want 1 (C)", p.SharedPartners)
	}
}
