package archive

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// VerifyResult — итог VerifyText: калиброванная проверка «этот текст написал
// подозреваемый?». Z — насколько профиль подозреваемого выделяется над фоном
// всех авторов для этого текста; сравнивается с эмпирическим НУЛЕВЫМ
// распределением (z того же подозреваемого на чужих текстах), что даёт порог с
// контролируемой долей ложных срабатываний (FPR).
type VerifyResult struct {
	Suspect         string
	SuspectBestID   int64
	SuspectBestName string
	SuspectAccounts int

	QueryNgrams int
	QueryTokens int
	LexWeight   float64

	StyleProfiles  int // всего стиль-профилей (полный фон)
	ActiveDays     int // окно рецент-фильтра фона (0 — выкл)
	MinAuthorNotes int // жанровый фильтр фона: кандидат ≥N заметок (0 — выкл)
	BgProfiles     int // размер фоновой популяции после фильтра (== StyleProfiles, если фильтр выкл)

	Z      float64 // комбинированный z подозреваемого на запросе
	StyleZ float64
	LexZ   float64
	HasLex bool

	NullN      int     // сколько чужих текстов в фоне
	NullMean   float64 // фон: среднее z подозреваемого
	NullStd    float64
	NullP95    float64 // порог при FPR 5%
	NullP99    float64 // порог при FPR 1%
	NullMax    float64
	Percentile float64 // доля фона строго ниже Z (0..1) — «похожее, чем N% случайных»
}

// VerifyText проверяет, мог ли подозреваемый (p<id>|u<id>|user_id) написать
// текст. Считает z подозреваемого на тексте и строит нулевое распределение по
// nullN случайным чужим заметкам; вердикт да/нет выносится в CLI по порогу при
// заданном FPR. lexWeight — вес лексики. activeDays/minAuthorNotes>0 сужают
// фоновую популяцию (эталон «случайного автора») до правдоподобных кандидатов —
// z подозреваемого меряется на фоне реальных, а не мёртвых/некомментаторских
// анкет; нулевое распределение считается тем же фоном, поэтому FPR сохраняется.
func (s *Store) VerifyText(ctx context.Context, text, suspectToken string, lexWeight float64, nullN, activeDays, minAuthorNotes int) (VerifyResult, error) {
	member := s.identityMembers(ctx, suspectToken)
	if member == nil {
		return VerifyResult{}, fmt.Errorf("verify: подозреваемый %q не найден или без анкет", suspectToken)
	}
	identity, _ := s.canonIdentity(ctx, suspectToken)
	sa, la, err := s.loadAttributors(ctx)
	if err != nil {
		return VerifyResult{}, err
	}
	cf, err := s.buildCandidateFilter(ctx, activeDays, minAuthorNotes)
	if err != nil {
		return VerifyResult{}, err
	}
	norm := normalizeStyle(text)
	qsv, qn, ok := sa.vec(norm)
	if !ok {
		return VerifyResult{}, fmt.Errorf("verify: текст слишком короткий")
	}
	var qlv []float32
	if la != nil {
		qlv, _, _ = la.vec(text)
	}

	res := VerifyResult{
		Suspect: identity, SuspectAccounts: len(member),
		QueryNgrams: qn, LexWeight: lexWeight,
		StyleProfiles: len(sa.ids), BgProfiles: len(sa.ids),
	}
	if cf.cutoff != "" {
		res.ActiveDays = activeDays
	}
	if cf.noteWriters != nil {
		res.MinAuthorNotes = minAuthorNotes
	}
	if cf.on() {
		res.BgProfiles = cf.count(sa.ids)
	}
	if la != nil && qlv != nil {
		res.QueryTokens = len(strings.Fields(text))
	}
	z, sz, lz, bestID, hasLex, ok := suspectScore(sa, la, qsv, qlv, member, lexWeight, cf)
	if !ok {
		return VerifyResult{}, fmt.Errorf("verify: у подозреваемого нет стиль-профиля (мало текста)")
	}
	res.Z, res.StyleZ, res.LexZ, res.HasLex, res.SuspectBestID = z, sz, lz, hasLex, bestID
	if names, err := s.namesByIDs(ctx, []int64{bestID}); err == nil {
		res.SuspectBestName = names[bestID]
	}

	null, err := s.suspectNull(ctx, sa, la, member, lexWeight, nullN, cf)
	if err != nil {
		return res, err
	}
	fillNullStats(&res, null)
	return res, nil
}

// NoteTexts возвращает непустые тексты заметок по id (для пула известных
// образцов в verify).
func (s *Store) NoteTexts(ctx context.Context, ids []int64) ([]string, error) {
	notes, err := s.notesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.text != "" {
			out = append(out, n.text)
		}
	}
	return out, nil
}

// suspectScore — комбинированный z подозреваемого (макс. по его анкетам) на
// готовых векторах запроса, плюс лучшая анкета. Фон (среднее/сигма косинуса)
// считается по кандидатам из cf (правдоподобные авторы); выключенный cf — по
// всем. ok=false — ни у одной анкеты подозреваемого нет профиля.
func suspectScore(sa *styleAttributor, la *lexisAttributor, qsv, qlv []float32,
	member map[int64]bool, lexWeight float64, cf *candidateFilter) (z, sz, lz float64, bestID int64, hasLex, ok bool) {
	sCos := sa.cosinesOf(qsv)
	at := &Attribution{LexWeight: lexWeight}
	at.StyleCosMean, at.StyleCosStd = meanStdFiltered(sa.ids, sCos, cf)
	var lexCos map[int64]float64
	if la != nil && qlv != nil {
		var lmean, lstd float64
		lexCos, lmean, lstd = la.cosinesOf(qlv)
		if cf != nil && cf.on() {
			lmean, lstd = meanStdMapFiltered(la.ids, lexCos, cf)
		}
		at.LexCosMean, at.LexCosStd = lmean, lstd
	}
	best := -1e18
	for k, id := range sa.ids {
		if !member[id] {
			continue
		}
		s2, l2, c, hl := combineScores(sCos[k], id, lexCos, at)
		if c > best {
			best, sz, lz, hasLex, bestID = c, s2, l2, hl, id
		}
	}
	if best <= -1e17 {
		return 0, 0, 0, 0, false, false
	}
	return best, sz, lz, bestID, hasLex, true
}

// suspectNull строит нулевое распределение: z подозреваемого на nullN случайных
// заметках ЧУЖИХ авторов (текст, который заведомо не его). Фон каждого z считается
// тем же фильтром cf, что и запрос, — иначе порог FPR был бы несопоставим.
func (s *Store) suspectNull(ctx context.Context, sa *styleAttributor, la *lexisAttributor,
	member map[int64]bool, lexWeight float64, nullN int, cf *candidateFilter) ([]float64, error) {
	exclude := make([]int64, 0, len(member))
	for id := range member {
		exclude = append(exclude, id)
	}
	texts, err := s.notesSample(ctx, exclude, nullN)
	if err != nil {
		return nil, err
	}
	null := make([]float64, 0, len(texts))
	for _, t := range texts {
		qsv, _, ok := sa.vec(normalizeStyle(t))
		if !ok {
			continue
		}
		var qlv []float32
		if la != nil {
			qlv, _, _ = la.vec(t)
		}
		if z, _, _, _, _, ok := suspectScore(sa, la, qsv, qlv, member, lexWeight, cf); ok {
			null = append(null, z)
		}
	}
	return null, nil
}

// notesSample берёт до limit случайных непустых заметок авторов, НЕ входящих в
// exclude.
func (s *Store) notesSample(ctx context.Context, exclude []int64, limit int) ([]string, error) {
	q := `SELECT text FROM notes WHERE author_id != 0 AND text != ''`
	if len(exclude) > 0 {
		q += ` AND author_id NOT IN (` + intList(exclude) + `)`
	}
	q += ` ORDER BY RANDOM() LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// fillNullStats заполняет статистики фона и перцентиль Z запроса.
func fillNullStats(res *VerifyResult, null []float64) {
	res.NullN = len(null)
	if len(null) == 0 {
		return
	}
	res.NullMean, res.NullStd = meanStd(null)
	sorted := append([]float64(nil), null...)
	sort.Float64s(sorted)
	res.NullP95 = quantile(sorted, 0.95)
	res.NullP99 = quantile(sorted, 0.99)
	res.NullMax = sorted[len(sorted)-1]
	below := 0
	for _, x := range null {
		if x < res.Z {
			below++
		}
	}
	res.Percentile = float64(below) / float64(len(null))
}

// quantile — линейный перцентиль отсортированного среза (q в [0,1]).
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(pos)
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[lo+1]*frac
}
