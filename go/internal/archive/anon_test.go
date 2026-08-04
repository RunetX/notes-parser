package archive

import "testing"

// Топ держится по убыванию z и не растёт сверх лимита — иначе скан по 20k+
// анонимок пришлось бы сортировать целиком.
func TestInsertHitKeepsTopByZ(t *testing.T) {
	var hits []AnonHit
	for i, z := range []float64{1.0, 3.5, 2.0, 0.5, 3.9, 2.7} {
		hits = insertHit(hits, AnonHit{NoteID: int64(i + 1), Z: z}, 3)
	}
	if len(hits) != 3 {
		t.Fatalf("длина топа = %d, ожидалось 3", len(hits))
	}
	want := []float64{3.9, 3.5, 2.7}
	for i, w := range want {
		if hits[i].Z != w {
			t.Errorf("hits[%d].Z = %.1f, ожидалось %.1f (порядок по убыванию)", i, hits[i].Z, w)
		}
	}
}

func TestFracBelow(t *testing.T) {
	null := []float64{0.0, 1.0, 2.0, 3.0}
	cases := []struct {
		z    float64
		want float64
	}{
		{z: -1, want: 0},
		{z: 1.5, want: 0.5},
		{z: 4, want: 1},
	}
	for _, c := range cases {
		if got := fracBelow(null, c.z); got != c.want {
			t.Errorf("fracBelow(%.1f) = %.2f, ожидалось %.2f", c.z, got, c.want)
		}
	}
	if got := fracBelow(nil, 1); got != 0 {
		t.Errorf("пустой фон: %.2f, ожидался 0", got)
	}
}

// Счётчики порогов вложены: то, что выше максимума фона, обязано попасть и в
// FPR1%, и в FPR5% — иначе сводка врёт про число кандидатов.
func TestCountAboveIsNested(t *testing.T) {
	res := AnonScanResult{NullP95: 1.5, NullP99: 2.1, NullMax: 2.6}
	for _, z := range []float64{0.4, 1.6, 2.2, 3.0} {
		countAbove(&res, z)
	}
	if res.AboveP95 != 3 || res.AboveP99 != 2 || res.AboveMax != 1 {
		t.Fatalf("пороги: p95=%d p99=%d max=%d, ожидалось 3/2/1",
			res.AboveP95, res.AboveP99, res.AboveMax)
	}
}
