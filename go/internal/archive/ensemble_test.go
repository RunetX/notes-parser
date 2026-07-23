package archive

import "testing"

// Композит: пара уровня ground-truth (стиль #1, handoff в окне, круг 0.57)
// должна уверенно превышать порог склейки 0.7; та же пара БЕЗ корроборации
// (стиль #1, но спаны далеко и круги не пересекаются) — оставаться ниже 0.7.
func TestEnsembleScore(t *testing.T) {
	strong := ensembleScore(1, 0.678, "handoff", 62, 0.57, 120)
	if strong < 0.9 || strong > 0.97 {
		t.Errorf("сильная пара: score %.3f, ожидалось [0.90,0.97]", strong)
	}
	styleOnly := ensembleScore(1, 0.678, "disjoint", 900, 0.0, 120)
	if styleOnly >= 0.7 {
		t.Errorf("только стиль без корроборации: score %.3f, должно быть <0.7", styleOnly)
	}
	// разрыв шире окна handoff весит меньше полного.
	wide := ensembleScore(1, 0.678, "handoff", 300, 0.57, 120)
	if wide >= strong {
		t.Errorf("широкий разрыв %.3f должен весить меньше узкого %.3f", wide, strong)
	}
}

func TestOverlapCoeff(t *testing.T) {
	ca := map[int64]bool{10: true, 20: true, 30: true, 2: true} // 2 — сама пара, игнор
	cb := map[int64]bool{20: true, 30: true, 40: true, 1: true} // 1 — сама пара, игнор
	// без пар: A={10,20,30}(3), B={20,30,40}(3), общих {20,30}=2, min=3 → 2/3.
	got := overlapCoeff(ca, cb, 1, 2)
	if want := 2.0 / 3.0; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("overlapCoeff = %.4f, want %.4f", got, want)
	}
}

// Направленный top-K ловит пару, даже когда другая сторона видит своего соседа
// ближе: у B ближайший — C, но у A ближайший — B, и пара (A,B) сохраняется
// (взаимный top-1 её бы отбросил).
func TestDirectionalStylePairs(t *testing.T) {
	pids := []int64{1, 2, 3} // A, B, C
	vecs := [][]float32{
		{1, 0, 0},        // A
		{0.9, 0.436, 0},  // B: A·B=0.90
		{0.8, 0.6, 0},    // C: B·C≈0.98, A·C=0.80
	}
	got := directionalStylePairs(pids, vecs, 0.5, 1)
	rank := map[[2]int64]int{}
	for _, c := range got {
		rank[[2]int64{c.a, c.b}] = c.rank
	}
	if r, ok := rank[[2]int64{1, 2}]; !ok || r != 1 {
		t.Errorf("пара A-B должна быть, ранг #1: %v (ok=%v)", r, ok)
	}
	if r, ok := rank[[2]int64{2, 3}]; !ok || r != 1 {
		t.Errorf("пара B-C должна быть, ранг #1: %v (ok=%v)", r, ok)
	}
	if _, ok := rank[[2]int64{1, 3}]; ok {
		t.Error("пары A-C быть не должно (не входят в top-1 друг друга)")
	}
}
