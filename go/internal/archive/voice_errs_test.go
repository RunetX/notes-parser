package archive

import (
	"strings"
	"testing"
)

func TestCountTsyaJudgesByNeighbour(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		// Морфологии у нас нет, судим по соседу слева.
		{"надо учится и всё", 1},        // после «надо» нужен мягкий знак
		{"он учиться в универе", 1},     // после «он» — не нужен
		{"надо учиться и всё", 0},       // верно
		{"он учится в универе", 0},      // верно
		{"пора расходится по домам", 1}, // «пора» просит инфинитив
		{"всё меняеться каждый день", 1},
		// Соседа нет в списках — не считаем вовсе: вписать человеку чужую
		// ошибку хуже, чем не заметить своей.
		{"вчера казалось получиться быстрее", 0},
	}
	for _, tc := range cases {
		if got := countTsya(tc.text); got != tc.want {
			t.Errorf("countTsya(%q) = %d, ожидалось %d", tc.text, got, tc.want)
		}
	}
}

func TestClassDetectors(t *testing.T) {
	cases := []struct {
		id   string
		text string
		want int
	}{
		{"no_comma_before_chto", "думаю что успею", 1},
		{"no_comma_before_chto", "думаю, что успею", 0},
		{"no_comma_before_chto", "а что делать", 1},
		{"colloquial", "щас приду, ваще ничего", 2},
		{"lower_after_dot", "Пришёл. посмотрел. Ушёл", 1},
		{"no_space_after_comma", "да,конечно", 1},
		{"no_space_after_comma", "было 1,5 часа", 0}, // число, а не пропущенный пробел
		{"no_space_after_dot", "пришёл.посмотрел", 1},
		{"double_space", "два  пробела", 1},
		{"long_ellipsis", "ну не знаю....", 1},
		{"long_ellipsis", "ну не знаю...", 0}, // обычное многоточие ошибкой не считается
	}
	byID := map[string]errDetector{}
	for _, d := range errDetectors {
		byID[d.id] = d
	}
	for _, tc := range cases {
		d, ok := byID[tc.id]
		if !ok {
			t.Fatalf("детектора %s нет", tc.id)
		}
		if got := d.count(tc.text); got != tc.want {
			t.Errorf("%s(%q) = %d, ожидалось %d", tc.id, tc.text, got, tc.want)
		}
	}
}

func TestNeighbors1(t *testing.T) {
	got := map[string]bool{}
	for _, n := range neighbors1("общем") {
		got[n] = true
	}
	for _, want := range []string{"вобщем", "бщем", "обшем", "бощем"} {
		if !got[want] {
			t.Errorf("сосед %q не порождён", want)
		}
	}
	if got["вообщем"] {
		t.Error("слово на расстоянии двух правок попало в соседей")
	}
}

// Ошибка попадает в карточку только тогда, когда автор делает её ЗАМЕТНО чаще
// остальных: иначе в характерные ошибки уедет общая манера эпохи.
func TestMeasureErrorsNeedsCorpusNorm(t *testing.T) {
	texts := []voiceText{}
	for i := 0; i < 20; i++ {
		texts = append(texts, voiceText{id: int64(i), kind: "comments",
			text: "думаю что успею вобщем это не так важно и ещё немного слов для веса"})
	}
	norm := CorpusNorm{
		Texts: 1000, Words: 100000,
		ClassRate: map[string]float64{"no_comma_before_chto": 0.5},
		WordFreq:  map[string]int{"общем": 5000, "вобщем": 3},
	}

	errs := measureErrors(texts, norm)
	var class, variant *VoiceError
	for i := range errs {
		switch {
		case errs[i].ID == "no_comma_before_chto":
			class = &errs[i]
		case errs[i].ID == VariantErrorID && errs[i].Variant == "вобщем":
			variant = &errs[i]
		}
	}
	if class == nil {
		t.Fatalf("пропущенная запятая перед «что» не найдена: %+v", errs)
	}
	if class.Ratio < errMinRatio {
		t.Errorf("отношение к норме %v — ниже порога", class.Ratio)
	}
	if variant == nil {
		t.Fatalf("личная словоформа «вобщем» не найдена: %+v", errs)
	}
	if variant.Norm != "общем" {
		t.Errorf("норма для «вобщем» = %q, ожидалось «общем»", variant.Norm)
	}

	// Та же выборка против корпуса, который пишет так же, — ошибок нет.
	norm.ClassRate["no_comma_before_chto"] = 100
	norm.WordFreq["вобщем"] = 5000
	for _, e := range measureErrors(texts, norm) {
		if e.ID == "no_comma_before_chto" || e.Variant == "вобщем" {
			t.Errorf("общая манера корпуса записана автору в ошибки: %+v", e)
		}
	}
}

// Редкое слово, написанное верно, ошибкой не считается — иначе персонажу
// вписали бы опечатку вместо его словаря.
func TestMeasureErrorsKeepsRareCorrectWords(t *testing.T) {
	texts := []voiceText{}
	for i := 0; i < 10; i++ {
		texts = append(texts, voiceText{id: int64(i), kind: "comments",
			text: "снабженец на складе считает поддоны и накладные каждый день"})
	}
	norm := CorpusNorm{Texts: 1000, Words: 100000, ClassRate: map[string]float64{},
		WordFreq: map[string]int{"снабженец": 40, "поддоны": 30}}
	for _, e := range measureErrors(texts, norm) {
		if e.ID == VariantErrorID {
			t.Errorf("верно написанное редкое слово записано в ошибки: %+v", e)
		}
	}
}

func TestCountErrWordsMatchesVocabTokenizer(t *testing.T) {
	// Токенизация обязана совпадать со словарём карты письма, иначе частоты
	// «на 1000 слов» несравнимы между картой и ошибками.
	text := "[b]Вот[/b] это да :::agree::: ёлки"
	words := map[string]int{}
	n := countErrWords(text, words)
	var viaVocab int
	forEachWord(stripSiteMarkup(text), func([]rune) { viaVocab++ })
	if n != viaVocab {
		t.Errorf("слов %d, у словаря %d", n, viaVocab)
	}
	if words["елки"] == 0 {
		t.Errorf("«ёлки» не приведено к «елки»: %v", words)
	}
	if strings.Contains(strings.Join(keysOf(words), " "), "agree") {
		t.Errorf("разметка сайта попала в слова: %v", words)
	}
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
