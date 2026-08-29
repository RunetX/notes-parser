package main

import (
	"strings"
	"testing"
	"time"

	"lovegw/internal/narod"
)

func donorCard(id, nick string) narod.Card {
	return narod.Card{
		Stamp: narod.NewStamp("test", time.Now()),
		ID:    id,
		Kind:  narod.KindSnapshot,
		Persona: narod.Bio{Nick: nick, Gender: "male", Age: 50,
			Facts: []string{"живёт в Бердске"}},
		Register: narod.Register{
			Runes:      narod.Dist{P10: 20, Median: 60, P90: 140, Max: 300},
			SentWords:  narod.Dist{P10: 3, Median: 7, P90: 16, Max: 28},
			SentWordSD: 5, ShortSents: 0.2, LongSents: 0.1,
			Punct:     map[string]float64{".": 20},
			ParenRuns: map[string]float64{"1": 0.1},
			Openings:  []narod.Count{{Text: "ну", Share: 0.1}},
		},
		Samples: []narod.Sample{{Text: "дословная фраза донора"}},
		Latency: narod.LatencyDist{
			ToThreadSec: narod.Dist{P10: 600, Median: 3600, P90: 20000, Max: 80000},
			ToReplySec:  narod.Dist{P10: 200, Median: 1200, P90: 9000, Max: 50000},
		},
		Rhythm: narod.Rhythm{TZ: "Asia/Novosibirsk"},
		Dice: narod.DiceParams{ComeToNote: 0.3, ReplyMention: 0.7, ReplyOther: 0.1,
			MaxPerThread: 3, MaxPerDay: 8},
		Relations: []narod.SeedEdge{{Actor: "p999", Sympathy: 5}},
		Seed:      1,
	}
}

func twoDonorRecipe() (composeRecipe, []narod.Card) {
	a, b := donorCard("p100", "Донор А"), donorCard("p200", "Донор Б")
	a.Vocab = []narod.Word{{Word: "склад", TFIDF: 0.3}, {Word: "бердск", TFIDF: 0.5}}
	b.Vocab = []narod.Word{{Word: "склад", TFIDF: 0.2}, {Word: "гараж", TFIDF: 0.4}}
	a.Errors = []narod.ErrorPattern{
		{ID: "no_comma_before_chto", Rate: 4},
		{ID: narod.VariantErrorID, Rate: 2, Norm: "общем", Variant: "вобщем"},
		{ID: narod.VariantErrorID, Rate: 1, Norm: "будущее", Variant: "будующее"},
	}
	b.Errors = []narod.ErrorPattern{
		{ID: "no_comma_before_chto", Rate: 2},
		{ID: narod.VariantErrorID, Rate: 3, Norm: "общем", Variant: "вобщем"},
	}
	r := composeRecipe{
		ID: "zavhoz", Donors: []string{"p100", "p200"}, Weights: []float64{1, 1},
		Persona: narod.Bio{Nick: "Завхоз", Gender: "male", Age: 54, City: "Бердск"},
	}
	return r, []narod.Card{a, b}
}

func TestComposeCardIsCompositeWithoutDonorTraces(t *testing.T) {
	r, donors := twoDonorRecipe()
	card, err := composeCard(r, donors, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if card.Kind != narod.KindComposite {
		t.Errorf("сорт карточки %q", card.Kind)
	}
	// Дословных фраз донора у жителя нет — это и есть та утечка, ради которой
	// композит заводился.
	if len(card.Samples) > 0 {
		t.Errorf("в жителя уехали образцы донора: %+v", card.Samples)
	}
	// Отношений у нового человека ещё нет ни с кем.
	if len(card.Relations) > 0 {
		t.Errorf("житель родился с чужими отношениями: %+v", card.Relations)
	}
	if card.Persona.Nick != "Завхоз" || card.Persona.City != "Бердск" {
		t.Errorf("биография взята не из рецепта: %+v", card.Persona)
	}
	// Числовые цели — между донорами, а не у одного из них.
	if card.Register.Runes.Median != 60 {
		t.Errorf("медиана длины %d, у обоих доноров 60", card.Register.Runes.Median)
	}
	if err := card.Validate(); err != nil {
		t.Errorf("собранный житель не проходит проверку: %v", err)
	}
	if err := narod.CheckLive([]narod.Card{card}); err != nil {
		t.Errorf("житель не допущен в live: %v", err)
	}
}

// Словарь — пересечение: характерное слово одного человека это его подпись, по
// ней в архиве и ищут альт-анкеты.
func TestBlendVocabKeepsOnlyShared(t *testing.T) {
	_, donors := twoDonorRecipe()
	vocab, rate := blendVocab(donors, []float64{0.5, 0.5})
	if len(vocab) != 1 || vocab[0].Word != "склад" {
		t.Fatalf("словарь жителя: %+v, ожидалось только общее слово", vocab)
	}
	if rate != 0 && vocab == nil {
		t.Errorf("норма словаря без словаря: %v", rate)
	}
	// Единственный донор — пересекать не с чем, словарь не берётся вовсе.
	if got, _ := blendVocab(donors[:1], []float64{1}); got != nil {
		t.Errorf("у одного донора словарь взят целиком: %+v", got)
	}
}

// Уникальная описка — подпись человека и в жителя не переносится; общая для
// доноров — обычная безграмотность, её можно.
func TestBlendErrorsDropsSingleDonorVariant(t *testing.T) {
	_, donors := twoDonorRecipe()
	errs := blendErrors(donors, []float64{0.5, 0.5})

	var shared, unique bool
	for _, e := range errs {
		switch e.Variant {
		case "вобщем":
			shared = true
		case "будующее":
			unique = true
		}
	}
	if !shared {
		t.Error("общая для доноров описка не перенесена")
	}
	if unique {
		t.Error("описка одного донора перенесена в жителя — это его подпись")
	}
}

func TestComposeVerifyCatchesLopsidedAndTraces(t *testing.T) {
	r, donors := twoDonorRecipe()

	if notes := composeVerify(mustCompose(t, r, donors), donors, r.Weights); len(notes) != 0 {
		t.Errorf("на честной смеси есть замечания: %v", notes)
	}

	// Девять десятых одного донора — это он и есть, только под другим именем.
	r.Weights = []float64{9, 1}
	notes := composeVerify(mustCompose(t, r, donors), donors, r.Weights)
	if !containsAny(notes, "весит") {
		t.Errorf("перекос весов не замечен: %v", notes)
	}

	// Один донор — смешивать не с чем.
	single := composeRecipe{ID: "odin", Donors: []string{"p100"}, Weights: []float64{1},
		Persona: narod.Bio{Nick: "Одинокий"}}
	notes = composeVerify(mustCompose(t, single, donors[:1]), donors[:1], single.Weights)
	if !containsAny(notes, "донор один") {
		t.Errorf("единственный донор не замечен: %v", notes)
	}

	// Ник жителя совпал с ником донора.
	r, donors = twoDonorRecipe()
	r.Persona.Nick = donors[0].Persona.Nick
	notes = composeVerify(mustCompose(t, r, donors), donors, r.Weights)
	if !containsAny(notes, "совпадает с ником донора") {
		t.Errorf("совпадение ника не замечено: %v", notes)
	}
}

func TestNormalizeWeights(t *testing.T) {
	got := normalizeWeights([]float64{3, 1})
	if got[0] != 0.75 || got[1] != 0.25 {
		t.Errorf("веса %v", got)
	}
	// Нули и отрицательные не должны рушить смесь: рецепт пишет человек.
	if got := normalizeWeights([]float64{0, 0}); got[0] != 0.5 {
		t.Errorf("нулевые веса дали %v, ожидалось поровну", got)
	}
}

func mustCompose(t *testing.T, r composeRecipe, donors []narod.Card) narod.Card {
	t.Helper()
	card, err := composeCard(r, donors, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return card
}

func containsAny(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

// Композит обязан унести ЗАМЕР ОТКЛИКА, и это самый дорогой перенос из всех.
//
// Тест поставлен по живому дефекту 29.08.2026: `Rate` появился днём раньше
// вместе с `archive.MineReplyRate`, а `compose` его не смешивал вовсе — девять
// собранных жителей вышли с пустым замером. Поломка молчаливая: слепки считались
// правильно, калибровка сходилась, и разошлось бы только на площадке, где кубик
// жителя откатился бы на запасные `Dice.ReplyOther` — те самые придуманные
// числа, которые замер опроверг в 20–40 раз.
//
// Проверяется не наличие поля, а ДОЛЯ: смешивать счётчики напрямую нельзя (у
// одного донора корпус в полсотни раз больше, и он забрал бы корзину независимо
// от весов), поэтому усредняются вероятности, а счётчики восстанавливаются под
// них.
func TestComposeCarriesTheReplyRate(t *testing.T) {
	r, donors := twoDonorRecipe()
	// Корпуса РАЗНЫЕ на два порядка: у первого донора вдесятеро больше
	// наблюдений, но вероятность вдвое ниже. Веса поровну — значит и доля
	// обязана выйти средней, а не съехать к крупному донору.
	donors[0].Rate = narod.ReplyRate{Threads: 300, Buckets: []narod.RateBucket{
		{Upto: 1 << 30, Chances: 100000, Answers: 1000, ToHimChances: 1000, ToHimAnswers: 400},
	}}
	donors[1].Rate = narod.ReplyRate{Threads: 100, Buckets: []narod.RateBucket{
		{Upto: 1 << 30, Chances: 1000, Answers: 20, ToHimChances: 100, ToHimAnswers: 80},
	}}
	card, err := composeCard(r, donors, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := card.Rate.Rate(1, false)
	if !ok {
		t.Fatal("у композита нет замера отклика — кубик откатится на выдуманные числа")
	}
	// (1 % + 2 %) / 2 = 1.5 %, а не 1.02 %, куда утянул бы крупный донор.
	if got < 0.0145 || got > 0.0155 {
		t.Errorf("влезть в чужой разговор %.4f, ожидалось 0.015 — доля съехала к большому корпусу", got)
	}
	him, ok := card.Rate.Rate(1, true)
	if !ok || him < 0.59 || him > 0.61 {
		t.Errorf("ответить на обращение %.3f (замер %v), ожидалось 0.60", him, ok)
	}
	if card.Rate.Threads == 0 {
		t.Error("тредов в замере ноль — по ним читают, насколько замеру верить")
	}
}

// Замер повторного захода обязан доехать до жителя ровно так же, как отклик:
// без него кубик снова упрётся в «один корень на жителя за всю жизнь заметки»,
// а это и был потолок, из-за которого тред выходил вчетверо короче живого.
func TestComposeCarriesTheRootRate(t *testing.T) {
	r, donors := twoDonorRecipe()
	// Корпуса разные на два порядка, доли — 2 % и 6 %, веса поровну.
	donors[0].Roots = narod.RootRate{Threads: 300, Firsts: 400, Repeats: 100,
		Buckets: []narod.RateBucket{{Upto: 1 << 30, Chances: 100000, Answers: 2000}}}
	donors[1].Roots = narod.RootRate{Threads: 100, Firsts: 40, Repeats: 60,
		Buckets: []narod.RateBucket{{Upto: 1 << 30, Chances: 1000, Answers: 60}}}
	card, err := composeCard(r, donors, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := card.Roots.Rate(1)
	if !ok {
		t.Fatal("у композита нет замера повторного захода — житель зайдёт в заметку один раз за её жизнь")
	}
	// (2 % + 6 %) / 2 = 4 %, а не 2.04 %, куда утянул бы крупный донор.
	if got < 0.039 || got > 0.041 {
		t.Errorf("повторный заход %.4f, ожидалось 0.04 — доля съехала к большому корпусу", got)
	}
	if card.Roots.Firsts == 0 || card.Roots.Repeats == 0 {
		t.Errorf("свидетельство замера потеряно: первых %d, повторных %d",
			card.Roots.Firsts, card.Roots.Repeats)
	}
}
