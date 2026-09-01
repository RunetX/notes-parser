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
	if r := ForeignScript(text); r != "" {
		return "чужая письменность «" + r + "»: обломок генерации, на сайте это мусор"
	}
	return ""
}

// ForeignScript — первая буква не из кириллицы и не из латиницы ("" — чисто).
// Поймано живым прогоном 25.08.2026: в утренней заметке модель написала «улетел
// на край系 и прислал домой фотографии» — иероглиф посреди русской фразы.
// Проверка на «слово из двух алфавитов» его не видела: она про латиницу внутри
// кириллического слова, а тут третья письменность. Смотрим на БУКВЫ: цифры,
// знаки и эмодзи здесь законны.
func ForeignScript(text string) string {
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= 0x0400 && r <= 0x052F: // кириллица
		case r >= 0x00C0 && r <= 0x024F: // латиница с диакритикой: é, ü, ł
		default:
			return string(r)
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

// Приметы НЕСМЕШНОГО — то, во что модель сползает, когда шутки не нашлось.
// Живут здесь по той же причине, по которой здесь живут правила сайта: сползает
// так ЛЮБОЙ наш текст, который она пишет, а не какой-то один жанр, — и две
// копии этих выражений в амвоне и в утренней заметке разошлись бы молча.
var (
	// reJokeTag — метка собственной шутки. Смешному она не нужна, несмешное не
	// спасает, а выдаёт того, кто боится, что его не поймут.
	reJokeTag = regexp.MustCompile(`(?i)\((шутка|сарказм|ирония)\)|/сарказм|\b(lol|лол)\b|ахах|бугага`)
	// reAphorism — симметричная конструкция: «дело не в X, а в Y», «это уже не
	// A, а B», «тревожит не то, что…». Читается как остроумие, а смеха не даёт
	// — именно из-за них вышли пресными первые черновики прикольщика
	// (16.08.2026). Шаблоны узкие: требуется вся симметрия целиком, а не первые
	// два слова.
	reAphorism = regexp.MustCompile(`(?i)(дело не в [^,.!?]{1,40}, а |` +
		`это уже не [^,.!?]{1,40}, а |` +
		`не в том, что [^,.!?]{1,60}, а в том|` +
		`(тревожит|беспокоит|пугает|смущает|интересно) не то, что )`)
)

// JokeTag — найденная метка собственной шутки ("" — чисто).
func JokeTag(text string) string { return reJokeTag.FindString(text) }

// Aphorism — найденная афористичная конструкция ("" — чисто).
func Aphorism(text string) string { return reAphorism.FindString(text) }

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

// Fingerprint — отпечаток текста для сверки «это та же реплика».
//
// Нужен ровно одному: узнать в реплике НГС СВОЮ копию, ушедшую туда из
// Зазеркалья, — а побайтово она не вернётся никогда. Сайт схлопывает пробелы,
// делает nl2br, подменяет эмодзи картинками и режет длинный текст; разбор
// возвращает нам уже не то, что мы посылали. Сравнивать поэтому надо то, что
// сайт не трогает: БУКВЫ И ЦИФРЫ.
//
// Отсюда и грубость: выброшено всё, кроме букв и цифр, остальное приведено к
// нижнему регистру. Ложное совпадение стоит одной пропущенной реплики, а
// ложное расхождение — дубля в чужом треде, который потом не убрать; и главное,
// у сверки поверх этого стоит собственный предохранитель — одну ушедшую строку
// нельзя опознать дважды.
func Fingerprint(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
		case unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SameText — один ли это текст с точностью до отпечатка. Пустое не совпадает ни
// с чем: у реплики, из которой сайт не оставил ни буквы, сверять нечего.
//
// Хвост допускается: сайт режет длинные тексты, а из ленты заметка приходит
// началом. Сравнивается поэтому ОБЩЕЕ НАЧАЛО, но не короче minPrefix — иначе
// «да)» совпало бы с любой репликой, начинающейся на «да».
func SameText(a, b string) bool {
	fa, fb := Fingerprint(a), Fingerprint(b)
	if fa == "" || fb == "" {
		return false
	}
	if fa == fb {
		return true
	}
	n := min(len(fa), len(fb))
	if n < minPrefix {
		return false
	}
	return fa[:n] == fb[:n]
}

// minPrefix — сколько букв общего начала считаем достаточным. Сотня: столько не
// совпадает у двух разных реплик случайно, и в неё укладывается срез ленты
// (короткие заметки она отдаёт целиком, а до сотни букв не дотягивает разве что
// реплика в одно слово — такую сверка честно не опознает).
const minPrefix = 100
