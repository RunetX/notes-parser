package archive

import (
	"context"
	"strings"
)

// LooResult — одна итерация leave-one-out: заметка отложена, эталон построен из
// ОСТАЛЬНЫХ, и мы смотрим, на каком месте этот эталон среди всех профилей для
// отложенного текста. Rank 1 — эталон-из-остальных ближе к невиданной заметке,
// чем любой из ~8900 реальных авторов (значит почерк предсказуем, не подгонка).
type LooResult struct {
	NoteID     int64
	Author     int64
	Chars      int
	Rank       int // место эталона среди корпуса+эталон (0 — текст непригоден)
	RefStyleZ  float64
	RefLexZ    float64
	RefScore   float64
	HasLex     bool
	BeatByID   int64 // сильнейший чужой профиль, обошедший эталон (0 — эталон #1)
	BeatByName string

	IdRank     int   // место лучшей именной анкеты автора на ЭТОЙ отложенной заметке (out-of-sample; 0 — личность не задана)
	IdBestID   int64
	IdBestName string
}

// Calibration — итог CalibrateNotes.
type Calibration struct {
	Notes         int
	Chars         int
	StyleProfiles int
	LexProfiles   int
	LexWeight     float64

	Loo           []LooResult
	LooTop1       int // в скольких заметках эталон-из-остальных занял 1-е место
	LooTop5       int
	LooMedianRank int

	IdMedianRank int // медиана ранга именной анкеты по отложенным заметкам (out-of-sample; 0 — личность не задана)
	IdTop10      int // в скольких отложенных заметках именная анкета попала в топ-10

	Pooled []AttributionCandidate // ближайшие к агрегату всех заметок (объединённый голос)

	// Разрыв с личностью (если задана): где её лучшая анкета в пуловом рейтинге.
	Identity          string
	IdentityRank      int // 0 — личность не задана / нет профилей
	IdentityBestID    int64
	IdentityBestName  string
	IdentityBestScore float64
}

// CalibrateNotes оценивает, есть ли у набора заметок (предположительно одного
// автора) устойчивый, ПРЕДСКАЗУЕМЫЙ отпечаток. Три среза:
//   - leave-one-out: для каждой заметки эталон строится из остальных и ранжируется
//     против всего корпуса на отложенном тексте — честная проверка обобщения;
//   - пул: все заметки объединяются в один голос → топ ближайших авторов;
//   - разрыв с identity (если задан p<id>|u<id>): где именные анкеты автора в пуле.
func (s *Store) CalibrateNotes(ctx context.Context, noteIDs []int64, identityToken string, lexWeight float64, topN int) (Calibration, error) {
	notes, err := s.notesByIDs(ctx, noteIDs)
	if err != nil {
		return Calibration{}, err
	}
	sa, la, err := s.loadAttributors(ctx)
	if err != nil {
		return Calibration{}, err
	}
	cal := Calibration{Notes: len(notes), StyleProfiles: len(sa.ids), LexWeight: lexWeight}
	if la != nil {
		cal.LexProfiles = len(la.ids)
	}

	items := s.prepNoteItems(notes, sa, la)
	for _, it := range items {
		cal.Chars += it.chars
	}
	member := s.identityMembers(ctx, identityToken)

	var ranks, idRanks []int
	for i := range items {
		lr, ok := s.looOne(items, i, sa, la, lexWeight, member)
		if !ok {
			continue
		}
		cal.Loo = append(cal.Loo, lr)
		ranks = append(ranks, lr.Rank)
		if lr.Rank <= 1 {
			cal.LooTop1++
		}
		if lr.Rank <= 5 {
			cal.LooTop5++
		}
		if lr.IdRank > 0 {
			idRanks = append(idRanks, lr.IdRank)
			if lr.IdRank <= 10 {
				cal.IdTop10++
			}
		}
	}
	cal.LooMedianRank = medianInt(ranks)
	cal.IdMedianRank = medianInt(idRanks)
	if err := s.fillLooNames(ctx, cal.Loo); err != nil {
		return cal, err
	}

	if err := s.calibPooled(ctx, items, sa, la, lexWeight, topN, identityToken, &cal); err != nil {
		return cal, err
	}
	return cal, nil
}

// noteItem — заметка с предпосчитанными векторами запроса.
type noteItem struct {
	note  authoredNote
	norm  string
	chars int
	sv    []float32 // центрированный стиль-вектор
	lv    []float32 // tf-idf-вектор (nil — лексики нет/пусто)
	okS   bool
}

func (s *Store) prepNoteItems(notes []authoredNote, sa *styleAttributor, la *lexisAttributor) []noteItem {
	items := make([]noteItem, 0, len(notes))
	for _, n := range notes {
		norm := normalizeStyle(n.text)
		sv, _, okS := sa.vec(norm)
		var lv []float32
		if la != nil {
			lv, _, _ = la.vec(n.text)
		}
		items = append(items, noteItem{
			note: n, norm: norm, chars: len([]rune(n.text)), sv: sv, lv: lv, okS: okS,
		})
	}
	return items
}

// looOne — одна leave-one-out итерация для заметки i.
func (s *Store) looOne(items []noteItem, i int, sa *styleAttributor, la *lexisAttributor,
	lexWeight float64, member map[int64]bool) (LooResult, bool) {
	it := items[i]
	lr := LooResult{NoteID: it.note.id, Author: it.note.author, Chars: it.chars}
	if !it.okS {
		return lr, false
	}
	// Эталон из ОСТАЛЬНЫХ заметок.
	var otherNorm, otherRaw []string
	for j, jt := range items {
		if j != i {
			otherNorm = append(otherNorm, jt.norm)
			otherRaw = append(otherRaw, jt.note.text)
		}
	}
	refS, _, okr := sa.vec(strings.Join(otherNorm, " "))
	if !okr {
		return lr, false
	}

	sCos := sa.cosinesOf(it.sv)
	at := &Attribution{LexWeight: lexWeight}
	at.StyleCosMean, at.StyleCosStd = meanStd(sCos)

	var lexCos map[int64]float64
	refLexCos, hasLex := 0.0, false
	if la != nil && it.lv != nil {
		lexCos, at.LexCosMean, at.LexCosStd = la.cosinesOf(it.lv)
		if refL, _, ok := la.vec(strings.Join(otherRaw, " ")); ok {
			refLexCos, hasLex = dot(it.lv, refL), true
		}
	}
	lr.RefStyleZ, lr.RefLexZ, lr.RefScore = refScore(dot(it.sv, refS), refLexCos, hasLex, at)
	lr.HasLex = hasLex

	// Скор каждого корпусного профиля на отложенном тексте.
	scores := make([]float64, len(sa.ids))
	bestID, bestScore := int64(0), -1e18
	for k := range sa.ids {
		_, _, scores[k], _ = combineScores(sCos[k], sa.ids[k], lexCos, at)
		if scores[k] > bestScore {
			bestScore, bestID = scores[k], sa.ids[k]
		}
	}
	// Ранг эталона-из-остальных.
	lr.Rank = 1 + countGreater(scores, lr.RefScore)
	if bestScore > lr.RefScore {
		lr.BeatByID = bestID
	}
	// Ранг лучшей именной анкеты автора (out-of-sample: заметка i не в её профиле).
	lr.IdRank, lr.IdBestID = bestMemberRank(scores, sa.ids, member)
	return lr, true
}

// countGreater — сколько значений строго больше threshold.
func countGreater(xs []float64, threshold float64) int {
	n := 0
	for _, x := range xs {
		if x > threshold {
			n++
		}
	}
	return n
}

// bestMemberRank — ранг и id самой высоко ранжированной анкеты из member.
func bestMemberRank(scores []float64, ids []int64, member map[int64]bool) (int, int64) {
	if member == nil {
		return 0, 0
	}
	bestScore, bestID := -1e18, int64(0)
	for k, id := range ids {
		if member[id] && scores[k] > bestScore {
			bestScore, bestID = scores[k], id
		}
	}
	if bestID == 0 {
		return 0, 0
	}
	return 1 + countGreater(scores, bestScore), bestID
}

// identityMembers — множество анкет личности (nil, если токен пуст/не резолвится).
func (s *Store) identityMembers(ctx context.Context, token string) map[int64]bool {
	if token == "" {
		return nil
	}
	identity, err := s.canonIdentity(ctx, token)
	if err != nil {
		return nil
	}
	accs, err := s.identityAccountIDs(ctx, identity)
	if err != nil || len(accs) == 0 {
		return nil
	}
	m := make(map[int64]bool, len(accs))
	for _, id := range accs {
		m[id] = true
	}
	return m
}

// refScore считает Z обоих сигналов и комбинированный скор ЭТАЛОНА (у него есть
// собственное лексическое значение, в отличие от «нет профиля»).
func refScore(styleCos, lexCos float64, hasLex bool, at *Attribution) (sz, lz, combined float64) {
	if at.StyleCosStd > 0 {
		sz = (styleCos - at.StyleCosMean) / at.StyleCosStd
	}
	if !hasLex || at.LexCosStd <= 0 {
		return sz, 0, sz
	}
	lz = (lexCos - at.LexCosMean) / at.LexCosStd
	return sz, lz, at.LexWeight*lz + (1-at.LexWeight)*sz
}

// calibPooled строит объединённый голос всех заметок, ранжирует корпус (топ-N) и
// находит позицию именных анкет личности в этом рейтинге.
func (s *Store) calibPooled(ctx context.Context, items []noteItem, sa *styleAttributor, la *lexisAttributor,
	lexWeight float64, topN int, identityToken string, cal *Calibration) error {
	var norm, raw []string
	for _, it := range items {
		norm = append(norm, it.norm)
		raw = append(raw, it.note.text)
	}
	psv, _, ok := sa.vec(strings.Join(norm, " "))
	if !ok {
		return nil
	}
	sCos := sa.cosinesOf(psv)
	at := &Attribution{LexWeight: lexWeight}
	at.StyleCosMean, at.StyleCosStd = meanStd(sCos)
	var lexCos map[int64]float64
	if la != nil {
		if plv, _, okL := la.vec(strings.Join(raw, " ")); okL {
			lexCos, at.LexCosMean, at.LexCosStd = la.cosinesOf(plv)
		}
	}

	order := s.rankAttribution(sa.ids, sCos, lexCos, at)
	n := topN
	if n <= 0 || n > len(order) {
		n = len(order)
	}
	pick := make([]int64, 0, n)
	for _, oi := range order[:n] {
		pick = append(pick, sa.ids[oi])
	}
	meta, err := s.attributionMeta(ctx, pick)
	if err != nil {
		return err
	}
	for r := 1; r <= n; r++ {
		cal.Pooled = append(cal.Pooled, s.attributionCandidate(r, order, sa.ids, sCos, lexCos, meta, at))
	}
	if identityToken == "" {
		return nil
	}
	return s.calibIdentityGap(ctx, identityToken, order, sa.ids, sCos, lexCos, at, cal)
}

// calibIdentityGap находит лучшую (самую высоко ранжированную) анкету личности в
// пуловом рейтинге — насколько «своё» именное письмо близко к анонимному голосу.
func (s *Store) calibIdentityGap(ctx context.Context, token string, order []int, ids []int64,
	sCos []float64, lexCos map[int64]float64, at *Attribution, cal *Calibration) error {
	identity, err := s.canonIdentity(ctx, token)
	if err != nil {
		return err
	}
	accSet, err := s.identityAccountIDs(ctx, identity)
	if err != nil {
		return err
	}
	member := make(map[int64]bool, len(accSet))
	for _, id := range accSet {
		member[id] = true
	}
	cal.Identity = identity
	for rank, oi := range order {
		if member[ids[oi]] {
			_, _, sc, _ := combineScores(sCos[oi], ids[oi], lexCos, at)
			cal.IdentityRank = rank + 1
			cal.IdentityBestID = ids[oi]
			cal.IdentityBestScore = sc
			break
		}
	}
	if cal.IdentityBestID != 0 {
		names, err := s.namesByIDs(ctx, []int64{cal.IdentityBestID})
		if err != nil {
			return err
		}
		cal.IdentityBestName = names[cal.IdentityBestID]
	}
	return nil
}

func (s *Store) notesByIDs(ctx context.Context, ids []int64) ([]authoredNote, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, COALESCE(author_id, 0), text FROM notes WHERE id IN (`+intList(ids)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authoredNote
	for rows.Next() {
		var n authoredNote
		if err := rows.Scan(&n.id, &n.author, &n.text); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) fillLooNames(ctx context.Context, loo []LooResult) error {
	need := map[int64]bool{}
	for _, l := range loo {
		if l.BeatByID != 0 {
			need[l.BeatByID] = true
		}
		if l.IdBestID != 0 {
			need[l.IdBestID] = true
		}
	}
	ids := make([]int64, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	names, err := s.namesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range loo {
		loo[i].BeatByName = names[loo[i].BeatByID]
		loo[i].IdBestName = names[loo[i].IdBestID]
	}
	return nil
}
