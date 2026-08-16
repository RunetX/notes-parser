package pulpit

// Нормализация и проверка того, что вернула модель. Обе функции чистые: текст
// уходит на сайт необратимо, и единственный способ проверить эти правила —
// таблица случаев, а не боевая реплика.
//
// Разделение труда такое: механическое и однозначное чиним подстановкой
// (normalize), спорное — бракуем и переспрашиваем (validate). Лишний круг к
// модели стоит 10–30 секунд, то есть ровно той валюты, ради которой всё
// затевалось, поэтому тире и кавычки не повод для переспроса. Валидатор их всё
// равно проверяет — как страховку от дырки в нормализации, а не как рабочую
// дорогу.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"lovegw/internal/love"
)

// nbsp — неразрывный пробел. Единственный способ показать на площадке отступ:
// обычные пробелы схлопываются (контейнер не pre-wrap), и так сделаны все 44
// живых комментария с видимым отступом. Подставляет его ИНСТРУМЕНТ, а не
// модель: узнав про этот трюк, она начнёт вставлять NBSP в середину фраз, и
// текст станет неразрывным полотном.
const nbsp = ' '

// maxBlankLines — сколько пустых строк подряд оставляем. Абзац отбивается
// одной; больше двух — уже не форма, а дыра в реплике.
const maxBlankLines = 2

// Ширина и высота рисунка из знаков. Шрифт на площадке пропорциональный:
// широкая картинка развалится, поэтому годятся разделители и силуэты, а не
// блочная графика с выравниванием по колонкам.
const (
	maxArtWidth = 30
	maxArtLines = 5
)

// maxOverlapShare — доля общих пятёрок слов с заметкой, после которой это уже
// пересказ, а не отклик.
const maxOverlapShare = 0.2

// tabWidth — во что разворачиваем табуляцию перед подстановкой NBSP.
const tabWidth = 4

var (
	reBBCode   = regexp.MustCompile(`(?i)\[/?(b|i|u|s|url|quote|img|color|size|code)\b[^\]]*\]`)
	reHTMLTag  = regexp.MustCompile(`(?i)<\s*/?\s*[a-z][a-z0-9]*(\s[^>\n]*)?>`)
	reThinking = regexp.MustCompile(`(?i)<\s*/?\s*thinking`)
	reWordSep  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	// reJokeTag — метка собственной шутки. Смешному она не нужна, несмешное не
	// спасает, а в реплике выдаёт того, кто боится, что его не поймут. Правило
	// узкое намеренно: каждый переспрос стоит 10–30 секунд, то есть ровно той
	// валюты, ради которой амвон и спешит быть первым.
	reJokeTag = regexp.MustCompile(`(?i)\((шутка|сарказм|ирония)\)|/сарказм|\b(lol|лол)\b|ахах|бугага`)
	// reReasoning — размышление, протёкшее в сам текст. Тег <thinking> ловится
	// отдельно, но с включённым усилием (effort) модель роняет в поле text и
	// голые обрывки хода мысли: «Wait, no.», «Hmm», «Let me». Замечено на живых
	// черновиках 16.08.2026, два случая из трёх. Площадка русская, английских
	// вставок в комментариях практически нет, так что правило дешёвое.
	reReasoning = regexp.MustCompile(`(?i)\b(wait|hmm+|okay|let me|i should|hold on|actually)\b`)
	// reMixedWord — слово из букв двух алфавитов («uправила»): такое не пишут
	// ни люди, ни опечатка раскладки — это склейка при генерации.
	reMixedWord = regexp.MustCompile(`[\p{Cyrillic}]+[a-zA-Z]|[a-zA-Z]+[\p{Cyrillic}]`)
	// reBrace — фигурная скобка: в живом комментарии её не пишут, а в разваленном
	// ответе модели это обломок самого JSON (замер 16.08.2026: реплика кончалась
	// на «фильтр работает.}»).
	reBrace = regexp.MustCompile(`[{}]`)
)

// textTells — приметы генерации, которые ловятся регуляркой. Таблицей, а не
// цепочкой if-ов: правил стало больше, чем читается за раз, а порядок в списке
// и есть порядок проверки. %s — попавшийся кусок, он уезжает в переспрос.
var textTells = []struct {
	re     *regexp.Regexp
	reason string
}{
	{reThinking, "служебный тег %s в тексте"},
	{reReasoning, "обрывок размышления «%s» в тексте: в реплику едет только сама реплика"},
	{reMixedWord, "слово из двух алфавитов («%s»): склейка при генерации"},
	{reBrace, "фигурная скобка «%s»: обломок JSON в тексте реплики"},
	{reBBCode, "BB-код %s: на сайте он напечатается буквально"},
	{reHTMLTag, "HTML-тег %s: сайт его экранирует, и он вылезет текстом"},
	{reJokeTag, "метка шутки «%s»: если смешно, она не нужна, если не смешно — не спасёт"},
}

// tellHit — первая сработавшая примета ("" — чисто).
func tellHit(text string) string {
	for _, t := range textTells {
		if m := t.re.FindString(text); m != "" {
			return fmt.Sprintf(t.reason, m)
		}
	}
	return ""
}

// dashes / quotes — что чиним подстановкой. Замер площадки: длинное тире у
// 0,52 % комментариев, ёлочки у 0,65 % — так пишет типографика, а не люди.
var typography = strings.NewReplacer(
	"—", "-", "–", "-", "―", "-", "‒", "-", "−", "-",
	"«", `"`, "»", `"`, "„", `"`, "“", `"`, "”", `"`, "‟", `"`,
	"‘", "'", "’", "'", "‚", "'", "‛", "'",
	"…", "...",
)

// normalize приводит текст к тому виду, в котором он поедет на сайт.
// Идемпотентна: второй прогон ничего не меняет и NBSP не плодит — иначе
// повторная генерация после брака удвоила бы отступы.
func normalize(text string) string {
	text = typography.Replace(text)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "\t", strings.Repeat(" ", tabWidth))

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blanks := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " 	"+string(nbsp))
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
			b.WriteString(strings.Repeat(string(nbsp), run))
		} else {
			b.WriteByte(' ')
		}
		i += run - 1
	}
	return b.String()
}

// validateConfig — пороги проверки одной реплики.
type validateConfig struct {
	MinRunes   int
	MaxRunes   int
	MaxLines   int
	AllowEmoji bool
	// Forms — разрешённые формы; пусто — форма не спрашивается (ответ на ответ).
	Forms []string
	// NoteText — текст заметки: по нему ловится пересказ.
	NoteText string
	// Nicks — ники, обращение к которым модель писать не должна: их
	// подставляет инструмент (свой ник, имя автора заметки, ник собеседника).
	Nicks []string
}

// validate возвращает причину брака ("" — годится). Причина едет хвостом в
// промпт переспроса, поэтому формулируется как указание, а не как код ошибки.
func validate(text, form string, cfg validateConfig) string {
	if strings.TrimSpace(text) == "" {
		return "пустой текст"
	}
	if n := utf8.RuneCountInString(text); n < cfg.MinRunes || n > cfg.MaxRunes {
		return "длина " + strconv.Itoa(n) + " знаков вне допустимых " +
			strconv.Itoa(cfg.MinRunes) + "–" + strconv.Itoa(cfg.MaxRunes)
	}
	if tell := tellHit(text); tell != "" {
		// Сюда попадают и служебный тег размышления, и обрывок хода мысли, и
		// разметка, которая на сайте напечаталась бы буквально.
		return tell
	}
	if md := markdownHit(text); md != "" {
		return "markdown (" + md + "): на сайте он напечатается буквально"
	}
	if bad := typographyHit(text); bad != "" {
		return "знак " + bad + " на площадке почти не встречается — только дефис и обычная кавычка"
	}
	lines := strings.Split(text, "\n")
	if cfg.MaxLines > 0 && len(lines) > cfg.MaxLines {
		return "строк " + strconv.Itoa(len(lines)) + ", потолок " + strconv.Itoa(cfg.MaxLines)
	}
	if reason := checkArt(lines); reason != "" {
		return reason
	}
	if !cfg.AllowEmoji && hasEmoji(text) {
		return "эмодзи, а в этот раз без них"
	}
	if p := love.AddressPrefix(text); p != "" && knownNick(p, cfg.Nicks) {
		// Обращение подставляет инструмент — так ник нельзя выдумать. Ловим
		// только НАСТОЯЩИЕ ники: по фикстуре живого треда видно, что сайт
		// выделяет жирным именно их, а обычный оборот «Смирение не в том, …»
		// он не трогает. Бить по любому слову перед запятой значило бы гонять
		// переспросы на ровном месте, а каждый стоит 10–30 секунд.
		return "обращение «" + p + ",» в начале: его подставляет инструмент, а не ты"
	}
	if share := overlapShare(text, cfg.NoteText); share > maxOverlapShare {
		return "пересказ заметки: слишком много общих оборотов"
	}
	if len(cfg.Forms) > 0 && !hasForm(cfg.Forms, form) {
		return "форма «" + form + "» не из предложенных (" + strings.Join(cfg.Forms, ", ") + ")"
	}
	return ""
}

// knownNick — совпало ли обращение с одним из тех ников, которые подставляет
// инструмент. Сравнение в нижнем регистре: AddressPrefix его уже привёл.
func knownNick(prefix string, nicks []string) bool {
	for _, n := range nicks {
		if n != "" && strings.EqualFold(strings.TrimSpace(n), prefix) {
			return true
		}
	}
	return false
}

// markdownHit — копия правила из archive/voice_gen.go: markdown на сайте
// печатается буквально, и это самый частый прокол генерации.
func markdownHit(text string) string {
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

// typographyHit — страховка от дырки в normalize: если типографский знак дожил
// до валидации, значит подстановка его не поймала.
func typographyHit(text string) string {
	for _, r := range []string{"—", "–", "―", "«", "»", "“", "”", "„"} {
		if strings.Contains(text, r) {
			return r
		}
	}
	return ""
}

// checkArt проверяет рисунок из знаков: он не должен быть шире 30 знаков и
// выше 5 строк подряд.
func checkArt(lines []string) string {
	block := 0
	for _, line := range lines {
		if !artLike(line) {
			block = 0
			continue
		}
		block++
		if w := utf8.RuneCountInString(strings.TrimSpace(line)); w > maxArtWidth {
			return "рисунок шире " + strconv.Itoa(maxArtWidth) + " знаков — на пропорциональном шрифте он развалится"
		}
		if block > maxArtLines {
			return "рисунок выше " + strconv.Itoa(maxArtLines) + " строк"
		}
	}
	return ""
}

// artLike — строка из знаков, а не из слов: в ней почти нет букв и цифр.
func artLike(line string) bool {
	trimmed := strings.TrimSpace(strings.ReplaceAll(line, string(nbsp), " "))
	if utf8.RuneCountInString(trimmed) < 3 {
		return false
	}
	letters, total := 0, 0
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	if total == 0 {
		return false
	}
	return float64(letters)/float64(total) < 0.2
}

// hasEmoji — есть ли в тексте эмодзи. Диапазоны грубые намеренно: нам нужен
// сам факт, а не классификация.
func hasEmoji(text string) bool {
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

// overlapShare — доля пятёрок слов реплики, дословно встречающихся в заметке.
// Пятёрка (а не тройка) выбрана как порог узнавания: три общих слова бывают
// случайно, пять подряд — это уже цитата.
func overlapShare(text, note string) float64 {
	const n = 5
	reply := words(text)
	if len(reply) < n {
		return 0
	}
	src := words(note)
	if len(src) < n {
		return 0
	}
	seen := make(map[string]bool, len(src))
	for i := 0; i+n <= len(src); i++ {
		seen[strings.Join(src[i:i+n], " ")] = true
	}
	total, shared := 0, 0
	for i := 0; i+n <= len(reply); i++ {
		total++
		if seen[strings.Join(reply[i:i+n], " ")] {
			shared++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(shared) / float64(total)
}

func words(s string) []string {
	fields := reWordSep.Split(strings.ToLower(s), -1)
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// hasForm — назвала ли модель одну из предложенных форм. Сравнение через
// formKey: «ложная серьезность» без «ё» — та же форма, а не повод переспрашивать.
func hasForm(forms []string, form string) bool {
	key := formKey(form)
	for _, f := range forms {
		if formKey(f) == key {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
