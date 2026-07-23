package archive

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// migrateV5SQL — стилометрические профили авторов (Фаза 2b). Для склейки альтов,
// сменивших фото (аватар их не ловит): у каждого автора — вектор частот
// символьных 3-грамм (feature hashing), по которому меряется похожесть письма.
const migrateV5SQL = `
CREATE TABLE style_profiles (
    user_id  INTEGER PRIMARY KEY REFERENCES users(id),
    ngrams   INTEGER NOT NULL,   -- сколько 3-грамм учтено (~объём текста)
    dims     INTEGER NOT NULL,   -- размерность хэш-вектора
    vec      BLOB NOT NULL,      -- dims × float32 LE, L2-нормированный
    built_at TEXT NOT NULL
);
`

// SignalStylometry — сигнал связи по стилю письма.
const SignalStylometry = "stylometry"

// StyleBuildStats — итог BuildStyleProfiles.
type StyleBuildStats struct {
	Authors  int // авторов с комментариями просмотрено
	Eligible int // профилей построено (текста ≥ minChars)
}

// styleAcc — аккумулятор профиля одного автора при потоковом обходе.
type styleAcc struct {
	vec    []float32
	chars  int
	ngrams int
}

// BuildStyleProfiles строит стилометрические профили: один проход по всем
// комментариям, накопление хэш-вектора символьных 3-грамм на автора, и для тех,
// у кого суммарно ≥ minChars символов, — L2-нормированный вектор в style_profiles.
// dims — размерность (напр. 512). Идемпотентно (upsert), перестраивает с нуля.
func (s *Store) BuildStyleProfiles(ctx context.Context, minChars, dims int, now time.Time) (StyleBuildStats, error) {
	acc, err := s.accumulateStyle(ctx, dims)
	if err != nil {
		return StyleBuildStats{}, err
	}
	st := StyleBuildStats{Authors: len(acc)}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	for uid, p := range acc {
		if p.chars < minChars || !l2Normalize(p.vec) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO style_profiles (user_id, ngrams, dims, vec, built_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET
				ngrams = excluded.ngrams, dims = excluded.dims,
				vec = excluded.vec, built_at = excluded.built_at`,
			uid, p.ngrams, dims, encodeVec(p.vec), nowStr); err != nil {
			return st, err
		}
		st.Eligible++
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// accumulateStyle читает все комментарии одним потоком и копит вектор 3-грамм на
// автора в памяти (≈16k авторов × dims float32 — десятки МБ).
func (s *Store) accumulateStyle(ctx context.Context, dims int) (map[int64]*styleAcc, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT author_id, text FROM comments WHERE author_id != 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	acc := map[int64]*styleAcc{}
	for rows.Next() {
		var aid int64
		var text string
		if err := rows.Scan(&aid, &text); err != nil {
			return nil, err
		}
		norm := normalizeStyle(text)
		if norm == "" {
			continue
		}
		p := acc[aid]
		if p == nil {
			p = &styleAcc{vec: make([]float32, dims)}
			acc[aid] = p
		}
		p.ngrams += addCharTrigrams(p.vec, norm, dims)
		p.chars += len([]rune(norm))
	}
	return acc, rows.Err()
}

// StylePair — пара авторов с центрированной косинусной похожестью стиля.
type StylePair struct {
	A, B   int64
	Cosine float64
}

// StyleClusterStats — итог ClusterStylometry.
type StyleClusterStats struct {
	Profiles int
	Pairs    int
	Top      []StylePair // топ пар для отчёта (не более ~20)
}

// ClusterStylometry ищет авторов с похожим стилем и пишет пары в
// alias_candidates(signal=stylometry). Похожесть — косинус ПОСЛЕ вычитания
// среднего профиля (убирает общий для всех регистр/тему, оставляя
// идиосинкразию). На автора берётся не более topK ближайших выше minCosine,
// глобально — не более maxPairs. Скор занижен (≤0.75): стиль слабее аватара/текста.
func (s *Store) ClusterStylometry(ctx context.Context, minCosine float64, topK, maxPairs int, now time.Time) (StyleClusterStats, error) {
	ids, vecs, err := s.loadStyleProfiles(ctx)
	if err != nil {
		return StyleClusterStats{}, err
	}
	st := StyleClusterStats{Profiles: len(ids)}
	if len(ids) < 2 {
		return st, nil
	}
	centerAndNormalize(vecs)

	pairs := topStylePairs(ids, vecs, minCosine, topK)
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Cosine > pairs[j].Cosine })
	if len(pairs) > maxPairs {
		pairs = pairs[:maxPairs]
	}
	st.Pairs = len(pairs)
	if len(pairs) > 20 {
		st.Top = pairs[:20]
	} else {
		st.Top = pairs
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	for _, p := range pairs {
		a, b := p.A, p.B
		if a > b {
			a, b = b, a
		}
		if err := upsertAliasCandidate(ctx, tx, aliasCand{
			a: a, b: b, signal: SignalStylometry, score: styloScore(p.Cosine, minCosine),
			evidence: fmt.Sprintf("стиль: центр-косинус %.3f", p.Cosine),
		}, nowStr); err != nil {
			return st, err
		}
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// StyleCosine возвращает центр-косинус для запрошенных пар (для валидации против
// известных альтов). NaN — если у кого-то из пары нет профиля.
func (s *Store) StyleCosine(ctx context.Context, want [][2]int64) ([]float64, error) {
	ids, vecs, err := s.loadStyleProfiles(ctx)
	if err != nil {
		return nil, err
	}
	centerAndNormalize(vecs)
	idx := make(map[int64]int, len(ids))
	for i, id := range ids {
		idx[id] = i
	}
	out := make([]float64, len(want))
	for k, pr := range want {
		i, iok := idx[pr[0]]
		j, jok := idx[pr[1]]
		if !iok || !jok {
			out[k] = math.NaN()
			continue
		}
		out[k] = dot(vecs[i], vecs[j])
	}
	return out, nil
}

func (s *Store) loadStyleProfiles(ctx context.Context) ([]int64, [][]float32, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, dims, vec FROM style_profiles`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var vecs [][]float32
	for rows.Next() {
		var uid int64
		var dims int
		var blob []byte
		if err := rows.Scan(&uid, &dims, &blob); err != nil {
			return nil, nil, err
		}
		ids = append(ids, uid)
		vecs = append(vecs, decodeVec(blob, dims))
	}
	return ids, vecs, rows.Err()
}

// neighbor — индекс соседа и косинус (внутреннее представление top-K).
type neighbor struct {
	j   int
	cos float64
}

// topStylePairs — ВЗАИМНЫЙ top-K: пара (i,j) идёт в результат только если j
// входит в topK ближайших i И одновременно i входит в topK ближайших j. Это
// режет «хабы» (один профиль, близкий ко многим, но те к нему — нет), которые
// на коротком тексте дают массу ложных пар.
func topStylePairs(ids []int64, vecs [][]float32, minCosine float64, topK int) []StylePair {
	top := make([][]neighbor, len(vecs))
	for i := range vecs {
		var best []neighbor
		for j := range vecs {
			if i == j {
				continue
			}
			c := dot(vecs[i], vecs[j])
			if c < minCosine {
				continue
			}
			best = insertTop(best, neighbor{j: j, cos: c}, topK)
		}
		top[i] = best
	}

	var out []StylePair
	seen := map[[2]int]bool{}
	for i, ns := range top {
		for _, n := range ns {
			if !hasNeighbor(top[n.j], i) { // взаимность
				continue
			}
			a, b := i, n.j
			if a > b {
				a, b = b, a
			}
			if seen[[2]int{a, b}] {
				continue
			}
			seen[[2]int{a, b}] = true
			out = append(out, StylePair{A: ids[i], B: ids[n.j], Cosine: n.cos})
		}
	}
	return out
}

func hasNeighbor(ns []neighbor, j int) bool {
	for _, n := range ns {
		if n.j == j {
			return true
		}
	}
	return false
}

// insertTop поддерживает срез из ≤k соседей, отсортированных по убыванию косинуса.
func insertTop(top []neighbor, n neighbor, k int) []neighbor {
	top = append(top, n)
	sort.Slice(top, func(i, j int) bool { return top[i].cos > top[j].cos })
	if len(top) > k {
		top = top[:k]
	}
	return top
}

// centerAndNormalize вычитает средний профиль из каждого вектора и заново
// L2-нормирует — так косинус меряет отклонение стиля от «среднего автора».
func centerAndNormalize(vecs [][]float32) {
	if len(vecs) == 0 {
		return
	}
	dims := len(vecs[0])
	mean := make([]float64, dims)
	for _, v := range vecs {
		for k, x := range v {
			mean[k] += float64(x)
		}
	}
	for k := range mean {
		mean[k] /= float64(len(vecs))
	}
	for _, v := range vecs {
		for k := range v {
			v[k] -= float32(mean[k])
		}
		l2Normalize(v)
	}
}

// styloScore переводит центр-косинус в вес [0.4,0.65]. Намеренно НИЖЕ дефолтного
// порога склейки personas cluster (0.7): стиль эмпирически шумный (не
// подтвердил аватар-пару, даёт хабы) — только советочный сигнал. Чтобы включить
// его в личности, порог опускают явно (`personas cluster -min-score 0.6`).
func styloScore(cos, minCosine float64) float64 {
	if cos <= minCosine {
		return 0.4
	}
	t := (cos - minCosine) / (1 - minCosine)
	return 0.4 + 0.25*t
}

// --- признаки и векторные утилиты ---

// normalizeStyle: нижний регистр + схлопывание любых пробелов в один. Разметку
// ([b][i], :::смайлы:::) НЕ трогаем — она общая для всех и уходит при центрировании,
// а частота её употребления сама по себе стилевой признак.
func normalizeStyle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
				space = true
			}
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// addCharTrigrams хэширует символьные 3-граммы строки в вектор (signed hashing
// trick — знак от старшего бита хэша снижает смещение от коллизий). Возвращает
// число учтённых 3-грамм.
func addCharTrigrams(vec []float32, s string, dims int) int {
	r := []rune(s)
	n := 0
	for i := 0; i+3 <= len(r); i++ {
		h := hashTrigram(r[i], r[i+1], r[i+2])
		bucket := int(h % uint64(dims))
		if h&(1<<63) != 0 {
			vec[bucket]--
		} else {
			vec[bucket]++
		}
		n++
	}
	return n
}

// hashTrigram — FNV-1a по байтам трёх рун.
func hashTrigram(a, b, c rune) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, x := range [3]rune{a, b, c} {
		v := uint32(x)
		for k := 0; k < 4; k++ {
			h ^= uint64(byte(v >> (k * 8)))
			h *= prime
		}
	}
	return h
}

// l2Normalize нормирует вектор на месте; false — нулевой вектор (нормировать нечего).
func l2Normalize(v []float32) bool {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return false
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return true
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

func encodeVec(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(x))
	}
	return buf
}

func decodeVec(buf []byte, dims int) []float32 {
	v := make([]float32, dims)
	for i := 0; i < dims && 4*i+4 <= len(buf); i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[4*i:]))
	}
	return v
}
