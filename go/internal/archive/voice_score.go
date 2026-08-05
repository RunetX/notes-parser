package archive

// «Голос» здесь — авторская манера письма, не голосовое сообщение (ср.
// tgx.VoiceHandler — то про ASR).
//
// Ядро скоринга произвольного текста против загруженного слоя профилей. Живёт
// внутри пакета намеренно: styleAttributor центрирует запрос тем же средним, что
// и профили, а lexisAttributor НАМЕРЕННО не центрирован (см. attribute_batch.go).
// Вынеси эту пару наружу — и однажды лексику отцентрируют «для симметрии», молча
// убив атрибуцию: замер показывал медиану ранга автора 2→55.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// BandContaminationMax — выше этой доли эталонная полоса не строится. Тексты
// полосы ВХОДЯТ в стиль-профиль автора (leave-one-out нарушен, тот же caveat, что
// у AttributeIdentityNotes), а черновик — нет. Полоса поэтому оптимистична, и у
// автора с четырьмя заметками одна заметка — четверть его профиля: сравнивать с
// такой полосой нельзя, честнее отказаться и сказать почему.
const BandContaminationMax = 0.05

// AcceptQuantile — черновик принят, если узнаётся не хуже нижней четверти
// РЕАЛЬНЫХ текстов автора. Порогом по рангу это выразить нельзя: у одного автора
// его собственные заметки лежат на 1–26, у другого на 40–3000.
const AcceptQuantile = 0.25

// VoiceScore — как атрибутор архива видит один текст.
type VoiceScore struct {
	Runes  int `json:"runes"`
	Ngrams int `json:"ngrams"` // ниже ~300 ранжирование заметно шумит
	Tokens int `json:"tokens"`

	Rank     int    `json:"rank"` // место лучшей анкеты цели; 0 — нет профиля/текст непригоден
	Of       int    `json:"of"`   // сколько кандидатов в рейтинге
	BestID   int64  `json:"best_id"`
	BestName string `json:"best_name,omitempty"`

	Score  float64 `json:"score"`
	StyleZ float64 `json:"style_z"`
	LexZ   float64 `json:"lex_z"`
	HasLex bool    `json:"has_lex"`

	TopID   int64  `json:"top_id"`
	TopName string `json:"top_name,omitempty"`
	Self    bool   `json:"self"` // топ-1 — та же личность (узнан хотя бы по альту)
}

// VoiceBand — ЭТАЛОННАЯ ПОЛОСА: ранги реальных held-out текстов той же личности,
// снятые тем же путём, что и черновик. Без неё «ранг 37» не значит ничего: на
// машинном тексте атрибуция заведомо слаба (см. attribute_batch.go про
// нецентрированную лексику), и читать ранг можно только против того, как этот же
// атрибутор узнаёт настоящие тексты этого же человека.
type VoiceBand struct {
	Identity string  `json:"identity"`
	Kind     string  `json:"kind"`
	N        int     `json:"n"`
	Ranks    []int   `json:"ranks"`
	IDs      []int64 `json:"ids"`

	Min    int `json:"min"`
	P25    int `json:"p25"`
	Median int `json:"median"`
	P75    int `json:"p75"`
	Max    int `json:"max"`
	Of     int `json:"of"`

	Contamination float64 `json:"contamination"` // медианная доля профиля, которую даёт один текст полосы
	ShortTexts    int     `json:"short_texts"`   // текстов ниже порога объёма — их ранг шумит
	Usable        bool    `json:"usable"`
	Why           string  `json:"why,omitempty"`
}

// voiceScorer — загруженный слой профилей плюс всё, что нужно серии замеров.
// Профили грузятся ОДИН раз: через AttributeText они перезагружались бы на каждый
// черновик (десятки секунд на живом архиве против ~30 мс на замер).
type voiceScorer struct {
	sa        *styleAttributor
	la        *lexisAttributor
	idmap     map[int64]string
	cf        *candidateFilter
	lexWeight float64
	kept      int // кандидатов после фильтра (знаменатель ранга)
}

func (s *Store) newVoiceScorer(ctx context.Context, genre string, lexWeight float64, activeDays, minAuthorNotes int) (*voiceScorer, error) {
	sa, la, err := s.loadAttributors(ctx, genre)
	if err != nil {
		return nil, err
	}
	cf, err := s.buildCandidateFilter(ctx, activeDays, minAuthorNotes)
	if err != nil {
		return nil, err
	}
	idmap, err := s.identityMap(ctx)
	if err != nil {
		return nil, err
	}
	v := &voiceScorer{sa: sa, la: la, idmap: idmap, cf: cf, lexWeight: lexWeight, kept: len(sa.ids)}
	if cf.on() {
		v.kept = cf.count(sa.ids)
	}
	return v, nil
}

// score меряет один текст: место лучшей анкеты цели среди кандидатов и кого
// атрибутор назвал первым. Имена проставляются отдельно, одним запросом.
func (v *voiceScorer) score(text string, member map[int64]bool, identity string) VoiceScore {
	sc := VoiceScore{Runes: len([]rune(text)), Of: v.kept}
	tr, ok := rankTextScores(text, v.sa, v.la, v.lexWeight, v.cf)
	if !ok {
		return sc
	}
	sc.Ngrams, sc.Tokens = tr.ngrams, tr.tokens
	sc.TopID = v.sa.ids[tr.topIdx]
	sc.Self = identity != "" && v.idmap[sc.TopID] == identity

	rank, of, bestID, bestIdx := rankAmong(tr.scores, v.sa.ids, member, v.cf)
	if rank == 0 {
		return sc
	}
	sc.Rank, sc.Of, sc.BestID = rank, of, bestID
	sc.Score = tr.scores[bestIdx]
	sc.StyleZ, sc.LexZ, _, sc.HasLex = combineScores(tr.sCos[bestIdx], bestID, tr.lexCos, tr.at)
	return sc
}

// rankAmong — место лучшей анкеты цели среди кандидатов, прошедших фильтр. Сама
// цель считается всегда, даже если фильтр её отсеял бы: иначе «ранг» подозреваемого
// зависел бы от того, попал ли он в правдоподобные, и сравнивать было бы нечего.
func rankAmong(scores []float64, ids []int64, member map[int64]bool, cf *candidateFilter) (rank, of int, bestID int64, bestIdx int) {
	best, bestIdx := -1e18, -1
	for k, id := range ids {
		if member[id] && scores[k] > best {
			best, bestIdx, bestID = scores[k], k, id
		}
	}
	if bestIdx < 0 {
		return 0, 0, 0, -1
	}
	rank, of = 1, 0
	for k, id := range ids {
		if !member[id] && cf != nil && cf.on() && !cf.ok(id) {
			continue
		}
		of++
		if scores[k] > best {
			rank++
		}
	}
	return rank, of, bestID, bestIdx
}

// BuildVoiceBand считает полосу по held-out текстам и честно отказывается, если
// они слишком велики относительно профиля автора (см. BandContaminationMax).
func (s *Store) BuildVoiceBand(ctx context.Context, identity, kind string, held []voiceText,
	v *voiceScorer, member map[int64]bool, profileNgrams map[int64]int) (VoiceBand, error) {
	b := VoiceBand{Identity: identity, Kind: kind, Of: v.kept}
	if len(held) == 0 {
		b.Why = "нет отложенных текстов (-band 0 или корпус слишком мал)"
		return b, nil
	}
	var contam []float64
	for _, t := range held {
		sc := v.score(t.text, member, identity)
		if sc.Rank == 0 {
			continue
		}
		b.Ranks = append(b.Ranks, sc.Rank)
		b.IDs = append(b.IDs, t.id)
		if sc.Ngrams < voiceShortNgrams {
			b.ShortTexts++
		}
		if pn := profileNgrams[t.author]; pn > 0 {
			contam = append(contam, float64(sc.Ngrams)/float64(pn))
		}
	}
	b.N = len(b.Ranks)
	if b.N == 0 {
		b.Why = "ни один отложенный текст не удалось отскорить (нет профиля или тексты слишком коротки)"
		return b, nil
	}
	b.Contamination = round4(medianFloat(contam))
	fillBandQuantiles(&b)
	if b.Contamination > BandContaminationMax {
		b.Why = fmt.Sprintf("контаминация %.1f%%: один текст полосы — такая доля стиль-профиля автора, "+
			"полоса была бы оптимистична настолько, что сравнивать с ней нельзя", b.Contamination*100)
		return b, nil
	}
	b.Usable = true
	return b, nil
}

// voiceShortNgrams — ниже этого объёма запроса ранжирование заметно шумит (тот же
// порог, что печатает personas attribute).
const voiceShortNgrams = 300

func fillBandQuantiles(b *VoiceBand) {
	f := make([]float64, len(b.Ranks))
	for i, r := range b.Ranks {
		f[i] = float64(r)
	}
	sort.Float64s(f)
	b.Min = int(f[0])
	b.P25 = int(quantile(f, 0.25) + 0.5)
	b.Median = int(quantile(f, 0.50) + 0.5)
	b.P75 = int(quantile(f, 0.75) + 0.5)
	b.Max = int(f[len(f)-1])
}

// BandQuantile — доля текстов полосы, которые атрибутор узнаёт ХУЖЕ черновика
// (ранг больше). 0.0 — черновик хуже всех настоящих текстов автора, 1.0 — лучше
// всех. Это и есть читаемая мера «попал ли в манеру».
func BandQuantile(b VoiceBand, rank int) float64 {
	if !b.Usable || b.N == 0 || rank <= 0 {
		return 0
	}
	worse := 0
	for _, r := range b.Ranks {
		if r > rank {
			worse++
		}
	}
	return round4(float64(worse) / float64(b.N))
}

func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// profileNgrams — объём стиль-профиля жанра по анкетам (знаменатель контаминации).
func (s *Store) profileNgrams(ctx context.Context, ids []int64, genre string) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, ngrams FROM style_profiles WHERE genre = ? AND user_id IN (`+intList(ids)+`)`, genre)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// --- страж копирования ---

// wordShingles — множество словных 5-грамм текста. Нужен, чтобы поймать
// черновик, который «победил» атрибутор пересказом образца, а не манерой.
func wordShingles(text string, n int) map[string]bool {
	var words []string
	forEachWord(text, func(w []rune) { words = append(words, string(w)) })
	out := map[string]bool{}
	for i := 0; i+n <= len(words); i++ {
		out[strings.Join(words[i:i+n], " ")] = true
	}
	return out
}

// CopyOverlap — доля 5-грамм черновика, встречающихся в образцах. 0 — ничего
// общего, 1 — пересказ. Короткий черновик (меньше 5 слов) — 0 по построению.
func CopyOverlap(draft string, samples []string) float64 {
	d := wordShingles(draft, 5)
	if len(d) == 0 {
		return 0
	}
	seen := map[string]bool{}
	for _, s := range samples {
		for sh := range wordShingles(s, 5) {
			seen[sh] = true
		}
	}
	hit := 0
	for sh := range d {
		if seen[sh] {
			hit++
		}
	}
	return round4(float64(hit) / float64(len(d)))
}

// textRanking — скоры ВСЕХ профилей слоя для одного текста (сонаправлены sa.ids)
// плюс всё, что нужно отчёту: сырые косинусы, фон и объём запроса.
type textRanking struct {
	sCos   []float64
	lexCos map[int64]float64
	at     *Attribution
	scores []float64
	topIdx int
	ngrams int // 3-грамм в запросе — мера объёма (ниже shortQueryNgrams ранг шумит)
	tokens int
}

// rankTextScores скорит текст против всех профилей уже загруженного слоя.
// cf != nil && cf.on() — фон (среднее/сигма косинуса) считается только по
// правдоподобным кандидатам, как в VerifyText; nil — по всем профилям
// (поведение scoreNote). ok=false — текст короче трёх символов, сравнивать нечего.
func rankTextScores(text string, sa *styleAttributor, la *lexisAttributor,
	lexWeight float64, cf *candidateFilter) (textRanking, bool) {
	sCos, ngrams, ok := sa.cosines(normalizeStyle(text))
	if !ok {
		return textRanking{}, false
	}
	tr := textRanking{sCos: sCos, ngrams: ngrams}
	at := &Attribution{LexWeight: lexWeight}
	if cf != nil && cf.on() {
		at.StyleCosMean, at.StyleCosStd = meanStdFiltered(sa.ids, sCos, cf)
	} else {
		at.StyleCosMean, at.StyleCosStd = meanStd(sCos)
	}

	if la != nil {
		lexCos, lmean, lstd, lok := la.cosines(text)
		if lok {
			if cf != nil && cf.on() {
				lmean, lstd = meanStdMapFiltered(la.ids, lexCos, cf)
			}
			tr.lexCos, at.LexCosMean, at.LexCosStd = lexCos, lmean, lstd
			tr.tokens = len(strings.Fields(text))
		}
	}

	tr.at = at
	tr.scores = make([]float64, len(sa.ids))
	for i := range sa.ids {
		_, _, tr.scores[i], _ = combineScores(sCos[i], sa.ids[i], tr.lexCos, at)
		if tr.scores[i] > tr.scores[tr.topIdx] {
			tr.topIdx = i
		}
	}
	return tr, true
}
