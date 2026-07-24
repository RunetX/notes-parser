package archive

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// init регистрирует ulower — Unicode-корректный lower(). Встроенные LIKE и
// lower() SQLite приводят регистр только ASCII, поэтому кириллические
// самораскрытия («Вторая анкета») мимо шаблонов бы прошли. Функция доступна
// всем соединениям, открытым после регистрации, — init гарантирует это до Open.
func init() {
	_ = sqlite.RegisterDeterministicScalarFunction("ulower", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			s, ok := args[0].(string)
			if !ok {
				return args[0], nil // NULL/не-строка — как есть
			}
			return strings.ToLower(s), nil
		})
}

// migrateV3SQL — слой распознавания личностей (persona resolution) поверх сырых
// users. Обратимый и производный: сами users не трогаем. disclosure_hits —
// комментарии-признаки альт-анкет; alias_candidates — парные связи «возможно
// один человек» с сигналом и весом; personas/user_personas — склеенные личности
// на ревью (pending → confirmed/rejected). Живёт в миграции, чтобы добавиться
// и в уже собранный архив.
const migrateV3SQL = `
CREATE TABLE disclosure_hits (
    comment_id INTEGER PRIMARY KEY REFERENCES comments(id),
    author_id  INTEGER NOT NULL REFERENCES users(id),
    pattern    TEXT NOT NULL,
    resolved   INTEGER NOT NULL DEFAULT 0   -- 1 — уже разобрано (в alias_candidates или тупик)
);
CREATE INDEX idx_disclosure_author   ON disclosure_hits(author_id);
CREATE INDEX idx_disclosure_resolved ON disclosure_hits(resolved);

CREATE TABLE alias_candidates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_a     INTEGER NOT NULL REFERENCES users(id),   -- всегда user_a < user_b
    user_b     INTEGER NOT NULL REFERENCES users(id),
    signal     TEXT NOT NULL,                           -- disclosure|name|avatar_phash|stylometry
    score      REAL NOT NULL,
    evidence   TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE(user_a, user_b, signal)
);

CREATE TABLE personas (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    label      TEXT NOT NULL DEFAULT '',
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE user_personas (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id),
    persona_id INTEGER NOT NULL REFERENCES personas(id),
    confidence REAL NOT NULL DEFAULT 0,
    status     TEXT NOT NULL DEFAULT 'pending'          -- pending|confirmed|rejected
);
CREATE INDEX idx_user_personas_persona ON user_personas(persona_id);
`

// SignalDisclosure — сигнал связи по самораскрытию в тексте (единственный в v1).
const SignalDisclosure = "disclosure"

// SignalConfirmed — must-link ребро из ручного подтверждения: держит группу вместе
// при любом пороге и не подчиняется гарду плотности.
const SignalConfirmed = "confirmed"

// candidateParticipantCap — предел числа участников заметки в одном кандидате,
// чтобы обсуждаемая заметка не раздувала выгрузку (для разрешения ников хватает
// активного ядра; остальных добираем из глобального users_index).
const candidateParticipantCap = 100

// FlagStats — итог прогона FlagDisclosures для отчёта.
type FlagStats struct {
	Inserted   int            // новых пометок за прогон
	Total      int            // всего пометок в таблице после прогона
	PerPattern map[string]int // сколько комментов впервые помечено каждым шаблоном
}

// FlagDisclosures сканирует comments.text по списку LIKE-шаблонов самораскрытия
// и наполняет disclosure_hits (идемпотентно, INSERT OR IGNORE по comment_id).
// Сравнение регистронезависимо через ulower. Порядок шаблонов важен для отчёта:
// пересекающийся коммент засчитывается первому сработавшему шаблону (comment_id —
// PK). Детерминированно, без сети.
func (s *Store) FlagDisclosures(ctx context.Context, patterns []string) (FlagStats, error) {
	st := FlagStats{PerPattern: make(map[string]int, len(patterns))}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck // после Commit — no-op

	for _, p := range patterns {
		res, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO disclosure_hits (comment_id, author_id, pattern)
			SELECT id, author_id, ? FROM comments WHERE ulower(text) LIKE ?`, p, p)
		if err != nil {
			return st, fmt.Errorf("шаблон %q: %w", p, err)
		}
		n, _ := res.RowsAffected()
		st.PerPattern[p] = int(n)
		st.Inserted += int(n)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM disclosure_hits`).Scan(&st.Total); err != nil {
		return st, err
	}
	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// CandidateUser — участник в материале для LLM (автор признания или участник
// заметки, среди которых разрешается упомянутый ник).
type CandidateUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Age       string `json:"age,omitempty"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// CandidateNote — заметка-контекст признания (id + короткая выдержка текста).
type CandidateNote struct {
	ID      int64  `json:"id"`
	Excerpt string `json:"excerpt"`
}

// CandidateHit — один непроработанный disclosure_hit, обогащённый контекстом для
// разрешения связи: кто, что написал, в какой заметке и кто ещё в ней участвовал.
type CandidateHit struct {
	CommentID    int64           `json:"comment_id"`
	Author       CandidateUser   `json:"author"`
	Pattern      string          `json:"pattern"`
	Text         string          `json:"text"`
	Note         CandidateNote   `json:"note"`
	Participants []CandidateUser `json:"participants"`
}

// DisclosureCandidates выгружает до limit непроработанных (resolved=0) пометок с
// контекстом — материал для извлечения связей. limit<=0 — без ограничения.
func (s *Store) DisclosureCandidates(ctx context.Context, limit int) ([]CandidateHit, error) {
	q := `
		SELECT h.comment_id, h.pattern, c.note_id, c.text,
		       u.id, u.name, u.age, n.text
		FROM disclosure_hits h
		JOIN comments c ON c.id = h.comment_id
		JOIN users    u ON u.id = h.author_id
		JOIN notes    n ON n.id = c.note_id
		WHERE h.resolved = 0
		ORDER BY h.comment_id`
	args := []any{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var hits []CandidateHit
	for rows.Next() {
		var h CandidateHit
		var noteText string
		if err := rows.Scan(&h.CommentID, &h.Pattern, &h.Note.ID, &h.Text,
			&h.Author.ID, &h.Author.Name, &h.Author.Age, &noteText); err != nil {
			rows.Close()
			return nil, err
		}
		h.Note.Excerpt = excerpt(noteText, 240)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Участники по каждой заметке — отдельными запросами (пометок немного).
	for i := range hits {
		parts, err := s.noteParticipants(ctx, hits[i].Note.ID)
		if err != nil {
			return nil, err
		}
		hits[i].Participants = parts
	}
	return hits, nil
}

// noteParticipants — авторы комментариев заметки плюс её автор (до cap), самые
// активные в этой заметке впереди (чтобы отсечка не срезала ядро обсуждения).
// Счётчики берём одним групповым запросом по заметке (idx_comments_note),
// без корреляции по всей таблице комментариев — иначе на 10-млн corpus'е это
// разворачивается в тысячи полных сканов.
func (s *Store) noteParticipants(ctx context.Context, noteID int64) ([]CandidateUser, error) {
	type authorCount struct {
		id int64
		n  int
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT author_id, COUNT(*) FROM comments WHERE note_id = ? GROUP BY author_id`, noteID)
	if err != nil {
		return nil, err
	}
	var list []authorCount
	seen := map[int64]bool{}
	for rows.Next() {
		var a authorCount
		if err := rows.Scan(&a.id, &a.n); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, a)
		seen[a.id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Автор самой заметки — участник, даже если не комментировал.
	var noteAuthor sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT author_id FROM notes WHERE id = ?`, noteID).Scan(&noteAuthor); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if noteAuthor.Valid && !seen[noteAuthor.Int64] {
		list = append(list, authorCount{id: noteAuthor.Int64})
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].id < list[j].id
	})
	if len(list) > candidateParticipantCap {
		list = list[:candidateParticipantCap]
	}
	ids := make([]int64, len(list))
	for i, a := range list {
		ids[i] = a.id
	}
	details, err := s.usersByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]CandidateUser, 0, len(ids))
	for _, id := range ids { // сохраняем порядок «активные впереди»
		if u, ok := details[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

// usersByIDs подтягивает реквизиты пользователей одним запросом (IN …).
func (s *Store) usersByIDs(ctx context.Context, ids []int64) (map[int64]CandidateUser, error) {
	m := make(map[int64]CandidateUser, len(ids))
	if len(ids) == 0 {
		return m, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, age, first_seen, last_seen FROM users WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u CandidateUser
		if err := rows.Scan(&u.ID, &u.Name, &u.Age, &u.FirstSeen, &u.LastSeen); err != nil {
			return nil, err
		}
		m[u.ID] = u
	}
	return m, rows.Err()
}

// UserIndexEntry — строка глобального индекса пользователей с активностью
// (для разрешения ников вне co-участников заметки).
type UserIndexEntry struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Age       string `json:"age,omitempty"`
	Comments  int    `json:"comments"`
	Notes     int    `json:"notes"`
	FirstSeen string `json:"first_seen,omitempty"`
	LastSeen  string `json:"last_seen,omitempty"`
}

// UsersIndex возвращает всех типажей с их активностью (по v_user_activity),
// самые активные впереди.
func (s *Store) UsersIndex(ctx context.Context) ([]UserIndexEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, age, comments, notes, first_seen, last_seen
		FROM v_user_activity ORDER BY comments DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserIndexEntry
	for rows.Next() {
		var e UserIndexEntry
		if err := rows.Scan(&e.ID, &e.Name, &e.Age, &e.Comments, &e.Notes,
			&e.FirstSeen, &e.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AliasLink — извлечённая связь «возможно один человек». CommentID (если задан) —
// пометка-источник, которая помечается resolved при импорте.
type AliasLink struct {
	UserA     int64   `json:"user_a"`
	UserB     int64   `json:"user_b"`
	Score     float64 `json:"score"`
	Evidence  string  `json:"evidence"`
	CommentID int64   `json:"comment_id,omitempty"`
}

// ImportStats — итог ImportAliasLinks.
type ImportStats struct {
	Links        int // импортировано связей (insert или update)
	Skipped      int // пропущено (a==b, несуществующий id)
	HitsResolved int // помечено disclosure_hits.resolved=1
}

// ImportAliasLinks заносит извлечённые связи в alias_candidates (signal=
// disclosure) и помечает разобранные пометки resolved. Порядок user_a/user_b
// нормализуется (a<b), само-связи и ссылки на несуществующих пользователей
// пропускаются. resolved — id комментов-пометок, признанных разобранными (в т.ч.
// тупики без связи); id из link.CommentID добавляются к ним автоматически.
func (s *Store) ImportAliasLinks(ctx context.Context, links []AliasLink, resolved []int64, now time.Time) (ImportStats, error) {
	var st ImportStats
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer tx.Rollback() //nolint:errcheck

	nowStr := fmtTime(now)
	toResolve := map[int64]bool{}
	for _, id := range resolved {
		if id > 0 {
			toResolve[id] = true
		}
	}
	for _, l := range links {
		a, b := l.UserA, l.UserB
		if a > b {
			a, b = b, a
		}
		if a <= 0 || b <= 0 || a == b {
			st.Skipped++
			continue
		}
		ok, err := usersExist(ctx, tx, a, b)
		if err != nil {
			return st, err
		}
		if !ok {
			st.Skipped++
			continue
		}
		if err := upsertAliasCandidate(ctx, tx,
			aliasCand{a: a, b: b, signal: SignalDisclosure, score: l.Score, evidence: l.Evidence}, nowStr); err != nil {
			return st, fmt.Errorf("связь %d↔%d: %w", a, b, err)
		}
		st.Links++
		if l.CommentID > 0 {
			toResolve[l.CommentID] = true
		}
	}

	for id := range toResolve {
		res, err := tx.ExecContext(ctx, `UPDATE disclosure_hits SET resolved = 1 WHERE comment_id = ?`, id)
		if err != nil {
			return st, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			st.HitsResolved++
		}
	}

	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// aliasCand — парная связь-кандидат для upsert (a<b нормализуется вызывающим).
type aliasCand struct {
	a, b     int64
	signal   string // disclosure|avatar_phash|…
	score    float64
	evidence string
}

// UserName — имя пользователя по id (для отчётов). false — пользователя нет.
func (s *Store) UserName(ctx context.Context, id int64) (string, bool) {
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, id).Scan(&name); err != nil {
		return "", false
	}
	return name, true
}

// upsertAliasCandidate заносит/обновляет парную связь с заданным сигналом.
// Общий для всех источников; UNIQUE(user_a,user_b,signal) → повтор = update.
func upsertAliasCandidate(ctx context.Context, tx *sql.Tx, c aliasCand, nowStr string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO alias_candidates (user_a, user_b, signal, score, evidence, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_a, user_b, signal) DO UPDATE SET
			score = excluded.score, evidence = excluded.evidence`,
		c.a, c.b, c.signal, c.score, c.evidence, nowStr)
	return err
}

// usersExist — оба id есть в users.
func usersExist(ctx context.Context, tx *sql.Tx, a, b int64) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE id IN (?, ?)`, a, b).Scan(&n); err != nil {
		return false, err
	}
	return n == 2, nil
}

// PersonaMember — участник кластера с числом его комментариев (для отчёта).
type PersonaMember struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Comments int    `json:"comments"`
}

// PersonaCluster — склеенная личность на ревью: состав, минимальная уверенность
// (слабейшее ребро) и цитаты-обоснования.
type PersonaCluster struct {
	PersonaID  int64           `json:"persona_id"`
	Label      string          `json:"label"`
	Confidence float64         `json:"confidence"`
	Members    []PersonaMember `json:"members"`
	Evidence   []string        `json:"evidence,omitempty"`
}

// ClusterParams — настройки склейки: порог веса ребра и защита от переклейки.
type ClusterParams struct {
	MinScore   float64
	MaxSize    int     // компонент больше стольких анкет — переклейка через хаб, не материализуем (0 — без лимита)
	MinDensity float64 // для компонент >4 анкет: доля рёбер от полного графа ниже — цепочка, отклоняем (0 — без проверки)
}

// DroppedComponent — компонент, отклонённый гардом (для отчёта).
type DroppedComponent struct {
	Size    int
	Edges   int
	Density float64
	Sample  []int64 // несколько id для лога
}

// ClusterPersonas строит личности как связные компоненты графа alias_candidates
// с score>=MinScore (объединение рёбер: A–B, B–C ⇒ {A,B,C}). Идемпотентно —
// пересчёт с нуля (прежние personas/user_personas удаляются, поэтому ручные
// статусы set сбрасываются: прогонять cluster до финального ревью). Гард против
// транзитивной переклейки: компонент крупнее MaxSize или рыхлее MinDensity
// (цепочка через хаб, а не почти-клика) не материализуется — его анкеты остаются
// сами по себе; компоненты с must-link ребром (ручное подтверждение) от гарда
// освобождены. confidence = минимальный вес ребра, label = ник самого активного.
// Возвращает отчёт-ревью и список отклонённых компонент.
func (s *Store) ClusterPersonas(ctx context.Context, p ClusterParams, now time.Time) ([]PersonaCluster, []DroppedComponent, error) {
	edges, err := s.loadAliasEdges(ctx, p.MinScore)
	if err != nil {
		return nil, nil, err
	}
	activity, err := s.commentCounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	kept, dropped := aggregateComponents(edges, p.MaxSize, p.MinDensity)
	clusters, err := s.materializePersonas(ctx, kept, activity, now)
	return clusters, dropped, err
}

// personaComponent — связная компонента графа alias_candidates: состав,
// слабейшее ребро (→ confidence), цитаты, число уникальных рёбер и метка
// «есть ручное подтверждение».
type personaComponent struct {
	root         int64
	members      []int64
	minScore     float64
	evidence     []string
	edges        int
	hasConfirmed bool
}

// aggregateComponents объединяет рёбра (union-find) в компоненты, по каждой
// собирает состав/вес/цитаты/плотность и делит на принятые и отклонённые гардом.
func aggregateComponents(edges []aliasEdge, maxSize int, minDensity float64) ([]personaComponent, []DroppedComponent) {
	uf := newUnionFind()
	for _, e := range edges {
		uf.union(e.a, e.b)
	}
	comps := collectComponents(uf, edges)
	return splitByGuard(comps, maxSize, minDensity)
}

// collectComponents накапливает по каждому корню состав, уникальные рёбра,
// слабейший вес, цитаты и признак ручного подтверждения.
func collectComponents(uf *unionFind, edges []aliasEdge) []*personaComponent {
	byRoot := map[int64]*personaComponent{}
	seenMember := map[string]bool{}
	seenPair := map[[2]int64]bool{}
	order := []int64{}
	for _, e := range edges {
		r := uf.find(e.a)
		c := byRoot[r]
		if c == nil {
			c = &personaComponent{root: r, minScore: e.score}
			byRoot[r] = c
			order = append(order, r)
		}
		c.absorb(e, seenMember, seenPair)
	}
	out := make([]*personaComponent, 0, len(order))
	for _, r := range order {
		out = append(out, byRoot[r])
	}
	return out
}

// absorb добавляет ребро в компоненту: новых участников, уникальную пару (число
// рёбер для плотности), слабейший вес, цитату и признак ручного подтверждения.
func (c *personaComponent) absorb(e aliasEdge, seenMember map[string]bool, seenPair map[[2]int64]bool) {
	for _, id := range [2]int64{e.a, e.b} {
		if k := key2(c.root, id); !seenMember[k] {
			seenMember[k] = true
			c.members = append(c.members, id)
		}
	}
	if pk := orderedPair(e.a, e.b); !seenPair[pk] { // одна пара — несколько сигналов: считаем раз
		seenPair[pk] = true
		c.edges++
	}
	c.hasConfirmed = c.hasConfirmed || e.confirmed
	if e.score < c.minScore {
		c.minScore = e.score
	}
	if e.evidence != "" {
		c.evidence = append(c.evidence, e.evidence)
	}
}

// splitByGuard делит компоненты на принятые и отклонённые (переклейка через хаб).
func splitByGuard(comps []*personaComponent, maxSize int, minDensity float64) ([]personaComponent, []DroppedComponent) {
	var kept []personaComponent
	var dropped []DroppedComponent
	for _, c := range comps {
		if len(c.members) < 2 {
			continue
		}
		if !c.hasConfirmed && tooLoose(len(c.members), c.edges, maxSize, minDensity) {
			dropped = append(dropped, DroppedComponent{
				Size: len(c.members), Edges: c.edges,
				Density: density(len(c.members), c.edges), Sample: sampleIDs(c.members),
			})
			continue
		}
		kept = append(kept, *c)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].root < kept[j].root })
	sort.Slice(dropped, func(i, j int) bool { return dropped[i].Size > dropped[j].Size })
	return kept, dropped
}

func orderedPair(a, b int64) [2]int64 {
	if a > b {
		return [2]int64{b, a}
	}
	return [2]int64{a, b}
}

// density — доля реальных рёбер от полного графа компоненты (клика = 1).
func density(n, e int) float64 {
	if n < 2 {
		return 1
	}
	return float64(e) / (float64(n) * float64(n-1) / 2)
}

// tooLoose — компонент похож на переклейку: слишком большой или (при >4 анкетах)
// слишком рыхлый (цепочка через хаб, а не почти-клика родственных анкет).
func tooLoose(n, e, maxSize int, minDensity float64) bool {
	if maxSize > 0 && n > maxSize {
		return true
	}
	return minDensity > 0 && n > 4 && density(n, e) < minDensity
}

func sampleIDs(ids []int64) []int64 {
	if len(ids) > 6 {
		return append([]int64(nil), ids[:6]...)
	}
	return append([]int64(nil), ids...)
}

// materializePersonas перезаписывает personas/user_personas по компонентам
// (идемпотентно — прежние удаляются) и возвращает отчёт-ревью.
func (s *Store) materializePersonas(ctx context.Context, comps []personaComponent, activity map[int64]userActivity, now time.Time) ([]PersonaCluster, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_personas`); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM personas`); err != nil {
		return nil, err
	}

	nowStr := fmtTime(now)
	out := make([]PersonaCluster, 0, len(comps))
	for _, c := range comps {
		cluster, err := insertPersona(ctx, tx, c, activity, nowStr)
		if err != nil {
			return nil, err
		}
		out = append(out, cluster)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// insertPersona вставляет одну личность и её участников, возвращает строку отчёта.
// label — ник самого активного участника; confidence — слабейшее ребро кластера.
func insertPersona(ctx context.Context, tx *sql.Tx, c personaComponent, activity map[int64]userActivity, nowStr string) (PersonaCluster, error) {
	mem := make([]PersonaMember, 0, len(c.members))
	for _, id := range c.members {
		mem = append(mem, PersonaMember{ID: id, Name: activity[id].name, Comments: activity[id].comments})
	}
	sort.Slice(mem, func(i, j int) bool {
		if mem[i].Comments != mem[j].Comments {
			return mem[i].Comments > mem[j].Comments
		}
		return mem[i].ID < mem[j].ID
	})
	label := mem[0].Name
	if label == "" {
		label = fmt.Sprintf("анкета %d", mem[0].ID)
	}
	pres, err := tx.ExecContext(ctx,
		`INSERT INTO personas (label, note, created_at) VALUES (?, '', ?)`, label, nowStr)
	if err != nil {
		return PersonaCluster{}, err
	}
	pid, err := pres.LastInsertId()
	if err != nil {
		return PersonaCluster{}, err
	}
	for _, m := range mem {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_personas (user_id, persona_id, confidence, status)
			VALUES (?, ?, ?, 'pending')`, m.ID, pid, c.minScore); err != nil {
			return PersonaCluster{}, fmt.Errorf("user_personas %d: %w", m.ID, err)
		}
	}
	return PersonaCluster{
		PersonaID: pid, Label: label, Confidence: c.minScore, Members: mem, Evidence: c.evidence,
	}, nil
}

type aliasEdge struct {
	a, b      int64
	score     float64
	evidence  string
	confirmed bool // ребро из ручного подтверждения (signal=confirmed) — гард его не рвёт
}

func (s *Store) loadAliasEdges(ctx context.Context, minScore float64) ([]aliasEdge, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_a, user_b, score, evidence, signal FROM alias_candidates WHERE score >= ?`, minScore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aliasEdge
	for rows.Next() {
		var e aliasEdge
		var signal string
		if err := rows.Scan(&e.a, &e.b, &e.score, &e.evidence, &signal); err != nil {
			return nil, err
		}
		e.confirmed = signal == SignalConfirmed
		out = append(out, e)
	}
	return out, rows.Err()
}

type userActivity struct {
	name     string
	comments int
}

// commentCounts — имя и число комментариев по каждому пользователю (для label и
// сортировки состава кластера).
func (s *Store) commentCounts(ctx context.Context) (map[int64]userActivity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, comments FROM v_user_activity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]userActivity{}
	for rows.Next() {
		var id int64
		var a userActivity
		if err := rows.Scan(&id, &a.name, &a.comments); err != nil {
			return nil, err
		}
		m[id] = a
	}
	return m, rows.Err()
}

// SetPersonaStatus проставляет статус всем участникам личности (после ревью).
// Возвращает число затронутых строк (0 — личности нет).
func (s *Store) SetPersonaStatus(ctx context.Context, personaID int64, status string) (int64, error) {
	switch status {
	case "pending", "confirmed", "rejected":
	default:
		return 0, fmt.Errorf("недопустимый статус %q (pending|confirmed|rejected)", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_personas SET status = ? WHERE persona_id = ?`, status, personaID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- вспомогательное ---

// unionFind — классический DSU с путевым сжатием (по рангу неважно на нашем масштабе).
type unionFind struct{ parent map[int64]int64 }

func newUnionFind() *unionFind { return &unionFind{parent: map[int64]int64{}} }

func (u *unionFind) find(x int64) int64 {
	p, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		return x
	}
	if p != x {
		u.parent[x] = u.find(p)
	}
	return u.parent[x]
}

func (u *unionFind) union(a, b int64) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}

// key2 — стабильный ключ пары (корень, элемент) для дедупликации состава.
func key2(a, b int64) string { return fmt.Sprintf("%d:%d", a, b) }

// excerpt — первые n рун строки (без разрыва UTF-8), с многоточием при обрезке.
func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ") // схлопнуть пробелы/переводы строк
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
