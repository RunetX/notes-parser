package archive

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Поиск анонимок: заметки без автора (author_id IS NULL) прогоняются через тот же
// скоринг, что и verify — комбинированный z подозреваемого (максимум по его
// анкетам) на фоне всех профилей, — и ранжируются по z. Порог берётся из того же
// нулевого распределения (z подозреваемого на чужих заметках), поэтому выдача
// сопоставима с одиночным verify.
//
// ВАЖНО про статистику: скан прогоняет десятки тысяч текстов, поэтому даже порог
// FPR 1% даёт сотни ложных срабатываний. Число ожидаемых ложных считается и
// печатается рядом — без него верхушка списка читается как «найдено», хотя это
// может быть чистый хвост фона.

// AnonScanParams — параметры поиска анонимок.
type AnonScanParams struct {
	Suspect        string  // p<id>|u<id>|user_id
	Genre          string  // all | notes
	LexWeight      float64 // вес лексики в комбинированном скоре
	ActiveDays     int     // фильтр фона: только активные за N суток (0 — все)
	MinAuthorNotes int     // фильтр фона: писавшие ≥N заметок (0 — все)
	From, To       string  // окно публикации, ISO-даты (пусто — без границы)
	MinChars       int     // короче — пропуск (ранжирование на коротком тексте бессмысленно)
	Top            int     // сколько лучших вернуть
	NullN          int     // размер нулевого распределения
}

// AnonHit — анонимная заметка, похожая на почерк подозреваемого.
type AnonHit struct {
	NoteID      int64
	PublishedAt string
	Chars       int
	Snippet     string
	Z           float64 // комбинированный z (стиль + лексика)
	StyleZ      float64
	LexZ        float64
	HasLex      bool
	BestID      int64  // анкета подозреваемого, давшая максимум
	BestName    string
	AbovePct    float64 // доля фона, которую текст перекрывает (0..1)
}

// AnonScanResult — итог скана.
type AnonScanResult struct {
	Identity      string
	Accounts      int
	Genre         string
	StyleProfiles int
	BgProfiles    int
	Scanned       int // сколько анонимок отскорено
	SkippedShort  int // отброшено по MinChars / нехватке 3-грамм
	NullN         int
	NullMean      float64
	NullStd       float64
	NullP95       float64
	NullP99       float64
	NullMax       float64
	AboveP95      int     // сколько анонимок выше порога FPR 5%
	AboveP99      int     // …и FPR 1%
	AboveMax      int     // …и выше максимума фона
	ExpectedFP95  float64 // сколько ложных ожидается на этом объёме
	ExpectedFP99  float64
	Hits          []AnonHit // Top лучших по z
}

// ScanAnonymous ищет среди анонимных заметок похожие на почерк подозреваемого.
func (s *Store) ScanAnonymous(ctx context.Context, p AnonScanParams) (AnonScanResult, error) {
	member := s.identityMembers(ctx, p.Suspect)
	if member == nil {
		return AnonScanResult{}, fmt.Errorf("anon: подозреваемый %q не найден или без анкет", p.Suspect)
	}
	identity, _ := s.canonIdentity(ctx, p.Suspect)
	sa, la, err := s.loadAttributors(ctx, p.Genre)
	if err != nil {
		return AnonScanResult{}, err
	}
	cf, err := s.buildCandidateFilter(ctx, p.ActiveDays, p.MinAuthorNotes)
	if err != nil {
		return AnonScanResult{}, err
	}

	res := AnonScanResult{
		Identity: identity, Accounts: len(member), Genre: p.Genre,
		StyleProfiles: len(sa.ids), BgProfiles: len(sa.ids),
	}
	if cf.on() {
		res.BgProfiles = cf.count(sa.ids)
	}

	// Нулевое распределение — тот же приём, что в verify: z подозреваемого на
	// заметках заведомо чужих авторов.
	null, err := s.suspectNull(ctx, sa, la, member, p.LexWeight, p.NullN, cf)
	if err != nil {
		return res, err
	}
	var vr VerifyResult
	fillNullStats(&vr, null)
	res.NullN, res.NullMean, res.NullStd = vr.NullN, vr.NullMean, vr.NullStd
	res.NullP95, res.NullP99, res.NullMax = vr.NullP95, vr.NullP99, vr.NullMax
	sortedNull := append([]float64(nil), null...)
	sort.Float64s(sortedNull)

	rows, err := s.anonRows(ctx, p)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	sc := &anonScorer{sa: sa, la: la, member: member, lexWeight: p.LexWeight, cf: cf, minChars: p.MinChars}
	hits := make([]AnonHit, 0, p.Top+1)
	for rows.Next() {
		var id int64
		var at, text string
		if err := rows.Scan(&id, &at, &text); err != nil {
			return res, err
		}
		hit, ok, err := sc.score(id, at, text)
		if err != nil {
			return res, err
		}
		if !ok {
			res.SkippedShort++
			continue
		}
		res.Scanned++
		countAbove(&res, hit.Z)
		hit.AbovePct = fracBelow(sortedNull, hit.Z)
		hits = insertHit(hits, hit, p.Top)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}

	// Ожидаемое число ложных на таком объёме: порог FPR × отсканировано.
	res.ExpectedFP95 = 0.05 * float64(res.Scanned)
	res.ExpectedFP99 = 0.01 * float64(res.Scanned)

	ids := make([]int64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.BestID)
	}
	if names, err := s.namesByIDs(ctx, ids); err == nil {
		for i := range hits {
			hits[i].BestName = names[hits[i].BestID]
		}
	}
	res.Hits = hits
	return res, nil
}

// anonScorer — один текст → комбинированный z подозреваемого. Профили и фильтр
// фона загружены один раз, поэтому скан идёт потоком.
type anonScorer struct {
	sa        *styleAttributor
	la        *lexisAttributor
	member    map[int64]bool
	lexWeight float64
	cf        *candidateFilter
	minChars  int
}

// score возвращает ok=false, если текст короче порога или из него не собрать
// вектор (ранжировать такое бессмысленно).
func (sc *anonScorer) score(id int64, at, text string) (AnonHit, bool, error) {
	runes := []rune(text)
	if len(runes) < sc.minChars {
		return AnonHit{}, false, nil
	}
	qsv, _, ok := sc.sa.vec(normalizeStyle(text))
	if !ok {
		return AnonHit{}, false, nil
	}
	var qlv []float32
	if sc.la != nil {
		qlv, _, _ = sc.la.vec(text)
	}
	z, sz, lz, bestID, hasLex, ok := suspectScore(sc.sa, sc.la, qsv, qlv, sc.member, sc.lexWeight, sc.cf)
	if !ok {
		return AnonHit{}, false, fmt.Errorf("anon: у подозреваемого нет стиль-профиля (мало текста)")
	}
	return AnonHit{
		NoteID: id, PublishedAt: at, Chars: len(runes), Snippet: snippet(text, 110),
		Z: z, StyleZ: sz, LexZ: lz, HasLex: hasLex, BestID: bestID,
	}, true, nil
}

// countAbove копит, сколько анонимок перекрыло пороги фона.
func countAbove(res *AnonScanResult, z float64) {
	if z > res.NullP95 {
		res.AboveP95++
	}
	if z > res.NullP99 {
		res.AboveP99++
	}
	if z > res.NullMax {
		res.AboveMax++
	}
}

// anonRows — курсор по анонимным заметкам окна (author_id IS NULL).
func (s *Store) anonRows(ctx context.Context, p AnonScanParams) (*sql.Rows, error) {
	q := `SELECT id, COALESCE(published_at,''), text FROM notes
	      WHERE author_id IS NULL AND text != ''`
	var args []any
	if p.From != "" {
		q += ` AND published_at >= ?`
		args = append(args, p.From)
	}
	if p.To != "" {
		q += ` AND published_at < ?`
		args = append(args, p.To)
	}
	q += ` ORDER BY id`
	return s.db.QueryContext(ctx, q, args...)
}

// insertHit держит топ-N по z без полной сортировки корпуса.
func insertHit(hits []AnonHit, h AnonHit, top int) []AnonHit {
	if top <= 0 {
		top = 20
	}
	i := sort.Search(len(hits), func(i int) bool { return hits[i].Z < h.Z })
	if i >= top {
		return hits
	}
	hits = append(hits, AnonHit{})
	copy(hits[i+1:], hits[i:])
	hits[i] = h
	if len(hits) > top {
		hits = hits[:top]
	}
	return hits
}

// fracBelow — доля фона строго ниже z (насколько текст «свой» относительно чужих).
func fracBelow(sortedNull []float64, z float64) float64 {
	if len(sortedNull) == 0 {
		return 0
	}
	i := sort.SearchFloat64s(sortedNull, z)
	return float64(i) / float64(len(sortedNull))
}

