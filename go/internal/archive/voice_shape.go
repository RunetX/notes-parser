package archive

// Измерения манеры письма: форма (ритм, длины, пунктуация, приметы, разметка,
// зачины) и характерная лексика. Всё считается по сырому тексту и никуда не
// уходит, кроме карты (voice_render.go); сборка корпуса — в voice.go.

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"lovegw/internal/love"
)

// --- измерения ---

// shapeAcc — накопитель замера: копит сырые счётчики по текстам, доли считает
// один раз в finish. Разнесено намеренно — единой функцией это нечитаемо.
type shapeAcc struct {
	runes, sentences, paragraphs []int

	totalRunes, totalWords, totalWordRunes, totalSentences, totalSmileys int
	capsWords                                                            int

	// счётчики ТЕКСТОВ с признаком (не вхождений)
	noFinal, allLower, startsLower, yo, emoji, urls, sadParens, addressed int
	endsQuestion, hasQuote, hasDigits                                     int

	sentWords []int          // длина каждого предложения в словах — ритм фразы
	person    map[string]int // я/ты/мы/вы

	punct       map[rune]int
	parenHas    map[string]int
	markup      map[string]int
	smileys     map[string]int
	smileyTexts map[string]int
	openings    map[string]int
}

func newShapeAcc() *shapeAcc {
	return &shapeAcc{
		punct: map[rune]int{}, parenHas: map[string]int{}, markup: map[string]int{},
		smileys: map[string]int{}, smileyTexts: map[string]int{}, openings: map[string]int{},
		person: map[string]int{},
	}
}

// personGroups — местоимения по лицам. Формы перечислены списком, а не
// вычисляются: морфологии в проекте нет и ради четырёх групп её тянуть незачем.
var personGroups = map[string][]string{
	"я":  {"я", "меня", "мне", "мной", "мною", "мой", "моя", "моё", "мое", "мои", "моего", "моей", "моим", "моих", "моему"},
	"ты": {"ты", "тебя", "тебе", "тобой", "тобою", "твой", "твоя", "твоё", "твое", "твои", "твоего", "твоей", "твоим", "твоих"},
	"мы": {"мы", "нас", "нам", "нами", "наш", "наша", "наше", "наши", "нашего", "нашей", "нашим", "наших"},
	"вы": {"вы", "вас", "вам", "вами", "ваш", "ваша", "ваше", "ваши", "вашего", "вашей", "вашим", "ваших"},
}

// personOf — лицо словоформы ("" — не местоимение). Построено один раз.
var personOf = func() map[string]string {
	m := map[string]string{}
	for group, forms := range personGroups {
		for _, f := range forms {
			m[f] = group
		}
	}
	return m
}()

// addRhythm — ритм фразы и лица. Ровный поток предложений одной длины — главный
// признак машинного текста, поэтому длины предложений копятся поштучно: нужны
// разброс и доли краёв, а не среднее.
func (a *shapeAcc) addRhythm(text string, r []rune) {
	for _, s := range splitSentences(text) {
		if n := sentWordCount(s); n > 0 {
			a.sentWords = append(a.sentWords, n)
		}
	}
	forEachWord(text, func(w []rune) {
		if p := personOf[strings.ToLower(string(w))]; p != "" {
			a.person[p]++
		}
	})
	if endsWithQuestion(r) {
		a.endsQuestion++
	}
	if strings.ContainsAny(text, `"«»`) || dialogDashRe.MatchString(text) {
		a.hasQuote++
	}
	if strings.ContainsAny(text, "0123456789") {
		a.hasDigits++
	}
}

// dialogDashRe — реплика с тире в начале строки.
var dialogDashRe = regexp.MustCompile(`(?m)^\s*[-—]\s`)

// sentWordCount — слов в предложении. Намеренно НЕ через forEachWord: тот требует
// букв ≥2 и не видит цифр, из-за чего «Я пошёл в 2019 году» становится двумя
// словами и попадает в «рубленые». Для ритма нужен честный счёт токенов.
func sentWordCount(s string) int {
	n, inWord := 0, false
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			if !inWord {
				n++
				inWord = true
			}
			continue
		}
		inWord = false
	}
	return n
}

// splitSentences — предложения по знакам конца и переводам строк. Тот же разрез,
// что у countSentences, но с самими строками: они нужны для ритма.
func splitSentences(text string) []string {
	fields := strings.FieldsFunc(text, func(c rune) bool {
		return c == '.' || c == '!' || c == '?' || c == '…' || c == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// endsWithQuestion — последний значащий знак текста вопросительный (скобки-улыбки
// и кавычки после него не считаются концом).
func endsWithQuestion(r []rune) bool {
	for i := len(r) - 1; i >= 0; i-- {
		switch r[i] {
		case ' ', '\n', '\t', '\r', ')', '(', '"', '»', '\'':
			continue
		case '?':
			return true
		default:
			return false
		}
	}
	return false
}

// addLengths — длины: руны, предложения, абзацы, слова.
func (a *shapeAcc) addLengths(text string, r []rune) {
	a.runes = append(a.runes, len(r))
	a.totalRunes += len(r)
	sn := countSentences(text)
	a.sentences = append(a.sentences, sn)
	a.totalSentences += sn
	a.paragraphs = append(a.paragraphs, countParagraphs(text))
	forEachWord(text, func(w []rune) {
		a.totalWords++
		a.totalWordRunes += len(w)
	})
	a.capsWords += countCapsWords(text)
}

// addPunct — пунктуация и скобочная подпись.
func (a *shapeAcc) addPunct(r []rune) {
	for _, c := range r {
		if _, ok := punctKeys[c]; ok {
			a.punct[c]++
		}
	}
	for _, b := range runBuckets(r, ')') {
		a.parenHas[b]++
	}
	if maxRun(r, '(') >= 2 {
		a.sadParens++
	}
}

// addTraits — признаки текста целиком: регистр, финал, «ё», эмодзи, ссылки.
func (a *shapeAcc) addTraits(text string, r []rune) {
	if lacksFinalTerminator(r) {
		a.noFinal++
	}
	if isAllLower(r) {
		a.allLower++
	}
	if firstLetterLower(r) {
		a.startsLower++
	}
	if strings.ContainsAny(text, "ёЁ") {
		a.yo++
	}
	if hasEmoji(r) {
		a.emoji++
	}
	if strings.Contains(text, "http") {
		a.urls++
	}
}

// addMarkup — разметка сайта и смайлы: часть голоса, не мусор (normalizeStyle их
// тоже сохраняет, то есть они входят в измеряемый атрибутором сигнал).
func (a *shapeAcc) addMarkup(text string) {
	for _, tag := range markupTags {
		if strings.Contains(text, tag) {
			a.markup[tag]++
		}
	}
	seen := map[string]bool{}
	for _, m := range smileyRe.FindAllStringSubmatch(text, -1) {
		a.smileys[m[1]]++
		a.totalSmileys++
		if !seen[m[1]] {
			seen[m[1]] = true
			a.smileyTexts[m[1]]++
		}
	}
}

// addOpening — обращение и зачин. Зачин считается по телу БЕЗ обращения: иначе
// «чем начинает» вырождается в список ников собеседников.
//
// Префикс сверяется с реальными никами (nicks). Без сверки love.AddressPrefix
// считает обращением ЛЮБУЮ раннюю запятую («всё как всегда, ничего нового»), а
// от этой доли зависит, подставлять ли ник в черновик, — завышать её нельзя.
// nicks == nil — сверка выключена (тогда доля читается как верхняя оценка).
func (a *shapeAcc) addOpening(text, kind string, nicks map[string]bool) {
	body := text
	if kind == "comments" {
		if pref := love.AddressPrefix(text); pref != "" && (nicks == nil || nicks[pref]) {
			a.addressed++
			// Режем по самой запятой: AddressPrefix отдаёт ник в нижнем регистре,
			// и TrimPrefix по нему не совпал бы с исходным «Аня,».
			if i := strings.IndexByte(body, ','); i >= 0 {
				body = strings.TrimSpace(body[i+1:])
			}
		}
	}
	if w := firstWord(body); w != "" {
		a.openings[w]++
	}
}

func (a *shapeAcc) finish(sh *VoiceShape, texts int) {
	n := float64(texts)
	sh.Runes, sh.Sentences, sh.Paragraphs = distOf(a.runes), distOf(a.sentences), distOf(a.paragraphs)
	if a.totalSentences > 0 {
		sh.WordsPerSentence = round2(float64(a.totalWords) / float64(a.totalSentences))
	}
	if a.totalWords > 0 {
		sh.WordRunes = round2(float64(a.totalWordRunes) / float64(a.totalWords))
		sh.AllCapsWords = round4(float64(a.capsWords) / float64(a.totalWords))
	}
	if a.totalRunes > 0 {
		for c, cnt := range a.punct {
			sh.Punct[string(c)] = round2(float64(cnt) * 1000 / float64(a.totalRunes))
		}
	}
	for b, cnt := range a.parenHas {
		sh.ParenRuns[b] = round4(float64(cnt) / n)
	}
	for tag, cnt := range a.markup {
		sh.Markup[tag] = round4(float64(cnt) / n)
	}
	sh.SadParens = round4(float64(a.sadParens) / n)
	sh.NoFinalPunct = round4(float64(a.noFinal) / n)
	sh.AllLower = round4(float64(a.allLower) / n)
	sh.StartsLower = round4(float64(a.startsLower) / n)
	sh.YoRate = round4(float64(a.yo) / n)
	sh.EmojiRate = round4(float64(a.emoji) / n)
	sh.URLRate = round4(float64(a.urls) / n)
	sh.SmileyRate = round2(float64(a.totalSmileys) / n)
	if sh.Kind == "comments" {
		sh.AddressPrefix = round4(float64(a.addressed) / n)
	}
	a.finishRhythm(sh, n)
	sh.TopSmileys = topCounts(a.smileys, a.smileyTexts, texts, 10)
	sh.TopOpenings = topCounts(a.openings, a.openings, texts, 8)
}

// measureShape меряет механику письма. nicks — множество реальных ников сайта
// (нижний регистр) для проверки обращений; nil — сверка выключена.
// finishRhythm — ритм фразы, лица и признаки живости в доли. Разброс считается
// как sd: именно он отличает человека от ровного машинного потока.
func (a *shapeAcc) finishRhythm(sh *VoiceShape, texts float64) {
	sh.SentWords = distOf(a.sentWords)
	if len(a.sentWords) > 0 {
		var mean float64
		for _, n := range a.sentWords {
			mean += float64(n)
		}
		mean /= float64(len(a.sentWords))
		var sq float64
		short, long := 0, 0
		for _, n := range a.sentWords {
			d := float64(n) - mean
			sq += d * d
			switch {
			case n <= shortSentWords:
				short++
			case n >= longSentWords:
				long++
			}
		}
		sh.SentWordSD = round2(math.Sqrt(sq / float64(len(a.sentWords))))
		sh.ShortSents = round4(float64(short) / float64(len(a.sentWords)))
		sh.LongSents = round4(float64(long) / float64(len(a.sentWords)))
	}
	if a.totalWords > 0 {
		sh.Person = map[string]float64{}
		for p, cnt := range a.person {
			sh.Person[p] = round2(100 * float64(cnt) / float64(a.totalWords))
		}
	}
	sh.EndsQuestion = round4(float64(a.endsQuestion) / texts)
	sh.HasQuote = round4(float64(a.hasQuote) / texts)
	sh.HasDigits = round4(float64(a.hasDigits) / texts)
}

// Границы «рубленой» и «длинной» фразы. Подобраны по живому корпусу: у Монаха
// 18% предложений ≤3 слов и 10% ≥18, у машинных черновиков 5% и 0%.
const (
	shortSentWords = 3
	longSentWords  = 18
)

func measureShape(texts []voiceText, kind string, nicks map[string]bool) VoiceShape {
	sh := VoiceShape{
		Kind: kind, Texts: len(texts),
		Punct: map[string]float64{}, ParenRuns: map[string]float64{}, Markup: map[string]float64{},
	}
	if len(texts) == 0 {
		return sh
	}
	a := newShapeAcc()
	for _, t := range texts {
		r := []rune(t.text)
		a.addLengths(t.text, r)
		a.addPunct(r)
		a.addTraits(t.text, r)
		a.addMarkup(t.text)
		a.addOpening(t.text, kind, nicks)
		a.addRhythm(t.text, r)
	}
	a.finish(&sh, len(texts))
	sh.From, sh.To = spanOf(texts)
	return sh
}

// SiteNickTokens — СЛОВА, из которых состоят ники сайта, нынешние и прежние.
//
// Понадобилось замером под карточку персонажа (эпик «народ»): у собеседницы,
// обращающейся по имени в 72 % реплик, характерными словами оказались ники
// соседей по треду. Для отчёта это мелочь, а для карточки — двойная беда:
// список уходит в промпт как «привычные слова», и персонаж начинает звать
// соседей настоящими никами живых людей.
//
// Именно СЛОВА, а не ники целиком: «Хатуль мадан» встречается в тексте двумя
// токенами, и сверка с целым ником не ловит ни одного. И именно с ИСТОРИЕЙ:
// users.name хранит только текущий ник, а обращались к человеку тем, который
// был у него тогда (nick_history завели ровно потому, что ники здесь меняют
// десятками).
func (s *Store) SiteNickTokens(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT name FROM users WHERE name != ''
		UNION
		SELECT DISTINCT nick FROM nick_history WHERE nick != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var nick string
		if err := rows.Scan(&nick); err != nil {
			return nil, err
		}
		forEachWord(nick, func(w []rune) { out[string(w)] = true })
	}
	return out, rows.Err()
}

// siteNicks — все ники сайта в нижнем регистре. Нужен для проверки обращений;
// ~22 тыс. строк, один запрос за сборку карты.
func (s *Store) siteNicks(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT name FROM users WHERE name != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(n))] = true
	}
	return out, rows.Err()
}

// punctKeys — знаки, чью частоту меряем (на 1000 рун).
var punctKeys = map[rune]struct{}{
	',': {}, '.': {}, '!': {}, '?': {}, '…': {}, '—': {}, '-': {},
	':': {}, ';': {}, '«': {}, '"': {}, '(': {}, ')': {},
}

var markupTags = []string{"[b]", "[i]", "[u]"}

// countSentences — грубое деление на предложения (терминаторы . ! ? … и перевод
// строки, серии схлопываются). Лингвистически неточно и не должно быть точным:
// число служит целью и диффом, а обе стороны меряются одной функцией.
func countSentences(s string) int {
	n, inTerm, seen := 0, false, false
	for _, c := range s {
		switch {
		case c == '.' || c == '!' || c == '?' || c == '…' || c == '\n':
			if !inTerm && seen {
				n++
			}
			inTerm = true
		case unicode.IsSpace(c):
			// пробел не закрывает и не открывает предложение
		default:
			inTerm = false
			seen = true
		}
	}
	if !inTerm && seen {
		n++
	}
	if n == 0 && seen {
		n = 1
	}
	return n
}

// countParagraphs — абзацы разделяются пустой строкой.
func countParagraphs(s string) int {
	n := 0
	for _, part := range strings.Split(s, "\n\n") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// runBuckets — какие длины серий символа c встречаются в тексте («1","2","3","4+»).
func runBuckets(r []rune, c rune) []string {
	seen := map[string]bool{}
	run := 0
	flush := func() {
		if run == 0 {
			return
		}
		b := "4+"
		if run < 4 {
			b = fmt.Sprintf("%d", run)
		}
		seen[b] = true
		run = 0
	}
	for _, x := range r {
		if x == c {
			run++
			continue
		}
		flush()
	}
	flush()
	out := make([]string, 0, len(seen))
	for b := range seen {
		out = append(out, b)
	}
	return out
}

func maxRun(r []rune, c rune) int {
	best, run := 0, 0
	for _, x := range r {
		if x == c {
			run++
			if run > best {
				best = run
			}
			continue
		}
		run = 0
	}
	return best
}

// lacksFinalTerminator — текст не закрыт знаком конца предложения. Скобка-улыбка
// в конце («день))») считается НЕзакрытым: для генерации важно ровно это — ставит
// человек точку или обрывает фразу.
func lacksFinalTerminator(r []rune) bool {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			continue
		}
		switch r[i] {
		case '.', '!', '?', '…':
			return false
		}
		return true
	}
	return false
}

func isAllLower(r []rune) bool {
	letter := false
	for _, c := range r {
		if unicode.IsLetter(c) {
			letter = true
			if unicode.IsUpper(c) {
				return false
			}
		}
	}
	return letter
}

func firstLetterLower(r []rune) bool {
	for _, c := range r {
		if unicode.IsLetter(c) {
			return unicode.IsLower(c)
		}
	}
	return false
}

// hasEmoji — грубая проверка диапазонов. Без зависимости: нам нужно лишь знать,
// водятся ли эмодзи у автора вообще (корпус старше их, и модель насыпет своих).
func hasEmoji(r []rune) bool {
	for _, c := range r {
		switch {
		case c >= 0x1F300 && c <= 0x1FAFF,
			c >= 0x2600 && c <= 0x27BF,
			c == 0xFE0F,
			c >= 0x1F000 && c <= 0x1F0FF:
			return true
		}
	}
	return false
}

func countCapsWords(s string) int {
	n := 0
	forEachWord(s, func(w []rune) {
		upper, letters := 0, 0
		for _, c := range w {
			if unicode.IsLetter(c) {
				letters++
				if unicode.IsUpper(c) {
					upper++
				}
			}
		}
		if letters >= 2 && upper == letters {
			n++
		}
	})
	return n
}

// firstWord — первое слово текста в нижнем регистре (зачины автора).
func firstWord(s string) string {
	var out string
	forEachWord(s, func(w []rune) {
		if out == "" {
			out = strings.ToLower(string(w))
		}
	})
	return out
}

func topCounts(counts, texts map[string]int, total, limit int) []VoiceCount {
	if len(counts) == 0 || total == 0 {
		return nil
	}
	out := make([]VoiceCount, 0, len(counts))
	for k, n := range counts {
		out = append(out, VoiceCount{Text: k, N: n, Share: round4(float64(texts[k]) / float64(total))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func spanOf(texts []voiceText) (string, string) {
	from, to := "", ""
	for _, t := range texts {
		if t.pub == "" {
			continue
		}
		if from == "" || t.pub < from {
			from = t.pub
		}
		if to == "" || t.pub > to {
			to = t.pub
		}
	}
	return shortDateStr(from), shortDateStr(to)
}

// measureRhythm — часы и дни недели в поясе сайта.
func measureRhythm(texts []voiceText) VoiceRhythm {
	rh := VoiceRhythm{TZ: siteTZ}
	loc, err := time.LoadLocation(siteTZ)
	if err != nil {
		loc = time.UTC
		rh.TZ = "UTC"
	}
	total := 0
	for _, t := range texts {
		if t.pub == "" {
			continue
		}
		ts, err := time.Parse(time.RFC3339, t.pub)
		if err != nil {
			continue
		}
		local := ts.In(loc)
		rh.Hours[local.Hour()]++
		rh.Weekdays[int(local.Weekday())]++
		total++
	}
	if total == 0 {
		return rh
	}
	avg := float64(total) / 24
	for h, n := range rh.Hours {
		if float64(n) > avg {
			rh.Peak = append(rh.Peak, h)
		}
	}
	return rh
}

// --- характерная лексика ---

// distinctiveWords ранжирует слова автора по tf·idf, беря IDF из УЖЕ построенного
// lexis_meta по корзине хэша слова. Так вес слова считается в том же пространстве,
// что и у атрибутора, и без прохода по всему корпусу (10,7 млн комментариев).
//
// Пределы, которые обязан печатать отчёт: корзины общие, поэтому редкая корзина
// может подарить вес частому слову; поймать мы умеем только ВНУТРИавторские
// коллизии (флаг Collision). forEachWord требует букв ≥2 — «и», «в», «не»
// невидимы (это пробел слоя funcwords, не наш).
func (s *Store) distinctiveWords(ctx context.Context, corpus []voiceText, genre string, top int) ([]VoiceWord, map[string]int, string, error) {
	counts, docs := wordCounts(corpus)
	idf, dims, err := s.loadLexisIDF(ctx, genre)
	if err != nil {
		return nil, counts, "", err
	}
	if dims == 0 || len(idf) == 0 {
		return nil, counts, "лексический слой жанра не построен (personas lexis build -genre " + genre + ")", nil
	}

	bucketWords := map[uint64]int{}
	for w := range counts {
		bucketWords[hashWordRunes([]rune(w))%uint64(dims)]++
	}
	words := make([]VoiceWord, 0, len(counts))
	for w, c := range counts {
		// Слово из одного текста — не привычка, а разовая тема: в список оно
		// попадает только высоким tf·idf и тянет генерацию в пересказ образца.
		if docs[w] < 2 && len(corpus) > 2 {
			continue
		}
		b := hashWordRunes([]rune(w)) % uint64(dims)
		wIDF := float64(idf[b])
		words = append(words, VoiceWord{
			Word: w, Count: c, Docs: docs[w], IDF: round2(wIDF),
			TFIDF:     round2((1 + math.Log(float64(c))) * wIDF),
			Collision: bucketWords[b] > 1,
		})
	}
	sort.Slice(words, func(i, j int) bool {
		if words[i].TFIDF != words[j].TFIDF {
			return words[i].TFIDF > words[j].TFIDF
		}
		return words[i].Word < words[j].Word
	})
	if len(words) > top {
		words = words[:top]
	}
	return words, counts, "", nil
}

// wordCounts — слова корпуса: сколько раз всего и в скольких разных текстах.
// Разметка сайта снимается: из «[color=red]» forEachWord достаёт «color» и «red»,
// и такие токены возглавляли список характерных слов, хотя это не слова автора, а
// теги. Частота разметки в промпт идёт отдельной строкой измерений.
func wordCounts(corpus []voiceText) (counts, docs map[string]int) {
	counts, docs = map[string]int{}, map[string]int{}
	for _, t := range corpus {
		seen := map[string]bool{}
		forEachWord(stripSiteMarkup(t.text), func(w []rune) {
			s := string(w)
			counts[s]++
			if !seen[s] {
				seen[s] = true
				docs[s]++
			}
		})
	}
	return counts, docs
}

// siteMarkupRe — теги сайта: [b]…[/b], [color=red], :::smile:::.
var siteMarkupRe = regexp.MustCompile(`\[[^\[\]\n]{1,40}\]|:::[^:\s]{1,40}:::`)

func stripSiteMarkup(text string) string { return siteMarkupRe.ReplaceAllString(text, " ") }

// vocabRate — слов из списка характерных на 100 слов текста. Норма автора против
// той же величины у черновика — прямая мера набивки: модель, получив список,
// склонна насыпать его вместо манеры (см. отрицательный styleZ при
// положительном lexZ в живых замерах).
func vocabRate(corpus []voiceText, vocab []VoiceWord) float64 {
	if len(vocab) == 0 {
		return 0
	}
	in := make(map[string]bool, len(vocab))
	for _, v := range vocab {
		in[v.Word] = true
	}
	var hits, total int
	for _, t := range corpus {
		forEachWord(stripSiteMarkup(t.text), func(w []rune) {
			total++
			if in[string(w)] {
				hits++
			}
		})
	}
	if total == 0 {
		return 0
	}
	return round2(100 * float64(hits) / float64(total))
}

// VocabUse — та же мера для одного текста (черновика) плюс абсолютное число
// попаданий. Без него короткая реплика отбраковывалась бы за одно уместное слово:
// на 20 словах это сразу 5 на 100.
func VocabUse(text string, vocab []VoiceWord) (rate float64, hits int) {
	if len(vocab) == 0 {
		return 0, 0
	}
	in := make(map[string]bool, len(vocab))
	for _, v := range vocab {
		in[v.Word] = true
	}
	var total int
	forEachWord(stripSiteMarkup(text), func(w []rune) {
		total++
		if in[string(w)] {
			hits++
		}
	})
	if total == 0 {
		return 0, hits
	}
	return round2(100 * float64(hits) / float64(total)), hits
}
