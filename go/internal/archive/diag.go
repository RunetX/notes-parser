package archive

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"time"
)

// PersonaDiag — диагностика набора ЗАВЕДОМО связанных анкет (ground truth) для
// калибровки сигналов склейки. Ничего не пишет — только читает. По каждой анкете:
// активность; по стилю — ближайшие соседи среди ВСЕХ профилей и ранг своих
// сиблингов (buried под чужими или всплывает?); попарно — пересечение
// собеседников, кросс-ответы и временной паттерн.
type PersonaDiag struct {
	Accounts []DiagAccount
	Style    []DiagStyle
	Pairs    []DiagPair
}

// DiagAccount — активность одной анкеты набора.
type DiagAccount struct {
	ID         int64
	Name       string
	Age        string
	Comments   int
	Notes      int
	ActiveFrom string
	ActiveTo   string
	HasProfile bool
	Ngrams     int // объём текста в профиле стиля
}

// DiagStyle — стилевая позиция анкеты: топ ближайших по центр-косинусу (свои
// помечены Known) + ранг/косинус каждого известного сиблинга среди всех профилей.
type DiagStyle struct {
	ID        int64
	Total     int // всего профилей сравнивалось
	Neighbors []DiagNeighbor
	Siblings  map[int64]DiagRank // по каждому другому id набора
}

// DiagNeighbor — ближайший по стилю сосед.
type DiagNeighbor struct {
	ID     int64
	Name   string
	Cosine float64
	Known  bool // входит в проверяемый набор
}

// DiagRank — позиция сиблинга в рейтинге стиля анкеты (1 = ближайший; 0 — нет профиля).
type DiagRank struct {
	Rank   int
	Cosine float64
}

// DiagPair — попарные сигналы между двумя анкетами набора.
type DiagPair struct {
	A, B             int64
	StyleCosine      float64 // NaN — у кого-то нет профиля
	CrossRepliesAB   int     // A отвечал на комментарии B
	CrossRepliesBA   int     // B отвечал на комментарии A
	PartnersA        int     // размер круга собеседников A (без анкет набора)
	PartnersB        int
	SharedPartners   int
	JaccardPartners  float64
	TemporalRelation string // overlap|handoff|disjoint|unknown
	GapDays          int    // >0 — разрыв между спанами; 0 — пересечение
}

// DiagPersonas собирает диагностику по набору id (все читающие запросы).
func (s *Store) DiagPersonas(ctx context.Context, ids []int64) (PersonaDiag, error) {
	known := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}

	acc, err := s.diagAccounts(ctx, ids)
	if err != nil {
		return PersonaDiag{}, err
	}

	pids, vecs, err := s.loadStyleProfiles(ctx, GenreAll)
	if err != nil {
		return PersonaDiag{}, err
	}
	centerAndNormalize(vecs)
	idx := make(map[int64]int, len(pids))
	for i, id := range pids {
		idx[id] = i
	}

	style, names := diagStyle(ids, known, pids, vecs, idx)
	if err := s.fillNames(ctx, names); err != nil {
		return PersonaDiag{}, err
	}
	for si := range style {
		for ni := range style[si].Neighbors {
			style[si].Neighbors[ni].Name = names[style[si].Neighbors[ni].ID]
		}
	}

	repliesTo, partners, err := s.diagInteractions(ctx, ids, known)
	if err != nil {
		return PersonaDiag{}, err
	}

	d := PersonaDiag{
		Accounts: s.finishAccounts(ctx, ids, acc, idx),
		Style:    style,
		Pairs:    diagPairs(ids, idx, vecs, repliesTo, partners, acc),
	}
	return d, nil
}

// diagAccounts — активность каждой анкеты (без профиля стиля; тот добавит идентификатор idx).
func (s *Store) diagAccounts(ctx context.Context, ids []int64) (map[int64]DiagAccount, error) {
	acc := make(map[int64]DiagAccount, len(ids))
	for _, id := range ids {
		a := DiagAccount{ID: id}
		err := s.db.QueryRowContext(ctx, `
			SELECT u.name, u.age, COUNT(c.id), COUNT(DISTINCT c.note_id),
			       COALESCE(MIN(c.published_at), ''), COALESCE(MAX(c.published_at), '')
			FROM users u LEFT JOIN comments c ON c.author_id = u.id
			WHERE u.id = ? GROUP BY u.id`, id).Scan(
			&a.Name, &a.Age, &a.Comments, &a.Notes, &a.ActiveFrom, &a.ActiveTo)
		if errors.Is(err, sql.ErrNoRows) {
			a.Name = "(нет в архиве)"
		} else if err != nil {
			return nil, err
		}
		acc[id] = a
	}
	return acc, nil
}

// finishAccounts помечает наличие профиля и подтягивает объём текста (Ngrams).
func (s *Store) finishAccounts(ctx context.Context, ids []int64, acc map[int64]DiagAccount, idx map[int64]int) []DiagAccount {
	out := make([]DiagAccount, 0, len(ids))
	for _, id := range ids {
		a := acc[id]
		if _, ok := idx[id]; ok {
			a.HasProfile = true
			var ng int
			if err := s.db.QueryRowContext(ctx,
				`SELECT ngrams FROM style_profiles WHERE user_id = ? AND genre = ?`, id, GenreAll).Scan(&ng); err == nil {
				a.Ngrams = ng
			}
		}
		out = append(out, a)
	}
	return out
}

// diagStyle считает стилевую позицию каждой анкеты и собирает id соседей для имён.
func diagStyle(ids []int64, known map[int64]bool, pids []int64, vecs [][]float32, idx map[int64]int) ([]DiagStyle, map[int64]string) {
	names := map[int64]string{}
	out := make([]DiagStyle, 0, len(ids))
	for _, id := range ids {
		st := DiagStyle{ID: id, Total: len(pids), Siblings: map[int64]DiagRank{}}
		if i, ok := idx[id]; ok {
			st.Neighbors, st.Siblings = styleNeighbors(i, pids, vecs, known)
			for _, n := range st.Neighbors {
				names[n.ID] = ""
			}
		}
		for _, other := range ids {
			if other == id {
				continue
			}
			if _, ok := st.Siblings[other]; !ok {
				st.Siblings[other] = DiagRank{Rank: 0, Cosine: math.NaN()}
			}
		}
		out = append(out, st)
	}
	return out, names
}

// styleNeighbors — топ-8 ближайших по центр-косинусу к профилю i и ранги сиблингов.
func styleNeighbors(i int, pids []int64, vecs [][]float32, known map[int64]bool) ([]DiagNeighbor, map[int64]DiagRank) {
	type sc struct {
		id  int64
		cos float64
	}
	all := make([]sc, 0, len(pids))
	for j := range vecs {
		if j == i {
			continue
		}
		all = append(all, sc{pids[j], dot(vecs[i], vecs[j])})
	}
	sort.Slice(all, func(x, y int) bool { return all[x].cos > all[y].cos })

	siblings := map[int64]DiagRank{}
	for r, x := range all {
		if known[x.id] {
			siblings[x.id] = DiagRank{Rank: r + 1, Cosine: x.cos}
		}
	}
	var neighbors []DiagNeighbor
	for k := 0; k < len(all) && k < 8; k++ {
		neighbors = append(neighbors, DiagNeighbor{ID: all[k].id, Cosine: all[k].cos, Known: known[all[k].id]})
	}
	return neighbors, siblings
}

// diagInteractions — карта «кому отвечал» (для кросс-ответов) и круг собеседников
// (union входящих/исходящих, без анкет самого набора) по каждой анкете.
func (s *Store) diagInteractions(ctx context.Context, ids []int64, known map[int64]bool) (map[int64]map[int64]int, map[int64]map[int64]bool, error) {
	repliesTo := map[int64]map[int64]int{}
	partners := map[int64]map[int64]bool{}
	for _, id := range ids {
		rt, err := s.replyOutCounts(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		rb, err := s.replyInCounts(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		repliesTo[id] = rt
		partners[id] = partnerSet(rt, rb, known)
	}
	return repliesTo, partners, nil
}

// partnerSet — круг собеседников: объединение авторов входящих/исходящих ответов,
// без нулевого id и без анкет самого набора.
func partnerSet(rt, rb map[int64]int, known map[int64]bool) map[int64]bool {
	set := map[int64]bool{}
	for _, m := range []map[int64]int{rt, rb} {
		for k := range m {
			if k != 0 && !known[k] {
				set[k] = true
			}
		}
	}
	return set
}

// diagPairs заполняет попарные сигналы (стиль/кросс-ответы/собеседники/время).
func diagPairs(ids []int64, idx map[int64]int, vecs [][]float32, repliesTo map[int64]map[int64]int, partners map[int64]map[int64]bool, acc map[int64]DiagAccount) []DiagPair {
	var out []DiagPair
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			out = append(out, onePair(ids[i], ids[j], idx, vecs, repliesTo, partners, acc))
		}
	}
	return out
}

func onePair(a, b int64, idx map[int64]int, vecs [][]float32, repliesTo map[int64]map[int64]int, partners map[int64]map[int64]bool, acc map[int64]DiagAccount) DiagPair {
	p := DiagPair{A: a, B: b, StyleCosine: math.NaN()}
	if ia, oka := idx[a]; oka {
		if ib, okb := idx[b]; okb {
			p.StyleCosine = dot(vecs[ia], vecs[ib])
		}
	}
	p.CrossRepliesAB = repliesTo[a][b]
	p.CrossRepliesBA = repliesTo[b][a]
	pa, pb := partners[a], partners[b]
	for k := range pa {
		if pb[k] {
			p.SharedPartners++
		}
	}
	p.PartnersA, p.PartnersB = len(pa), len(pb)
	if u := p.PartnersA + p.PartnersB - p.SharedPartners; u > 0 {
		p.JaccardPartners = float64(p.SharedPartners) / float64(u)
	}
	p.TemporalRelation, p.GapDays = temporalRel(acc[a], acc[b])
	return p
}

// replyOutCounts — кому автор адресовал реплики: адресат → счётчик.
func (s *Store) replyOutCounts(ctx context.Context, id int64) (map[int64]int, error) {
	return s.replyCounts(ctx, `
		SELECT `+sqlAddressee+`, COUNT(*)
		FROM comments c `+sqlAddresseeJoin+`
		WHERE c.author_id = ? AND c.parent_id != 0
		GROUP BY 1`, id)
}

// replyInCounts — кто адресовал реплики автору: их автор → счётчик.
// Список анкет здесь из одного id, поэтому он подставляется дважды.
func (s *Store) replyInCounts(ctx context.Context, id int64) (map[int64]int, error) {
	q := `SELECT author_id, COUNT(*) FROM (` +
		sqlInboundReplies("?", "c.author_id AS author_id") + `) GROUP BY author_id`
	return s.replyCountsArgs(ctx, q, id, id)
}

func (s *Store) replyCounts(ctx context.Context, query string, id int64) (map[int64]int, error) {
	return s.replyCountsArgs(ctx, query, id)
}

func (s *Store) replyCountsArgs(ctx context.Context, query string, args ...any) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var aid int64
		var n int
		if err := rows.Scan(&aid, &n); err != nil {
			return nil, err
		}
		out[aid] = n
	}
	return out, rows.Err()
}

// fillNames заполняет имена по id (для отчёта соседей).
func (s *Store) fillNames(ctx context.Context, names map[int64]string) error {
	for id := range names {
		var n string
		err := s.db.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, id).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			n = "?"
		} else if err != nil {
			return err
		}
		names[id] = n
	}
	return nil
}

// temporalRel классифицирует взаимное расположение спанов активности двух анкет.
// overlap — спаны пересекаются; handoff — разрыв ≤1 года (похоже на переход на
// альт); disjoint — разрыв больше; unknown — нет дат.
func temporalRel(a, b DiagAccount) (string, int) {
	af, aok := parseDay(a.ActiveFrom)
	at, _ := parseDay(a.ActiveTo)
	bf, bok := parseDay(b.ActiveFrom)
	bt, _ := parseDay(b.ActiveTo)
	if !aok || !bok {
		return "unknown", 0
	}
	if !af.After(bt) && !bf.After(at) {
		return "overlap", 0
	}
	var gap int
	if at.Before(bf) {
		gap = int(bf.Sub(at).Hours() / 24)
	} else {
		gap = int(af.Sub(bt).Hours() / 24)
	}
	if gap > 365 {
		return "disjoint", gap
	}
	return "handoff", gap
}

func parseDay(s string) (time.Time, bool) {
	if len(s) < 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
