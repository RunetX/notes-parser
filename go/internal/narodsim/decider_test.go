package narodsim

import (
	"context"
	"math"
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

// Монетка обязана падать с той частотой, которую ей назвали, — и падать так на
// ПОДРЯД ИДУЩИХ номерах реплик, потому что в треде они именно такие.
//
// Проверка не педантизм: близкие зёрна у генератора — известное место, где
// частота может уехать, а первое число потока здесь и есть всё решение. Замер
// 28.08.2026 показал, что у PCG с этим всё в порядке, — и тест держит это
// свойство впредь: ключ монетки ещё будет меняться.
func TestCardDeciderKeepsTheOddsOnConsecutiveIDs(t *testing.T) {
	card := testCard()
	for _, want := range []float64{0.1, 0.3, 0.9} {
		card.Dice.ReplyOther = want
		d := &CardDecider{Card: card, Seed: 7}
		const n = 20000
		came := 0
		for i := range int64(n) {
			// Номера идут подряд, как в настоящем треде.
			got, _ := d.Decide(context.Background(), DecisionPoint{
				Actor: 9, NoteID: 313000, TriggerID: 63238000 + i,
			})
			if got.Speak {
				came++
			}
		}
		// Допуск — три стандартных отклонения биномиальной доли: тест обязан
		// ловить перекос, а не пенять на честный разброс.
		sd := 3 * math.Sqrt(want*(1-want)/n)
		if got := float64(came) / n; got < want-sd || got > want+sd {
			t.Errorf("при вероятности %.2f житель приходил в %.3f случаев (допуск ±%.3f)", want, got, sd)
		}
	}
}

// Заметка входит в ключ монетки. Без неё точка решения НА САМОЙ заметке
// (номера реплики у неё нет вовсе) выпадала бы одинаково во всех тредах:
// смолчавший на первой молчал бы дальше везде. Первый прогон в вакууме
// 28.08.2026 дал ровно это — десять заметок подряд без единой реплики.
func TestCardDeciderVariesByNote(t *testing.T) {
	d := &CardDecider{Card: testCard(), Seed: 7}
	came, notes := 0, 200
	for n := range int64(notes) {
		got, err := d.Decide(context.Background(), DecisionPoint{Actor: 9, NoteID: 300000 + n})
		if err != nil {
			t.Fatal(err)
		}
		if got.Speak {
			came++
		}
	}
	// Приходить он должен примерно в трети заметок; проверяется не точность
	// доли, а то, что решение вообще МЕНЯЕТСЯ от заметки к заметке.
	if came == 0 || came == notes {
		t.Fatalf("на %d заметках житель решил одинаково %d раз — монетка не зависит от заметки",
			notes, came)
	}
	// И при этом остаётся повторяемой: та же заметка — тот же ответ.
	p := DecisionPoint{Actor: 9, NoteID: 300007}
	first, _ := d.Decide(context.Background(), p)
	for range 20 {
		if got, _ := d.Decide(context.Background(), p); got != first {
			t.Fatalf("та же заметка дала другое решение: %+v против %+v", got, first)
		}
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
	if p, dist := chanceOf(c, DecisionPoint{TriggerID: 0}); p != 0.3 || dist != c.Latency.ToThreadSec {
		t.Errorf("на заметке: p=%v, dist=%+v", p, dist)
	}
	if p, dist := chanceOf(c, DecisionPoint{TriggerID: 5, Addressed: true}); p != 0.9 || dist != c.Latency.ToReplySec {
		t.Errorf("на обращении: p=%v, dist=%+v", p, dist)
	}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5}); p != 0.1 {
		t.Errorf("в чужом разговоре: p=%v", p)
	}
}

// Замер СИЛЬНЕЕ придуманного числа, и падает он вместе с глубиной треда.
//
// Правило оплачено замером 28.08.2026: придуманное «влезть в чужой разговор =
// 0.15» оказалось завышено в 20–40 раз (настоящее от 0.4 % до 3.7 %), и в треде
// на 298 реплик кубик приходил 71 раз мимо при одном попадании.
func TestChanceOfPrefersMeasuredRate(t *testing.T) {
	c := testCard()
	c.Rate = narod.ReplyRate{Threads: 300, Buckets: []narod.RateBucket{
		{Upto: 10, Chances: 1000, Answers: 30, ToHimChances: 100, ToHimAnswers: 70},
		{Upto: 1 << 30, Chances: 1000, Answers: 10, ToHimChances: 100, ToHimAnswers: 50},
	}}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5, Seen: 3}); p != 0.03 {
		t.Errorf("в начале треда p=%v, ожидалось 0.03 из замера, а не 0.1 из карточки", p)
	}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5, Seen: 200}); p != 0.01 {
		t.Errorf("в глубине треда p=%v, ожидалось 0.01", p)
	}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5, Seen: 3, Addressed: true}); p != 0.7 {
		t.Errorf("на обращении p=%v, ожидалось 0.7 из замера", p)
	}
	// «Прийти в новую заметку» замером не покрыто и покрыто быть не может: в
	// архиве видно, куда человек пришёл, и не видно, что он пролистал.
	if p, dist := chanceOf(c, DecisionPoint{TriggerID: 0, Seen: 0}); p != 0.3 || dist != c.Latency.ToThreadSec {
		t.Errorf("на заметке p=%v", p)
	}
}

// Тощая корзина замером НЕ считается: доля по трём случаям — это не редкость
// события, а отсутствие данных, и подставлять её в кубик значило бы выдать шум
// за характер. Тогда работает число карточки.
func TestChanceOfFallsBackOnThinBucket(t *testing.T) {
	c := testCard()
	c.Rate = narod.ReplyRate{Buckets: []narod.RateBucket{
		{Upto: 1 << 30, Chances: 5, Answers: 5, ToHimChances: 4, ToHimAnswers: 4},
	}}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5, Seen: 3}); p != 0.1 {
		t.Errorf("на тощей корзине p=%v, ожидалось 0.1 из карточки", p)
	}
	if p, _ := chanceOf(c, DecisionPoint{TriggerID: 5, Seen: 3, Addressed: true}); p != 0.9 {
		t.Errorf("на тощей корзине обращения p=%v, ожидалось 0.9 из карточки", p)
	}
}

// rootCard — карточка с ЗАМЕРЕННЫМ повторным заходом и нулевым откликом:
// отвечать житель не будет никогда, значит всё, что он скажет, — корни.
func rootCard(p float64) narod.Card {
	c := testCard()
	c.Dice.MaxPerThread = 0
	c.Dice.ReplyOther, c.Dice.ReplyMention = 0, 0
	c.Rate = narod.ReplyRate{Buckets: []narod.RateBucket{
		{Upto: 1 << 30, Chances: 10000, Answers: 0, ToHimChances: 10000, ToHimAnswers: 0},
	}}
	c.Roots = narod.RootRate{Buckets: []narod.RateBucket{
		{Upto: 1 << 30, Chances: 10000, Answers: int(p * 10000)},
	}}
	return c
}

// Заговоривший в треде возвращается В ЗАМЕТКУ, а не только отвечает. Без этого
// «прийти в заметку» бросалось один раз за её жизнь, и корней у тридцати
// жителей не могло стать больше двадцати девяти ни при какой вероятности.
func TestCardDeciderComesBackToTheNote(t *testing.T) {
	d := &CardDecider{Card: rootCard(0.3), Seed: 7}
	roots, replies := 0, 0
	for i := 0; i < 2000; i++ {
		got, err := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, NoteID: 500, TriggerID: int64(3000 + i), Said: 1, Seen: 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case got.Speak && got.Root:
			roots++
		case got.Speak:
			replies++
		}
	}
	if replies != 0 {
		t.Errorf("при нулевом отклике житель ответил %d раз", replies)
	}
	// Доля обязана держаться замера, а не быть «хоть сколько-нибудь».
	if share := float64(roots) / 2000; share < 0.26 || share > 0.34 {
		t.Errorf("повторных заходов %.3f, замер говорит 0.30", share)
	}
	// Задержка идёт от ЭТОЙ реплики, а не от публикации заметки: возможность
	// создала она.
	got, _ := d.Decide(context.Background(), DecisionPoint{
		Actor: 9, NoteID: 500, TriggerID: 3001, Said: 1, Seen: 20})
	if got.Speak && got.After > time.Duration(rootCard(0.3).Latency.ToReplySec.Max)*time.Second {
		t.Errorf("задержка %s взята не из ToReplySec", got.After)
	}
}

// Первый заход этой мерой НЕ покрыт: в архиве видно, куда человек пришёл, и не
// видно, что он пролистал. Молчавший в треде корня не даёт, сколько ни бросай.
func TestCardDeciderRootNeedsAVoiceInTheThread(t *testing.T) {
	d := &CardDecider{Card: rootCard(0.9), Seed: 7}
	for i := 0; i < 500; i++ {
		got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, NoteID: 500, TriggerID: int64(3000 + i), Said: 0, Seen: 20})
		if got.Root {
			t.Fatalf("житель, ни разу не сказавший в треде, вернулся в заметку (точка %d)", i)
		}
	}
}

// Пустой замер запасного числа не имеет намеренно: иначе рядом с замером
// немедленно завелась бы вторая выдумка вроде прежнего «влезть = 0.15».
func TestCardDeciderRootHasNoFallback(t *testing.T) {
	c := rootCard(0.9)
	c.Roots = narod.RootRate{}
	d := &CardDecider{Card: c, Seed: 7}
	for i := 0; i < 500; i++ {
		got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, NoteID: 500, TriggerID: int64(3000 + i), Said: 3, Seen: 20})
		if got.Root {
			t.Fatalf("без замера житель всё равно вернулся в заметку (точка %d)", i)
		}
	}
}

// Готовность зайти в новую заметку берётся из ЗАМЕРА, а придуманное число
// карточки остаётся запасным путём. Порядок тот же, что у отклика, и по той же
// причине: замер знает про человека то, чего не угадать, — у одного это каждая
// вторая заметка, у другого одна из двадцати.
func TestCardDeciderPrefersMeasuredComeRate(t *testing.T) {
	c := testCard()
	c.Dice.ComeToNote = 0.05 // выдумка, которую замер обязан вытеснить
	c.Come = narod.ComeRate{Days: 300, Chances: 1000, Came: 600}
	d := &CardDecider{Card: c, Seed: 7}
	came := 0
	for i := 0; i < 2000; i++ {
		got, err := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, NoteID: int64(500 + i), TriggerID: 0,
			Now: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		if got.Speak {
			came++
		}
	}
	// Час суток здесь ещё домножает, поэтому проверяется не точная доля, а то,
	// что замер вытеснил выдумку: 5 % не дали бы и трети от этого.
	if share := float64(came) / 2000; share < 0.25 {
		t.Errorf("зашёл в %.3f заметок — замер 0.60 не вытеснил выдумку 0.05", share)
	}
	// Пустой замер — запасной путь, а не ноль.
	c.Come = narod.ComeRate{Days: 300, Chances: 5, Came: 3}
	d = &CardDecider{Card: c, Seed: 7}
	came = 0
	for i := 0; i < 2000; i++ {
		got, _ := d.Decide(context.Background(), DecisionPoint{
			Actor: 9, NoteID: int64(500 + i), TriggerID: 0,
			Now: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)})
		if got.Speak {
			came++
		}
	}
	if share := float64(came) / 2000; share > 0.15 {
		t.Errorf("тощий замер принят за 0.60: доля %.3f", share)
	}
}
