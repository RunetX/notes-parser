package archive

// Выдача карты: детерминированный разрез корпуса на образцы и эталонную полосу,
// мелкие утилиты и рендер в текст. Измерения — в voice_shape.go.

import (
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"sort"
	"strings"
)

// --- разрез корпуса ---

// voiceSplit детерминированно делит корпус на образцы для промпта и held-out
// полосу. Ключ текста — FNV(seed, id): один и тот же seed на той же БД даёт ту же
// выборку. Образцы берутся ПОСЛОЙНО по длине (корпус режется на samples корзин по
// рунам, из каждой — текст с минимальным ключом): иначе промпт уезжает в один
// регистр и «манера» получается не та.
//
// Пересечения образцов и полосы НЕТ по построению: полоса меряет, как атрибутор
// узнаёт тексты, КОТОРЫХ МОДЕЛЬ НЕ ВИДЕЛА.
func voiceSplit(texts []voiceText, samples, band int, seed int64) (sample, held []voiceText) {
	if len(texts) == 0 {
		return nil, nil
	}
	key := func(t voiceText) uint64 {
		h := fnv.New64a()
		fmt.Fprintf(h, "%d:%d", seed, t.id)
		return h.Sum64()
	}
	byLen := append([]voiceText{}, texts...)
	sort.Slice(byLen, func(i, j int) bool {
		li, lj := len([]rune(byLen[i].text)), len([]rune(byLen[j].text))
		if li != lj {
			return li < lj
		}
		return byLen[i].id < byLen[j].id
	})

	chosen := map[int64]bool{}
	for i := 0; i < samples && i < len(byLen); i++ {
		lo, hi := i*len(byLen)/samples, (i+1)*len(byLen)/samples
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(byLen) {
			hi = len(byLen)
		}
		best, bestKey := -1, uint64(0)
		for k := lo; k < hi; k++ {
			if chosen[byLen[k].id] {
				continue
			}
			if kk := key(byLen[k]); best < 0 || kk < bestKey {
				best, bestKey = k, kk
			}
		}
		if best >= 0 {
			chosen[byLen[best].id] = true
			sample = append(sample, byLen[best])
		}
	}

	rest := make([]voiceText, 0, len(texts))
	for _, t := range texts {
		if !chosen[t.id] {
			rest = append(rest, t)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return key(rest[i]) < key(rest[j]) })
	if band > 0 && band < len(rest) {
		rest = rest[:band]
	}
	return sample, rest
}

// --- утилиты ---

func distOf(xs []int) Dist {
	d := Dist{N: len(xs)}
	if len(xs) == 0 {
		return d
	}
	f := make([]float64, len(xs))
	sum := 0
	for i, x := range xs {
		f[i] = float64(x)
		sum += x
	}
	sort.Float64s(f)
	d.Mean = round2(float64(sum) / float64(len(xs)))
	d.P10, d.P25 = int(quantile(f, 0.10)+0.5), int(quantile(f, 0.25)+0.5)
	d.Median = int(quantile(f, 0.50) + 0.5)
	d.P75, d.P90 = int(quantile(f, 0.75)+0.5), int(quantile(f, 0.90)+0.5)
	d.Max = int(f[len(f)-1])
	return d
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// --- рендер карты ---

// WriteVoiceBrief печатает карту компактным русским блоком. Один и тот же текст
// идёт и человеку в отчёт, и в промпт модели: если он нечитаем человеком, то и
// модели он не помогает.
func WriteVoiceBrief(w io.Writer, c *VoiceCard, kind string) error {
	sh := c.Notes
	if kind == "comments" {
		sh = c.Comments
	}
	name := c.Label
	if name == "" && len(c.Accounts) > 0 {
		name = c.Accounts[0].Name
	}
	fmt.Fprintf(w, "=== КАРТА ПИСЬМА %s «%s» (жанр замера: %s) ===\n", c.Identity, name, sh.Kind)
	if sh.Texts == 0 {
		fmt.Fprintf(w, "текстов этого жанра нет\n")
		return nil
	}
	fmt.Fprintf(w, "Замерено текстов: %d из %d", sh.Texts, sh.TotalHave)
	if sh.From != "" {
		fmt.Fprintf(w, " (%s — %s)", sh.From, sh.To)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Длина: медиана %d рун (p10 %d, p90 %d, макс %d) — цель %d–%d.\n",
		sh.Runes.Median, sh.Runes.P10, sh.Runes.P90, sh.Runes.Max, sh.Runes.P25, sh.Runes.P75)
	fmt.Fprintf(w, "Предложений: медиана %d; слов в предложении %.1f; длина слова %.1f; абзацев медиана %d.\n",
		sh.Sentences.Median, sh.WordsPerSentence, sh.WordRunes, sh.Paragraphs.Median)
	// РИТМ — главная цель. Ровный поток предложений одной длины читается как
	// машинный текст даже при верном среднем, поэтому здесь и разброс, и края.
	fmt.Fprintf(w, "РИТМ ФРАЗЫ (важнее среднего): длина предложения p10 %d, медиана %d, p90 %d, макс %d; "+
		"разброс sd %.1f.\n", sh.SentWords.P10, sh.SentWords.Median, sh.SentWords.P90, sh.SentWords.Max, sh.SentWordSD)
	fmt.Fprintf(w, "  рубленых (≤%d слов) %s всех предложений, длинных (≥%d слов) %s — мешай их, "+
		"ровный поток одинаковых фраз это провал.\n",
		shortSentWords, pct(sh.ShortSents), longSentWords, pct(sh.LongSents))
	if len(sh.Person) > 0 {
		fmt.Fprintf(w, "Кому говорит, местоимений на 100 слов: я %.1f, ты %.1f, мы %.1f, вы %.1f.\n",
			sh.Person["я"], sh.Person["ты"], sh.Person["мы"], sh.Person["вы"])
	}
	fmt.Fprintf(w, "Кончает вопросом %s текстов; кавычки или диалог в %s; цифры в %s.\n",
		pct(sh.EndsQuestion), pct(sh.HasQuote), pct(sh.HasDigits))
	fmt.Fprintf(w, "Пунктуация на 1000 рун: %s\n", fmtRates(sh.Punct, 8))
	fmt.Fprintf(w, "Скобки-улыбки: %s; грустных «((» в %s текстов.\n",
		fmtParens(sh.ParenRuns), pct(sh.SadParens))
	fmt.Fprintf(w, "Регистр: с маленькой начинает %s текстов; целиком строчными %s; КАПС %s слов.\n",
		pct(sh.StartsLower), pct(sh.AllLower), pct(sh.AllCapsWords))
	fmt.Fprintf(w, "Точку в конце не ставит в %s текстов. «ё» пишет в %s текстов.\n",
		pct(sh.NoFinalPunct), pct(sh.YoRate))
	if len(sh.Markup) > 0 {
		fmt.Fprintf(w, "Разметка сайта: %s\n", fmtRates(sh.Markup, 4))
	}
	// При 0.00 на текст показываем долю текстов: иначе строка «0.00 на текст
	// (чаще :::santaclaus:::)» читается как противоречие.
	switch {
	case len(sh.TopSmileys) == 0:
		fmt.Fprintln(w, "Смайлы сайта: не ставит.")
	case sh.SmileyRate < 0.01:
		fmt.Fprintf(w, "Смайлы сайта: почти не ставит (%s), из редких — %s.\n",
			pct(smileyTextShare(sh)), fmtSmileys(sh.TopSmileys, 3))
	default:
		fmt.Fprintf(w, "Смайлы сайта: %.2f на текст (чаще %s).\n",
			sh.SmileyRate, fmtSmileys(sh.TopSmileys, 5))
	}
	if sh.EmojiRate == 0 {
		fmt.Fprintln(w, "Эмодзи: не использует — запрещены.")
	} else {
		fmt.Fprintf(w, "Эмодзи: в %s текстов.\n", pct(sh.EmojiRate))
	}
	if sh.Kind == "comments" {
		fmt.Fprintf(w, "Обращение «Ник, …» в начале реплики: %s.\n", pct(sh.AddressPrefix))
	}
	if len(sh.TopOpenings) > 0 {
		ws := make([]string, 0, 6)
		for i, o := range sh.TopOpenings {
			if i == 6 {
				break
			}
			ws = append(ws, o.Text)
		}
		fmt.Fprintf(w, "Чем начинает: %s\n", strings.Join(ws, ", "))
	}
	if len(c.Vocab) > 0 {
		ws := make([]string, 0, len(c.Vocab))
		for _, v := range c.Vocab {
			ws = append(ws, v.Word)
		}
		// Список подаётся НЕ как цель: набивка характерных слов поднимает
		// лексический косинус, обрушивая стилевой (живые замеры: styleZ < 0 при
		// lexZ > 0). Поэтому рядом идёт норма самого автора и потолок.
		fmt.Fprintf(w, "Привычные слова автора (НЕ список обязательных — берётся только то, "+
			"что ложится само): %s\n", strings.Join(ws, ", "))
		fmt.Fprintf(w, "Сам автор употребляет их %.2f на 100 слов — держись этой нормы, "+
			"насыпать их больше значит выдать подделку.\n", c.VocabRate)
	} else if c.VocabNote != "" {
		fmt.Fprintf(w, "Характерные слова: %s\n", c.VocabNote)
	}
	return nil
}

// smileyTextShare — доля текстов хотя бы с одним смайлом (верхняя оценка: берём
// самый частый код).
func smileyTextShare(sh VoiceShape) float64 {
	if len(sh.TopSmileys) == 0 {
		return 0
	}
	return sh.TopSmileys[0].Share
}

func fmtSmileys(top []VoiceCount, limit int) string {
	codes := make([]string, 0, limit)
	for i, sm := range top {
		if i == limit {
			break
		}
		codes = append(codes, ":::"+sm.Text+":::")
	}
	return strings.Join(codes, " ")
}

func fmtRates(m map[string]float64, limit int) string {
	type kv struct {
		k string
		v float64
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		if v > 0 {
			all = append(all, kv{k, v})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	if len(all) > limit {
		all = all[:limit]
	}
	parts := make([]string, 0, len(all))
	for _, p := range all {
		parts = append(parts, fmt.Sprintf("%s %g", p.k, p.v))
	}
	return strings.Join(parts, " | ")
}

func fmtParens(m map[string]float64) string {
	order := []string{"1", "2", "3", "4+"}
	parts := make([]string, 0, len(order))
	for _, b := range order {
		if v, ok := m[b]; ok && v > 0 {
			parts = append(parts, fmt.Sprintf("«%s» в %s", strings.Repeat(")", runLen(b)), pct(v)))
		}
	}
	if len(parts) == 0 {
		return "не ставит"
	}
	return strings.Join(parts, ", ")
}

func runLen(bucket string) int {
	if bucket == "4+" {
		return 4
	}
	return int(bucket[0] - '0')
}

func pct(x float64) string { return fmt.Sprintf("%.0f%%", x*100) }
