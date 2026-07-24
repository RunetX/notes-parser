package archive

import (
	"context"
	"math"
	"testing"
)

func TestNormalizeStyle(t *testing.T) {
	got := normalizeStyle("  Привет\n\tМИР   !!  ")
	if got != "привет мир !!" {
		t.Errorf("normalizeStyle: got %q", got)
	}
	if normalizeStyle("   \n ") != "" {
		t.Error("пустой после нормализации должен быть пустым")
	}
}

func TestTrigramCosine(t *testing.T) {
	const dims = 128
	mk := func(s string) []float32 {
		v := make([]float32, dims)
		addCharTrigrams(v, normalizeStyle(s), dims)
		l2Normalize(v)
		return v
	}
	same1 := mk("одинаковый текст про котиков и погоду")
	same2 := mk("одинаковый текст про котиков и погоду")
	diff := mk("совершенно другая тема авиастроение и турбины")

	if c := dot(same1, same2); math.Abs(c-1) > 1e-6 {
		t.Errorf("идентичный текст: косинус %.4f, want 1", c)
	}
	if c := dot(same1, diff); c > 0.9 {
		t.Errorf("разный текст: косинус %.4f, ожидался заметно ниже 1", c)
	}
}

func TestBuildAndClusterStylometry(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	// A и B пишут идентично; C и D — иначе. Профили строятся на объединении
	// комментариев автора.
	styleAB := "виджет фробникатор блёстки виджет фробникатор блёстки виджет"
	users := []User{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}, {ID: 3, Name: "C"}, {ID: 4, Name: "D"}}
	comments := []Comment{
		{ID: 10, NoteID: 1, AuthorID: 1, Text: styleAB},
		{ID: 11, NoteID: 1, AuthorID: 2, Text: styleAB},
		{ID: 12, NoteID: 1, AuthorID: 3, Text: "квантовый поток конденсатор реактор плазма квантовый поток"},
		{ID: 13, NoteID: 1, AuthorID: 4, Text: "быстрая бурая лиса прыгает через ленивого пса и спит"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, comments, users, testNow); err != nil {
		t.Fatal(err)
	}

	bst, err := s.BuildStyleProfiles(ctx, 20, 128, GenreAll, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if bst.Eligible != 4 {
		t.Fatalf("профилей построено: got %d, want 4", bst.Eligible)
	}

	// A↔B (идентичный стиль) должны иметь косинус ≈1 после центрирования.
	cos, err := s.StyleCosine(ctx, [][2]int64{{1, 2}, {1, 999}})
	if err != nil {
		t.Fatal(err)
	}
	if cos[0] < 0.99 {
		t.Errorf("центр-косинус A↔B: got %.4f, want ≈1", cos[0])
	}
	if !math.IsNaN(cos[1]) {
		t.Errorf("отсутствующий профиль должен дать NaN, got %v", cos[1])
	}

	cst, err := s.ClusterStylometry(ctx, 0.5, 2, 100, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if cst.Profiles != 4 {
		t.Errorf("профилей в кластеризации: got %d, want 4", cst.Profiles)
	}
	// Пара A↔B должна быть записана как stylometry-кандидат.
	if n := count(t, s, "SELECT COUNT(*) FROM alias_candidates WHERE signal='stylometry' AND user_a=1 AND user_b=2"); n != 1 {
		t.Errorf("A↔B не записана как stylometry-связь: %d", n)
	}
	// Вес в верхней части советочного диапазона (косинус ≈1 → скор ≈0.65),
	// намеренно ниже дефолтного порога склейки 0.7.
	var score float64
	if err := s.db.QueryRowContext(ctx,
		"SELECT score FROM alias_candidates WHERE signal='stylometry' AND user_a=1 AND user_b=2").Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score < 0.6 || score > 0.7 {
		t.Errorf("вес A↔B: got %.3f, ожидался ~0.65 (косинус ≈1, советочный диапазон)", score)
	}
}
