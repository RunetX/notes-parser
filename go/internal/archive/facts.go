package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// migrateV8SQL — факты о личностях (Фаза 3): интересы и черты, извлечённые из
// текстов заметок и комментариев. Слой производный и обратимый (пересчитывается
// с нуля), сырые comments/notes не трогает. Ключ identity — 'p<id>'|'u<id>' из
// v_identity; он производен от кластеризации, поэтому порядок прогона: сначала
// финализировать personas cluster, потом facts (после переклейки — пересканить,
// llm-строки ремапятся при импорте по account_ids из выгрузки кандидатов).
const migrateV8SQL = `
CREATE TABLE identity_facts (
    identity    TEXT NOT NULL,                       -- 'p<id>' | 'u<id>' (как в v_identity)
    topic       TEXT NOT NULL,                       -- ключ темы: dogs|cats|sea|…
    source      TEXT NOT NULL,                       -- lexicon | llm | manual
    polarity    TEXT NOT NULL DEFAULT 'mentions',    -- likes|dislikes|owns|mentions
    score       REAL NOT NULL,                       -- сила сигнала [0..1] (lexicon) / confidence (llm)
    hits        INTEGER NOT NULL DEFAULT 0,          -- комментариев/заметок с темой
    notes_count INTEGER NOT NULL DEFAULT 0,          -- в скольких РАЗНЫХ заметках (анти-цитирование)
    evidence    TEXT NOT NULL DEFAULT '[]',          -- JSON [{comment_id,note_id,quote,marker}]
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (identity, topic, source)
);
CREATE INDEX idx_identity_facts_topic ON identity_facts(topic);
`

// Источники фактов: лексиконный скан, внешняя LLM-разметка, ручная правка.
const (
	FactSourceLexicon = "lexicon"
	FactSourceLLM     = "llm"
	FactSourceManual  = "manual"
)

// Полярности факта. Лексикон надёжно даёт mentions (интерес/упоминание);
// likes/dislikes/owns из явных маркеров в том же предложении — грубая эвристика,
// которую уточняет LLM-проход (facts candidates → import).
const (
	PolarityLikes    = "likes"
	PolarityDislikes = "dislikes"
	PolarityOwns     = "owns" // «у меня есть», «езжу», «была на» — обладание/практика
	PolarityMentions = "mentions"
)

// TopicLexicon — тема со списком основ. Основа с завершающей '*' — префикс
// (совпадает с любым продолжением из букв: «собак*» → собака/собачий), без '*' —
// точная словоформа («пса»). Граница слова слева обязательна: Go '\b' работает
// только для ASCII, поэтому компилируется (?:^|[^\p{L}]) — иначе «задача»
// ловилась бы префиксом «дач*» (замерено: без границ 80–85% срабатываний — мусор).
type TopicLexicon struct {
	Key   string   `json:"key"`
	Title string   `json:"title"` // русское название для отчётов
	Stems []string `json:"stems"`
}

// compile собирает регэксп темы: граница слова, альтернативы основ, для
// префиксов — хвост из букв, справа — граница (у префиксов она автоматична).
func (t TopicLexicon) compile() (*regexp.Regexp, error) {
	if len(t.Stems) == 0 {
		return nil, fmt.Errorf("тема %q: пустой список основ", t.Key)
	}
	alts := make([]string, 0, len(t.Stems))
	for _, s := range t.Stems {
		if p, ok := strings.CutSuffix(s, "*"); ok {
			alts = append(alts, regexp.QuoteMeta(p)+`\p{L}*`)
			continue
		}
		alts = append(alts, regexp.QuoteMeta(s))
	}
	return regexp.Compile(`(?i)(?:^|[^\p{L}])(?:` + strings.Join(alts, "|") + `)(?:[^\p{L}]|$)`)
}

// DefaultTopics — встроенный набор тем (переопределяется -topics file.json).
// Основы подобраны под словограничное совпадение: короткие корни («кот», «пёс»)
// заданы точными формами, чтобы префикс не съедал «который»/«психолог».
func DefaultTopics() []TopicLexicon {
	return []TopicLexicon{
		{Key: "dogs", Title: "собаки", Stems: []string{
			"собак*", "собач*", "щенк*", "щенок", "псин*",
			"пёс", "пес", "пса", "псу", "псом", "псе", "псы", "псов",
		}},
		{Key: "cats", Title: "кошки", Stems: []string{
			"кошк*", "кошач*", "котёнок", "котенок", "котёнк*", "котенк*", "котят*", "котик*",
			"кот", "кота", "коту", "котом", "коте", "коты", "котов", "котам", "котах",
		}},
		{Key: "sea", Title: "море и отпуск", Stems: []string{
			"море", "моря", "морю", "морем", "пляж*", "курорт*", "отпуск*",
			"загора*", "загорел*", "путёвк*", "путевк*", "турци*", "тайланд*", "таиланд*",
			"египет", "египт*", "сочи", "анап*", "крым", "крыму", "крыма",
		}},
		{Key: "dacha", Title: "дача и огород", Stems: []string{
			"дач*", "огород*", "грядк*", "теплиц*", "рассад*", "урожа*",
		}},
		{Key: "fishing", Title: "рыбалка", Stems: []string{
			"рыбалк*", "рыбач*", "удочк*", "спиннинг*", "наживк*",
		}},
		{Key: "cars", Title: "машины", Stems: []string{
			"машин*", "автомобил*", "авто", "гараж*", "бензин*",
			"за рулём", "за рулем", "водител*",
		}},
		{Key: "kids", Title: "дети и внуки", Stems: []string{
			"дети", "детей", "детям", "детьми", "детях", "детишк*", "детск*",
			"ребёнок", "ребенок", "ребёнка", "ребенка", "ребёнку", "ребенку",
			"сын", "сына", "сыну", "сыном", "сынов*", "дочь", "дочер*", "дочк*",
			"внук", "внук*", "внучк*",
		}},
		{Key: "sport", Title: "спорт", Stems: []string{
			"спорт*", "тренировк*", "тренаж*", "фитнес*", "пробежк*", "бегаю",
			"качалк*", "лыж*", "велосипед*", "бассейн*",
		}},
		{Key: "cooking", Title: "кухня и готовка", Stems: []string{
			"готовлю", "готовк*", "рецепт*", "борщ*", "пирог*", "пирожк*",
			"испекл*", "выпечк*", "блины", "блинчик*",
		}},
		{Key: "alcohol", Title: "рюмочная тема", Stems: []string{
			"пиво", "пива", "пиву", "пивом", "пивко", "вино", "вина", "вином",
			"коньяк*", "водк*", "рюмк*", "шампанск*", "наливк*",
		}},
		{Key: "music", Title: "музыка", Stems: []string{
			"музык*", "гитар*", "концерт*", "песн*", "пою", "танц*",
		}},
		{Key: "travel", Title: "путешествия", Stems: []string{
			"путешеств*", "поездк*", "автостоп*", "поход*", "палатк*",
		}},
	}
}

// Маркеры полярности в предложении с темой. Порядок проверки важен: сначала
// dislikes (снимает «не люблю»/«не нравится» раньше, чем likes увидит «люблю»),
// потом likes, потом owns. Всё это грубая эвристика для первичной разметки —
// отрицания в соседнем предложении и иронию разбирает LLM-проход.
var (
	factDislikesRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:ненави\p{L}*|терпеть не могу|не люблю|не нравится|не нравятся|не хочу|боюсь|бесит|бесят|надоел\p{L}*|раздража\p{L}*|достал\p{L}*)(?:[^\p{L}]|$)`)
	factLikesRe    = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:люблю|обожаю|нравится|нравятся|нравилось|тащусь|мечтаю|хочу|кайф\p{L}*|здорово|классно)(?:[^\p{L}]|$)`)
	factOwnsRe     = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:у меня|у нас|завёл\p{L}*|завел\p{L}*|держу|держим|езжу|ездим|еду|едем|поеду|поедем|была на|был на|были на|летал\p{L}*|летим|лечу)(?:[^\p{L}]|$)`)
)

// FactEvidence — цитата-обоснование факта. CommentID 0 — цитата из текста заметки.
type FactEvidence struct {
	CommentID int64  `json:"comment_id,omitempty"`
	NoteID    int64  `json:"note_id"`
	Quote     string `json:"quote"`
	Marker    string `json:"marker,omitempty"` // likes|dislikes|owns — если в предложении был маркер
}

// FactScanParams — настройки лексиконного скана.
type FactScanParams struct {
	MinHits     int // мин. комментариев/заметок с темой для записи факта
	MinNotes    int // мин. разных заметок (защита от цитирования одной перепалки)
	EvidencePer int // сколько цитат хранить на факт
}

// TopicScanStat — итог по одной теме.
type TopicScanStat struct {
	Hits int // комментариев/заметок с темой (до порога)
	Rows int // записано фактов (пар личность×тема)
}

// FactScanStats — итог ScanFacts.
type FactScanStats struct {
	Comments int // просмотрено комментариев
	Notes    int // просмотрено заметок
	Rows     int // записано фактов всего
	PerTopic map[string]TopicScanStat
}

// factAcc — аккумулятор одного факта (личность×тема) при потоковом скане.
type factAcc struct {
	hits     int
	likes    int
	dislikes int
	owns     int
	noteIDs  map[int64]bool
	evidence []FactEvidence
}

// compiledTopic — тема с готовым регэкспом.
type compiledTopic struct {
	TopicLexicon
	re *regexp.Regexp
}

// factScanner — состояние потокового скана: темы, аккумуляторы, статистика.
type factScanner struct {
	topics      []compiledTopic
	acc         map[string]*factAcc // ключ identity+"\x00"+topic
	evidencePer int
	perTopic    map[string]TopicScanStat
}

// scannedText — одна строка источника (комментарий или заметка) при скане.
type scannedText struct {
	ident  string
	id     int64
	noteID int64
	text   string
	isNote bool
}

// ScanFacts — лексиконный скан интересов: один потоковый проход по всем
// комментариям и заметкам (образец — accumulateStyle), словограничное
// совпадение тем, агрегация на личности через v_identity. Факт пишется при
// hits ≥ MinHits и notes_count ≥ MinNotes; полярность — по явным маркерам в
// предложении с темой, иначе mentions. Идемпотентно: строки source='lexicon'
// пересчитываются с нуля, llm/manual не трогаются.
func (s *Store) ScanFacts(ctx context.Context, topics []TopicLexicon, p FactScanParams, now time.Time) (FactScanStats, error) {
	st := FactScanStats{PerTopic: map[string]TopicScanStat{}}
	sc := &factScanner{acc: map[string]*factAcc{}, evidencePer: p.EvidencePer, perTopic: st.PerTopic}
	for _, t := range topics {
		re, err := t.compile()
		if err != nil {
			return st, err
		}
		sc.topics = append(sc.topics, compiledTopic{TopicLexicon: t, re: re})
	}
	identity, err := s.identityMap(ctx)
	if err != nil {
		return st, err
	}

	if err := s.scanFactSource(ctx,
		`SELECT id, note_id, author_id, text FROM comments WHERE author_id != 0`,
		identity, sc, false, &st.Comments); err != nil {
		return st, err
	}
	if err := s.scanFactSource(ctx,
		`SELECT id, id, author_id, text FROM notes WHERE author_id IS NOT NULL AND author_id != 0`,
		identity, sc, true, &st.Notes); err != nil {
		return st, err
	}
	return st, s.writeLexiconFacts(ctx, sc.acc, p, &st, now)
}

// scanFactSource прогоняет через сканер один источник текстов.
func (s *Store) scanFactSource(ctx context.Context, query string, identity map[int64]string, sc *factScanner, isNote bool, counter *int) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, noteID, authorID int64
		var text string
		if err := rows.Scan(&id, &noteID, &authorID, &text); err != nil {
			return err
		}
		*counter++
		if ident := identity[authorID]; ident != "" && text != "" {
			sc.scanText(scannedText{ident: ident, id: id, noteID: noteID, text: text, isNote: isNote})
		}
	}
	return rows.Err()
}

// scanText проверяет один текст на все темы.
func (sc *factScanner) scanText(row scannedText) {
	for i := range sc.topics {
		sc.matchTopic(&sc.topics[i], row)
	}
}

// matchTopic проверяет текст на одну тему и обновляет аккумулятор: первый матч
// в тексте (одно упоминание на комментарий — метрика устойчивее сырых матчей),
// предложение вокруг — на маркеры полярности, цитата — в evidence.
func (sc *factScanner) matchTopic(t *compiledTopic, row scannedText) {
	loc := t.re.FindStringIndex(row.text)
	if loc == nil {
		return
	}
	ts := sc.perTopic[t.Key]
	ts.Hits++
	sc.perTopic[t.Key] = ts

	key := row.ident + "\x00" + t.Key
	a := sc.acc[key]
	if a == nil {
		a = &factAcc{noteIDs: map[int64]bool{}}
		sc.acc[key] = a
	}
	a.hits++
	a.noteIDs[row.noteID] = true

	sentence := sentenceAround(row.text, loc[0], loc[1])
	marker := a.countMarker(sentence)
	ev := FactEvidence{NoteID: row.noteID, Quote: excerpt(sentence, 200), Marker: marker}
	if !row.isNote {
		ev.CommentID = row.id
	}
	a.addEvidence(ev, sc.evidencePer)
}

// countMarker ищет маркер полярности в предложении и обновляет счётчики.
// Порядок важен: dislikes раньше likes («не люблю» не должно засчитаться как
// «люблю»).
func (a *factAcc) countMarker(sentence string) string {
	switch {
	case factDislikesRe.MatchString(sentence):
		a.dislikes++
		return PolarityDislikes
	case factLikesRe.MatchString(sentence):
		a.likes++
		return PolarityLikes
	case factOwnsRe.MatchString(sentence):
		a.owns++
		return PolarityOwns
	}
	return ""
}

// addEvidence держит до cap цитат, предпочитая маркированные (с полярностью)
// немаркированным: при заполнении маркированная вытесняет первую без маркера.
func (a *factAcc) addEvidence(ev FactEvidence, cap int) {
	if cap <= 0 {
		return
	}
	if len(a.evidence) < cap {
		a.evidence = append(a.evidence, ev)
		return
	}
	if ev.Marker == "" {
		return
	}
	for i := range a.evidence {
		if a.evidence[i].Marker == "" {
			a.evidence[i] = ev
			return
		}
	}
}

// sentenceAround — предложение вокруг совпадения [start,end): расширение до
// ближайших границ предложений, но не дальше capRunes рун в каждую сторону.
func sentenceAround(text string, start, end int) string {
	const capBytes = 320 // ~160 кириллических рун в каждую сторону
	lo := start
	for lo > 0 && start-lo < capBytes {
		r := text[lo-1]
		if r == '.' || r == '!' || r == '?' || r == '\n' || r == ';' {
			break
		}
		lo--
	}
	hi := end
	for hi < len(text) && hi-end < capBytes {
		r := text[hi]
		if r == '.' || r == '!' || r == '?' || r == '\n' || r == ';' {
			hi++ // включаем знак конца предложения
			break
		}
		hi++
	}
	// не рвать UTF-8: сдвиг к началу руны
	for lo > 0 && lo < len(text) && text[lo]&0xC0 == 0x80 {
		lo--
	}
	for hi < len(text) && text[hi]&0xC0 == 0x80 {
		hi++
	}
	return strings.TrimSpace(text[lo:hi])
}

// identityMap — карта анкета→личность по v_identity (один запрос, ~20k строк).
func (s *Store) identityMap(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, identity FROM v_identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]string{}
	for rows.Next() {
		var uid int64
		var ident string
		if err := rows.Scan(&uid, &ident); err != nil {
			return nil, err
		}
		m[uid] = ident
	}
	return m, rows.Err()
}

// writeLexiconFacts перезаписывает строки source='lexicon' по аккумулятору.
func (s *Store) writeLexiconFacts(ctx context.Context, acc map[string]*factAcc, p FactScanParams, st *FactScanStats, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM identity_facts WHERE source = ?`, FactSourceLexicon); err != nil {
		return err
	}
	nowStr := fmtTime(now)
	for key, a := range acc {
		if a.hits < p.MinHits || len(a.noteIDs) < p.MinNotes {
			continue
		}
		ident, topic, _ := strings.Cut(key, "\x00")
		evJSON, err := json.Marshal(a.evidence)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO identity_facts (identity, topic, source, polarity, score, hits, notes_count, evidence, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ident, topic, FactSourceLexicon, decidePolarity(a.likes, a.dislikes, a.owns),
			factScore(a.hits, len(a.noteIDs)), a.hits, len(a.noteIDs), string(evJSON), nowStr); err != nil {
			return fmt.Errorf("факт %s/%s: %w", ident, topic, err)
		}
		st.Rows++
		ts := st.PerTopic[topic]
		ts.Rows++
		st.PerTopic[topic] = ts
	}
	return tx.Commit()
}

// decidePolarity — итог по счётчикам маркеров: явное «не люблю» перевешивает,
// одиночный маркер не считается (шум), без маркеров — mentions.
func decidePolarity(likes, dislikes, owns int) string {
	switch {
	case dislikes >= 2 && dislikes > likes:
		return PolarityDislikes
	case likes >= 2 && likes > dislikes:
		return PolarityLikes
	case owns >= 2:
		return PolarityOwns
	default:
		return PolarityMentions
	}
}

// factScore — грубая сила сигнала [0..1]: растёт с числом упоминаний и особенно
// с числом разных заметок. Не вероятность — ранжировка для отчётов.
func factScore(hits, notes int) float64 {
	s := 0.1*float64(hits) + 0.15*float64(notes)
	if s > 1 {
		return 1
	}
	return s
}

// --- чтение фактов (portrait/report) ---

// IdentityFact — факт для отчётов: лучший источник на тему (manual > llm > lexicon).
type IdentityFact struct {
	Identity   string         `json:"identity"`
	Topic      string         `json:"topic"`
	Source     string         `json:"source"`
	Polarity   string         `json:"polarity"`
	Score      float64        `json:"score"`
	Hits       int            `json:"hits"`
	NotesCount int            `json:"notes_count"`
	Evidence   []FactEvidence `json:"evidence,omitempty"`
}

// sourceRank — приоритет источника при схлопывании до одной строки на тему.
func sourceRank(source string) int {
	switch source {
	case FactSourceManual:
		return 3
	case FactSourceLLM:
		return 2
	default:
		return 1
	}
}

// IdentityFacts — факты одной личности, по теме — лучший источник, сильные впереди.
func (s *Store) IdentityFacts(ctx context.Context, identity string) ([]IdentityFact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity, topic, source, polarity, score, hits, notes_count, evidence
		FROM identity_facts WHERE identity = ?`, identity)
	if err != nil {
		return nil, err
	}
	facts, err := scanFactRows(rows)
	if err != nil {
		return nil, err
	}
	return bestFactPerTopic(facts), nil
}

// AllFacts — факты всех личностей (для report/graph), по теме — лучший источник.
// Возвращает карту identity → факты, сильные впереди.
func (s *Store) AllFacts(ctx context.Context) (map[string][]IdentityFact, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity, topic, source, polarity, score, hits, notes_count, evidence
		FROM identity_facts`)
	if err != nil {
		return nil, err
	}
	facts, err := scanFactRows(rows)
	if err != nil {
		return nil, err
	}
	byIdent := map[string][]IdentityFact{}
	for _, f := range facts {
		byIdent[f.Identity] = append(byIdent[f.Identity], f)
	}
	out := make(map[string][]IdentityFact, len(byIdent))
	for ident, list := range byIdent {
		out[ident] = bestFactPerTopic(list)
	}
	return out, nil
}

func scanFactRows(rows *sql.Rows) ([]IdentityFact, error) {
	defer rows.Close()
	var out []IdentityFact
	for rows.Next() {
		var f IdentityFact
		var evJSON string
		if err := rows.Scan(&f.Identity, &f.Topic, &f.Source, &f.Polarity,
			&f.Score, &f.Hits, &f.NotesCount, &evJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(evJSON), &f.Evidence); err != nil {
			f.Evidence = nil // повреждённый JSON цитат — не критично
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// bestFactPerTopic схлопывает до одной строки на тему (лучший источник) и
// сортирует: сильные впереди.
func bestFactPerTopic(facts []IdentityFact) []IdentityFact {
	best := map[string]IdentityFact{}
	for _, f := range facts {
		cur, ok := best[f.Topic]
		if !ok || sourceRank(f.Source) > sourceRank(cur.Source) {
			best[f.Topic] = f
		}
	}
	out := make([]IdentityFact, 0, len(best))
	for _, f := range best {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Topic < out[j].Topic
	})
	return out
}

// --- кандидаты для LLM и импорт ---

// FactCandidateEvidence — цитата с контекстом ±1 (родитель и первый ответ) для
// разбора отрицаний/иронии внешней LLM.
type FactCandidateEvidence struct {
	CommentID int64  `json:"comment_id,omitempty"`
	NoteID    int64  `json:"note_id"`
	Quote     string `json:"quote"`
	Marker    string `json:"marker,omitempty"`
	Parent    string `json:"parent,omitempty"` // выдержка родительского комментария
	Reply     string `json:"reply,omitempty"`  // выдержка первого ответа
}

// FactCandidate — одна пара личность×тема на LLM-разметку (строка JSONL).
type FactCandidate struct {
	Identity   string                  `json:"identity"`
	Label      string                  `json:"label"`
	AccountIDs []int64                 `json:"account_ids"` // для ремапа импорта после переклейки кластеров
	Topic      string                  `json:"topic"`
	TopicTitle string                  `json:"topic_title,omitempty"`
	Polarity   string                  `json:"lexicon_polarity"`
	Hits       int                     `json:"hits"`
	NotesCount int                     `json:"notes_count"`
	Evidence   []FactCandidateEvidence `json:"evidence"`
}

// FactCandidates выгружает lexicon-факты выше порогов с контекстом цитат —
// материал для LLM-нормализации полярности. titles — карта key→Title тем.
func (s *Store) FactCandidates(ctx context.Context, minHits, minNotes int, titles map[string]string) ([]FactCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.identity, COALESCE(a.label, ''), f.topic, f.polarity, f.hits, f.notes_count, f.evidence
		FROM identity_facts f
		LEFT JOIN v_persona_activity a ON a.identity = f.identity
		WHERE f.source = ? AND f.hits >= ? AND f.notes_count >= ?
		ORDER BY f.identity, f.topic`, FactSourceLexicon, minHits, minNotes)
	if err != nil {
		return nil, err
	}
	var out []FactCandidate
	for rows.Next() {
		var c FactCandidate
		var evJSON string
		if err := rows.Scan(&c.Identity, &c.Label, &c.Topic, &c.Polarity,
			&c.Hits, &c.NotesCount, &evJSON); err != nil {
			rows.Close()
			return nil, err
		}
		c.TopicTitle = titles[c.Topic]
		var evs []FactEvidence
		if err := json.Unmarshal([]byte(evJSON), &evs); err == nil {
			for _, ev := range evs {
				c.Evidence = append(c.Evidence, FactCandidateEvidence{
					CommentID: ev.CommentID, NoteID: ev.NoteID, Quote: ev.Quote, Marker: ev.Marker,
				})
			}
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range out {
		if err := s.fillFactContext(ctx, out[i].Evidence); err != nil {
			return nil, err
		}
		if out[i].AccountIDs, err = s.identityAccountIDs(ctx, out[i].Identity); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillFactContext дописывает цитатам родителя и первый ответ (по PK/индексам —
// дёшево даже на десятках тысяч цитат).
func (s *Store) fillFactContext(ctx context.Context, evs []FactCandidateEvidence) error {
	for i := range evs {
		if evs[i].CommentID == 0 {
			continue // цитата из текста заметки — родителя/ответа нет
		}
		var parent sql.NullString
		if err := s.db.QueryRowContext(ctx, `
			SELECT p.text FROM comments c JOIN comments p ON p.id = c.parent_id
			WHERE c.id = ?`, evs[i].CommentID).Scan(&parent); err == nil && parent.Valid {
			evs[i].Parent = excerpt(parent.String, 160)
		}
		var reply sql.NullString
		if err := s.db.QueryRowContext(ctx, `
			SELECT text FROM comments WHERE parent_id = ? ORDER BY id LIMIT 1`,
			evs[i].CommentID).Scan(&reply); err == nil && reply.Valid {
			evs[i].Reply = excerpt(reply.String, 160)
		}
	}
	return nil
}

// identityAccountIDs — id анкет личности (по v_identity).
func (s *Store) identityAccountIDs(ctx context.Context, identity string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id FROM v_identity WHERE identity = ? ORDER BY user_id`, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FactImport — размеченный LLM факт. Verdict "reject" — лексикон ошибся
// (цитирование, ирония, ложная тема): факт удаляется целиком (см. rejectFact).
// Identity ремапится по AccountIDs, если кластеры переклеены после выгрузки.
type FactImport struct {
	Identity   string         `json:"identity"`
	AccountIDs []int64        `json:"account_ids,omitempty"`
	Topic      string         `json:"topic"`
	Verdict    string         `json:"verdict,omitempty"` // confirm (по умолчанию) | reject
	Polarity   string         `json:"polarity"`
	Confidence float64        `json:"confidence"`
	Evidence   []FactEvidence `json:"evidence,omitempty"`
}

// FactImportStats — итог ImportFacts.
type FactImportStats struct {
	Written  int // записано llm-фактов
	Rejected int // отклонено разметкой (reject)
	Skipped  int // пропущено (личность не найдена, кривая полярность/тема)
	Remapped int // личность найдена через account_ids (после переклейки)
}

// ImportFacts заносит LLM-разметку как source='llm' (upsert по PK). hits и
// notes_count наследуются от lexicon-строки той же темы, если она есть.
func (s *Store) ImportFacts(ctx context.Context, items []FactImport, now time.Time) (FactImportStats, error) {
	var st FactImportStats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	for _, it := range items {
		if err := importOneFact(ctx, tx, it, nowStr, &st); err != nil {
			return st, err
		}
	}
	return st, tx.Commit()
}

// importOneFact разбирает один элемент разметки и обновляет статистику.
func importOneFact(ctx context.Context, tx *sql.Tx, it FactImport, nowStr string, st *FactImportStats) error {
	if it.Verdict == "reject" {
		return rejectFact(ctx, tx, it, st)
	}
	if it.Topic == "" || !validPolarity(it.Polarity) {
		st.Skipped++
		return nil
	}
	ident, remapped, err := resolveIdentity(ctx, tx, it.Identity, it.AccountIDs)
	if err != nil {
		return err
	}
	if ident == "" {
		st.Skipped++
		return nil
	}
	if remapped {
		st.Remapped++
	}
	evJSON, err := json.Marshal(nonNilEvidence(it.Evidence))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO identity_facts (identity, topic, source, polarity, score, hits, notes_count, evidence, updated_at)
		VALUES (?, ?, ?, ?, ?,
		        COALESCE((SELECT hits FROM identity_facts WHERE identity=? AND topic=? AND source=?), 0),
		        COALESCE((SELECT notes_count FROM identity_facts WHERE identity=? AND topic=? AND source=?), 0),
		        ?, ?)
		ON CONFLICT(identity, topic, source) DO UPDATE SET
			polarity = excluded.polarity, score = excluded.score,
			evidence = excluded.evidence, updated_at = excluded.updated_at`,
		ident, it.Topic, FactSourceLLM, it.Polarity, it.Confidence,
		ident, it.Topic, FactSourceLexicon,
		ident, it.Topic, FactSourceLexicon,
		string(evJSON), nowStr); err != nil {
		return fmt.Errorf("факт %s/%s: %w", ident, it.Topic, err)
	}
	st.Written++
	return nil
}

// rejectFact удаляет ошибочно распознанный факт (lexicon поймал цитирование,
// иронию или ложную тему — «Рыбачий» как топоним, а не рыбалка). Снимает и
// lexicon-, и llm-строки темы; ручную (manual) не трогает. Ограничение:
// следующий `facts scan` пересоздаст lexicon-строку — reject действует до
// пересканирования (для устойчивого подавления темы нужен -topics без неё или
// ручная manual-правка).
func rejectFact(ctx context.Context, tx *sql.Tx, it FactImport, st *FactImportStats) error {
	ident, _, err := resolveIdentity(ctx, tx, it.Identity, it.AccountIDs)
	if err != nil {
		return err
	}
	if ident == "" || it.Topic == "" {
		st.Skipped++
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM identity_facts WHERE identity = ? AND topic = ? AND source IN (?, ?)`,
		ident, it.Topic, FactSourceLexicon, FactSourceLLM); err != nil {
		return err
	}
	st.Rejected++
	return nil
}

func validPolarity(p string) bool {
	switch p {
	case PolarityLikes, PolarityDislikes, PolarityOwns, PolarityMentions:
		return true
	}
	return false
}

func nonNilEvidence(evs []FactEvidence) []FactEvidence {
	if evs == nil {
		return []FactEvidence{}
	}
	return evs
}

// resolveIdentity проверяет, что identity существует; иначе пытается ремапнуть
// по любому из account_ids (переклейка кластеров между выгрузкой и импортом).
// Пустая строка — не нашли.
func resolveIdentity(ctx context.Context, tx *sql.Tx, identity string, accountIDs []int64) (string, bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM v_identity WHERE identity = ?`, identity).Scan(&n); err != nil {
		return "", false, err
	}
	if n > 0 {
		return identity, false, nil
	}
	for _, uid := range accountIDs {
		var ident string
		err := tx.QueryRowContext(ctx,
			`SELECT identity FROM v_identity WHERE user_id = ?`, uid).Scan(&ident)
		if err == nil {
			return ident, true, nil
		}
	}
	return "", false, nil
}
