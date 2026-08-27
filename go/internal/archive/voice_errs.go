package archive

// Характерные ошибки: чем письмо человека отличается от грамотного текста.
//
// Замер под эмуляцию («народ»). Просить ошибки у модели нельзя — «пиши с
// ошибками» она понимает как карикатуру и ошибается каждый раз по-новому, а у
// человека ошибка одна и та же годами: он не знает, что она ошибка. Поэтому
// здесь снимается ЧИСЛОВАЯ ЦЕЛЬ (какая ошибка и сколько раз на тысячу слов), а
// вносит её потом детерминированный постпроцессор.
//
// Замер сравнительный, и иначе нельзя: «запятая перед „что" пропущена трижды» не
// говорит ни о чём, пока не известно, сколько раз её пропускают все. Норма
// снимается по выборке корпуса (BuildCorpusNorm), и в карточку идёт только то,
// что у автора заметно чаще общего.
//
// Ошибок ДВА рода, и второй важнее первого:
//   - классовая — из закрытого списка ниже (тся/ться, «щас», строчная после
//     точки). Их немного, и они узнаваемы;
//   - ЛИЧНАЯ словоформа — «вобщем» вместо «в общем», «ложить», «ихний». Такие в
//     список не впишешь заранее: их находит сама выборка, сравнивая словарь
//     автора с корпусным.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Пороги отбора. Взяты с запасом в сторону молчания: ложная «характерная
// ошибка» портит письмо персонажа заметнее, чем её отсутствие.
const (
	errMinHits     = 5   // реже — это случайность, а не привычка
	errMinRatio    = 3.0 // во столько раз чаще, чем в корпусе
	errVariantMin  = 5   // столько раз автор написал слово по-своему
	errVariantMult = 50  // и во столько раз чаще корпус пишет его правильно
	errMaxPatterns = 12  // больше в одну реплику всё равно не поместится
)

// VoiceError — ошибка как цель для постпроцессора.
type VoiceError struct {
	ID      string  `json:"id"`
	Rate    float64 `json:"rate"`              // на 1000 слов
	Hits    int     `json:"hits"`              // сколько раз встретилась в корпусе автора
	Ratio   float64 `json:"ratio,omitempty"`   // во сколько раз чаще, чем у всех
	Norm    string  `json:"norm,omitempty"`    // как правильно
	Variant string  `json:"variant,omitempty"` // как пишет он
}

// VariantErrorID — общий id для личных словоформ: класс у них один, а различает
// их пара «как правильно → как пишет он».
const VariantErrorID = "variant"

// errDetector — классовая ошибка: как её найти в тексте.
type errDetector struct {
	id    string
	count func(text string) int
}

// errDetectors — ЗАКРЫТЫЙ список классовых ошибок.
//
// Закрытый намеренно, как platform.AutoHideable: детектор, добавленный между
// делом, начинает подмешивать в письмо жителей то, чего в них не мерили. Здесь
// нет ни «нет точки в конце», ни скобочной лесенки — это не ошибки, а регистр,
// и они уже замерены в карте письма.
var errDetectors = []errDetector{
	{"tsya", countTsya},
	{"colloquial", countColloquial},
	{"no_comma_before_chto", countNoCommaBeforeChto},
	{"lower_after_dot", countRe(lowerAfterDotRe)},
	{"no_space_after_comma", countRe(noSpaceAfterCommaRe)},
	{"no_space_after_dot", countRe(noSpaceAfterDotRe)},
	{"double_space", countRe(doubleSpaceRe)},
	{"long_ellipsis", countRe(longEllipsisRe)},
}

// Границы слова здесь НЕ через `\b`: в RE2 он считается по ASCII и после
// кириллицы не совпадает никогда (тот же урок стоил утренней заметке
// пропущенной проверки даты — см. morning/validate.go). Поэтому там, где нужна
// граница слова, текст разбирается на слова, а регулярки остались только у
// признаков, которые про ЗНАКИ, а не про слова.
var (
	lowerAfterDotRe     = regexp.MustCompile(`[.!?]\s+[а-яё]`)
	noSpaceAfterCommaRe = regexp.MustCompile(`,[а-яёa-z]`)
	noSpaceAfterDotRe   = regexp.MustCompile(`[а-яё]\.[а-яё]`)
	doubleSpaceRe       = regexp.MustCompile(`[^\S\n]{2,}`)
	longEllipsisRe      = regexp.MustCompile(`\.{4,}`)
	chtoRe              = regexp.MustCompile(`(?i)([а-яё,])\s+что(?:[^а-яё]|$)`)
)

// colloquialWords — разговорные написания. «ё» здесь приведена к «е»: словарь
// сверяется с wordList, а тот нормализует так же, как словарь карты письма.
var colloquialWords = map[string]bool{
	"щас": true, "ваще": true, "че": true, "чо": true, "тока": true,
	"скока": true, "ниче": true, "канеш": true, "токо": true, "седня": true,
}

func countColloquial(text string) int {
	var n int
	for _, w := range wordList(text) {
		if colloquialWords[w] {
			n++
		}
	}
	return n
}

// wordList — слова текста по порядку. Токенизация та же, что у словаря карты
// письма (forEachWord): нижний регистр, «ё» → «е».
func wordList(text string) []string {
	var out []string
	forEachWord(stripSiteMarkup(text), func(w []rune) { out = append(out, string(w)) })
	return out
}

// tsyaInfinitive — слова, после которых идёт «что делать»: «надо учиться».
var tsyaInfinitive = map[string]bool{
	"надо": true, "нужно": true, "можно": true, "нельзя": true, "пора": true,
	"хочу": true, "хочет": true, "хочешь": true, "хотел": true, "хотела": true,
	"буду": true, "будет": true, "будешь": true, "будем": true, "будут": true,
	"может": true, "могу": true, "можешь": true, "должен": true, "должна": true,
	"стал": true, "стала": true, "начал": true, "начала": true, "перестал": true,
	"давай": true, "любит": true, "люблю": true, "готов": true, "готова": true,
	"старается": true, "пытается": true, "решил": true, "решила": true, "умеет": true,
}

// tsyaThirdPerson — слова, после которых идёт «что делает»: «он учится».
// Ключи в том же виде, в каком их отдаёт wordList: строчными и с «е» вместо «ё».
var tsyaThirdPerson = map[string]bool{
	"он": true, "она": true, "оно": true, "они": true, "кто": true, "что": true,
	"это": true, "все": true, "тут": true, "там": true, "никто": true,
	"человек": true, "мужчина": true, "женщина": true, "жизнь": true,
}

// countTsya — перепутанные «-тся» и «-ться».
//
// Морфологии у нас нет, поэтому судим по СОСЕДУ слева: после «надо» стоит
// вопрос «что делать» (мягкий знак нужен), после «он» — «что делает» (не нужен).
// Слова, соседа которым не нашлось, не считаются вовсе: ошибиться здесь значит
// вписать человеку ошибку, которой он не делает.
func countTsya(text string) int {
	ws := wordList(text)
	var n int
	for i := 1; i < len(ws); i++ {
		word := ws[i]
		soft := strings.HasSuffix(word, "ться")
		if !soft && !strings.HasSuffix(word, "тся") {
			continue
		}
		switch prev := ws[i-1]; {
		case tsyaInfinitive[prev] && !soft:
			n++
		case tsyaThirdPerson[prev] && soft:
			n++
		}
	}
	return n
}

// countNoCommaBeforeChto — «думаю что» вместо «думаю, что». Запятая перед «что»
// нужна не всегда («что» бывает и вопросом), поэтому считаются только случаи,
// где слева стоит слово, а не знак.
func countNoCommaBeforeChto(text string) int {
	var n int
	for _, m := range chtoRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "," {
			n++
		}
	}
	return n
}

func countRe(re *regexp.Regexp) func(string) int {
	return func(text string) int { return len(re.FindAllString(text, -1)) }
}

// ErrorClasses — какие классы ошибок майнер умеет находить.
//
// Существует ради парного теста: замеренная, но не вносимая ошибка — это молча
// потерянная черта персонажа, а заметить такую потерю на глаз нельзя.
func ErrorClasses() []string {
	out := make([]string, 0, len(errDetectors))
	for _, d := range errDetectors {
		out = append(out, d.id)
	}
	return out
}

// TsyaMarkers — списки соседей, по которым судят о верной форме «-тся/-ться».
// Тоже ради парного теста: постпроцессор эмуляции держит их копию, потому что
// ядро эмуляции архив не импортирует.
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

// CorpusNorm — как пишут ВСЕ. Без неё «ошибка автора» неотличима от общей
// манеры эпохи: в 2013-м строчная после точки была нормой у половины сайта.
type CorpusNorm struct {
	Texts     int                `json:"texts"`
	Words     int                `json:"words"`
	ClassRate map[string]float64 `json:"class_rate"` // на 1000 слов
	WordFreq  map[string]int     `json:"word_freq"`
	Built     string             `json:"built"`
}

// BuildCorpusNorm снимает норму по выборке последних комментариев.
//
// Комментариев, а не заметок: реплика — это то, что персонаж пишет, а заметки
// на сайте писались с большей оглядкой (их видно в ленте целиком).
func (s *Store) BuildCorpusNorm(ctx context.Context, sample int) (CorpusNorm, error) {
	if sample <= 0 {
		sample = 100000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM comments WHERE text != '' ORDER BY id DESC LIMIT ?`, sample)
	if err != nil {
		return CorpusNorm{}, err
	}
	defer rows.Close()

	norm := CorpusNorm{ClassRate: map[string]float64{}, WordFreq: map[string]int{}}
	hits := map[string]int{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return CorpusNorm{}, err
		}
		norm.Texts++
		norm.Words += countErrWords(text, norm.WordFreq)
		for _, d := range errDetectors {
			hits[d.id] += d.count(text)
		}
	}
	if err := rows.Err(); err != nil {
		return CorpusNorm{}, err
	}
	if norm.Words == 0 {
		return CorpusNorm{}, fmt.Errorf("норма корпуса: в выборке из %d комментариев нет слов", sample)
	}
	for id, n := range hits {
		norm.ClassRate[id] = per1000(n, norm.Words)
	}
	return norm, nil
}

// MineErrors снимает характерные ошибки личности.
func (s *Store) MineErrors(ctx context.Context, accIDs []int64, norm CorpusNorm, recent int) ([]VoiceError, error) {
	if len(accIDs) == 0 {
		return nil, fmt.Errorf("ошибки: не задана ни одна анкета")
	}
	if norm.Words == 0 {
		return nil, fmt.Errorf("ошибки: не снята норма корпуса — сравнивать не с чем")
	}
	texts, err := s.voiceTexts(ctx, accIDs, "comments", recent)
	if err != nil {
		return nil, err
	}
	if len(texts) == 0 {
		return nil, nil
	}
	return measureErrors(texts, norm), nil
}

// measureErrors — чистое ядро отбора.
func measureErrors(texts []voiceText, norm CorpusNorm) []VoiceError {
	words := map[string]int{}
	var total int
	hits := map[string]int{}
	for _, t := range texts {
		total += countErrWords(t.text, words)
		for _, d := range errDetectors {
			hits[d.id] += d.count(t.text)
		}
	}
	if total == 0 {
		return nil
	}

	var out []VoiceError
	for _, d := range errDetectors {
		n := hits[d.id]
		if n < errMinHits {
			continue
		}
		rate := per1000(n, total)
		base := norm.ClassRate[d.id]
		// Нулевая норма означала бы бесконечное отношение: считаем такую ошибку
		// заметной, но отношение не выдумываем.
		ratio := 0.0
		if base > 0 {
			if ratio = rate / base; ratio < errMinRatio {
				continue
			}
		}
		out = append(out, VoiceError{ID: d.id, Rate: round2(rate), Hits: n, Ratio: round2(ratio)})
	}
	out = append(out, variantErrors(words, total, norm)...)

	// Порядок — по заметности: постпроцессор возьмёт первые, если карточку
	// придётся урезать.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate != out[j].Rate {
			return out[i].Rate > out[j].Rate
		}
		return out[i].ID+out[i].Variant < out[j].ID+out[j].Variant
	})
	if len(out) > errMaxPatterns {
		out = out[:errMaxPatterns]
	}
	return out
}

// variantErrors — личные словоформы. Слово автора считается его ошибкой, если
// корпус пишет его иначе НАМНОГО чаще и отличается одной правкой: так «вобщем»
// находит «общем», а редкое, но верно написанное слово остаётся в покое.
func variantErrors(words map[string]int, total int, norm CorpusNorm) []VoiceError {
	var out []VoiceError
	for w, n := range words {
		if n < errVariantMin || len([]rune(w)) < 4 {
			continue
		}
		mine := norm.WordFreq[w]
		best, bestFreq := "", 0
		for _, cand := range neighbors1(w) {
			f := norm.WordFreq[cand]
			if f > bestFreq {
				best, bestFreq = cand, f
			}
		}
		// «Намного чаще» считается от корпусной частоты САМОГО слова, а не от
		// авторской: слово, которое и весь сайт пишет так же, — это не ошибка,
		// а слово.
		if best == "" || bestFreq < errVariantMult*maxInt(mine, 1) {
			continue
		}
		out = append(out, VoiceError{
			ID: VariantErrorID, Rate: round2(per1000(n, total)), Hits: n,
			Norm: best, Variant: w,
		})
	}
	return out
}

// alphabet — буквы, из которых строятся соседи. «ё» здесь нет: forEachWord
// приводит её к «е», и словари обеих сторон уже в этом виде.
const alphabet = "абвгдежзийклмнопрстуфхцчшщъыьэюя"

// neighbors1 — все слова на расстоянии одной правки (вставка, удаление, замена,
// перестановка соседних). Соседи ПОРОЖДАЮТСЯ, а не ищутся перебором словаря:
// корпусный словарь — это сотни тысяч слов, а соседей у восьмибуквенного слова
// около шестисот.
func neighbors1(w string) []string {
	r := []rune(w)
	out := make([]string, 0, len(r)*len(alphabet))
	for i := range r {
		out = append(out, string(append(append([]rune{}, r[:i]...), r[i+1:]...))) // удаление
	}
	for i := 0; i+1 < len(r); i++ {
		t := append([]rune{}, r...)
		t[i], t[i+1] = t[i+1], t[i]
		out = append(out, string(t)) // перестановка
	}
	for _, c := range alphabet {
		for i := range r {
			if r[i] == c {
				continue
			}
			t := append([]rune{}, r...)
			t[i] = c
			out = append(out, string(t)) // замена
		}
		for i := 0; i <= len(r); i++ {
			t := append([]rune{}, r[:i]...)
			t = append(t, c)
			t = append(t, r[i:]...)
			out = append(out, string(t)) // вставка
		}
	}
	return out
}

// countErrWords считает слова текста и заодно копит словарь. Токенизация —
// forEachWord, та же, что у словаря карты письма: две разные меры «слова»
// сделали бы частоты несравнимыми.
func countErrWords(text string, into map[string]int) int {
	var n int
	forEachWord(stripSiteMarkup(text), func(w []rune) {
		n++
		if into != nil {
			into[string(w)]++
		}
	})
	return n
}

func per1000(hits, words int) float64 {
	if words == 0 {
		return 0
	}
	return 1000 * float64(hits) / float64(words)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
