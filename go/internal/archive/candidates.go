package archive

import "context"

// candidateFilter — общий фильтр правдоподобных кандидатов-авторов, два среза
// (обе идеи пользователя): рецент (анкета недавно появлялась на сайте) и жанр
// (анкета пишет заметки, а не только комментарии). Атрибутируем текст-ЗАМЕТКУ,
// поэтому сравнивать её осмысленно лишь с теми, кто физически мог её написать:
// «мёртвая» анкета не напишет свежий текст, а чистый комментатор — заметку.
// Выключенный фильтр (оба среза off) пропускает всех. Общий для attribute,
// verify и calibrate.
type candidateFilter struct {
	lastActive  map[int64]string // author_id → последняя дата активности (ISO)
	cutoff      string           // порог свежести (ISO); "" — рецент-срез выключен
	noteWriters map[int64]bool   // анкеты с ≥N заметок; nil — жанровый срез выключен
}

// buildCandidateFilter собирает фильтр по окну активности (activeDays сут) и
// минимуму заметок (minAuthorNotes). При обоих нулях — выключенный фильтр
// (on()==false), пропускающий всех.
func (s *Store) buildCandidateFilter(ctx context.Context, activeDays, minAuthorNotes int) (*candidateFilter, error) {
	la, cutoff, err := s.recencyCutoff(ctx, activeDays)
	if err != nil {
		return nil, err
	}
	cf := &candidateFilter{lastActive: la, cutoff: cutoff}
	if minAuthorNotes > 0 {
		nc, err := s.noteCounts(ctx)
		if err != nil {
			return nil, err
		}
		cf.noteWriters = make(map[int64]bool, len(nc))
		for id, n := range nc {
			if n >= minAuthorNotes {
				cf.noteWriters[id] = true
			}
		}
	}
	return cf, nil
}

// on — включён ли хоть один срез фильтра.
func (cf *candidateFilter) on() bool { return cf.cutoff != "" || cf.noteWriters != nil }

// ok — проходит ли кандидат все включённые срезы (живой И пишет заметки).
func (cf *candidateFilter) ok(id int64) bool {
	if cf.cutoff != "" && cf.lastActive[id] < cf.cutoff {
		return false
	}
	if cf.noteWriters != nil && !cf.noteWriters[id] {
		return false
	}
	return true
}

// count — сколько из ids проходит фильтр (для сводок).
func (cf *candidateFilter) count(ids []int64) int {
	n := 0
	for _, id := range ids {
		if cf.ok(id) {
			n++
		}
	}
	return n
}

// meanStdFiltered — фон косинуса только по кандидатам, прошедшим фильтр (при
// выключенном фильтре — по всем); ids и cos сонаправлены. Слишком узкий отбор
// (<2) → откат к полному фону, чтобы не делить на ноль.
func meanStdFiltered(ids []int64, cos []float64, cf *candidateFilter) (float64, float64) {
	if cf == nil || !cf.on() {
		return meanStd(cos)
	}
	sub := make([]float64, 0, len(cos))
	for k, id := range ids {
		if cf.ok(id) {
			sub = append(sub, cos[k])
		}
	}
	if len(sub) < 2 {
		return meanStd(cos)
	}
	return meanStd(sub)
}

// meanStdMapFiltered — то же для лексики, где косинусы лежат в карте id→cos.
func meanStdMapFiltered(ids []int64, cos map[int64]float64, cf *candidateFilter) (float64, float64) {
	sub := make([]float64, 0, len(cos))
	for _, id := range ids {
		c, ok := cos[id]
		if !ok {
			continue
		}
		if cf == nil || !cf.on() || cf.ok(id) {
			sub = append(sub, c)
		}
	}
	if len(sub) < 2 {
		return 0, 0
	}
	return meanStd(sub)
}
