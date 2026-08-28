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

// Час в тексте против часа выхода.
//
// Заметку читают в тот самый час, на который стоит слот, и «человека только что
// подняли в шесть утра» в заметке, вышедшей в пять, читается как враньё
// (замечание владельца 28.08.2026 по живой заметке 313100 — промахов там было
// два). Строгой привязки нет: про другой час можно сказать с оговоркой — «до
// семи ещё можно поспать». Ловится ровно утверждение: «<час> утра» без такой
// оговорки. Граница слова стоит группой `[^\p{L}]`, а не ``: в Go (RE2)
// граница считается по ASCII и после кириллицы не совпадает НИКОГДА — тот же
// грабль, что у правила про дату.
var reHourAM = regexp.MustCompile(`(?i)(^|[^\p{L}])(до|к|после|через|перед)?\s*(\d{1,2}|` +
	`двух|трёх|трех|четырёх|четырех|пяти|шести|семи|восьми|девяти|десяти|` +
	`два|три|четыре|пять|шесть|семь|восемь|девять|десять)\s+утра`)

var hourWords = map[string]int{
	"два": 2, "двух": 2, "три": 3, "трёх": 3, "трех": 3,
	"четыре": 4, "четырёх": 4, "четырех": 4, "пять": 5, "пяти": 5,
	"шесть": 6, "шести": 6, "семь": 7, "семи": 7, "восемь": 8, "восьми": 8,
	"девять": 9, "девяти": 9, "десять": 10, "десяти": 10,
}

// checkHour — названный как текущий час обязан совпасть с часом выхода.
func checkHour(text string, hour int) string {
	if hour <= 0 {
		return ""
	}
	for _, m := range reHourAM.FindAllStringSubmatch(text, -1) {
		if m[2] != "" {
			continue // «до семи», «к девяти» — про другой час намеренно
		}
		word := strings.ToLower(m[3])
		got, ok := hourWords[word]
		if !ok {
			got, _ = strconv.Atoi(word)
		}
		if got != 0 && got != hour {
			return "«" + strings.TrimSpace(m[0]) + "» не вяжется с часом выхода: заметка выходит в " +
				strconv.Itoa(hour) + " утра"
		}
	}
	return ""
}

type validateConfig struct {
	MinRunes int
	MaxRunes int
	MaxLines int
	// Day / Month — сегодняшнее число и месяц: другую дату называть нельзя.
	Day   int
	Month int
	// Weekday — сегодняшний день недели.
	Weekday string
	// Hour — час выхода заметки: чужое время в тексте читается как враньё.
	Hour int
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
	// Приметы несмешного — те же, что у амвона, и живут они в одном месте:
	// сползает в них любой текст, который пишет модель. Формулировка причины
	// своя — она едет в промпт переспроса, а говорить надо про утреннюю строку.
	if m := sitetext.Aphorism(text); m != "" {
		return "афоризм «" + m + "»: это наблюдение сверху, а не панч — влезь внутрь положения"
	}
	if m := sitetext.JokeTag(text); m != "" {
		return "метка шутки «" + m + "»: если смешно, она не нужна, если не смешно — не спасёт"
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
	if reason := checkHour(text, cfg.Hour); reason != "" {
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
	head, rest := splitGreeting(text)
	lowerHead := strings.ToLower(head)

	// Приветствие: дата и день недели обязаны быть сегодняшними. Ошибка тут
	// самая обидная из возможных — заметка выйдет вовремя и соврёт о том, какой
	// сегодня день.
	for _, m := range reDate.FindAllStringSubmatch(head, -1) {
		if day, _ := strconv.Atoi(m[1]); day != cfg.Day || monthIndex(m[2]) != cfg.Month {
			return "в приветствии дата «" + m[0] + "» не сегодняшняя: сегодня " +
				strconv.Itoa(cfg.Day) + " " + months[cfg.Month-1]
		}
	}
	if w := otherWeekday(lowerHead, cfg.Weekday); w != "" {
		return "в приветствии другой день недели («" + w + "»), а сегодня " + cfg.Weekday
	}

	// Дальше по тексту чужой день и чужая дата РАЗРЕШЕНЫ: «до субботы далеко» и
	// «до первого сентября неделя» — шутка про календарь, а не ложь про
	// сегодня. Запрет на них стоил живого прогона 24.08.2026: три попытки
	// подряд забракованы, заметка не вышла бы вовсе. Ложью такое становится
	// рядом со словом «сегодня» — вот это и проверяем.
	lowerRest := strings.ToLower(rest)
	for _, m := range reToday.FindAllStringIndex(lowerRest, -1) {
		window := lowerRest[m[1]:min(len(lowerRest), m[1]+todayWindow)]
		if w := otherWeekday(window, cfg.Weekday); w != "" {
			return "«сегодня " + w + "» — а сегодня " + cfg.Weekday
		}
		for _, d := range reDate.FindAllStringSubmatch(window, -1) {
			if day, _ := strconv.Atoi(d[1]); day != cfg.Day || monthIndex(d[2]) != cfg.Month {
				return "«сегодня " + d[0] + "» — а сегодня " +
					strconv.Itoa(cfg.Day) + " " + months[cfg.Month-1]
			}
		}
	}
	return ""
}

// reToday — слова, после которых названная дата перестаёт быть шуткой про
// календарь и становится утверждением о нынешнем дне.
var reToday = regexp.MustCompile(`(сегодня|нынче|в этот день)`)

// todayWindow — сколько байт после «сегодня» считаем утверждением о нём. Тридцать
// с небольшим — это два-три слова: «сегодня, во вторник, …», «сегодня 25 августа».
const todayWindow = 40

// splitGreeting — первая строка (приветствие с днём и числом) и всё остальное.
// Ритуал живёт в первой строке, там и проверяется строго.
func splitGreeting(text string) (head, rest string) {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i], text[i:]
	}
	return text, ""
}

// otherWeekday — назван ли в куске текста день недели, отличный от сегодняшнего.
// Сравнение по корню: «среда» без огласовки совпала бы со «средой», а
// морфологии у нас нет.
func otherWeekday(lower, today string) string {
	for _, w := range weekdays {
		if w == today {
			continue
		}
		root := []rune(w)
		if strings.Contains(lower, string(root[:len(root)-1])) {
			return w
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

// Плотность рубрик.
//
// Сайт делает nl2br, поэтому одиночный перенос работает — а модель, которой
// сказано «отбивай строки пустой строкой», отбивала ею КАЖДУЮ, и заметка на НГС
// вышла растянутой на два экрана (правил владелец руками 27.08.2026). Форма,
// которую он оставил: пустая строка ПЕРЕД заголовком рубрики, а внутри рубрики —
// обычные переносы.
//
// Правило механическое, а не просьба в промпте: промпт про это уже просили, и
// одной строкой он не держится — модель отбивает пустой строкой всё, что
// считает абзацем.
func tightenRubrics(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
			continue
		}
		// Пустая строка выживает, только если СЛЕДУЮЩАЯ непустая — не пункт
		// рубрики: перед заголовком, перед вводным абзацем и перед финалом
		// воздух нужен, внутри рубрики он и есть та самая растяжка.
		if !startsItem(nextNonEmpty(lines, i+1)) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func nextNonEmpty(lines []string, from int) string {
	for ; from < len(lines); from++ {
		if strings.TrimSpace(lines[from]) != "" {
			return lines[from]
		}
	}
	return ""
}

// startsItem — строка начинается со значка, то есть это пункт рубрики. Значок в
// начале строки ставит сама заметка (правило формата), поэтому признак надёжнее
// любых догадок по смыслу.
func startsItem(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	r := []rune(line)[0]
	return sitetext.CountEmoji(string(r)) > 0
}
