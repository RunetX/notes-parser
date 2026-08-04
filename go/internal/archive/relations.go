package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// migrateV9SQL — отношения между личностями (Фаза 3): тональность переписки и
// LLM-разметка типа отношений (дружба/конфликт/флирт). Слой производный и
// обратимый, ключи identity — из v_identity (порядок прогона тот же, что у
// identity_facts: сначала финализировать кластеры). v_relations — лучший
// источник на пару: manual > llm > tone.
const migrateV9SQL = `
CREATE TABLE relation_edges (
    from_identity TEXT NOT NULL,
    to_identity   TEXT NOT NULL,
    source        TEXT NOT NULL,               -- tone | llm | manual
    kind          TEXT NOT NULL,               -- tone: 'tone'; llm/manual: friendship|conflict|flirt|neutral
    score         REAL NOT NULL,               -- tone: знаковый (pos-neg)/total [-1..1]; llm: confidence [0..1]
    replies       INTEGER NOT NULL DEFAULT 0,  -- реплик в этом направлении
    reciprocity   REAL NOT NULL DEFAULT 0,     -- min(a→b,b→a)/max(...) по паре
    pos           INTEGER NOT NULL DEFAULT 0,  -- реплик с позитивным тоном (tone)
    neg           INTEGER NOT NULL DEFAULT 0,  -- реплик с негативным тоном (tone)
    evidence      TEXT NOT NULL DEFAULT '[]',  -- JSON (tone: id примеров; llm: цитаты)
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (from_identity, to_identity, source)
);
CREATE INDEX idx_relation_edges_to ON relation_edges(to_identity);

CREATE VIEW v_relations AS
SELECT re.*
FROM relation_edges re
JOIN (
    SELECT from_identity, to_identity,
           MAX(CASE source WHEN 'manual' THEN 3 WHEN 'llm' THEN 2 ELSE 1 END) AS pri
    FROM relation_edges GROUP BY from_identity, to_identity
) b ON b.from_identity = re.from_identity AND b.to_identity = re.to_identity
   AND CASE re.source WHEN 'manual' THEN 3 WHEN 'llm' THEN 2 ELSE 1 END = b.pri;
`

// Источники рёбер отношений.
const (
	RelSourceTone   = "tone"
	RelSourceLLM    = "llm"
	RelSourceManual = "manual"
)

// Типы отношений. KindTone — служебный kind тональных строк: сырой тон НЕ
// публикуется как «дружба»/«конфликт» (дружеская пикировка сайта знакомств
// неотличима от ссоры словарём) — тип ставит только LLM/ручная разметка.
const (
	KindTone       = "tone"
	KindFriendship = "friendship"
	KindConflict   = "conflict"
	KindFlirt      = "flirt"
	KindNeutral    = "neutral"
)

// Тональность смайлов сайта (:::код::: в тексте). Точный, не текстовый признак:
// flowers/respect врагам не шлют. Замер: коды есть в ~3.8% комментариев.
var smileyTone = map[string]int{
	"agree": 1, "flowers": 1, "respect": 1, "boogi": 1, "live": 1,
	"sad2": -1, "mad": -1, "mad2": -1, "sorrow": -1, "tease": -1, "pester": -1,
}

var smileyRe = regexp.MustCompile(`:::([a-z0-9_]+):::`)

// Текстовые скобки-смайлы: «)))» — позитив, «(((» — негатив (2+ подряд).
var (
	textSmilePosRe = regexp.MustCompile(`\){2,}`)
	textSmileNegRe = regexp.MustCompile(`\({2,}`)
)

// Словарные маркеры тона реплики. Намеренно консервативные: обсценная лексика
// между завсегдатаями бывает дружеской, поэтому в негатив идут только прямые
// оскорбления. Границы слов — кириллические (см. TopicLexicon.compile).
var (
	tonePosRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:спасибо|благодар\p{L}*|молодец|умница|умничка|красав\p{L}*|солнышк\p{L}*|обнима\p{L}*|целую|браво|классно|супер|милашк\p{L}*|поздравля\p{L}*)(?:[^\p{L}]|$)`)
	toneNegRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:дура|дурак|идиот\p{L}*|козёл|козел|хам|хамло|хамк\p{L}*|мерзк\p{L}*|гадост\p{L}*|гадин\p{L}*|отвали|заткнись|нахал\p{L}*|бред|чушь|тупой|тупая|мразь|тварь|сволоч\p{L}*|скотин\p{L}*|врёшь|врешь|лжёшь|иди лесом)(?:[^\p{L}]|$)`)
)

// replyTone — тон одной реплики: +1/-1/0 по смайлам сайта, скобкам и словарю.
func replyTone(text string) int {
	pos, neg := 0, 0
	for _, m := range smileyRe.FindAllStringSubmatch(text, -1) {
		switch smileyTone[m[1]] {
		case 1:
			pos++
		case -1:
			neg++
		}
	}
	if textSmilePosRe.MatchString(text) {
		pos++
	}
	if textSmileNegRe.MatchString(text) {
		neg++
	}
	if tonePosRe.MatchString(text) {
		pos++
	}
	if toneNegRe.MatchString(text) {
		neg++
	}
	switch {
	case pos > neg:
		return 1
	case neg > pos:
		return -1
	}
	return 0
}

// toneAcc — аккумулятор направленной пары при потоковом проходе по ответам.
type toneAcc struct {
	total  int
	pos    int
	neg    int
	posIDs []int64 // до 2 примеров позитива (id комментариев)
	negIDs []int64 // до 2 примеров негатива
}

// ToneParams — настройки тонального скоринга.
type ToneParams struct {
	MinReplies int // писать только пары с реплик ≥ (в этом направлении)
}

// ToneStats — итог ScoreTone.
type ToneStats struct {
	Replies int // просмотрено ответов
	Pairs   int // направленных пар всего (до порога)
	Written int // записано строк source='tone'
}

// ScoreTone — тональный скоринг рёбер: один потоковый проход по всем ответам
// (reply → родитель), тон каждой реплики по смайлам/словарю, агрегация на
// направленные пары личностей. score = (pos-neg)/total. Само-петли (ответы
// между альтами одной личности) пропускаются. Идемпотентно: строки
// source='tone' пересчитываются с нуля.
func (s *Store) ScoreTone(ctx context.Context, p ToneParams, now time.Time) (ToneStats, error) {
	var st ToneStats
	acc := map[string]*toneAcc{} // ключ from+"\x00"+to
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.text, fi.identity, ti.identity
		FROM comments c `+sqlAddresseeJoin+`
		JOIN v_identity fi ON fi.user_id = c.author_id
		JOIN v_identity ti ON ti.user_id = `+sqlAddressee+`
		WHERE c.parent_id != 0`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var text, from, to string
		if err := rows.Scan(&id, &text, &from, &to); err != nil {
			return st, err
		}
		st.Replies++
		if from == to {
			continue
		}
		key := from + "\x00" + to
		a := acc[key]
		if a == nil {
			a = &toneAcc{}
			acc[key] = a
		}
		a.add(id, replyTone(text))
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	st.Pairs = len(acc)
	return st, s.writeToneRows(ctx, acc, p.MinReplies, &st, now)
}

// add учитывает одну реплику с её тоном, придерживая примеры для evidence.
func (a *toneAcc) add(id int64, tone int) {
	a.total++
	switch tone {
	case 1:
		a.pos++
		if len(a.posIDs) < 2 {
			a.posIDs = append(a.posIDs, id)
		}
	case -1:
		a.neg++
		if len(a.negIDs) < 2 {
			a.negIDs = append(a.negIDs, id)
		}
	}
}

// toneEvidence — evidence тональной строки: id комментариев-примеров.
type toneEvidence struct {
	PosIDs []int64 `json:"pos_ids,omitempty"`
	NegIDs []int64 `json:"neg_ids,omitempty"`
}

// writeToneRows перезаписывает строки source='tone' по аккумулятору.
func (s *Store) writeToneRows(ctx context.Context, acc map[string]*toneAcc, minReplies int, st *ToneStats, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM relation_edges WHERE source = ?`, RelSourceTone); err != nil {
		return err
	}
	nowStr := fmtTime(now)
	for key, a := range acc {
		if a.total < minReplies {
			continue
		}
		from, to, _ := strings.Cut(key, "\x00")
		recip := reciprocity(a.total, backTotal(acc, to, from))
		evJSON, err := json.Marshal(toneEvidence{PosIDs: a.posIDs, NegIDs: a.negIDs})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO relation_edges (from_identity, to_identity, source, kind, score,
			                            replies, reciprocity, pos, neg, evidence, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			from, to, RelSourceTone, KindTone,
			float64(a.pos-a.neg)/float64(a.total),
			a.total, recip, a.pos, a.neg, string(evJSON), nowStr); err != nil {
			return fmt.Errorf("тон %s→%s: %w", from, to, err)
		}
		st.Written++
	}
	return tx.Commit()
}

// backTotal — реплики в обратном направлении (0, если пары нет).
func backTotal(acc map[string]*toneAcc, from, to string) int {
	if a := acc[from+"\x00"+to]; a != nil {
		return a.total
	}
	return 0
}

// reciprocity — взаимность пары: min/max реплик по направлениям.
func reciprocity(ab, ba int) float64 {
	lo, hi := ab, ba
	if lo > hi {
		lo, hi = hi, lo
	}
	if hi == 0 {
		return 0
	}
	return float64(lo) / float64(hi)
}

// --- кандидаты для LLM-разметки пар ---

// RelationExchange — один обмен реплик пары (родитель → ответ) для контекста LLM.
type RelationExchange struct {
	NoteID int64  `json:"note_id"`
	At     string `json:"at,omitempty"` // дата ответа (YYYY-MM-DD)
	From   string `json:"from"`         // identity автора ответа
	Parent string `json:"parent"`       // выдержка родительской реплики
	Reply  string `json:"reply"`        // выдержка ответа
}

// RelationCandidate — пара на LLM-разметку (строка JSONL): статистика обоих
// направлений + сэмпл обменов, стратифицированный по времени.
type RelationCandidate struct {
	A         string             `json:"a"`
	B         string             `json:"b"`
	LabelA    string             `json:"label_a"`
	LabelB    string             `json:"label_b"`
	AccountsA []int64            `json:"accounts_a"` // для ремапа импорта после переклейки
	AccountsB []int64            `json:"accounts_b"`
	RepliesAB int                `json:"replies_ab"`
	RepliesBA int                `json:"replies_ba"`
	ToneAB    float64            `json:"tone_ab"`
	ToneBA    float64            `json:"tone_ba"`
	Exchanges []RelationExchange `json:"exchanges"`
}

// RelationCandidateParams — отбор пар: ядро (сумма реплик ≥ MinReplies) плюс
// самые поляризованные по тону из полосы [BandMin, MinReplies).
type RelationCandidateParams struct {
	MinReplies int // ядро: сумма реплик пары в обе стороны
	BandMin    int // нижняя граница полосы
	BandTop    int // сколько поляризованных пар взять из полосы (0 — не брать)
	Exchanges  int // обменов на пару (делится между направлениями)
}

// pairStat — незанаправленная пара по тональным строкам.
type pairStat struct {
	a, b               string
	repliesAB, repliesBA int
	toneAB, toneBA     float64
}

// RelationCandidates отбирает пары и сэмплирует их обмены. Требует прогнанного
// ScoreTone (кандидаты строятся по строкам source='tone'; их низкий порог —
// «-rel-min-replies» — должен быть ≤ BandMin, иначе полоса окажется пустой).
func (s *Store) RelationCandidates(ctx context.Context, p RelationCandidateParams) ([]RelationCandidate, error) {
	pairs, err := s.tonePairs(ctx)
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("нет тональных строк — сначала `personas relations score`")
	}
	selected := selectPairs(pairs, p)

	labels, err := s.identityLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RelationCandidate, 0, len(selected))
	for _, ps := range selected {
		c := RelationCandidate{
			A: ps.a, B: ps.b, LabelA: labels[ps.a], LabelB: labels[ps.b],
			RepliesAB: ps.repliesAB, RepliesBA: ps.repliesBA,
			ToneAB: ps.toneAB, ToneBA: ps.toneBA,
		}
		if c.AccountsA, err = s.identityAccountIDs(ctx, ps.a); err != nil {
			return nil, err
		}
		if c.AccountsB, err = s.identityAccountIDs(ctx, ps.b); err != nil {
			return nil, err
		}
		half := p.Exchanges / 2
		exAB, err := s.sampleExchanges(ctx, c.AccountsA, c.AccountsB, ps.a, half)
		if err != nil {
			return nil, err
		}
		exBA, err := s.sampleExchanges(ctx, c.AccountsB, c.AccountsA, ps.b, p.Exchanges-half)
		if err != nil {
			return nil, err
		}
		c.Exchanges = mergeByTime(exAB, exBA)
		out = append(out, c)
	}
	return out, nil
}

// tonePairs собирает незанаправленные пары из тональных строк.
func (s *Store) tonePairs(ctx context.Context) ([]pairStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT from_identity, to_identity, replies, score
		FROM relation_edges WHERE source = ?`, RelSourceTone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byPair := map[[2]string]*pairStat{}
	for rows.Next() {
		var from, to string
		var replies int
		var score float64
		if err := rows.Scan(&from, &to, &replies, &score); err != nil {
			return nil, err
		}
		key := [2]string{from, to}
		flip := from > to
		if flip {
			key = [2]string{to, from}
		}
		ps := byPair[key]
		if ps == nil {
			ps = &pairStat{a: key[0], b: key[1]}
			byPair[key] = ps
		}
		if flip {
			ps.repliesBA, ps.toneBA = replies, score
		} else {
			ps.repliesAB, ps.toneAB = replies, score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]pairStat, 0, len(byPair))
	for _, ps := range byPair {
		out = append(out, *ps)
	}
	sort.Slice(out, func(i, j int) bool { // стабильный порядок выгрузки
		if ti, tj := out[i].total(), out[j].total(); ti != tj {
			return ti > tj
		}
		return out[i].a < out[j].a
	})
	return out, nil
}

func (p pairStat) total() int { return p.repliesAB + p.repliesBA }

// meanTone — средний тон пары, взвешенный по репликам направлений.
func (p pairStat) meanTone() float64 {
	if p.total() == 0 {
		return 0
	}
	return (p.toneAB*float64(p.repliesAB) + p.toneBA*float64(p.repliesBA)) / float64(p.total())
}

// selectPairs — ядро (сумма ≥ MinReplies) + топ-BandTop по |тону| из полосы.
func selectPairs(pairs []pairStat, p RelationCandidateParams) []pairStat {
	var core, band []pairStat
	for _, ps := range pairs {
		switch {
		case ps.total() >= p.MinReplies:
			core = append(core, ps)
		case ps.total() >= p.BandMin:
			band = append(band, ps)
		}
	}
	if p.BandTop > 0 && len(band) > 0 {
		sort.Slice(band, func(i, j int) bool {
			ti, tj := band[i].meanTone(), band[j].meanTone()
			if ti < 0 {
				ti = -ti
			}
			if tj < 0 {
				tj = -tj
			}
			if ti != tj {
				return ti > tj
			}
			return band[i].total() > band[j].total()
		})
		if len(band) > p.BandTop {
			band = band[:p.BandTop]
		}
		core = append(core, band...)
	}
	return core
}

// identityLabels — карта identity→label по v_persona_activity.
func (s *Store) identityLabels(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT identity, label FROM v_persona_activity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var ident, label string
		if err := rows.Scan(&ident, &label); err != nil {
			return nil, err
		}
		m[ident] = label
	}
	return m, rows.Err()
}

// sampleExchanges — до limit обменов «from отвечает to», равномерно по времени
// (начало/середина/конец знакомства — отношения дрейфуют).
func (s *Store) sampleExchanges(ctx context.Context, fromAcc, toAcc []int64, fromIdent string, limit int) ([]RelationExchange, error) {
	if limit <= 0 || len(fromAcc) == 0 || len(toAcc) == 0 {
		return nil, nil
	}
	// pc.text остаётся текстом корня ветки — это контекст разговора; адресность
	// же определяется слоем адресатов, иначе в выборку попадают реплики соседям.
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.note_id, COALESCE(c.published_at, ''), c.text, pc.text
		FROM comments c `+sqlAddresseeJoin+`
		WHERE c.author_id IN (`+intList(fromAcc)+`) AND `+sqlAddressee+` IN (`+intList(toAcc)+`)
		ORDER BY c.published_at, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []RelationExchange
	for rows.Next() {
		var ex RelationExchange
		var at, reply, parent string
		if err := rows.Scan(&ex.NoteID, &at, &reply, &parent); err != nil {
			return nil, err
		}
		ex.At = shortDateStr(at)
		ex.From = fromIdent
		ex.Reply = excerpt(reply, 200)
		ex.Parent = excerpt(parent, 160)
		all = append(all, ex)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return stratify(all, limit), nil
}

// stratify — limit элементов равномерно по всему срезу (первый и последний включаются).
func stratify(all []RelationExchange, limit int) []RelationExchange {
	if len(all) <= limit {
		return all
	}
	out := make([]RelationExchange, 0, limit)
	step := float64(len(all)-1) / float64(limit-1)
	prev := -1
	for i := 0; i < limit; i++ {
		idx := int(float64(i)*step + 0.5)
		if idx == prev { // защита от повтора на округлении
			continue
		}
		prev = idx
		out = append(out, all[idx])
	}
	return out
}

// mergeByTime сливает обмены двух направлений в хронологию.
func mergeByTime(a, b []RelationExchange) []RelationExchange {
	out := append(append([]RelationExchange{}, a...), b...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// shortDateStr — YYYY-MM-DD из RFC3339 (или как есть, если короче).
func shortDateStr(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// --- импорт LLM-разметки ---

// RelationImport — размеченная LLM пара. Kind neutral тоже пишется (пара
// разобрана, отношения ровные). Ремап identity — по спискам анкет.
type RelationImport struct {
	A          string   `json:"a"`
	B          string   `json:"b"`
	AccountsA  []int64  `json:"accounts_a,omitempty"`
	AccountsB  []int64  `json:"accounts_b,omitempty"`
	Kind       string   `json:"kind"`
	Confidence float64  `json:"confidence"`
	Evidence   []string `json:"evidence,omitempty"` // цитаты-обоснования
}

// RelationImportStats — итог ImportRelations.
type RelationImportStats struct {
	Written  int // записано пар (по две направленные строки)
	Skipped  int // пропущено (личность не найдена, кривой kind)
	Remapped int // личность найдена через анкеты
}

// ImportRelations заносит LLM-разметку как source='llm': по две направленные
// строки на пару (kind/confidence одинаковы, replies/reciprocity наследуются
// от тональных строк, если есть). Upsert по PK — повторный импорт обновляет.
func (s *Store) ImportRelations(ctx context.Context, items []RelationImport, now time.Time) (RelationImportStats, error) {
	var st RelationImportStats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck
	nowStr := fmtTime(now)
	for _, it := range items {
		if err := importOneRelation(ctx, tx, it, nowStr, &st); err != nil {
			return st, err
		}
	}
	return st, tx.Commit()
}

// importOneRelation разбирает одну пару разметки.
func importOneRelation(ctx context.Context, tx *sql.Tx, it RelationImport, nowStr string, st *RelationImportStats) error {
	if !validKind(it.Kind) {
		st.Skipped++
		return nil
	}
	a, remapA, err := resolveIdentity(ctx, tx, it.A, it.AccountsA)
	if err != nil {
		return err
	}
	b, remapB, err := resolveIdentity(ctx, tx, it.B, it.AccountsB)
	if err != nil {
		return err
	}
	if a == "" || b == "" || a == b {
		st.Skipped++
		return nil
	}
	if remapA || remapB {
		st.Remapped++
	}
	evJSON, err := json.Marshal(nonNilStrings(it.Evidence))
	if err != nil {
		return err
	}
	for _, dir := range [2][2]string{{a, b}, {b, a}} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO relation_edges (from_identity, to_identity, source, kind, score,
			                            replies, reciprocity, pos, neg, evidence, updated_at)
			VALUES (?, ?, ?, ?, ?,
			        COALESCE((SELECT replies     FROM relation_edges WHERE from_identity=? AND to_identity=? AND source=?), 0),
			        COALESCE((SELECT reciprocity FROM relation_edges WHERE from_identity=? AND to_identity=? AND source=?), 0),
			        0, 0, ?, ?)
			ON CONFLICT(from_identity, to_identity, source) DO UPDATE SET
				kind = excluded.kind, score = excluded.score,
				evidence = excluded.evidence, updated_at = excluded.updated_at`,
			dir[0], dir[1], RelSourceLLM, it.Kind, it.Confidence,
			dir[0], dir[1], RelSourceTone,
			dir[0], dir[1], RelSourceTone,
			string(evJSON), nowStr); err != nil {
			return fmt.Errorf("отношение %s→%s: %w", dir[0], dir[1], err)
		}
	}
	st.Written++
	return nil
}

func validKind(k string) bool {
	switch k {
	case KindFriendship, KindConflict, KindFlirt, KindNeutral:
		return true
	}
	return false
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// --- чтение отношений (portrait/report/graph) ---

// RelationRow — отношение для отчётов: лучший источник (v_relations) + тон.
type RelationRow struct {
	From        string  `json:"from"`
	To          string  `json:"to"`
	Label       string  `json:"label"` // подпись собеседника
	Source      string  `json:"source"`
	Kind        string  `json:"kind"`
	Score       float64 `json:"score"`
	Replies     int     `json:"replies"`
	Reciprocity float64 `json:"reciprocity"`
	Tone        float64 `json:"tone"` // знаковый тон направления (из tone-строки)
	Pos         int     `json:"pos"`
	Neg         int     `json:"neg"`
}

// IdentityRelations — отношения личности, лучший источник на пару, по убыванию
// объёма переписки. top ≤ 0 — все.
func (s *Store) IdentityRelations(ctx context.Context, identity string, top int) ([]RelationRow, error) {
	// Ярлык собеседника берём из лёгкой v_identity (users+personas), а НЕ из
	// v_persona_activity: та агрегирует все ~10.7 млн комментариев (материализация
	// холодно — секунды), хотя здесь нужен лишь label. Группировка v_identity по
	// identity даёт тот же MAX(label) без прохода по comments.
	q := `
		SELECT r.from_identity, r.to_identity, COALESCE(a.label, ''), r.source, r.kind, r.score,
		       r.replies, r.reciprocity, COALESCE(t.score, 0), COALESCE(t.pos, 0), COALESCE(t.neg, 0)
		FROM v_relations r
		LEFT JOIN relation_edges t ON t.from_identity = r.from_identity
		     AND t.to_identity = r.to_identity AND t.source = 'tone'
		LEFT JOIN (SELECT identity, MAX(label) AS label FROM v_identity GROUP BY identity) a
		     ON a.identity = r.to_identity
		WHERE r.from_identity = ?
		ORDER BY r.replies DESC, r.to_identity`
	args := []any{identity}
	if top > 0 {
		q += ` LIMIT ?`
		args = append(args, top)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelationRow
	for rows.Next() {
		var r RelationRow
		if err := rows.Scan(&r.From, &r.To, &r.Label, &r.Source, &r.Kind, &r.Score,
			&r.Replies, &r.Reciprocity, &r.Tone, &r.Pos, &r.Neg); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RelationMarks — карта направленных пар → (kind из llm/manual, тон из tone)
// для окраски графа. Ключ — from+"\x00"+to.
type RelationMark struct {
	Kind string  // '' — типа нет (только тон)
	Tone float64 // 0 — тона нет
}

// AllRelationMarks — метки всех пар для экспорта графа.
func (s *Store) AllRelationMarks(ctx context.Context) (map[string]RelationMark, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT from_identity, to_identity, source, kind, score FROM relation_edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]RelationMark{}
	for rows.Next() {
		var from, to, source, kind string
		var score float64
		if err := rows.Scan(&from, &to, &source, &kind, &score); err != nil {
			return nil, err
		}
		key := from + "\x00" + to
		mark := m[key]
		if source == RelSourceTone {
			mark.Tone = score
		} else if mark.Kind == "" || source == RelSourceManual {
			mark.Kind = kind
		}
		m[key] = mark
	}
	return m, rows.Err()
}
