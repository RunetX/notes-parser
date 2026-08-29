package narodsim

// testCard — простая карточка на выдуманных числах: ею проверяется механика
// прогона, а не поведение. Замерные числа живут в measuredCard.
//
// Своя копия, а не общая с ядром: помощник теста не экспортируется, а
// экспортировать его ради второго пакета значило бы вынести тестовую фикстуру в
// боевой код.

import "lovegw/internal/narod"

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
