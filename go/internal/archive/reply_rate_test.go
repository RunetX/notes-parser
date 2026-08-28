package archive

import "testing"

// Корзины идут по возрастанию, и позиция попадает ровно в одну.
func TestBucketOf(t *testing.T) {
	for _, c := range []struct {
		pos  int
		upto int
	}{{1, 10}, {10, 10}, {11, 25}, {26, 50}, {51, 100}, {101, 250}, {5000, 1 << 30}} {
		if got := rateBuckets[bucketOf(c.pos)]; got != c.upto {
			t.Errorf("позиция %d попала в корзину %d, ожидалась %d", c.pos, got, c.upto)
		}
	}
}

// Тощая корзина ЗАМЕРОМ НЕ СЧИТАЕТСЯ и говорит об этом вторым значением. Ноль
// сам по себе неотличим от честного «никогда не отвечает», а разница между «не
// отвечает» и «мы не знаем» — это разница между характером и пустотой.
func TestRateRefusesThinBucket(t *testing.T) {
	r := ReplyRate{Buckets: []RateBucket{
		{Upto: 10, Chances: 5, Answers: 1, ToHimChances: 200, ToHimAnswers: 100},
		{Upto: 1 << 30, Chances: 1000, Answers: 20},
	}}
	if _, ok := r.Rate(3, false); ok {
		t.Error("тощая корзина принята за замер")
	}
	// А соседняя мера в той же корзине может быть набрана — они считаются
	// порознь, потому что «к нему обратились» и «мимо него говорят» отличаются
	// на порядок.
	if p, ok := r.Rate(3, true); !ok || p != 0.5 {
		t.Errorf("обращение в той же корзине: p=%v, ok=%v", p, ok)
	}
	if p, ok := r.Rate(500, false); !ok || p != 0.02 {
		t.Errorf("глубокая корзина: p=%v, ok=%v", p, ok)
	}
}

// Пустой замер молчит, а не отвечает нулём.
func TestRateEmpty(t *testing.T) {
	if _, ok := (ReplyRate{}).Rate(1, false); ok {
		t.Error("пустой замер выдал себя за измерение")
	}
}
