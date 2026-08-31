package speech

// СВОД — чем наша речь отличается от живой НА УРОВНЕ СЛОВ.
//
// Заведён 01.09.2026 после того, как разовый скрипт нашёл штампы машины
// (складно ×730, вслух ×50, будто ×20 против корпуса). Скрипт выбросили бы, а
// список штампов — ПОЛ, а не потолок: он ловит слова, которыми модель выдала
// себя на первых девятистах репликах, и после каждой правки промпта почерк
// уезжает в другие слова. Значит замер обязан быть повторяемым одной командой,
// иначе через неделю он снимется заново на глаз или не снимется вовсе.
//
// ТРИ ПРАВИЛА, и каждое оплачено ошибкой первого прогона.
//
// Частота считается НА СЛОВО, а не долей реплик. Наши реплики в полтора раза
// длиннее корпусных по числу слов, и на долях завышенным выходило ВСЁ подряд
// впятеро — «потом», «себя», «тебе» в том числе; находка от такого сравнения не
// отличается от артефакта.
//
// Слово обязано жить во ВСЕХ поданных тредах. Слово одной темы — это тема, а не
// почерк: в треде про ночные смены «ночь» стоит у тридцати пяти жителей из
// сорока девяти, и это ничего не говорит о машине.
//
// Доля реплик со штампом сравнивается на РЕПЛИКАХ ТОЙ ЖЕ ДЛИНЫ. Корпус целиком
// для этого не годится: там половина реплик вдвое короче наших, а короткая
// реплика реже содержит любое заданное слово просто потому, что слов в ней
// меньше.

import (
	"regexp"
	"sort"
	"strings"
)

// SweepWord — одно слово свода.
type SweepWord struct {
	Word string
	// Ours и Corpus — вхождений на 1000 слов, а не доля реплик (см. шапку).
	Ours   float64
	Corpus float64
	Times  float64 // во сколько раз чаще у нас
	Count  int     // сколько раз сказано у нас — чтобы видеть, на чём держится
	Known  bool    // слово уже в списке штампов, то есть находка не новая
}

// SweepResult — весь свод.
type SweepResult struct {
	Words []SweepWord // кандидаты, по убыванию отрыва
	// Rare — слова, которых в корпусе почти НЕТ, а у нас они есть. Отдельным
	// разделом, а не в общем списке, потому что отношение у них не считается:
	// делить на десяток вхождений в пяти миллионах слов значит печатать
	// случайное число с тремя знаками. Раздел этот важнее общего списка —
	// первый же прогон свода не увидел в нём «складно», самый сильный штамп
	// (×730 по разовому скрипту), ровно потому, что в корпусе он реже порога.
	Rare []SweepWord
	// Control — слова, где мы НЕ переигрываем. Печатать их обязательно:
	// систематический сдвиг сравнения виден только по ним, и первый прогон
	// именно на них и поймался.
	Control   []SweepWord
	OurWords  int
	CorpWords int
	// OurStamp и HumanStamp — доля реплик, где стоит хоть один ИЗВЕСТНЫЙ штамп.
	// Это и есть мера того, сработал ли евал: она обязана падать к человеческой.
	OurStamp   float64
	HumanStamp float64
	// LoWords и HiWords — полоса длины, на которой считалась HumanStamp.
	// Выводится из НАШИХ реплик, а не задана числом: полоса переедет вместе с
	// манерой, и замер останется сопоставимым сам с собой.
	LoWords int
	HiWords int
	Texts   int
}

var sweepToken = regexp.MustCompile(`[а-яё]+`)

// минимумы свода. Слово, сказанное у нас девять раз, — это девять бросков
// монетки, а не почерк; слово, встреченное в корпусе полсотни раз, не даёт
// знаменателя, которому можно верить.
const (
	sweepMinOurs = 10
	// SweepMinCorpus назван наружу: отчёт обязан сказать, ниже какого порога
	// отрыв не считается, — иначе раздел «почти нет в корпусе» читается как
	// «ноль», а это разные вещи.
	SweepMinCorpus = 50
	sweepMinCorpus = SweepMinCorpus
	sweepMinLen    = 4
	// sweepTimes — отрыв, с которого слово попадает в кандидаты. Четыре, а не
	// два: контрольные слова первого прогона легли в 0,4–1,0×, и полоса до
	// четырёх — это шум сравнения, а не почерк.
	sweepTimes = 4
)

// Sweep сравнивает наши реплики с корпусом. threads — наши реплики, РАЗБИТЫЕ ПО
// ТРЕДАМ (по файлу на тред): без разбиения правило «слово живёт во всех тредах»
// невыразимо, а без него свод выдаёт тему за почерк.
func Sweep(threads [][]string, corpus []string) SweepResult {
	res := SweepResult{}
	ourFreq := map[string]int{}
	seenIn := map[string]int{}
	var lens []int
	for _, thread := range threads {
		here := map[string]bool{}
		for _, t := range thread {
			res.Texts++
			ws := sweepToken.FindAllString(strings.ToLower(t), -1)
			res.OurWords += len(ws)
			lens = append(lens, len(ws))
			for _, w := range ws {
				ourFreq[w]++
				here[w] = true
			}
		}
		for w := range here {
			seenIn[w]++
		}
	}
	if res.Texts == 0 || len(corpus) == 0 {
		return res
	}
	res.LoWords, res.HiWords = band(lens)

	corpFreq := map[string]int{}
	var human, humanTotal int
	for _, t := range corpus {
		ws := sweepToken.FindAllString(strings.ToLower(t), -1)
		res.CorpWords += len(ws)
		for _, w := range ws {
			corpFreq[w]++
		}
		if len(ws) >= res.LoWords && len(ws) <= res.HiWords {
			humanTotal++
			if anyStamp(ws) {
				human++
			}
		}
	}
	if humanTotal > 0 {
		res.HumanStamp = float64(human) / float64(humanTotal)
	}

	var ours int
	for _, thread := range threads {
		for _, t := range thread {
			if anyStamp(sweepToken.FindAllString(strings.ToLower(t), -1)) {
				ours++
			}
		}
	}
	res.OurStamp = float64(ours) / float64(res.Texts)

	for w, k := range ourFreq {
		if k < sweepMinOurs || len([]rune(w)) < sweepMinLen || seenIn[w] < len(threads) {
			continue
		}
		cb := corpFreq[w]
		if cb < sweepMinCorpus {
			res.Rare = append(res.Rare, SweepWord{Word: w,
				Ours:   float64(k) * 1000 / float64(res.OurWords),
				Corpus: float64(cb) * 1000 / float64(res.CorpWords),
				Count:  k, Known: stampWords[w]})
			continue
		}
		a := float64(k) * 1000 / float64(res.OurWords)
		b := float64(cb) * 1000 / float64(res.CorpWords)
		row := SweepWord{Word: w, Ours: a, Corpus: b, Times: a / b, Count: k, Known: stampWords[w]}
		if row.Times >= sweepTimes {
			res.Words = append(res.Words, row)
		} else if row.Times <= 1 {
			res.Control = append(res.Control, row)
		}
	}
	sort.Slice(res.Words, func(i, j int) bool { return res.Words[i].Times > res.Words[j].Times })
	sort.Slice(res.Rare, func(i, j int) bool { return res.Rare[i].Ours > res.Rare[j].Ours })
	sort.Slice(res.Control, func(i, j int) bool { return res.Control[i].Count > res.Control[j].Count })
	if len(res.Control) > 8 {
		res.Control = res.Control[:8]
	}
	return res
}

// anyStamp — стоит ли в наборе слов хоть один известный штамп.
func anyStamp(words []string) bool {
	for _, w := range words {
		if stampWords[w] {
			return true
		}
	}
	return false
}

// band — полоса длины, на которой честно сравнивать доли: с десятой по
// девяностую долю наших реплик. Края отрезаны, потому что одна реплика в два
// слова и одна в триста растянули бы полосу на весь корпус, то есть отменили бы
// сопоставление, ради которого она и заводится.
func band(lens []int) (lo, hi int) {
	if len(lens) == 0 {
		return 0, 0
	}
	s := append([]int(nil), lens...)
	sort.Ints(s)
	lo, hi = s[len(s)/10], s[len(s)*9/10]
	if lo < 1 {
		lo = 1
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}
