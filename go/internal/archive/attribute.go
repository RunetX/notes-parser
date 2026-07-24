package archive

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// AttributionCandidate — кандидат-автор анонимного текста. Скор комбинирует два
// сигнала: стилометрию (символьные 3-граммы, идиолект) и лексику (TF-IDF по
// словам, темы/лексикон). Для каждого — сырой косинус и Z (отклонение от фона в
// сигмах); Score — комбинированный Z, по нему ранжирование.
type AttributionCandidate struct {
	Rank     int
	UserID   int64
	Name     string
	Gender   string // male | female | "" (не размечен)
	Identity string // токен v_identity: pN — личность, uN — одиночная анкета
	Persona  bool   // анкета входит в личность (identity = pN)

	StyleCos float64 // центр-косинус стиля запрос↔профиль
	StyleZ   float64 // отклонение стиля от фона, в сигмах
	LexCos   float64 // косинус tf-idf запрос↔профиль
	LexZ     float64 // отклонение лексики от фона, в сигмах
	HasLex   bool    // у автора есть лексический профиль (иначе Score = StyleZ)

	Score  float64 // комбинированный Z (ранжирующий)
	Ngrams int     // объём стиль-профиля (3-грамм) — надёжность эталона
}

// Attribution — итог AttributeText.
type Attribution struct {
	StyleProfiles int     // стиль-профилей сравнено
	LexProfiles   int     // лексических профилей сравнено (0 — слой не построен)
	QueryNgrams   int     // 3-грамм в запросе (объём для стиля)
	QueryTokens   int     // слов в запросе (объём для лексики)
	StyleCosMean  float64 // фон косинуса стиля
	StyleCosStd   float64
	LexCosMean    float64 // фон косинуса лексики
	LexCosStd     float64
	LexWeight      float64 // вес лексики в комбинированном скоре [0..1]
	ActiveDays     int     // окно рецент-фильтра в сутках (0 — выкл)
	MinAuthorNotes int     // жанровый фильтр: кандидат ≥N заметок (0 — выкл)
	KeptProfiles   int     // кандидатов осталось после отсева неправдоподобных анкет
	Candidates     []AttributionCandidate
	Want           *AttributionCandidate // позиция автора wantID (валидация); nil — нет профиля/отфильтрован
	WantFiltered   bool                  // у wantID есть профиль, но он выбыл по фильтру (мёртвый/не пишет заметок)
}

// AttributeText ранжирует авторов архива по похожести на текст, комбинируя
// стилометрию и лексику. Стиль: char-3-граммы, центрирование средним профилей,
// косинус. Лексика (если построена, `personas lexis build`): tf-idf по словам,
// косинус. Оба сигнала переводятся в Z (сигмы над фоном популяции) — так шкалы
// сопоставимы независимо от объёма запроса. Комбинированный Z = lexWeight·LexZ +
// (1-lexWeight)·StyleZ; у кого лексики нет — только StyleZ. wantID != 0 — вернуть
// также позицию этого автора (валидация на заметке с известным автором).
// activeDays/minAuthorNotes>0 — отсечь неправдоподобных кандидатов из выдачи
// (рецент + жанр): «мёртвые» анкеты и чистые комментаторы заметку не писали.
func (s *Store) AttributeText(ctx context.Context, text string, topN int, wantID int64, lexWeight float64, activeDays, minAuthorNotes int) (Attribution, error) {
	norm := normalizeStyle(text)
	if norm == "" {
		return Attribution{}, fmt.Errorf("attribute: пустой текст")
	}
	sIDs, sVecs, err := s.loadStyleProfiles(ctx)
	if err != nil {
		return Attribution{}, err
	}
	if len(sIDs) < 2 {
		return Attribution{}, errFewStyleProfiles(len(sIDs))
	}
	sCos, qn, err := styleQueryCosines(norm, sVecs)
	if err != nil {
		return Attribution{}, err
	}
	at := Attribution{StyleProfiles: len(sIDs), QueryNgrams: qn, LexWeight: lexWeight}
	at.StyleCosMean, at.StyleCosStd = meanStd(sCos)

	lexCos, err := s.lexisQueryCosines(ctx, text, &at)
	if err != nil {
		return at, err
	}

	order := s.rankAttribution(sIDs, sCos, lexCos, &at)
	order, err = s.filterCandidates(ctx, order, sIDs, activeDays, minAuthorNotes, &at)
	if err != nil {
		return at, err
	}
	if topN <= 0 || topN > len(order) {
		topN = len(order)
	}

	pick := make([]int64, 0, topN+1) // id для обогащения: топ + wantID
	for _, oi := range order[:topN] {
		pick = append(pick, sIDs[oi])
	}
	wantRank, wantExists := resolveWant(order, sIDs, wantID)
	if wantRank > 0 {
		pick = append(pick, wantID)
	}
	meta, err := s.attributionMeta(ctx, pick)
	if err != nil {
		return at, err
	}
	build := func(rank int) AttributionCandidate {
		return s.attributionCandidate(rank, order, sIDs, sCos, lexCos, meta, &at)
	}
	for r := 1; r <= topN; r++ {
		at.Candidates = append(at.Candidates, build(r))
	}
	switch {
	case wantRank > 0:
		w := build(wantRank)
		at.Want = &w
	case wantExists:
		at.WantFiltered = true // профиль есть, но кандидат выбыл по фильтру
	}
	return at, nil
}

// resolveWant ищет позицию автора wantID в отфильтрованном рейтинге order.
// rank>0 — его место (1-индекс) среди прошедших фильтр; exists — есть ли у него
// стиль-профиль вообще (различает «нет профиля» и «выбыл по фильтру»).
func resolveWant(order []int, ids []int64, wantID int64) (rank int, exists bool) {
	if wantID == 0 {
		return 0, false
	}
	for _, id := range ids {
		if id == wantID {
			exists = true
			break
		}
	}
	for r, oi := range order {
		if ids[oi] == wantID {
			return r + 1, exists
		}
	}
	return 0, exists
}

// filterCandidates при включённых фильтрах выкидывает из рейтинга неправдоподобные
// анкеты: «мёртвые» (рецент — не активны в окне) и чистых комментаторов без
// заметок (жанр). Тот, кто давно не появлялся или заметок не пишет, не мог
// написать этот текст-заметку. Порядок сохраняется; заполняет
// at.ActiveDays/MinAuthorNotes/KeptProfiles.
func (s *Store) filterCandidates(ctx context.Context, order []int, ids []int64, activeDays, minAuthorNotes int, at *Attribution) ([]int, error) {
	cf, err := s.buildCandidateFilter(ctx, activeDays, minAuthorNotes)
	if err != nil || !cf.on() {
		return order, err
	}
	if cf.cutoff != "" {
		at.ActiveDays = activeDays
	}
	if cf.noteWriters != nil {
		at.MinAuthorNotes = minAuthorNotes
	}
	kept := make([]int, 0, len(order))
	for _, oi := range order {
		if cf.ok(ids[oi]) {
			kept = append(kept, oi)
		}
	}
	at.KeptProfiles = len(kept)
	return kept, nil
}

// styleQueryCosines строит центрированный вектор запроса (char-3-граммы) и
// возвращает косинусы со всеми стиль-профилями (профили центрируются на месте).
func styleQueryCosines(norm string, vecs [][]float32) ([]float64, int, error) {
	query := make([]float32, len(vecs[0]))
	qn := addCharTrigrams(query, norm, len(query))
	if qn == 0 || !l2Normalize(query) {
		return nil, 0, fmt.Errorf("attribute: текст короче 3 символов — сравнивать нечего")
	}
	mean := meanVec(vecs)
	for _, v := range vecs {
		centerVec(v, mean)
	}
	centerVec(query, mean)
	cos := make([]float64, len(vecs))
	for i, v := range vecs {
		cos[i] = dot(query, v)
	}
	return cos, qn, nil
}

// lexisQueryCosines считает косинусы tf-idf запроса со всеми лексическими
// профилями (карта user_id→косинус) и заполняет лексические поля at. Пустая
// карта — слой не построен или запрос без слов (сигнал недоступен).
func (s *Store) lexisQueryCosines(ctx context.Context, text string, at *Attribution) (map[int64]float64, error) {
	lIDs, lVecs, idf, dims, err := s.loadLexisProfiles(ctx)
	if err != nil {
		return nil, err
	}
	if len(lIDs) < 2 {
		return nil, nil
	}
	at.LexProfiles = len(lIDs)
	q, tokens, ok := buildLexisQuery(text, idf, dims)
	at.QueryTokens = tokens
	if !ok {
		return nil, nil
	}
	cos := make([]float64, len(lVecs))
	for i, v := range lVecs {
		cos[i] = dot(q, v)
	}
	at.LexCosMean, at.LexCosStd = meanStd(cos)
	m := make(map[int64]float64, len(lIDs))
	for i, id := range lIDs {
		m[id] = cos[i]
	}
	return m, nil
}

// rankAttribution возвращает индексы стиль-профилей, отсортированные по убыванию
// комбинированного скора.
func (s *Store) rankAttribution(sIDs []int64, sCos []float64, lexCos map[int64]float64, at *Attribution) []int {
	order := make([]int, len(sCos))
	score := make([]float64, len(sCos))
	for i := range order {
		order[i] = i
		_, _, score[i], _ = combineScores(sCos[i], sIDs[i], lexCos, at)
	}
	sort.Slice(order, func(a, b int) bool { return score[order[a]] > score[order[b]] })
	return order
}

// combineScores переводит косинусы одного кандидата в Z по обоим сигналам и
// комбинированный скор. hasLex=false — у автора нет лексического профиля.
func combineScores(styleCos float64, id int64, lexCos map[int64]float64, at *Attribution) (sz, lz, combined float64, hasLex bool) {
	if at.StyleCosStd > 0 {
		sz = (styleCos - at.StyleCosMean) / at.StyleCosStd
	}
	if at.LexCosStd <= 0 { // лексический сигнал недоступен для запроса — только стиль
		return sz, 0, sz, false
	}
	if c, ok := lexCos[id]; ok {
		hasLex = true
		lz = (c - at.LexCosMean) / at.LexCosStd
	}
	// Нет лексического профиля → lz=0 (нейтральный фон), а НЕ неразбавленный
	// стиль-скор: иначе мелкие анкеты без лексики всплывают на шумном стиле.
	combined = at.LexWeight*lz + (1-at.LexWeight)*sz
	return sz, lz, combined, hasLex
}

// attributionCandidate собирает кандидата по позиции rank (1-индекс) в order.
func (s *Store) attributionCandidate(rank int, order []int, sIDs []int64, sCos []float64,
	lexCos map[int64]float64, meta map[int64]AttributionCandidate, at *Attribution) AttributionCandidate {
	oi := order[rank-1]
	id := sIDs[oi]
	c := meta[id]
	c.Rank, c.UserID = rank, id
	c.StyleCos = sCos[oi]
	c.StyleZ, c.LexZ, c.Score, c.HasLex = combineScores(sCos[oi], id, lexCos, at)
	if c.HasLex {
		c.LexCos = lexCos[id]
	}
	return c
}

// attributionMeta — имя/пол/личность/объём стиль-профиля отобранных кандидатов
// одним запросом.
func (s *Store) attributionMeta(ctx context.Context, ids []int64) (map[int64]AttributionCandidate, error) {
	out := map[int64]AttributionCandidate{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.name, u.gender,
		       COALESCE(i.identity, ''), COALESCE(i.is_persona, 0),
		       COALESCE(sp.ngrams, 0)
		FROM users u
		LEFT JOIN v_identity i ON i.user_id = u.id
		LEFT JOIN style_profiles sp ON sp.user_id = u.id
		WHERE u.id IN (`+placeholders(len(args))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c AttributionCandidate
		var isPersona int
		if err := rows.Scan(&c.UserID, &c.Name, &c.Gender, &c.Identity, &isPersona, &c.Ngrams); err != nil {
			return nil, err
		}
		c.Persona = isPersona == 1
		out[c.UserID] = c
	}
	return out, rows.Err()
}

// errFewStyleProfiles — общая ошибка «профили ещё не построены».
func errFewStyleProfiles(n int) error {
	return fmt.Errorf("attribute: стиль-профилей %d — сначала `personas stylometry build`", n)
}

// meanStd — среднее и сигма (population) значений.
func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var m float64
	for _, x := range xs {
		m += x
	}
	m /= float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return m, math.Sqrt(v / float64(len(xs)))
}
