package main

// Сборка жителя площадки из слепков доноров.
//
// Житель — это КОМПОЗИТ: числовые цели манеры смешаны из нескольких реальных
// участников архива, а имя, биография и интересы написаны владельцем. Смысл
// приёма в том, что похожей выходит МАНЕРА, а не человек: у неё нет ни имени
// донора, ни его биографии, ни единой его дословной фразы.
//
// Прецедент — dump/voice/style-monk-decadence.md: голос, собранный из числовых
// целей двух живых людей, который не метит ни в чью манеру и никому не
// принадлежит.
//
// Сторож здесь не дисциплина, а проверка: composeVerify считает, не оказался ли
// «композит» одним донором с косметикой, и не уехало ли в него что-то, чего нет
// у остальных.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lovegw/internal/narod"
)

// composeRecipe — что владелец решает про жителя сам.
//
// Биография пишется руками намеренно и это не недоделка: придумать человеку
// город и работу — авторское решение садовника, а вывести их из донора значило
// бы перенести в жителя самого донора.
type composeRecipe struct {
	ID        string            `json:"id"`
	Donors    []string          `json:"donors"`            // id слепков в каталоге
	Weights   []float64         `json:"weights,omitempty"` // пусто — поровну
	Persona   narod.Bio         `json:"persona"`
	Emoji     []string          `json:"emoji,omitempty"`
	Parasites []string          `json:"parasites,omitempty"`
	Triggers  []narod.Topic     `json:"triggers,omitempty"`
	Dice      *narod.DiceParams `json:"dice,omitempty"`
	Seed      int64             `json:"seed,omitempty"`
}

// lopsidedWeight — доля, выше которой композит перестаёт быть композитом.
// Смесь «девять десятых одного донора» — это тот же донор, только с чужим
// именем, а это ровно то, чего мы избегаем.
const lopsidedWeight = 0.75

// minDonorsForVariant — личная опечатка переносится в жителя, только если её
// делает не один донор. Уникальная описка — это подпись человека.
const minDonorsForVariant = 2

func loadRecipe(path string) (composeRecipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return composeRecipe{}, err
	}
	var r composeRecipe
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return composeRecipe{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if len(r.Donors) == 0 {
		return composeRecipe{}, fmt.Errorf("%s: не назван ни один донор", filepath.Base(path))
	}
	if len(r.Weights) == 0 {
		r.Weights = make([]float64, len(r.Donors))
		for i := range r.Weights {
			r.Weights[i] = 1
		}
	}
	if len(r.Weights) != len(r.Donors) {
		return composeRecipe{}, fmt.Errorf("%s: весов %d, доноров %d", filepath.Base(path), len(r.Weights), len(r.Donors))
	}
	return r, nil
}

// composeCard смешивает слепки в жителя.
func composeCard(r composeRecipe, donors []narod.Card, now time.Time) (narod.Card, error) {
	w := normalizeWeights(r.Weights)

	card := narod.Card{
		Stamp:   narod.NewStamp("lovegw narod compose", now),
		ID:      r.ID,
		Kind:    narod.KindComposite,
		Sources: append([]string(nil), r.Donors...),
		Persona: r.Persona,
		Seed:    r.Seed,
	}
	if card.Seed == 0 {
		card.Seed = 1
	}
	card.Register = blendRegisters(donors, w)
	card.Register.Emoji = r.Emoji
	card.Register.Parasites = r.Parasites
	if len(r.Emoji) == 0 {
		// Эмодзи не смешиваются: набор — это выбор человека, а не число.
		// Не названы в рецепте — значит житель их не ставит.
		card.Register.EmojiRate = 0
	}
	card.Latency = blendLatency(donors, w)
	card.Rhythm = blendRhythm(donors, w)
	card.Vocab, card.VocabRate = blendVocab(donors, w)
	card.Errors = blendErrors(donors, w)
	card.Triggers = r.Triggers
	card.Dice = blendDice(donors, w)
	if r.Dice != nil {
		card.Dice = *r.Dice
	}
	// Relations и Samples у жителя пусты по построению: отношений он ещё ни с
	// кем не завёл, а дословные фразы донора — это и есть та утечка, ради
	// которой композит заводился.

	if err := card.Validate(); err != nil {
		return narod.Card{}, err
	}
	return card, nil
}

func normalizeWeights(ws []float64) []float64 {
	var sum float64
	for _, x := range ws {
		if x < 0 {
			x = 0
		}
		sum += x
	}
	out := make([]float64, len(ws))
	if sum == 0 {
		for i := range out {
			out[i] = 1 / float64(len(ws))
		}
		return out
	}
	for i, x := range ws {
		out[i] = x / sum
	}
	return out
}

func blendRegisters(donors []narod.Card, w []float64) narod.Register {
	var r narod.Register
	for i, d := range donors {
		k := w[i]
		r.Runes = addDist(r.Runes, d.Register.Runes, k)
		r.SentWords = addDist(r.SentWords, d.Register.SentWords, k)
		r.SentWordSD += k * d.Register.SentWordSD
		r.ShortSents += k * d.Register.ShortSents
		r.LongSents += k * d.Register.LongSents
		r.SmileyRate += k * d.Register.SmileyRate
		r.EmojiRate += k * d.Register.EmojiRate
		r.AllLower += k * d.Register.AllLower
		r.StartsLower += k * d.Register.StartsLower
		r.NoFinalPunct += k * d.Register.NoFinalPunct
		r.YoRate += k * d.Register.YoRate
		r.AddressPrefix += k * d.Register.AddressPrefix
		r.Punct = addRates(r.Punct, d.Register.Punct, k)
		r.ParenRuns = addRates(r.ParenRuns, d.Register.ParenRuns, k)
	}
	r.Smileys = blendCounts(donors, w, func(c narod.Card) []narod.Count { return c.Register.Smileys }, 8)
	r.Openings = blendCounts(donors, w, func(c narod.Card) []narod.Count { return c.Register.Openings }, 6)
	return r
}

func addDist(acc, d narod.Dist, k float64) narod.Dist {
	return narod.Dist{
		P10:    acc.P10 + int(float64(d.P10)*k+0.5),
		Median: acc.Median + int(float64(d.Median)*k+0.5),
		P90:    acc.P90 + int(float64(d.P90)*k+0.5),
		Max:    acc.Max + int(float64(d.Max)*k+0.5),
	}
}

func addRates(acc, m map[string]float64, k float64) map[string]float64 {
	if len(m) == 0 {
		return acc
	}
	if acc == nil {
		acc = map[string]float64{}
	}
	for key, v := range m {
		acc[key] = round2(acc[key] + v*k)
	}
	return acc
}

func blendCounts(donors []narod.Card, w []float64, get func(narod.Card) []narod.Count, top int) []narod.Count {
	shares := map[string]float64{}
	for i, d := range donors {
		for _, c := range get(d) {
			shares[c.Text] += w[i] * c.Share
		}
	}
	out := make([]narod.Count, 0, len(shares))
	for text, share := range shares {
		out = append(out, narod.Count{Text: text, Share: round2(share)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Share != out[j].Share {
			return out[i].Share > out[j].Share
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > top {
		out = out[:top]
	}
	return out
}

func blendLatency(donors []narod.Card, w []float64) narod.LatencyDist {
	var l narod.LatencyDist
	for i, d := range donors {
		l.ToThreadSec = addDist(l.ToThreadSec, d.Latency.ToThreadSec, w[i])
		l.ToReplySec = addDist(l.ToReplySec, d.Latency.ToReplySec, w[i])
	}
	return l
}

// blendRhythm смешивает суточный профиль. Часы — доли, а не абсолютные числа:
// у донора с сотней тысяч реплик они иначе задавили бы остальных целиком.
func blendRhythm(donors []narod.Card, w []float64) narod.Rhythm {
	out := narod.Rhythm{TZ: donors[0].Rhythm.TZ}
	var hours [24]float64
	var days [7]float64
	for i, d := range donors {
		hs, ds := shareOf(d.Rhythm.Hours[:]), shareOf(d.Rhythm.Weekdays[:])
		for h := range hours {
			hours[h] += w[i] * hs[h]
		}
		for k := range days {
			days[k] += w[i] * ds[k]
		}
	}
	// Обратно в целые: карточку читает человек, и доли с четырьмя знаками в
	// ней ничего не сообщают.
	for h, v := range hours {
		out.Hours[h] = int(v*1000 + 0.5)
	}
	for k, v := range days {
		out.Weekdays[k] = int(v*1000 + 0.5)
	}
	return out
}

func shareOf(xs []int) []float64 {
	var sum int
	for _, x := range xs {
		sum += x
	}
	out := make([]float64, len(xs))
	if sum == 0 {
		return out
	}
	for i, x := range xs {
		out[i] = float64(x) / float64(sum)
	}
	return out
}

// blendVocab оставляет только те слова, которые есть У ВСЕХ доноров.
//
// Пересечение, а не объединение: характерное слово одного человека — это его
// подпись (в архиве по ней и ищут альт-анкеты), а слово, которое употребляют
// оба донора, подписью быть перестаёт. Цена честная: у непохожих доноров
// словарь жителя выходит почти пустым — и это правильнее, чем склеить его из
// двух чужих словарей.
func blendVocab(donors []narod.Card, w []float64) ([]narod.Word, float64) {
	if len(donors) == 1 {
		// Единственный донор — пересекать не с чем, и словарь пришлось бы взять
		// целиком. Не берём вовсе: composeVerify об этом скажет отдельно.
		return nil, 0
	}
	weight := map[string]float64{}
	seen := map[string]int{}
	for i, d := range donors {
		for _, v := range d.Vocab {
			weight[v.Word] += w[i] * v.TFIDF
			seen[v.Word]++
		}
	}
	var out []narod.Word
	for word, n := range seen {
		if n == len(donors) {
			out = append(out, narod.Word{Word: word, TFIDF: round2(weight[word])})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TFIDF != out[j].TFIDF {
			return out[i].TFIDF > out[j].TFIDF
		}
		return out[i].Word < out[j].Word
	})

	var rate float64
	for i, d := range donors {
		rate += w[i] * d.VocabRate
	}
	if len(out) == 0 {
		return nil, 0
	}
	return out, round2(rate)
}

// blendErrors смешивает ошибки. Классовые складываются по весам, а ЛИЧНАЯ
// словоформа переносится, только если её делает не один донор: уникальная
// описка — это подпись человека, и она узнаётся вернее словаря.
func blendErrors(donors []narod.Card, w []float64) []narod.ErrorPattern {
	classRate := map[string]float64{}
	variantRate := map[string]float64{}
	variantSeen := map[string]int{}
	variantPair := map[string][2]string{}

	for i, d := range donors {
		for _, e := range d.Errors {
			if e.ID != narod.VariantErrorID {
				classRate[e.ID] += w[i] * e.Rate
				continue
			}
			key := e.Norm + "→" + e.Variant
			variantRate[key] += w[i] * e.Rate
			variantSeen[key]++
			variantPair[key] = [2]string{e.Norm, e.Variant}
		}
	}

	var out []narod.ErrorPattern
	for id, rate := range classRate {
		out = append(out, narod.ErrorPattern{ID: id, Rate: round2(rate)})
	}
	for key, n := range variantSeen {
		if n < minDonorsForVariant {
			continue
		}
		p := variantPair[key]
		out = append(out, narod.ErrorPattern{
			ID: narod.VariantErrorID, Rate: round2(variantRate[key]), Norm: p[0], Variant: p[1],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate != out[j].Rate {
			return out[i].Rate > out[j].Rate
		}
		return out[i].ID+out[i].Variant < out[j].ID+out[j].Variant
	})
	return out
}

func blendDice(donors []narod.Card, w []float64) narod.DiceParams {
	var d narod.DiceParams
	var thread, day float64
	for i, c := range donors {
		d.ComeToNote += w[i] * c.Dice.ComeToNote
		d.ReplyMention += w[i] * c.Dice.ReplyMention
		d.ReplyOther += w[i] * c.Dice.ReplyOther
		thread += w[i] * float64(c.Dice.MaxPerThread)
		day += w[i] * float64(c.Dice.MaxPerDay)
	}
	d.ComeToNote, d.ReplyMention, d.ReplyOther = round2(d.ComeToNote), round2(d.ReplyMention), round2(d.ReplyOther)
	d.MaxPerThread, d.MaxPerDay = int(thread+0.5), int(day+0.5)
	return d
}

// composeVerify — сторож близости к донорам. Возвращает замечания словами:
// решает по ним человек, потому что «слишком похож» — это не порог, а суждение.
func composeVerify(card narod.Card, donors []narod.Card, weights []float64) []string {
	var out []string
	if len(donors) < 2 {
		out = append(out, "донор один: манера жителя — это манера живого человека целиком, "+
			"смешивать не с чем (для калибровки годится, для площадки нет)")
	}
	w := normalizeWeights(weights)
	for i, k := range w {
		if k >= lopsidedWeight {
			out = append(out, fmt.Sprintf("донор %s весит %.0f%% — это он и есть, только под другим именем",
				donors[i].ID, k*100))
		}
	}
	if len(card.Samples) > 0 {
		out = append(out, "в житель уехали дословные образцы донора")
	}
	// Слово, которого нет у остальных доноров, — подпись одного человека.
	for _, v := range card.Vocab {
		var seen int
		for _, d := range donors {
			for _, dv := range d.Vocab {
				if dv.Word == v.Word {
					seen++
					break
				}
			}
		}
		if seen < len(donors) {
			out = append(out, fmt.Sprintf("слово %q есть не у всех доноров — это подпись одного из них", v.Word))
		}
	}
	for _, e := range card.Errors {
		if e.ID != narod.VariantErrorID {
			continue
		}
		var seen int
		for _, d := range donors {
			for _, de := range d.Errors {
				if de.ID == narod.VariantErrorID && de.Variant == e.Variant {
					seen++
					break
				}
			}
		}
		if seen < minDonorsForVariant {
			out = append(out, fmt.Sprintf("описка %q встречается у одного донора — это его подпись", e.Variant))
		}
	}
	if strings.TrimSpace(card.Persona.Nick) == "" {
		out = append(out, "у жителя нет ника")
	}
	for _, d := range donors {
		if strings.EqualFold(d.Persona.Nick, card.Persona.Nick) {
			out = append(out, fmt.Sprintf("ник жителя совпадает с ником донора %s", d.ID))
		}
	}
	return out
}

// narodCompose собирает жителя по рецепту.
func narodCompose(_ context.Context, dir, recipePath string, verifyOnly bool) error {
	r, err := loadRecipe(recipePath)
	if err != nil {
		return err
	}
	donors := make([]narod.Card, 0, len(r.Donors))
	for _, id := range r.Donors {
		c, err := narod.LoadCard(filepath.Join(dir, id+narod.CardExt))
		if err != nil {
			return fmt.Errorf("донор %s: %w", id, err)
		}
		donors = append(donors, c)
	}

	card, err := composeCard(r, donors, time.Now())
	if err != nil {
		return err
	}
	notes := composeVerify(card, donors, r.Weights)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "внимание:", n)
	}
	if verifyOnly {
		if len(notes) == 0 {
			fmt.Fprintln(os.Stderr, "замечаний нет")
		}
		return narod.WriteCardBrief(os.Stdout, card)
	}

	path := filepath.Join(dir, card.ID+narod.CardExt)
	if err := writeCardFile(path, card); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "житель %s → %s\n", card.ID, path)
	return narod.WriteCardBrief(os.Stdout, card)
}
