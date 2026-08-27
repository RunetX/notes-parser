package narod

// Рендер карточки словами.
//
// Один и тот же текст идёт человеку в отчёт и модели в промпт — приём взят у
// archive.WriteVoiceBrief и оплачен там же: бриф, нечитаемый человеком, не
// помогает и модели, а два разных описания одной карточки разъезжаются молча.
//
// Числа подаются как ЦЕЛИ, а не как приказ. Разница видна на словаре: список
// характерных слов, поданный требованием, модель набивает — лексическое сходство
// растёт, стилевое падает, и подделка становится заметнее оригинала.

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteCardBrief печатает карточку компактным русским блоком.
//
// Дословных образцов здесь нет намеренно, даже когда они в карточке есть: бриф
// уходит в промпт, а у композита образцов нет вовсе — показывать их у слепка
// значило бы, что калибровка и живая игра идут по разным описаниям.
func WriteCardBrief(w io.Writer, c Card) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "=== КТО ТЫ: %s ===\n", c.Persona.Nick)
	writeBio(b, c.Persona)
	writeRegister(b, c.Register)
	writeVocab(b, c)
	writeErrors(b, c.Errors)
	writeTriggers(b, c.Triggers)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeBio(w io.Writer, p Bio) {
	parts := make([]string, 0, 4)
	if p.Age > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", p.Age, yearsWord(p.Age)))
	}
	switch p.Gender {
	case "male":
		parts = append(parts, "мужчина")
	case "female":
		parts = append(parts, "женщина")
	}
	if p.City != "" {
		parts = append(parts, p.City)
	}
	if p.Job != "" {
		parts = append(parts, p.Job)
	}
	if p.Family != "" {
		parts = append(parts, p.Family)
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "%s.\n", strings.Join(parts, ", "))
	}
	if len(p.Facts) > 0 {
		// Факты — не украшение: противоречить им нельзя, и сказанное проверяется
		// механически. Поэтому они названы отдельной строкой, а не растворены в
		// описании.
		fmt.Fprintf(w, "Про себя знаешь твёрдо (противоречить этому нельзя): %s\n",
			strings.Join(p.Facts, "; "))
	}
}

func writeRegister(w io.Writer, r Register) {
	fmt.Fprintf(w, "\nКАК ТЫ ПИШЕШЬ\n")
	fmt.Fprintf(w, "Длина реплики: обычно %d %s (короткие %d, длинные %d, дальше почти не пишешь).\n",
		r.Runes.Median, plural(r.Runes.Median, "знак", "знака", "знаков"), r.Runes.P10, r.Runes.P90)
	// Ритм фразы — главная цель. Ровный поток предложений одной длины читается
	// как машинный текст даже при верной средней длине, поэтому здесь и разброс,
	// и обе доли краёв.
	fmt.Fprintf(w, "РИТМ ФРАЗЫ (важнее длины): предложения от %d до %d слов, обычно %d, разброс sd %.1f.\n",
		r.SentWords.P10, r.SentWords.P90, r.SentWords.Median, r.SentWordSD)
	fmt.Fprintf(w, "  рубленых %s, длинных %s — мешай их: ровный поток одинаковых фраз выдаёт машину.\n",
		pct(r.ShortSents), pct(r.LongSents))
	if s := fmtRates(r.Punct, 8); s != "" {
		fmt.Fprintf(w, "Знаки на 1000 рун: %s\n", s)
	}
	if s := fmtParens(r.ParenRuns); s != "" {
		fmt.Fprintf(w, "Скобки-улыбки: %s.\n", s)
	}
	fmt.Fprintf(w, "Регистр: с маленькой начинаешь %s реплик, целиком строчными %s; "+
		"точку в конце не ставишь в %s.\n", pct(r.StartsLower), pct(r.AllLower), pct(r.NoFinalPunct))
	if r.YoRate > 0 {
		fmt.Fprintf(w, "Букву «ё» пишешь.\n")
	} else {
		fmt.Fprintf(w, "Букву «ё» не пишешь никогда.\n")
	}
	switch {
	case len(r.Smileys) == 0:
		fmt.Fprintf(w, "Смайлы сайта: не ставишь.\n")
	case r.SmileyRate < 0.01:
		fmt.Fprintf(w, "Смайлы сайта: почти не ставишь, из редких — %s.\n", fmtCounts(r.Smileys, 3))
	default:
		fmt.Fprintf(w, "Смайлы сайта: %.2f на реплику, чаще %s.\n", r.SmileyRate, fmtCounts(r.Smileys, 5))
	}
	switch {
	case r.EmojiRate == 0:
		fmt.Fprintf(w, "Эмодзи: не ставишь — запрещены.\n")
	case len(r.Emoji) == 0:
		// Замер знает ЧАСТОТУ эмодзи, но не набор: какие именно — решает
		// владелец, когда собирает жителя. Пока набор не назван, называть его
		// пустым списком нельзя — это читается как «ставишь вот эти: ».
		fmt.Fprintf(w, "Эмодзи: ставишь изредка, в %s реплик.\n", pct(r.EmojiRate))
	default:
		fmt.Fprintf(w, "Эмодзи: в %s реплик, из них %s.\n", pct(r.EmojiRate), strings.Join(r.Emoji, " "))
	}
	fmt.Fprintf(w, "Обращение «Ник, …» в начале: %s реплик.\n", pct(r.AddressPrefix))
	if len(r.Openings) > 0 {
		fmt.Fprintf(w, "Чем обычно начинаешь: %s\n", fmtCounts(r.Openings, 6))
	}
	if len(r.Parasites) > 0 {
		fmt.Fprintf(w, "Словечки-приклейки: %s\n", strings.Join(r.Parasites, ", "))
	}
}

func writeVocab(w io.Writer, c Card) {
	if len(c.Vocab) == 0 {
		return
	}
	ws := make([]string, 0, len(c.Vocab))
	for _, v := range c.Vocab {
		ws = append(ws, v.Word)
	}
	fmt.Fprintf(w, "Привычные слова (НЕ список обязательных — только то, что ложится само): %s\n",
		strings.Join(ws, ", "))
	if c.VocabRate > 0 {
		fmt.Fprintf(w, "Сам ты употребляешь их %.2f на 100 слов — держись этой нормы: "+
			"насыпать их больше значит выдать подделку.\n", c.VocabRate)
	}
}

// writeErrors называет ошибки, но НЕ просит их делать: вносит их постпроцессор
// с замеренной частотой. Модель, которой велено ошибаться, ошибается карикатурно
// и каждый раз по-новому, а у человека ошибка одна и та же годами.
func writeErrors(w io.Writer, errs []ErrorPattern) {
	if len(errs) == 0 {
		return
	}
	fmt.Fprintf(w, "Пишешь без оглядки на грамотность — опечатки в твой текст внесут за тебя, "+
		"сам их не изображай (%d %s).\n", len(errs), plural(len(errs), "вид", "вида", "видов"))
}

func writeTriggers(w io.Writer, ts []Topic) {
	if len(ts) == 0 {
		return
	}
	var likes, bores []string
	for _, t := range ts {
		switch {
		case t.Weight > 0:
			likes = append(likes, t.Name)
		case t.Weight < 0:
			bores = append(bores, t.Name)
		}
	}
	fmt.Fprintf(w, "\nЧТО ТЕБЯ ЦЕПЛЯЕТ\n")
	if len(likes) > 0 {
		fmt.Fprintf(w, "Цепляет: %s\n", strings.Join(likes, ", "))
	}
	if len(bores) > 0 {
		// Равнодушие — рабочий исход, а не отсутствие мнения: молчание в этом
		// мире обязательно и часто.
		fmt.Fprintf(w, "Оставляет равнодушным: %s — про такое ты обычно просто молчишь.\n",
			strings.Join(bores, ", "))
	}
}

// --- мелочи вывода ---

func pct(x float64) string { return fmt.Sprintf("%.0f%%", x*100) }

// plural — русское согласование числительного. Бриф читает человек, и «61 рун»
// в нём выдаёт машину ровно так же, как ровный поток одинаковых фраз в реплике.
func plural(n int, one, few, many string) string {
	switch t, h := n%10, n%100; {
	case h >= 11 && h <= 14:
		return many
	case t == 1:
		return one
	case t >= 2 && t <= 4:
		return few
	default:
		return many
	}
}

func yearsWord(n int) string { return plural(n, "год", "года", "лет") }

// fmtRates печатает частоты по убыванию, оставляя top самых заметных. Порядок
// по значению, а не по ключу: читателю нужны привычки автора, а не алфавит.
func fmtRates(m map[string]float64, top int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if top > 0 && len(keys) > top {
		keys = keys[:top]
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %.1f", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// fmtParens — скобочная подпись словами. «)» и «))» это разные люди, поэтому
// длины перечисляются порознь, а не сводятся к средней.
func fmtParens(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}
	order := []string{"1", "2", "3", "4+"}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if v, ok := m[k]; ok && v > 0 {
			parts = append(parts, fmt.Sprintf("%s в %s реплик", strings.Repeat(")", parenLen(k)), pct(v)))
		}
	}
	return strings.Join(parts, ", ")
}

func parenLen(k string) int {
	switch k {
	case "2":
		return 2
	case "3":
		return 3
	case "4+":
		return 4
	default:
		return 1
	}
}

func fmtCounts(cs []Count, top int) string {
	parts := make([]string, 0, top)
	for i, c := range cs {
		if i == top {
			break
		}
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, ", ")
}
