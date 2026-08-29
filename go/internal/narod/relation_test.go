package narod

import (
	"context"
	"math"
	"testing"
	"time"
)

// relCard — карточка с ЗАМЕРЕННЫМИ корзинами знакомства: незнакомому отвечают
// в 1 % случаев, давнему знакомому в 4 %. Средний отклик 2 %, значит размах
// рычага ровно вдвое — и на столько же, не больше, вправе двигать отношение.
func relCard() Card {
	c := measuredCard()
	c.Rate.Familiar = []RateBucket{
		{Upto: 1, Chances: 10000, Answers: 100, ToHimChances: 1000, ToHimAnswers: 500},
		{Upto: 1 << 30, Chances: 10000, Answers: 400, ToHimChances: 1000, ToHimAnswers: 500},
	}
	return c
}

func TestFamiliarSpanIsMeasured(t *testing.T) {
	span, ok := relCard().Rate.FamiliarSpan(false)
	if !ok {
		t.Fatal("размах не замерен на корзинах, где он очевиден")
	}
	if math.Abs(span-1.6) > 0.01 {
		t.Fatalf("размах %.3f, ждали 1,6 (4 %% против среднего 2,5 %%)", span)
	}
}

// Рычаг отношения упирается в ЗАМЕРЕННЫЙ размах знакомства и ни на шаг дальше:
// придумано в нём только направление, величина взята из архива. Полного размаха
// достигает ВРАЖДА, симпатия берёт его корень — асимметрия решением владельца.
func TestRelationLiftBoundedByMeasuredSpan(t *testing.T) {
	card := relCard()
	span, _ := card.Rate.FamiliarSpan(false)

	down := relationLift(card, DecisionPoint{Tone: -1})
	if math.Abs(down-span) > 1e-9 {
		t.Fatalf("при полной неприязни рычаг %.3f, а замеренный потолок %.3f", down, span)
	}
	up := relationLift(card, DecisionPoint{Tone: 1})
	if math.Abs(up-math.Sqrt(span)) > 1e-9 {
		t.Fatalf("при полной симпатии рычаг %.3f, ждали корень размаха %.3f", up, math.Sqrt(span))
	}
	if up >= down {
		t.Fatalf("симпатия тянет не слабее вражды: %.3f против %.3f", up, down)
	}
	if got := relationLift(card, DecisionPoint{Tone: 0}); got != 1 {
		t.Fatalf("безразличие двигает вероятность: %.3f", got)
	}
	// Тон за пределами шкалы (порченый граф, чужая карта) рычаг не разгоняет.
	if got := relationLift(card, DecisionPoint{Tone: 12}); math.Abs(got-math.Sqrt(span)) > 1e-9 {
		t.Fatalf("тон 12 дал рычаг %.3f — потолок пробит", got)
	}
	if got := relationLift(card, DecisionPoint{Tone: -12}); math.Abs(got-span) > 1e-9 {
		t.Fatalf("тон −12 дал рычаг %.3f — потолок пробит", got)
	}
}

// Минимум рычага стоит на РАВНОДУШИИ, а не на неприязни. Это и есть вся правка
// 29.08.2026: пока неприязнь гасила отклик, ссоре в мире взяться было неоткуда —
// поссорившиеся расходились по разным углам вместо того, чтобы сцепиться.
func TestIndifferenceIsTheQuietest(t *testing.T) {
	card := relCard()
	flat := relationLift(card, DecisionPoint{Tone: 0})
	for _, tone := range []float64{-1, -0.5, -0.2, 0.2, 0.5, 1} {
		if got := relationLift(card, DecisionPoint{Tone: tone}); got <= flat {
			t.Fatalf("тон %.1f дал рычаг %.3f — не выше равнодушия (%.3f)", tone, got, flat)
		}
	}
}

// На ПРЯМОЕ обращение отношение не влияет вовсе: по тому же замеру, по которому
// туда не пущено знакомство. Человек отвечает и нелюбимому, когда обратились
// лично; влезать в его разговор — не станет.
func TestRelationLiftSkipsAddressed(t *testing.T) {
	card := relCard()
	for _, tone := range []float64{-1, -0.5, 0.5, 1} {
		if got := relationLift(card, DecisionPoint{Tone: tone, Addressed: true}); got != 1 {
			t.Fatalf("тон %.1f при прямом обращении дал рычаг %.3f", tone, got)
		}
	}
}

// Без замеренных корзин рычага нет вовсе — как и у знакомства. Отсутствие
// замера не повод подставить правдоподобное число.
func TestRelationLiftSilentWithoutMeasurement(t *testing.T) {
	card := measuredCard() // Familiar не заполнены
	if got := relationLift(card, DecisionPoint{Tone: 1}); got != 1 {
		t.Fatalf("без замера рычаг %.3f — число взялось из воздуха", got)
	}
}

// Отношение доходит до самого решения, а не остаётся числом в карточке: тот же
// житель на той же точке приходит чаще и к неприятному, и к приятному, чем к
// безразличному, — и чаще всего к неприятному.
func TestToneReachesTheDecision(t *testing.T) {
	ctx := context.Background()
	card := relCard()
	card.Dice.ComeToNote = 0.5
	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	count := func(tone float64) int {
		var n int
		for seed := range uint64(400) {
			d := &CardDecider{Card: card, Seed: seed + 1}
			got, err := d.Decide(ctx, DecisionPoint{
				Now: at, Actor: 1, NoteID: int64(seed) + 1, Tone: tone,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Speak {
				n++
			}
		}
		return n
	}
	warm, cold, flat := count(1), count(-1), count(0)
	if cold <= flat {
		t.Fatalf("к неприятному пришли %d раз, к безразличному %d — рычаг гасит ссору", cold, flat)
	}
	if warm <= flat {
		t.Fatalf("к приятному пришли %d раз, к безразличному %d — рычаг не работает", warm, flat)
	}
	if cold <= warm {
		t.Fatalf("к неприятному пришли %d раз, к приятному %d — асимметрия потерялась", cold, warm)
	}
}

// measuredCard — карточка с ЗАМЕРЕННЫМИ числами живого слепка ДВ (u498196).
func measuredCard() Card {
	c := testCard()
	c.Latency = LatencyDist{
		ToThreadSec: Dist{P10: 508, Median: 2587, P90: 10882, Max: 60783},
		ToReplySec:  Dist{P10: 116, Median: 385, P90: 2033, Max: 42852},
	}
	c.Dice.MaxPerThread = 11
	c.Rate = ReplyRate{
		Threads: 300,
		Buckets: []RateBucket{{
			Upto: 1 << 30, Chances: 10000, Answers: 100,
			ToHimChances: 1000, ToHimAnswers: 500,
		}},
	}
	return c
}
