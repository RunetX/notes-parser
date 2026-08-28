package narodsim

import (
	"context"
	"testing"
	"time"

	"lovegw/internal/narod"
)

func testCard() narod.Card {
	return narod.Card{
		Dice: narod.DiceParams{
			ComeToNote: 0.3, ReplyMention: 0.9, ReplyOther: 0.1, MaxPerThread: 3,
		},
		Latency: narod.LatencyDist{
			ToThreadSec: narod.Dist{P10: 60, Median: 600, P90: 3600, Max: 20000},
			ToReplySec:  narod.Dist{P10: 30, Median: 120, P90: 900, Max: 7200},
		},
	}
}

// Монетка выводится из (зерно, анкета, реплика), а не из последовательного
// генератора: иначе два прогона перестали бы сравниваться, стоит поменяться
// порядку или числу точек решения.
func TestCardDeciderIsDeterministicPerPoint(t *testing.T) {
	d := &CardDecider{Card: testCard(), Seed: 7}
	p := DecisionPoint{Actor: 9, TriggerID: 2001, Addressed: true}
	first, err := d.Decide(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, _ := d.Decide(context.Background(), p)
		if got != first {
			t.Fatalf("прогон %d дал другое решение: %+v против %+v", i, got, first)
		}
	}
	// И оно не зависит от того, спрашивали ли до этого про другие точки.
	other := &CardDecider{Card: testCard(), Seed: 7}
	other.Decide(context.Background(), DecisionPoint{Actor: 9, TriggerID: 111})
	other.Decide(context.Background(), DecisionPoint{Actor: 9, TriggerID: 222})
	if got, _ := other.Decide(context.Background(), p); got != first {
		t.Errorf("решение зависит от истории вызовов: %+v против %+v", got, first)
	}
}

// Разные зёрна дают разные прогоны — иначе «повторить с другим зерном» ничего
// бы не значило.
func TestCardDeciderSeedMatters(t *testing.T) {
	same := 0
	const n = 60
	for i := int64(0); i < n; i++ {
		p := DecisionPoint{Actor: 9, TriggerID: i}
		a, _ := (&CardDecider{Card: testCard(), Seed: 1}).Decide(context.Background(), p)
		b, _ := (&CardDecider{Card: testCard(), Seed: 2}).Decide(context.Background(), p)
		if a.Speak == b.Speak {
			same++
		}
	}
	if same == n {
		t.Error("зерно ни на что не влияет")
	}
}

// Обращение к жителю поднимает вероятность ответа: 0,9 против 0,1 в карточке —
// значит на длинной серии разница обязана быть видна.
func TestCardDeciderAnswersMentionsMoreOften(t *testing.T) {
	d := &CardDecider{Card: testCard(), Seed: 3}
	var mention, other int
	for i := int64(1); i <= 400; i++ {
		if got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, TriggerID: i, Addressed: true,
		}); got.Speak {
			mention++
		}
		if got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, TriggerID: i, Addressed: false,
		}); got.Speak {
			other++
		}
	}
	if mention <= other*2 {
		t.Errorf("на обращения отвечает %d раз, на чужое %d — разница не видна", mention, other)
	}
}

// Потолок реплик на тред — часть характера: человек с полутора десятками реплик
// в замере на шестидесяти выглядел бы одержимым.
func TestCardDeciderStopsAtThreadCap(t *testing.T) {
	d := &CardDecider{Card: testCard(), Seed: 5}
	for i := int64(1); i <= 100; i++ {
		got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, TriggerID: i, Addressed: true, Said: 3,
		})
		if got.Speak {
			t.Fatalf("заговорил, сказав уже 3 при потолке 3 (точка %d)", i)
		}
	}
}

// Задержка восстанавливается по квантилям и остаётся в пределах замера.
func TestSampleDist(t *testing.T) {
	d := narod.Dist{P10: 60, Median: 600, P90: 3600, Max: 20000}
	cases := []struct {
		u    float64
		want time.Duration
	}{
		{0, 0},
		{0.1, 60 * time.Second},
		{0.5, 600 * time.Second},
		{0.9, 3600 * time.Second},
	}
	for _, tc := range cases {
		if got := sampleDist(d, tc.u); got != tc.want {
			t.Errorf("sampleDist(u=%v) = %v, ожидалось %v", tc.u, got, tc.want)
		}
	}
	// Монотонность: чем больше u, тем дольше молчал.
	prev := time.Duration(-1)
	for u := 0.0; u < 1; u += 0.01 {
		got := sampleDist(d, u)
		if got < prev {
			t.Fatalf("при u=%v задержка уменьшилась: %v после %v", u, got, prev)
		}
		prev = got
	}
	if got := sampleDist(d, 0.999); got > 20000*time.Second {
		t.Errorf("хвост вылез за максимум замера: %v", got)
	}
	// Пустой замер не должен выдумывать задержку.
	if got := sampleDist(narod.Dist{}, 0.5); got != 0 {
		t.Errorf("по пустому распределению получено %v", got)
	}
}

// Заметка и реплика меряются РАЗНЫМИ распределениями: «через сколько пришёл в
// тред» и «через сколько ответил» — величины разного порядка.
func TestChanceOfPicksRightDistribution(t *testing.T) {
	c := testCard()
	if p, dist := chanceOf(c.Dice, c.Latency, DecisionPoint{TriggerID: 0}); p != 0.3 || dist != c.Latency.ToThreadSec {
		t.Errorf("на заметке: p=%v, dist=%+v", p, dist)
	}
	if p, dist := chanceOf(c.Dice, c.Latency, DecisionPoint{TriggerID: 5, Addressed: true}); p != 0.9 || dist != c.Latency.ToReplySec {
		t.Errorf("на обращении: p=%v, dist=%+v", p, dist)
	}
	if p, _ := chanceOf(c.Dice, c.Latency, DecisionPoint{TriggerID: 5}); p != 0.1 {
		t.Errorf("в чужом разговоре: p=%v", p)
	}
}
