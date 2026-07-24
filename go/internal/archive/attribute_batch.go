package archive

import (
	"context"
	"sort"
	"strings"
)

// NoteAttribution — итог атрибуции одной заметки известного автора (валидация):
// на каком месте оказалась его собственная анкета и кого система поставила
// первым.
type NoteAttribution struct {
	NoteID     int64
	AuthorID   int64
	AuthorName string
	Chars      int    // длина текста, рун
	Snippet    string // начало текста для узнавания
	Rank       int    // место реального автора (0 — у анкеты нет стиль-профиля/текст непригоден)
	Score      float64
	StyleZ     float64
	LexZ       float64
	HasLex     bool
	TopID      int64  // кого система поставила на 1-е место
	TopName    string
	TopScore   float64
	Self       bool // топ-1 принадлежит той же личности (человек «узнан», пусть и по альту)
}

// IdentityNotesReport — сводка атрибуции по всем заметкам личности.
type IdentityNotesReport struct {
	Identity      string
	Accounts      int
	StyleProfiles int
	LexProfiles   int
	LexWeight     float64
	Notes         []NoteAttribution // в порядке заметок (по id)
	Scored        int               // заметок с валидным рангом
	Top1, Top5    int
	Top10         int
	SelfTop1      int // в скольких топ-1 — та же личность
	MedianRank    int
}

// AttributeIdentityNotes прогоняет все заметки личности (token: p<id>|u<id>|user_id)
// через атрибуцию, загрузив профили ОДИН раз. Для каждой заметки — ранг реальной
// анкеты автора среди всех и кого система назвала первым. lexWeight — вес лексики.
// Caveat: текст заметки входит в профиль её автора (leave-one-out нарушен), поэтому
// ранги оптимистичны; сигнал всё равно показателен относительно.
func (s *Store) AttributeIdentityNotes(ctx context.Context, token string, lexWeight float64) (IdentityNotesReport, error) {
	identity, err := s.canonIdentity(ctx, token)
	if err != nil {
		return IdentityNotesReport{}, err
	}
	accIDs, err := s.identityAccountIDs(ctx, identity)
	if err != nil {
		return IdentityNotesReport{}, err
	}
	sa, la, err := s.loadAttributors(ctx)
	if err != nil {
		return IdentityNotesReport{}, err
	}
	idmap, err := s.identityMap(ctx)
	if err != nil {
		return IdentityNotesReport{}, err
	}
	notes, err := s.notesByAuthors(ctx, accIDs)
	if err != nil {
		return IdentityNotesReport{}, err
	}

	rep := IdentityNotesReport{
		Identity: identity, Accounts: len(accIDs),
		StyleProfiles: len(sa.ids), LexWeight: lexWeight,
	}
	if la != nil {
		rep.LexProfiles = len(la.ids)
	}
	sIndex := make(map[int64]int, len(sa.ids))
	for i, id := range sa.ids {
		sIndex[id] = i
	}

	var ranks []int
	needNames := map[int64]bool{}
	for _, n := range notes {
		na := s.scoreNote(n, sa, la, sIndex, idmap, identity, lexWeight)
		rep.Notes = append(rep.Notes, na)
		needNames[na.AuthorID] = true
		if na.Rank == 0 {
			continue
		}
		needNames[na.TopID] = true
		ranks = append(ranks, na.Rank)
		rep.Scored++
		if na.Rank <= 1 {
			rep.Top1++
		}
		if na.Rank <= 5 {
			rep.Top5++
		}
		if na.Rank <= 10 {
			rep.Top10++
		}
		if na.Self {
			rep.SelfTop1++
		}
	}
	if err := s.fillNoteNames(ctx, rep.Notes, needNames); err != nil {
		return rep, err
	}
	rep.MedianRank = medianInt(ranks)
	return rep, nil
}

// scoreNote атрибутирует один текст: ранг реального автора и топ-1.
func (s *Store) scoreNote(n authoredNote, sa *styleAttributor, la *lexisAttributor,
	sIndex map[int64]int, idmap map[int64]string, identity string, lexWeight float64) NoteAttribution {
	na := NoteAttribution{
		NoteID: n.id, AuthorID: n.author,
		Chars: len([]rune(n.text)), Snippet: snippet(n.text, 60),
	}
	authorIdx, ok := sIndex[n.author]
	if !ok {
		return na // у анкеты автора нет стиль-профиля — ранг неопределён (0)
	}
	sCos, _, ok := sa.cosines(normalizeStyle(n.text))
	if !ok {
		return na
	}
	at := &Attribution{LexWeight: lexWeight}
	at.StyleCosMean, at.StyleCosStd = meanStd(sCos)
	var lexCos map[int64]float64
	if la != nil {
		lexCos, at.LexCosMean, at.LexCosStd, _ = la.cosines(n.text)
	}

	scores := make([]float64, len(sa.ids))
	topIdx := 0
	for i := range sa.ids {
		_, _, scores[i], _ = combineScores(sCos[i], sa.ids[i], lexCos, at)
		if scores[i] > scores[topIdx] {
			topIdx = i
		}
	}
	aScore := scores[authorIdx]
	rank := 1
	for _, sc := range scores {
		if sc > aScore {
			rank++
		}
	}
	na.Rank, na.Score = rank, aScore
	na.StyleZ, na.LexZ, _, na.HasLex = combineScores(sCos[authorIdx], n.author, lexCos, at)
	na.TopID, na.TopScore = sa.ids[topIdx], scores[topIdx]
	na.Self = identity != "" && idmap[na.TopID] == identity
	return na
}

// loadAttributors грузит стиль- и (если построен) лексический слой в готовые к
// серии запросов аттрибуторы.
func (s *Store) loadAttributors(ctx context.Context) (*styleAttributor, *lexisAttributor, error) {
	sIDs, sVecs, err := s.loadStyleProfiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(sIDs) < 2 {
		return nil, nil, errFewStyleProfiles(len(sIDs))
	}
	sa := newStyleAttributor(sIDs, sVecs)

	lIDs, lVecs, idf, dims, err := s.loadLexisProfiles(ctx)
	if err != nil {
		return nil, nil, err
	}
	var la *lexisAttributor
	if len(lIDs) >= 2 {
		la = &lexisAttributor{ids: lIDs, vecs: lVecs, idf: idf, dims: dims}
	}
	return sa, la, nil
}

// authoredNote — сырой снимок заметки для атрибуции.
type authoredNote struct {
	id     int64
	author int64
	text   string
}

func (s *Store) notesByAuthors(ctx context.Context, authorIDs []int64) ([]authoredNote, error) {
	if len(authorIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, author_id, text FROM notes
		 WHERE author_id IN (`+intList(authorIDs)+`) AND text != '' ORDER BY id`)
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

// fillNoteNames проставляет имена автора и топ-1 одним запросом.
func (s *Store) fillNoteNames(ctx context.Context, notes []NoteAttribution, need map[int64]bool) error {
	ids := make([]int64, 0, len(need))
	for id := range need {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	names, err := s.namesByIDs(ctx, ids)
	if err != nil {
		return err
	}
	for i := range notes {
		notes[i].AuthorName = names[notes[i].AuthorID]
		notes[i].TopName = names[notes[i].TopID]
	}
	return nil
}

// --- аттрибуторы (профили грузятся один раз, обслуживают серию запросов) ---

// styleAttributor — центрированные стиль-профили для серии запросов.
type styleAttributor struct {
	ids  []int64
	vecs [][]float32
	mean []float64
	dims int
}

func newStyleAttributor(ids []int64, vecs [][]float32) *styleAttributor {
	mean := meanVec(vecs)
	for _, v := range vecs {
		centerVec(v, mean)
	}
	return &styleAttributor{ids: ids, vecs: vecs, mean: mean, dims: len(vecs[0])}
}

// vec строит центрированный вектор текста (запроса ИЛИ эталона) тем же средним,
// что и профили. false — текст короче 3 символов.
func (a *styleAttributor) vec(norm string) ([]float32, int, bool) {
	q := make([]float32, a.dims)
	qn := addCharTrigrams(q, norm, a.dims)
	if qn == 0 || !l2Normalize(q) {
		return nil, qn, false
	}
	centerVec(q, a.mean)
	return q, qn, true
}

// cosinesOf — косинусы готового вектора со всеми профилями.
func (a *styleAttributor) cosinesOf(v []float32) []float64 {
	cos := make([]float64, len(a.vecs))
	for i, pv := range a.vecs {
		cos[i] = dot(v, pv)
	}
	return cos
}

// cosines центрирует вектор запроса тем же средним и меряет косинус со всеми
// профилями. false — текст короче 3 символов.
func (a *styleAttributor) cosines(norm string) ([]float64, int, bool) {
	v, qn, ok := a.vec(norm)
	if !ok {
		return nil, qn, false
	}
	return a.cosinesOf(v), qn, true
}

// lexisAttributor — лексические профили + IDF для серии запросов.
type lexisAttributor struct {
	ids  []int64
	vecs [][]float32
	idf  []float32
	dims int
}

// vec — tf-idf-вектор текста (запроса ИЛИ эталона). false — запрос без слов.
func (a *lexisAttributor) vec(text string) ([]float32, int, bool) {
	return buildLexisQuery(text, a.idf, a.dims)
}

// cosinesOf — косинусы готового вектора со всеми профилями + фон (среднее/сигма).
func (a *lexisAttributor) cosinesOf(v []float32) (map[int64]float64, float64, float64) {
	cos := make([]float64, len(a.vecs))
	for i, pv := range a.vecs {
		cos[i] = dot(v, pv)
	}
	mean, std := meanStd(cos)
	m := make(map[int64]float64, len(a.ids))
	for i, id := range a.ids {
		m[id] = cos[i]
	}
	return m, mean, std
}

// cosines возвращает карту user_id→косинус, фон (среднее/сигма) и ok=false при
// запросе без слов.
func (a *lexisAttributor) cosines(text string) (map[int64]float64, float64, float64, bool) {
	v, _, ok := a.vec(text)
	if !ok {
		return nil, 0, 0, false
	}
	m, mean, std := a.cosinesOf(v)
	return m, mean, std, true
}

// --- утилиты ---

func medianInt(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	return s[len(s)/2]
}

// snippet — однострочное начало текста (до n рун) для узнавания заметки.
func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
