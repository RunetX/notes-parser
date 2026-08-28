package narodsim

// Базовый кубик — БАЗЛАЙН этапа 2, а не окончательное правило.
//
// Он смотрит ровно на три вещи: обратились ли к жителю, сколько он уже сказал в
// этом треде и что говорит его карточка о частоте. Ни темы, ни отношений, ни
// часа суток здесь нет намеренно — они появятся на этапе 3 вместе с миром, и
// смысл базлайна в том, чтобы к их появлению уже было ЧИСЛО, которое они обязаны
// улучшить. Пороги калибровки, придуманные раньше первого прогона, были бы
// выдумкой.
//
// Бросок выводится из (зерно, анкета, реплика), а не из последовательного
// генератора. Разница существенная: последовательный даёт разный результат,
// стоит поменяться порядку или числу точек решения — например, если завтра
// добавится ещё один источник событий, — и два прогона перестанут сравниваться.
// Здесь же монетка на конкретной реплике одна и та же всегда.

import (
	"context"
	"math/rand/v2"
	"time"

	"lovegw/internal/narod"
)

// CardDecider — «прийти или смолчать» по карточке.
type CardDecider struct {
	Card narod.Card
	Seed uint64
}

// Decide бросает монетку на одной точке.
func (d *CardDecider) Decide(_ context.Context, p DecisionPoint) (Decision, error) {
	dice := d.Card.Dice
	// Потолок реплик на тред — часть характера, а не защита: человек, у которого
	// в замере полтора десятка реплик за тред, на шестидесяти выглядел бы
	// одержимым.
	if dice.MaxPerThread > 0 && p.Said >= dice.MaxPerThread {
		return Decision{}, nil
	}

	prob, dist := chanceOf(dice, d.Card.Latency, p)
	rng := rand.New(rand.NewPCG(d.Seed^uint64(p.Actor), uint64(p.TriggerID)+1))
	if rng.Float64() >= prob {
		return Decision{}, nil
	}
	return Decision{Speak: true, After: sampleDist(dist, rng.Float64())}, nil
}

// chanceOf — вероятность заговорить и распределение задержки для точки.
func chanceOf(dice narod.DiceParams, lat narod.LatencyDist, p DecisionPoint) (float64, narod.Dist) {
	switch {
	case p.TriggerID == 0:
		// Заметка: «прийти в новую» — и задержка меряется от её публикации.
		return dice.ComeToNote, lat.ToThreadSec
	case p.Addressed:
		return dice.ReplyMention, lat.ToReplySec
	default:
		return dice.ReplyOther, lat.ToReplySec
	}
}

// sampleDist — задержка по квантилям замера.
//
// Распределение задано четырьмя числами (P10, медиана, P90, максимум), и
// восстанавливается оно кусочно-линейно между ними. Это заведомо грубее правды,
// но честнее любого красивого закона: показательное или логнормальное
// распределение подогнало бы хвост под формулу, а хвост здесь и есть содержание —
// человек отвечает то через минуту, то через три часа.
func sampleDist(d narod.Dist, u float64) time.Duration {
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	switch {
	case d.Median <= 0:
		return 0
	case u < 0.1:
		return sec(lerp(0, float64(d.P10), u/0.1))
	case u < 0.5:
		return sec(lerp(float64(d.P10), float64(d.Median), (u-0.1)/0.4))
	case u < 0.9:
		return sec(lerp(float64(d.Median), float64(d.P90), (u-0.5)/0.4))
	default:
		top := float64(d.Max)
		if top < float64(d.P90) {
			top = float64(d.P90)
		}
		return sec(lerp(float64(d.P90), top, (u-0.9)/0.1))
	}
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }
