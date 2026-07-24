package archive

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"math/bits"
	"testing"
)

// gradient — плавный горизонтальный градиент серого (dark→bright, при reverse
// наоборот). Даёт детерминированный dHash: возрастающий → все биты 0, убывающий
// → все 64 бита 1.
func gradient(w, h int, reverse bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(x * 255 / (w - 1))
			if reverse {
				v = 255 - v
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	return img
}

func TestDHash(t *testing.T) {
	up := gradient(32, 32, false)
	upSame := gradient(32, 32, false)
	down := gradient(32, 32, true)

	if h := dHash(up); h != 0 {
		t.Errorf("возрастающий градиент: dHash=%016x, want 0 (левый темнее правого)", h)
	}
	if dHash(up) != dHash(upSame) {
		t.Error("одинаковые изображения дали разный dHash")
	}
	if d := bits.OnesCount64(dHash(up) ^ dHash(down)); d < 60 {
		t.Errorf("противоположные градиенты: расстояние %d, want ~64", d)
	}

	// JPEG-round-trip: dHash устойчив к сжатию (гладкий градиент почти не плывёт).
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, up, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	got, err := DHashFromBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if d := bits.OnesCount64(got ^ dHash(up)); d > 4 {
		t.Errorf("dHash после JPEG уплыл на %d бит, want ≤4", d)
	}

	if _, err := DHashFromBytes([]byte("не картинка")); err == nil {
		t.Error("ожидалась ошибка декодирования на мусоре")
	}
}

func TestClusterAvatars(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	// Пользователи: 1..4 — реальные кандидаты; 10..14 — общая generic-картинка.
	users := []User{
		{ID: 1, Name: "u1"}, {ID: 2, Name: "u2"}, {ID: 3, Name: "u3"}, {ID: 4, Name: "u4"},
		{ID: 10}, {ID: 11}, {ID: 12}, {ID: 13}, {ID: 14},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 1, Text: "n"}, nil, users, testNow); err != nil {
		t.Fatal(err)
	}

	base := uint64(0x00000000FFFFFFFF)
	seed := func(id int64, p uint64) {
		if err := s.SaveAvatarHash(ctx, id, "http://a/"+string(rune(id)), &p, "ok", testNow); err != nil {
			t.Fatal(err)
		}
	}
	seed(1, base)           // dist 0 к 2
	seed(2, base)           // точная копия
	seed(3, base^1)         // dist 1
	seed(4, base^0xFFFF)    // dist 16 — далеко, не склеится
	const generic = uint64(0x5555555555555555)
	for _, id := range []int64{10, 11, 12, 13, 14} {
		seed(id, generic) // 5 анкет с одной картинкой > generic-max=4 → пропуск
	}

	st, err := s.ClusterAvatars(ctx, 2, 4, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.OK != 9 {
		t.Errorf("ok-хэшей: got %d, want 9", st.OK)
	}
	if st.GenericHash != 1 || st.Skipped != 5 {
		t.Errorf("generic: групп=%d (want 1), пропущено=%d (want 5)", st.GenericHash, st.Skipped)
	}
	// Рёбра только среди {1,2,3} (1-2, 1-3, 2-3 = 3 ребра). 4 и generic — нет.
	if st.Pairs != 3 {
		t.Errorf("рёбер avatar_phash: got %d, want 3", st.Pairs)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM alias_candidates WHERE signal='avatar_phash'"); n != 3 {
		t.Errorf("записей avatar_phash в БД: got %d, want 3", n)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM alias_candidates WHERE user_a=4 OR user_b=4"); n != 0 {
		t.Errorf("анкета 4 (dist 16) не должна склеиться: %d рёбер", n)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM alias_candidates WHERE user_a>=10"); n != 0 {
		t.Errorf("generic-группа не должна давать рёбра: %d", n)
	}

	// Точная копия 1-2 должна иметь максимальный вес (dist 0 → 0.9).
	var score float64
	if err := s.db.QueryRowContext(ctx,
		"SELECT score FROM alias_candidates WHERE user_a=1 AND user_b=2 AND signal='avatar_phash'").Scan(&score); err != nil {
		t.Fatal(err)
	}
	if score != 0.9 {
		t.Errorf("вес точной копии 1-2: got %v, want 0.9", score)
	}

	// Склейка в личности видит avatar-рёбра: {1,2,3} → одна личность.
	clusters, _, err := s.ClusterPersonas(ctx, ClusterParams{MinScore: 0.7}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || len(clusters[0].Members) != 3 {
		t.Fatalf("ожидалась 1 личность из 3 анкет, got %d кластеров", len(clusters))
	}
}
