package archive

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// SignalEnsemble — композитный сигнал: направленный ранг стиля, подкреплённый
// временным паттерном (handoff) и пересечением круга собеседников. Ни один из
// них по отдельности не проходит планку склейки; вместе — проходят.
const SignalEnsemble = "ensemble"

// EnsembleParams — настройки прогона ансамбля.
type EnsembleParams struct {
	MinCosine   float64 // порог косинуса для соседа-кандидата по стилю
	TopK        int     // сколько ближайших по стилю берём с каждой стороны (направленно)
	HandoffDays int     // макс. разрыв спанов, ещё считающийся «эстафетой»
	Floor       float64 // писать кандидата, только если композит ≥ Floor
	MaxPairs    int     // предел числа записанных пар
}

// EnsembleResult — оценённая пара-кандидат.
type EnsembleResult struct {
	A, B      int64
	NameA     string
	NameB     string
	Score     float64
	StyleRank int     // лучший (минимальный) ранг среди двух направлений; 0 — стиля нет
	StyleCos  float64 // центр-косинус пары
	Temporal  string  // overlap|handoff|disjoint|unknown
	GapDays   int
	Overlap   float64 // overlap-коэффициент кругов собеседников
	Evidence  string
}

// EnsembleStats — итог ClusterEnsemble.
type EnsembleStats struct {
	StyleCandidates int              // пар-кандидатов из стиля (до фильтра по Floor)
	Written         int              // записано в alias_candidates
	Top             []EnsembleResult // топ для отчёта (≤20)
}

// styleCand — пара по стилю с лучшим направленным рангом.
type styleCand struct {
	a, b int64
	rank int
	cos  float64
}

// ClusterEnsemble генерирует пары-кандидаты из направленного top-K по стилю,
// подкрепляет каждую временем и пересечением круга собеседников, и пишет пары с
// композитным весом ≥ Floor в alias_candidates(signal=ensemble). Идемпотентно.
func (s *Store) ClusterEnsemble(ctx context.Context, p EnsembleParams, now time.Time) (EnsembleStats, error) {
	pids, vecs, err := s.loadStyleProfiles(ctx)
	if err != nil {
		return EnsembleStats{}, err
	}
	if len(pids) < 2 {
		return EnsembleStats{}, nil
	}
	centerAndNormalize(vecs)
	cands := directionalStylePairs(pids, vecs, p.MinCosine, p.TopK)
	authors := candidateAuthors(cands)

	spans, err := s.activitySpans(ctx, authors)
	if err != nil {
		return EnsembleStats{}, err
	}
	circles, err := s.circleSets(ctx, authors)
	if err != nil {
		return EnsembleStats{}, err
	}
	names, err := s.namesByIDs(ctx, authors)
	if err != nil {
		return EnsembleStats{}, err
	}

	results := scoreCandidates(cands, spans, circles, names, p.Floor, p.HandoffDays)
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > p.MaxPairs {
		results = results[:p.MaxPairs]
	}
	if err := s.writeEnsemble(ctx, results, now); err != nil {
		return EnsembleStats{}, err
	}

	st := EnsembleStats{StyleCandidates: len(cands), Written: len(results)}
	if len(results) > 20 {
		st.Top = results[:20]
	} else {
		st.Top = results
	}
	return st, nil
}

// candidateAuthors — уникальные id из пар-кандидатов (для батч-запросов).
func candidateAuthors(cands []styleCand) []int64 {
	seen := map[int64]bool{}
	authors := make([]int64, 0, len(cands)*2)
	for _, c := range cands {
		for _, id := range [2]int64{c.a, c.b} {
			if !seen[id] {
				seen[id] = true
				authors = append(authors, id)
			}
		}
	}
	return authors
}

// scoreCandidates оценивает каждую пару ансамблем и оставляет ≥ floor.
func scoreCandidates(cands []styleCand, spans map[int64]DiagAccount, circles map[int64]map[int64]bool, names map[int64]string, floor float64, handoffDays int) []EnsembleResult {
	results := make([]EnsembleResult, 0, len(cands))
	for _, c := range cands {
		rel, gap := temporalRel(spans[c.a], spans[c.b])
		oc := overlapCoeff(circles[c.a], circles[c.b], c.a, c.b)
		score := ensembleScore(c.rank, c.cos, rel, gap, oc, handoffDays)
		if score < floor {
			continue
		}
		results = append(results, EnsembleResult{
			A: c.a, B: c.b, NameA: names[c.a], NameB: names[c.b],
			Score: score, StyleRank: c.rank, StyleCos: c.cos,
			Temporal: rel, GapDays: gap, Overlap: oc,
			Evidence: ensembleEvidence(c.rank, c.cos, rel, gap, oc),
		})
	}
	return results
}

// directionalStylePairs — пары, где хотя бы одна сторона видит другую в своём
// top-K (в отличие от взаимного top-K). Для пары хранится ЛУЧШИЙ (минимальный)
// ранг из двух направлений: если у A ближайший — B, сигнал силён, даже когда
// у крупного B (хаб на большом корпусе) A лишь в середине списка.
func directionalStylePairs(pids []int64, vecs [][]float32, minCosine float64, topK int) []styleCand {
	best := map[[2]int64]styleCand{}
	for i := range vecs {
		for rank, n := range topNeighbors(i, vecs, minCosine, topK) { // по убыванию косинуса
			key := pairKey(pids[i], pids[n.j])
			cand := styleCand{a: key[0], b: key[1], rank: rank + 1, cos: n.cos}
			if ex, ok := best[key]; !ok || cand.rank < ex.rank {
				best[key] = cand
			}
		}
	}
	out := make([]styleCand, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	return out
}

// topNeighbors — до topK ближайших к профилю i соседей с косинусом ≥ minCosine.
func topNeighbors(i int, vecs [][]float32, minCosine float64, topK int) []neighbor {
	var top []neighbor
	for j := range vecs {
		if i == j {
			continue
		}
		c := dot(vecs[i], vecs[j])
		if c < minCosine {
			continue
		}
		top = insertTop(top, neighbor{j: j, cos: c}, topK)
	}
	return top
}

func pairKey(a, b int64) [2]int64 {
	if a > b {
		a, b = b, a
	}
	return [2]int64{a, b}
}

// ensembleScore комбинирует сигналы в композит [0, 0.97]: база от ранга стиля +
// вклад времени + вклад пересечения кругов. Веса подобраны так, что пара только
// со стилем (без корроборации) остаётся ниже дефолтного порога склейки 0.7.
func ensembleScore(rank int, cos float64, temporal string, gap int, oc float64, handoffDays int) float64 {
	return math.Min(0.97, styleRankScore(rank, cos)+temporalScore(temporal, gap, handoffDays)+overlapScore(oc))
}

func styleRankScore(rank int, cos float64) float64 {
	base := 0.22
	switch {
	case rank == 1:
		base = 0.45
	case rank <= 3:
		base = 0.38
	case rank <= 10:
		base = 0.30
	}
	bonus := 0.05 * clamp01((cos-0.5)/0.4) // +0..0.05 за абсолютную близость
	return base + bonus
}

func temporalScore(rel string, gap, handoffDays int) float64 {
	switch rel {
	case "handoff": // старый замолк → новый начал: миграция на альт
		if gap <= handoffDays {
			return 0.25
		}
		return 0.15 // разрыв шире окна «эстафеты», но спаны смежны
	case "overlap":
		return 0.12 // одновременные анкеты — слабее
	}
	return 0 // disjoint/unknown
}

func overlapScore(oc float64) float64 {
	switch {
	case oc >= 0.5:
		return 0.25
	case oc >= 0.3:
		return 0.15
	case oc >= 0.15:
		return 0.08
	}
	return 0
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func ensembleEvidence(rank int, cos float64, rel string, gap int, oc float64) string {
	t := rel
	if gap > 0 {
		t = fmt.Sprintf("%s %dд", rel, gap)
	}
	return fmt.Sprintf("стиль #%d (cos %.2f) · %s · круг %.2f", rank, cos, t, oc)
}

func (s *Store) writeEnsemble(ctx context.Context, results []EnsembleResult, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	for _, r := range results {
		if err := upsertAliasCandidate(ctx, tx, aliasCand{
			a: r.A, b: r.B, signal: SignalEnsemble, score: r.Score, evidence: r.Evidence,
		}, nowStr); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- признаки времени и круга (батч по кандидатам) ---

// activitySpans — реальный спан активности (MIN/MAX published_at) для набора
// авторов, обёрнутый в DiagAccount для temporalRel.
func (s *Store) activitySpans(ctx context.Context, ids []int64) (map[int64]DiagAccount, error) {
	out := make(map[int64]DiagAccount, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT author_id, COALESCE(MIN(published_at), ''), COALESCE(MAX(published_at), '')
		FROM comments WHERE author_id IN (`+intList(ids)+`) GROUP BY author_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var from, to string
		if err := rows.Scan(&id, &from, &to); err != nil {
			return nil, err
		}
		out[id] = DiagAccount{ID: id, ActiveFrom: from, ActiveTo: to}
	}
	return out, rows.Err()
}

// circleSets — круг собеседников (авторы входящих и исходящих ответов) для набора
// авторов. Две запроса, отфильтрованных по кандидатам через IN.
func (s *Store) circleSets(ctx context.Context, ids []int64) (map[int64]map[int64]bool, error) {
	out := make(map[int64]map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	in := intList(ids)
	add := func(x, partner int64) {
		if partner == 0 {
			return
		}
		set := out[x]
		if set == nil {
			set = map[int64]bool{}
			out[x] = set
		}
		set[partner] = true
	}
	// исходящие: на кого кандидат отвечал.
	if err := s.scanPairs(ctx, `
		SELECT c.author_id, pc.author_id
		FROM comments c JOIN comments pc ON pc.id = c.parent_id
		WHERE c.author_id IN (`+in+`) AND c.parent_id != 0`, add); err != nil {
		return nil, err
	}
	// входящие: кто отвечал кандидату.
	if err := s.scanPairs(ctx, `
		SELECT pc.author_id, child.author_id
		FROM comments child JOIN comments pc ON pc.id = child.parent_id
		WHERE pc.author_id IN (`+in+`)`, add); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) scanPairs(ctx context.Context, query string, add func(x, partner int64)) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var x, partner int64
		if err := rows.Scan(&x, &partner); err != nil {
			return err
		}
		add(x, partner)
	}
	return rows.Err()
}

// overlapCoeff — доля пересечения кругов: |A∩B| / min(|A|,|B|), исключая сами
// анкеты пары (иначе их взаимные ответы завышали бы пересечение).
func overlapCoeff(ca, cb map[int64]bool, a, b int64) float64 {
	shared, na, nb := 0, 0, 0
	for k := range ca {
		if k == a || k == b {
			continue
		}
		na++
		if cb[k] {
			shared++
		}
	}
	for k := range cb {
		if k != a && k != b {
			nb++
		}
	}
	m := na
	if nb < m {
		m = nb
	}
	if m == 0 {
		return 0
	}
	return float64(shared) / float64(m)
}

// namesByIDs — имена для набора id (для отчёта).
func (s *Store) namesByIDs(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM users WHERE id IN (`+intList(ids)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
