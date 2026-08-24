package morning

// Проверка того, что вернула модель.
//
// Здесь возможны проверки, которых у амвона нет и быть не могло: факты подаём
// МЫ, поэтому враньё ловится механически, а не на глаз. Дата обязана совпасть с
// сегодняшней, год — найтись среди поданных поводов, а названный номер повода —
// подтвердиться словом из его названия в самом тексте. Это не педантизм:
// заметка уходит на сайт необратимо, и «Международный день, которого нет»
// исправлять будет уже нечем.
//
// Общие с амвоном правила сайта (типографика, разметка, обломки генерации)
// живут в `internal/sitetext` — второй копии этих правил в проекте не заводится.

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"lovegw/internal/holidays"
	"lovegw/internal/sitetext"
)

// reDate — «24 августа», «1 сентября»: дата, названная в тексте.
//
// Хвостовой границы слова здесь нет намеренно, и это не небрежность: в Go (RE2)
// `\b` считается по ASCII, поэтому после кириллической буквы она не совпадает
// НИКОГДА — с ней правило молча пропускало любую подменённую дату (поймано
// тестом «чужая дата»). Названия месяцев и без границы ни с чем не путаются.
var reDate = regexp.MustCompile(`(\d{1,2})\s+(` + strings.Join(months[:], "|") + `)`)

// reYear — год в тексте. Четыре цифры — потому что трёхзначные годы модель
// пишет редко, а «300 человек» ловилось бы каждый раз.
var reYear = regexp.MustCompile(`\b(1\d{3}|20\d{2})\b`)

// factWordPrefix — по скольким первым рунам слова считаем, что повод назван.
// Столько же, сколько у амвона в hookInNote, и по той же причине: русский
// склоняет, а морфологии у нас нет. «чипсов» и «чипсы» сходятся на «чипс».
const factWordPrefix = 4

// factWordMin — слова короче в расчёт не идут: «день», «мира», «года» есть
// в половине названий и не доказывают ничего.
const factWordMin = 5

// minEmoji — сколько значков обязано быть в заметке. Эмодзи здесь не
// украшение, а интонация: ни курсива, ни голоса у текста нет, и заметка без
// единого значка читается сводкой календаря (замечание владельца 24.08.2026 по
// первому живому черновику — «сухо»). Это ПОЛ, а не цель: промпт просит
// четыре-шесть на строку поводов, и до порога дело доходить не должно.
const minEmoji = 2

type validateConfig struct {
	MinRunes int
	MaxRunes int
	MaxLines int
	// Day / Month — сегодняшнее число и месяц: другую дату называть нельзя.
	Day   int
	Month int
	// Weekday — сегодняшний день недели.
	Weekday string
	// Facts — поводы, которые видела модель.
	Facts []holidays.Occasion
}

// validate возвращает причину брака ("" — годится). Причина едет хвостом в
// промпт переспроса, поэтому формулируется как указание, а не как код ошибки.
func validate(d draft, cfg validateConfig) string {
	text := d.Text
	if strings.TrimSpace(text) == "" {
		return "пустой текст"
	}
	if n := utf8.RuneCountInString(text); n < cfg.MinRunes || n > cfg.MaxRunes {
		return "длина " + strconv.Itoa(n) + " знаков вне допустимых " +
			strconv.Itoa(cfg.MinRunes) + "–" + strconv.Itoa(cfg.MaxRunes)
	}
	if lines := strings.Split(text, "\n"); cfg.MaxLines > 0 && len(lines) > cfg.MaxLines {
		return "строк " + strconv.Itoa(len(lines)) + ", потолок " + strconv.Itoa(cfg.MaxLines)
	}
	if tell := sitetext.MachineTell(text); tell != "" {
		return tell
	}
	if md := sitetext.MarkdownHit(text); md != "" {
		return "markdown (" + md + "): на сайте он напечатается буквально"
	}
	if bad := sitetext.TypographyHit(text); bad != "" {
		return "знак " + bad + " на площадке почти не встречается — только дефис и обычная кавычка"
	}
	if frag := sitetext.LatinFragment(text); frag != "" {
		return "латинский огрызок «" + frag + "» в русском тексте: мусор генерации"
	}
	if !isGreeting(text) {
		// Тем же закрытым списком, которым мы узнаём ЧУЖОЕ приветствие в ленте.
		// Симметрия не случайна: если наша заметка не читается как «доброе
		// утро», ритуала нет — и наш же детектор чужого утра её не узнал бы.
		return "заметка не начинается с приветствия: первыми словами должно быть «Доброе утро»"
	}
	if n := sitetext.CountEmoji(text); n < minEmoji {
		return "эмодзи " + strconv.Itoa(n) + ", нужно хотя бы " + strconv.Itoa(minEmoji) +
			": одно у приветствия и по одному в начале строки каждого повода"
	}
	if reason := checkDate(text, cfg); reason != "" {
		return reason
	}
	if reason := checkYears(text, cfg.Facts); reason != "" {
		return reason
	}
	return checkUsed(d, cfg.Facts, text)
}

// checkDate — названная дата и день недели обязаны быть сегодняшними. Ошибка
// тут самая обидная из возможных: заметка выйдет вовремя и будет врать о том,
// какой сегодня день.
func checkDate(text string, cfg validateConfig) string {
	for _, m := range reDate.FindAllStringSubmatch(text, -1) {
		day, _ := strconv.Atoi(m[1])
		month := monthIndex(m[2])
		if day != cfg.Day || month != cfg.Month {
			return "дата «" + m[0] + "» не сегодняшняя: сегодня " +
				strconv.Itoa(cfg.Day) + " " + months[cfg.Month-1]
		}
	}
	lower := strings.ToLower(text)
	for _, w := range weekdays {
		if w == cfg.Weekday {
			continue
		}
		// «среда» без огласовки совпала бы со «средой» — сравниваем по корню,
		// как везде в проекте.
		root := []rune(w)
		if strings.Contains(lower, string(root[:len(root)-1])) {
			return "в тексте другой день недели («" + w + "»), а сегодня " + cfg.Weekday
		}
	}
	return ""
}

func monthIndex(name string) int {
	for i, m := range months {
		if m == name {
			return i + 1
		}
	}
	return 0
}

// checkYears — любой год в тексте обязан быть среди поданных поводов. Так
// ловится выдуманная историческая дата: пересказать повод своими словами модель
// вправе, придумать ему год — нет.
func checkYears(text string, facts []holidays.Occasion) string {
	known := make(map[string]bool, len(facts))
	for _, f := range facts {
		if f.Year > 0 {
			known[strconv.Itoa(f.Year)] = true
		}
	}
	for _, m := range reYear.FindAllString(text, -1) {
		if !known[m] {
			return "год " + m + " не из поводов дня: годы бери только оттуда"
		}
	}
	return ""
}

// checkUsed — названные номера поводов существуют, и каждый подтверждён словом
// из своего названия. Проверка не строгая по смыслу (пересказ мы разрешаем), но
// она ловит главное: взяли повод «с потолка», а номер поставили для вида.
func checkUsed(d draft, facts []holidays.Occasion, text string) string {
	if len(facts) == 0 {
		return "" // поводов не давали — спрашивать не с чего
	}
	if len(d.Used) == 0 {
		return "не назван ни один повод из списка: возьми два-три и назови их номера в used"
	}
	lower := strings.ToLower(text)
	for _, n := range d.Used {
		if n < 1 || n > len(facts) {
			return "повода №" + strconv.Itoa(n) + " в списке нет"
		}
		f := facts[n-1]
		if !mentions(f.Title, lower) {
			return "повод №" + strconv.Itoa(n) + " («" + f.Title +
				"») назван в used, но в тексте его не видно"
		}
	}
	return ""
}

// mentions — есть ли в тексте хоть одно значимое слово названия. Одного
// достаточно: модель пересказывает повод своей фразой, и требовать совпадения
// целиком значило бы жечь переспросы на склонениях.
func mentions(title, lowerText string) bool {
	judged := false
	for _, w := range sitetext.Words(title) {
		r := []rune(w)
		if len(r) < factWordMin {
			continue
		}
		judged = true
		if strings.Contains(lowerText, string(r[:factWordPrefix])) {
			return true
		}
	}
	// Значимых слов в названии не нашлось — судить не по чему.
	return !judged
}
