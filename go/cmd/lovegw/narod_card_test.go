package main

// Парные тесты эпика «народ».
//
// Живут здесь потому, что здесь единственное место, знающее оба мира: ядро
// эмуляции не импортирует архив, а архив не знает про эмуляцию. Ровно из-за
// этой границы правила и живут в двух экземплярах — и ровно поэтому нужен
// сторож, который заметит, что они разъехались.

import (
	"reflect"
	"strings"
	"testing"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// Перенос замеров манеры обязан быть полным. Молча потерянное поле выглядит на
// странице как «персонаж пишет ровно», а не как поломка: нулевой разброс длин
// предложений — это в точности машинный текст.
func TestRegisterFromShapeCarriesEveryMeasure(t *testing.T) {
	sh := archive.VoiceShape{
		Kind:          "comments",
		Runes:         archive.Dist{P10: 20, Median: 60, P90: 140, Max: 300},
		SentWords:     archive.Dist{P10: 3, Median: 7, P90: 16, Max: 28},
		SentWordSD:    5.4,
		ShortSents:    0.24,
		LongSents:     0.08,
		Punct:         map[string]float64{".": 21.4},
		ParenRuns:     map[string]float64{"2": 0.3},
		TopSmileys:    []archive.VoiceCount{{Text: "agree", Share: 0.1}},
		SmileyRate:    0.2,
		EmojiRate:     0.05,
		AllLower:      0.31,
		StartsLower:   0.44,
		NoFinalPunct:  0.38,
		YoRate:        0.02,
		TopOpenings:   []archive.VoiceCount{{Text: "ну", Share: 0.11}},
		AddressPrefix: 0.52,
	}
	r := registerFromShape(sh)

	// Каждое поле карточки обязано быть заполнено чем-то из замера: пустое
	// поле здесь и есть тихая потеря.
	checks := map[string]bool{
		"Runes":         r.Runes == narod.Dist{P10: 20, Median: 60, P90: 140, Max: 300},
		"SentWords":     r.SentWords == narod.Dist{P10: 3, Median: 7, P90: 16, Max: 28},
		"SentWordSD":    r.SentWordSD == sh.SentWordSD,
		"ShortSents":    r.ShortSents == sh.ShortSents,
		"LongSents":     r.LongSents == sh.LongSents,
		"Punct":         reflect.DeepEqual(r.Punct, sh.Punct),
		"ParenRuns":     reflect.DeepEqual(r.ParenRuns, sh.ParenRuns),
		"Smileys":       len(r.Smileys) == 1 && r.Smileys[0].Text == "agree",
		"SmileyRate":    r.SmileyRate == sh.SmileyRate,
		"EmojiRate":     r.EmojiRate == sh.EmojiRate,
		"AllLower":      r.AllLower == sh.AllLower,
		"StartsLower":   r.StartsLower == sh.StartsLower,
		"NoFinalPunct":  r.NoFinalPunct == sh.NoFinalPunct,
		"YoRate":        r.YoRate == sh.YoRate,
		"Openings":      len(r.Openings) == 1 && r.Openings[0].Text == "ну",
		"AddressPrefix": r.AddressPrefix == sh.AddressPrefix,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("замер %s не доехал до карточки", name)
		}
	}

	// И обратная сторона: у карточки не должно завестись поля, которое перенос
	// не заполняет. Тест перечисляет их поимённо, поэтому новое поле придётся
	// либо заполнить, либо назвать здесь осознанно.
	// Эти поля замером не заполняются: набор эмодзи и словечки-приклейки пишет
	// владелец, когда собирает жителя. У архива их нет — там есть только
	// частота эмодзи, а какие именно, он не различает.
	known := map[string]bool{"Parasites": true, "Emoji": true}
	v := reflect.TypeOf(r)
	for i := 0; i < v.NumField(); i++ {
		name := v.Field(i).Name
		if _, checked := checks[name]; !checked && !known[name] {
			t.Errorf("поле карточки %s не проверено переносом — либо заполните его, либо назовите в known", name)
		}
	}
}

// Списки слов-соседей для «-тся/-ться» живут в двух пакетах: майнер ими ищет
// ошибку, постпроцессор — вносит. Разъехавшись, они дали бы персонажа, который
// делает не ту ошибку, что у него замерена, и заметить это было бы нечем.
func TestTsyaMarkersAgree(t *testing.T) {
	mineInf, mineThird := archive.TsyaMarkers()
	injInf, injThird := narod.TsyaMarkers()
	if !reflect.DeepEqual(mineInf, injInf) {
		t.Errorf("списки инфинитивных соседей разошлись:\nмайнер:  %v\nвставка: %v", mineInf, injInf)
	}
	if !reflect.DeepEqual(mineThird, injThird) {
		t.Errorf("списки соседей третьего лица разошлись:\nмайнер:  %v\nвставка: %v", mineThird, injThird)
	}
}

// Класс личной словоформы называется одинаково с обеих сторон: майнер пишет в
// карточку id, постпроцессор по нему же выбирает правку.
func TestVariantErrorIDAgrees(t *testing.T) {
	if archive.VariantErrorID != narod.VariantErrorID {
		t.Errorf("id личной словоформы разошёлся: %q против %q", archive.VariantErrorID, narod.VariantErrorID)
	}
}

// Каждому классу ошибок, который умеет находить майнер, обязана отвечать
// правка: замеренная, но не вносимая ошибка — это молча потерянная черта
// персонажа.
func TestEveryMinedErrorClassCanBeInjected(t *testing.T) {
	for _, id := range archive.ErrorClasses() {
		if !narod.CanInject(id) {
			t.Errorf("класс %q майнер находит, а постпроцессор внести не умеет", id)
		}
	}
}

// Потолок разговорчивости берётся ИЗ ЗАМЕРА, а не с потолка. Правило оплачено
// первым же бесплатным прогоном реплея 28.08.2026: у ДВ в карточке стояло 5
// реплик на тред против настоящих 44–53, кубик глох после пятой, и калибровка
// ловила 6 % его настоящих ответов вместо 21 %.
func TestDiceCapsComeFromMeasurement(t *testing.T) {
	sh := archive.VoiceShape{AddressPrefix: 0.72}
	load := archive.Dist{P10: 1, Median: 3, P90: 11, Max: 53}
	d := diceFromShape(sh, load)
	if d.MaxPerThread != 11 {
		t.Errorf("потолок на тред %d, ожидался p90 = 11", d.MaxPerThread)
	}
	// Не максимум: у говорливого автора максимум — это один скандал на всю
	// историю, и мерить им обычный вечер значит разрешать скандал каждый раз.
	if d.MaxPerThread == load.Max {
		t.Error("потолок взят по максимуму замера")
	}
	if d.MaxPerDay <= d.MaxPerThread {
		t.Errorf("суточный потолок %d не выше тредового %d — он молча заменит собой тредовый",
			d.MaxPerDay, d.MaxPerThread)
	}
	// Пустой замер не должен обнулять потолки: житель молчал бы всегда.
	if got := diceFromShape(sh, archive.Dist{}); got.MaxPerThread <= 0 || got.MaxPerDay <= 0 {
		t.Errorf("без замера потолки обнулились: %+v", got)
	}
	// Вероятности замерить нельзя — они остаются базой брифа, и это не дефект.
	if d.ComeToNote != 0.35 || d.ReplyMention != 0.7 {
		t.Errorf("вероятности уехали от базы: %+v", d)
	}
	// Разговорчивого (обращается по имени) отличает готовность влезть в чужое.
	if quiet := diceFromShape(archive.VoiceShape{AddressPrefix: 0.1}, load); quiet.ReplyOther >= d.ReplyOther {
		t.Errorf("молчун влезает в чужой разговор не реже разговорчивого: %v против %v",
			quiet.ReplyOther, d.ReplyOther)
	}
}

func TestSlugOfIdentity(t *testing.T) {
	cases := map[string]string{
		"p1527":  "p1527",
		"u21247": "u21247",
		"P1527":  "p1527",
	}
	for in, want := range cases {
		if got := slugOfIdentity(in); got != want {
			t.Errorf("slugOfIdentity(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

func TestParseAge(t *testing.T) {
	cases := map[string]int{
		"52 года": 52,
		"41 год":  41,
		"":        0,
		"—":       0,
		"12 лет":  0, // подростков на сайте знакомств не бывает: это мусор в поле
		"200":     0,
	}
	for in, want := range cases {
		if got := parseAge(in); got != want {
			t.Errorf("parseAge(%q) = %d, ожидалось %d", in, got, want)
		}
	}
}

// Тон переносится в симпатию по шкале мира, а пары без тона не переносятся
// вовсе: нулевая симпатия и отсутствие отношения — разные вещи.
func TestRelationsFromArchive(t *testing.T) {
	rels := []archive.RelationRow{
		{To: "p100", Label: "Тёплая", Tone: 0.4, Replies: 300},
		{To: "p200", Label: "Колючий", Tone: -0.6, Replies: 120},
		{To: "p300", Label: "Никакой", Tone: 0, Replies: 90},
	}
	got := relationsFromArchive(rels)
	if len(got) != 2 {
		t.Fatalf("перенесено %d рёбер, ожидалось 2: %+v", len(got), got)
	}
	if got[0].Sympathy != 4 || got[1].Sympathy != -6 {
		t.Errorf("шкала симпатии переведена неверно: %+v", got)
	}
	for _, e := range got {
		if strings.Contains(e.Actor, " ") {
			t.Errorf("ключ актора не приведён к slug: %q", e.Actor)
		}
	}
}

// Полярность важнее силы: «не любит» обязано давать отрицательный вес — это
// законный вход для «промолчать».
func TestTriggersFromFacts(t *testing.T) {
	facts := []archive.IdentityFact{
		{Topic: "cars", Polarity: archive.PolarityLikes, Score: 0.8},
		{Topic: "kids", Polarity: archive.PolarityDislikes, Score: 0.6},
		{Topic: "sea", Polarity: archive.PolarityMentions, Score: 0.4},
	}
	got := triggersFromFacts(facts)
	byName := map[string]float64{}
	for _, tp := range got {
		byName[tp.Name] = tp.Weight
	}
	// Темы названы по-русски: карточка уходит и в промпт, и человеку в отчёт,
	// а «alcohol, dacha, kids» не читается ни тем, ни другим.
	if _, ok := byName["cars"]; ok {
		t.Errorf("тема осталась ключом лексикона: %+v", got)
	}
	if byName["машины"] <= 0 {
		t.Errorf("«любит» дало вес %v", byName["машины"])
	}
	if byName["дети"] >= 0 {
		t.Errorf("«не любит» дало вес %v — молчать персонажу будет не с чего", byName["дети"])
	}
	if byName["море"] >= byName["машины"] {
		t.Errorf("просто упомянутая тема весит не меньше любимой: %v против %v", byName["море"], byName["машины"])
	}
	// Порядок — по убыванию веса: карточку читает человек.
	for i := 1; i < len(got); i++ {
		if got[i-1].Weight < got[i].Weight {
			t.Fatalf("темы не отсортированы: %+v", got)
		}
	}
}

// Список тем обрезается: на живом архиве разговорчивый человек за годы
// упоминает все двенадцать тем лексикона, и «цепляет всё» не отличает его ни от
// кого. Остаются сильные с ОБОИХ концов — и любимое, и постылое.
func TestTriggersFromFactsKeepsStrongestFromBothEnds(t *testing.T) {
	var facts []archive.IdentityFact
	for topic, score := range map[string]float64{
		"cars": 0.9, "kids": 0.8, "sea": 0.7, "dacha": 0.6,
		"music": 0.5, "sport": 0.4, "cooking": 0.3, "travel": 0.2,
	} {
		facts = append(facts, archive.IdentityFact{Topic: topic, Polarity: archive.PolarityLikes, Score: score})
	}
	facts = append(facts, archive.IdentityFact{Topic: "alcohol", Polarity: archive.PolarityDislikes, Score: 0.85})

	got := triggersFromFacts(facts)
	if len(got) != maxTriggers {
		t.Fatalf("тем %d, ожидалось %d: %+v", len(got), maxTriggers, got)
	}
	var hasDislike bool
	for _, tp := range got {
		if tp.Weight < 0 {
			hasDislike = true
		}
	}
	if !hasDislike {
		t.Errorf("сильная антипатия вытеснена слабыми симпатиями: %+v", got)
	}
}
