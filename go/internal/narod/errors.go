package narod

// Постпроцессор ошибок: вносит в готовую реплику то, что человек делает не
// задумываясь.
//
// Почему инструментом, а не просьбой к модели. Просьбу «пиши с ошибками» модель
// исполняет карикатурой — сыплет опечатки где попало и каждый раз новые, — а у
// человека ошибка ровно одна и та же годами: он не знает, что это ошибка.
// Отсюда устройство: майнер архива снял ЧИСЛОВУЮ ЦЕЛЬ (какая ошибка и сколько
// раз на тысячу слов), а здесь она вносится детерминированно.
//
// Детерминированно — значит от зерна: одна и та же реплика с одним и тем же
// зерном портится одинаково. Это нужно не тестам, а отладке: без этого нельзя
// ни повторить чужую жалобу, ни сравнить два прогона реплея.
//
// Место в конвейере строгое: генерация → Normalize → евалы → ЗДЕСЬ →
// публикация. Раньше евалов нельзя (Normalize чинит ровно то, что мы вносим, а
// детектор «похоже на ИИ» судил бы уже испорченный текст), позже публикации
// поздно.

import (
	"math/rand/v2"
	"regexp"
	"sort"
	"strings"
)

// injector — как вносится ошибка одного класса: где у неё места и что делать с
// выбранным. Возвращает текст с ОДНОЙ внесённой ошибкой в позиции site.
type injector struct {
	sites func(text string) []int // смещения в байтах, где ошибка возможна
	apply func(text string, site int) string
}

// injectors — закрытый список, зеркальный детекторам майнера
// (archive/voice_errs.go). Пары обязаны сходиться: замерили «пропущенную
// запятую перед что» — вносим её же, а не что-то похожее. Разъехавшись, они
// дали бы персонажа, который делает не те ошибки, что у него замерены, и
// заметить это было бы нечем.
var injectors = map[string]injector{
	"no_comma_before_chto": {sites: sitesRe(commaChtoRe, 1), apply: dropByte},
	"no_space_after_comma": {sites: sitesRe(commaSpaceRe, 1), apply: dropByte},
	"no_space_after_dot":   {sites: sitesRe(dotSpaceMidRe, 1), apply: dropByte},
	"lower_after_dot":      {sites: sitesRe(afterDotUpperRe, 1), apply: lowerRuneAt},
	"double_space":         {sites: sitesRe(wordSpaceRe, 1), apply: doubleSpaceAt},
	"long_ellipsis":        {sites: sitesRe(ellipsisRe, 0), apply: extendEllipsis},
	"colloquial":           {sites: colloquialSites, apply: colloquialSwap},
	"tsya":                 {sites: tsyaSites, apply: tsyaSwap},
}

var (
	commaChtoRe     = regexp.MustCompile(`(,)\s+что(?:[^а-яё]|$)`)
	commaSpaceRe    = regexp.MustCompile(`,( )[а-яё]`)
	dotSpaceMidRe   = regexp.MustCompile(`[а-яё]\.( )[а-яё]`)
	afterDotUpperRe = regexp.MustCompile(`[.!?]\s+([А-ЯЁ])`)
	wordSpaceRe     = regexp.MustCompile(`[а-яё]( )[а-яё]`)
	ellipsisRe      = regexp.MustCompile(`\.\.\.`)
)

// colloquialPairs — как правильно и как пишет он. Направление одностороннее:
// «щас» обратно в «сейчас» никто не превращает.
var colloquialPairs = [][2]string{
	{"сейчас", "щас"}, {"вообще", "ваще"}, {"только", "тока"},
	{"сколько", "скока"}, {"ничего", "ниче"}, {"сегодня", "седня"},
	{"конечно", "канеш"},
}

// InjectErrors вносит в текст ошибки персонажа.
//
// Число вносимых ошибок считается от ДЛИНЫ текста: частота замерена на тысячу
// слов, и в короткой реплике ошибка чаще всего не появляется вовсе — как у
// человека. Дробный остаток разыгрывается монеткой, иначе персонаж с частотой
// 3 на 1000 слов не ошибался бы никогда: реплики у него по шестьдесят знаков.
func InjectErrors(text string, pats []ErrorPattern, seed uint64) string {
	if text == "" || len(pats) == 0 {
		return text
	}
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	words := len(strings.Fields(text))
	if words == 0 {
		return text
	}

	for _, p := range pats {
		n := drawCount(p.Rate*float64(words)/1000, rng)
		for i := 0; i < n; i++ {
			var next string
			if p.ID == VariantErrorID {
				next = injectVariant(text, p, rng)
			} else {
				next = injectClass(text, p.ID, rng)
			}
			if next == text {
				break // мест не осталось — дальше пытаться незачем
			}
			text = next
		}
	}
	return text
}

// drawCount — сколько раз вносить ошибку при ожидании want. Целая часть плюс
// монетка на остаток: так частота держится в среднем, а не округляется до нуля
// на каждой короткой реплике.
func drawCount(want float64, rng *rand.Rand) int {
	if want <= 0 {
		return 0
	}
	n := int(want)
	if rng.Float64() < want-float64(n) {
		n++
	}
	return n
}

func injectClass(text, id string, rng *rand.Rand) string {
	inj, ok := injectors[id]
	if !ok {
		return text // незнакомый класс молча пропускаем: карточка старше кода
	}
	sites := inj.sites(text)
	if len(sites) == 0 {
		return text
	}
	return inj.apply(text, sites[rng.IntN(len(sites))])
}

// injectVariant заменяет правильное написание на авторское. Слово ищется
// целиком: «общем» внутри «общемировой» — не то слово.
func injectVariant(text string, p ErrorPattern, rng *rand.Rand) string {
	if p.Norm == "" || p.Variant == "" {
		return text
	}
	sites := wordSites(text, p.Norm)
	if len(sites) == 0 {
		return text
	}
	at := sites[rng.IntN(len(sites))]
	return text[:at] + matchCase(text[at:at+len(p.Norm)], p.Variant) + text[at+len(p.Norm):]
}

// CanInject — умеет ли постпроцессор внести ошибку этого класса. Личная
// словоформа идёт особняком: у неё нет своего места в тексте, место ей задаёт
// сама пара «как правильно → как пишет он».
//
// Существует ради парного теста с майнером архива: класс, который умеют найти,
// но не умеют внести, — это молча потерянная черта персонажа.
func CanInject(id string) bool {
	if id == VariantErrorID {
		return true
	}
	_, ok := injectors[id]
	return ok
}

// TsyaMarkers — копия списков майнера, отданная наружу для того же парного
// теста.
func TsyaMarkers() (infinitive, thirdPerson []string) {
	return sortedKeys(tsyaInfinitive), sortedKeys(tsyaThirdPerson)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- места ---

// sitesRe — смещения группы group у каждого совпадения.
func sitesRe(re *regexp.Regexp, group int) func(string) []int {
	return func(text string) []int {
		var out []int
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			if i := 2 * group; i+1 < len(m) && m[i] >= 0 {
				out = append(out, m[i])
			}
		}
		return out
	}
}

// wordSites — смещения слова целиком, без учёта регистра первой буквы.
func wordSites(text, word string) []int {
	if word == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var out []int
	for from := 0; ; {
		i := strings.Index(lower[from:], word)
		if i < 0 {
			return out
		}
		at := from + i
		if isWordBoundary(lower, at, at+len(word)) {
			out = append(out, at)
		}
		from = at + len(word)
	}
}

func isWordBoundary(s string, start, end int) bool {
	return !isWordByteAt(s, start-1) && !isWordByteAt(s, end)
}

// isWordByteAt — стоит ли по этому смещению буква. Кириллица в UTF-8 занимает
// два байта, поэтому «не буква» проверяется по обоим: середина буквы — это
// продолжающий байт, и границей слова он не является.
func isWordByteAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	b := s[i]
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b >= 0x80: // байт многобайтовой руны — считаем буквой
		return true
	}
	return false
}

func colloquialSites(text string) []int {
	var out []int
	for _, p := range colloquialPairs {
		out = append(out, wordSites(text, p[0])...)
	}
	return out
}

// tsyaSites — глаголы, у которых можно перепутать «-тся» и «-ться».
//
// Два условия, и оба обязательны. Первое: сосед слева должен говорить, какая
// форма верна («надо» просит инфинитив, «он» — третье лицо), — без него подмена
// внесла бы не ошибку, а другое слово. Второе: форма сейчас должна быть ВЕРНОЙ.
// Мы ошибки ВНОСИМ, а не переключаем: без этой проверки две вставки подряд
// возвращали текст в исходный вид, и частота ошибки у говорливого персонажа
// схлопывалась вдвое.
func tsyaSites(text string) []int {
	var out []int
	for _, m := range tsyaWordRe.FindAllStringSubmatchIndex(text, -1) {
		// «ё» приводится к «е» так же, как это делает майнер: иначе списки
		// соседей у нас и у него совпадали бы по виду, но не по действию.
		prev := normalizeYo(strings.ToLower(text[m[2]:m[3]]))
		soft := strings.HasSuffix(strings.ToLower(text[m[4]:m[5]]), "ться")
		if (tsyaInfinitive[prev] && soft) || (tsyaThirdPerson[prev] && !soft) {
			out = append(out, m[4])
		}
	}
	return out
}

var tsyaWordRe = regexp.MustCompile(`(?i)([а-яё]+)\s+([а-яё]+ть?ся)(?:[^а-яё]|$)`)

// tsyaInfinitive / tsyaThirdPerson — те же списки, что у майнера. Держатся
// здесь копией намеренно: ядро эмуляции не импортирует архив (см. шапку
// пакета), а парный тест стережёт, чтобы списки не разъехались.
var tsyaInfinitive = map[string]bool{
	"надо": true, "нужно": true, "можно": true, "нельзя": true, "пора": true,
	"хочу": true, "хочет": true, "хочешь": true, "хотел": true, "хотела": true,
	"буду": true, "будет": true, "будешь": true, "будем": true, "будут": true,
	"может": true, "могу": true, "можешь": true, "должен": true, "должна": true,
	"стал": true, "стала": true, "начал": true, "начала": true, "перестал": true,
	"давай": true, "любит": true, "люблю": true, "готов": true, "готова": true,
	"старается": true, "пытается": true, "решил": true, "решила": true, "умеет": true,
}

var tsyaThirdPerson = map[string]bool{
	"он": true, "она": true, "оно": true, "они": true, "кто": true, "что": true,
	"это": true, "все": true, "тут": true, "там": true, "никто": true,
	"человек": true, "мужчина": true, "женщина": true, "жизнь": true,
}

// normalizeYo — «ё» → «е». Той же заменой живёт словарь майнера, поэтому ключи
// обоих списков хранятся уже в этом виде.
func normalizeYo(s string) string { return strings.ReplaceAll(s, "ё", "е") }

// --- правки ---

// dropByte убирает один байт (пробел или запятую) — им и вносится «пропущено».
func dropByte(text string, site int) string { return text[:site] + text[site+1:] }

func doubleSpaceAt(text string, site int) string { return text[:site] + " " + text[site:] }

func extendEllipsis(text string, site int) string { return text[:site] + "." + text[site:] }

func lowerRuneAt(text string, site int) string {
	r := []rune(text[site:])
	if len(r) == 0 {
		return text
	}
	return text[:site] + strings.ToLower(string(r[0])) + string(r[1:])
}

func colloquialSwap(text string, site int) string {
	for _, p := range colloquialPairs {
		if len(text) >= site+len(p[0]) && strings.EqualFold(text[site:site+len(p[0])], p[0]) {
			return text[:site] + matchCase(text[site:site+len(p[0])], p[1]) + text[site+len(p[0]):]
		}
	}
	return text
}

// tsyaSwap переставляет мягкий знак: «учиться» ↔ «учится».
func tsyaSwap(text string, site int) string {
	end := site
	for end < len(text) && isWordByteAt(text, end) {
		end++
	}
	word := text[site:end]
	switch {
	case strings.HasSuffix(word, "ться"):
		word = strings.TrimSuffix(word, "ться") + "тся"
	case strings.HasSuffix(word, "тся"):
		word = strings.TrimSuffix(word, "тся") + "ться"
	default:
		return text
	}
	return text[:site] + word + text[end:]
}

// matchCase переносит регистр первой буквы: подмена в начале предложения не
// должна ронять заглавную — это была бы вторая, незамеренная ошибка.
func matchCase(src, dst string) string {
	sr, dr := []rune(src), []rune(dst)
	if len(sr) == 0 || len(dr) == 0 {
		return dst
	}
	if strings.ToUpper(string(sr[0])) == string(sr[0]) && strings.ToLower(string(sr[0])) != string(sr[0]) {
		return strings.ToUpper(string(dr[0])) + string(dr[1:])
	}
	return dst
}
