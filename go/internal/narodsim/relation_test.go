package narodsim

import (
	"context"
	"math"
	"testing"
	"time"

	"lovegw/internal/narod"
)

// relCard — карточка с ЗАМЕРЕННЫМИ корзинами знакомства: незнакомому отвечают
// в 1 % случаев, давнему знакомому в 4 %. Средний отклик 2 %, значит размах
// рычага ровно вдвое — и на столько же, не больше, вправе двигать отношение.
func relCard() narod.Card {
	c := measuredCard()
	c.Rate.Familiar = []narod.RateBucket{
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
// придумано в нём только направление, величина взята из архива.
func TestRelationLiftBoundedByMeasuredSpan(t *testing.T) {
	card := relCard()
	span, _ := card.Rate.FamiliarSpan(false)

	up := relationLift(card, DecisionPoint{Tone: 1})
	if math.Abs(up-span) > 1e-9 {
		t.Fatalf("при полной симпатии рычаг %.3f, а замеренный потолок %.3f", up, span)
	}
	down := relationLift(card, DecisionPoint{Tone: -1})
	if math.Abs(down-1/span) > 1e-9 {
		t.Fatalf("при полной неприязни рычаг %.3f, ждали %.3f — тот же множитель обращённым",
			down, 1/span)
	}
	if got := relationLift(card, DecisionPoint{Tone: 0}); got != 1 {
		t.Fatalf("безразличие двигает вероятность: %.3f", got)
	}
	// Тон за пределами шкалы (порченый граф, чужая карта) рычаг не разгоняет.
	if got := relationLift(card, DecisionPoint{Tone: 12}); math.Abs(got-span) > 1e-9 {
		t.Fatalf("тон 12 дал рычаг %.3f — потолок пробит", got)
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
// житель на той же точке приходит при симпатии и молчит при неприязни.
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
	warm, cold := count(1), count(-1)
	if warm <= cold {
		t.Fatalf("к симпатичному пришли %d раз, к неприятному %d — рычаг не работает", warm, cold)
	}
}
