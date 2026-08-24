// Package sitetext — приведение НАШЕГО текста к виду, в котором его не испортит
// love.ngs.ru, и приметы генерации, которые до сайта доезжать не должны.
//
// Пакет заведён 24.08.2026, когда за амвоном (комментарий под чужой заметкой)
// появилась утренняя заметка (`morning`): правила у них одни и те же — их
// задаёт САЙТ, а не жанр реплики, — и второго способа их выразить заводиться не
// должно. Разойдясь, две копии разойдутся молча: сайт не отвечает отказом на
// длинное тире, он просто печатает его в нашем тексте.
//
// Правила сняты замером 15.08.2026 по 61 177 живым комментариям:
//   - переносы строк работают (сайт делает nl2br), пустая строка между абзацами
//     держится;
//   - пробелы схлопываются, отступ живёт ТОЛЬКО через NBSP;
//   - BB-коды не работают с 02.06.2014 — печатаются буквально;
//   - HTML сайт экранирует сам, так что тег вылезет текстом;
//   - длинное тире у 0,52 % комментариев, ёлочки у 0,65 % — так пишет
//     типографика, а не люди, и они выдают машину.
//
// Разделение труда: механическое и однозначное чиним подстановкой (Normalize),
// спорное — бракуем и переспрашиваем (детекторы ниже возвращают ПРИЧИНУ, а не
// код ошибки: она едет хвостом в промпт переспроса). Все функции чистые.
package sitetext

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NBSP — неразрывный пробел. Единственный способ показать на площадке отступ:
// обычные пробелы схлопываются (контейнер не pre-wrap), и так сделаны все 44
// живых комментария с видимым отступом. Подставляет его ИНСТРУМЕНТ, а не
// модель: узнав про этот трюк, она начнёт вставлять NBSP в середину фраз, и
// текст станет неразрывным полотном.
const NBSP = ' '

// maxBlankLines — сколько пустых строк подряд оставляем. Абзац отбивается
// одной; больше двух — уже не форма, а дыра в тексте.
const maxBlankLines = 2

// tabWidth — во что разворачиваем табуляцию перед подстановкой NBSP.
const tabWidth = 4

var (
	reBBCode   = regexp.MustCompile(`(?i)\[/?(b|i|u|s|url|quote|img|color|size|code)\b[^\]]*\]`)
	reHTMLTag  = regexp.MustCompile(`(?i)<\s*/?\s*[a-z][a-z0-9]*(\s[^>\n]*)?>`)
	reThinking = regexp.MustCompile(`(?i)<\s*/?\s*thinking`)
	reWordSep  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	// reReasoning — размышление, протёкшее в сам текст. Тег <thinking> ловится
	// отдельно, но с включённым усилием (effort) модель роняет в видимое поле и
	// голые обрывки хода мысли: «Wait, no.», «Hmm», «Let me». Замечено на живых
	// черновиках 16.08.2026, два случая из трёх. Площадка русская, английских
	// вставок в комментариях практически нет, так что правило дешёвое.
	reReasoning = regexp.MustCompile(`(?i)\b(wait|hmm+|okay|let me|i should|hold on|actually)\b`)
	// reMixedWord — слово из букв двух алфавитов («uправила»): такое не пишут
	// ни люди, ни опечатка раскладки — это склейка при генерации.
	reMixedWord = regexp.MustCompile(`[\p{Cyrillic}]+[a-zA-Z]|[a-zA-Z]+[\p{Cyrillic}]`)
	// reBrace — фигурная скобка: в живом комментарии её не пишут, а в
	// разваленном ответе модели это обломок самого JSON (замер 16.08.2026:
	// реплика кончалась на «фильтр работает.}»).
	reBrace = regexp.MustCompile(`[{}]`)
)

// machineTells — приметы генерации, общие для любого нашего текста. Таблицей, а
// не цепочкой if-ов: порядок в списке и есть порядок проверки. %s — попавшийся
// кусок, он уезжает в переспрос.
var machineTells = []struct {
	re     *regexp.Regexp
	reason string
}{
	{reThinking, "служебный тег %s в тексте"},
	{reReasoning, "обрывок размышления «%s» в тексте: в ответ едет только сам ответ"},
	{reMixedWord, "слово из двух алфавитов («%s»): склейка при генерации"},
	{reBrace, "фигурная скобка «%s»: обломок JSON в тексте"},
	{reBBCode, "BB-код %s: на сайте он напечатается буквально"},
	{reHTMLTag, "HTML-тег %s: сайт его экранирует, и он вылезет текстом"},
}

// MachineTell — первая сработавшая примета генерации ("" — чисто).
func MachineTell(text string) string {
	for _, t := range machineTells {
		if m := t.re.FindString(text); m != "" {
			return fmt.Sprintf(t.reason, m)
		}
	}
	return ""
}

// typography — что чиним подстановкой.
var typography = strings.NewReplacer(
	"—", "-", "–", "-", "―", "-", "‒", "-", "−", "-",
	"«", `"`, "»", `"`, "„", `"`, "“", `"`, "”", `"`, "‟", `"`,
	"‘", "'", "’", "'", "‚", "'", "‛", "'",
	"…", "...",
)

// Normalize приводит текст к тому виду, в котором он поедет на сайт.
// Идемпотентна: второй прогон ничего не меняет и NBSP не плодит — иначе
// повторная генерация после брака удвоила бы отступы.
func Normalize(text string) string {
	text = typography.Replace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "\t", strings.Repeat(" ", tabWidth))

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t"+string(NBSP))
		if line == "" {
			blanks++
			if blanks > maxBlankLines {
				continue
			}
			out = append(out, "")
			continue
		}
		blanks = 0
		out = append(out, spacesToNBSP(line))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// spacesToNBSP заменяет ведущие пробелы строки и любой пробег из двух и более
// на неразрывные. Одиночные пробелы между словами не трогаются никогда — это и
// делает функцию идемпотентной.
func spacesToNBSP(line string) string {
	var b strings.Builder
	b.Grow(len(line))
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		if runes[i] != ' ' {
			b.WriteRune(runes[i])
			continue
		}
		run := 1
		for i+run < len(runes) && runes[i+run] == ' ' {
			run++
		}
		// Ведущий отступ или пробег из двух и более: на сайте они схлопнутся.
		if b.Len() == 0 || run >= 2 {
			b.WriteString(strings.Repeat(string(NBSP), run))
		} else {
			b.WriteByte(' ')
		}
		i += run - 1
	}
	return b.String()
}

// MarkdownHit — копия правила из archive/voice_gen.go: markdown на сайте
// печатается буквально, и это самый частый прокол генерации.
func MarkdownHit(text string) string {
	for _, m := range []string{"**", "##", "```", "~~"} {
		if strings.Contains(text, m) {
			return m
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			return "список дефисом"
		}
	}
	return ""
}

// TypographyHit — страховка от дырки в Normalize: если типографский знак дожил
// до валидации, значит подстановка его не поймала.
func TypographyHit(text string) string {
	for _, r := range []string{"—", "–", "―", "«", "»", "“", "”", "„"} {
		if strings.Contains(text, r) {
			return r
		}
	}
	return ""
}

// maxStrayLatin — до скольки букв латинский огрызок в русском тексте считается
// мусором генерации. Живые слова латиницей на площадке длиннее (WhatsApp,
// Instagram), а «pt», «ru», «th» — это хвост, отвалившийся от модели.
const maxStrayLatin = 3

var reLatinRun = regexp.MustCompile(`[a-zA-Z]+`)

// LatinFragment — латинский огрызок в русском тексте ("" — чисто). Родня
// reMixedWord, но ловит несклеенный случай: «Календарь справился бы быстрее.pt»
// (живой черновик 16.08.2026) — точка между ними, и проверка на смешанное слово
// молчит. Текст целиком на латинице (его у нас не бывает) не судится.
func LatinFragment(text string) string {
	if !strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
		return ""
	}
	for _, m := range reLatinRun.FindAllString(text, -1) {
		if utf8.RuneCountInString(m) <= maxStrayLatin {
			return m
		}
	}
	return ""
}

// HasEmoji — есть ли в тексте эмодзи. Диапазоны грубые намеренно: нужен сам
// факт, а не классификация.
func HasEmoji(text string) bool {
	for _, r := range text {
		switch {
		case r >= 0x1F000 && r <= 0x1FAFF,
			r >= 0x2600 && r <= 0x27BF,
			r >= 0x2B00 && r <= 0x2BFF,
			r == 0x2764, r == 0xFE0F, r == 0x203C, r == 0x2049:
			return true
		}
	}
	return false
}

// CountEmoji — сколько в тексте эмодзи. Модификаторы не считаются отдельными
// знаками: селектор начертания (U+FE0F), тон кожи и склейка ZWJ — это часть
// соседнего значка, а не второй значок рядом.
func CountEmoji(text string) int {
	n := 0
	for _, r := range text {
		switch {
		case r == 0xFE0F, r == 0x200D, r >= 0x1F3FB && r <= 0x1F3FF:
			// модификатор — часть предыдущего
		case r >= 0x1F000 && r <= 0x1FAFF,
			r >= 0x2600 && r <= 0x27BF,
			r >= 0x2B00 && r <= 0x2BFF,
			r == 0x2764, r == 0x203C, r == 0x2049:
			n++
		}
	}
	return n
}

// Words — текст словами в нижнем регистре: общая мера для сравнений («это уже
// было», «деталь названа словами автора», пересказ).
func Words(s string) []string {
	fields := reWordSep.Split(strings.ToLower(s), -1)
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
