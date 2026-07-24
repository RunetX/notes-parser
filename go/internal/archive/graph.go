package archive

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// migrateV6SQL — persona-aware представления: соцграф по ЛЮДЯМ, а не анкетам.
// v_identity сводит каждую анкету к «личности» (persona, если пользователь в
// не-rejected кластере, иначе сам по себе). Поверх — активность и рёбра ответов
// в терминах личностей. Отклонённые (rejected) кластеры игнорируются (анкета
// остаётся собой). Идентификатор узла — текст 'p<persona_id>' | 'u<user_id>'.
const migrateV6SQL = `
CREATE VIEW v_identity AS
SELECT u.id AS user_id,
       CASE WHEN up.persona_id IS NOT NULL AND up.status != 'rejected'
            THEN 'p' || up.persona_id ELSE 'u' || u.id END AS identity,
       CASE WHEN up.persona_id IS NOT NULL AND up.status != 'rejected'
            THEN p.label ELSE u.name END AS label,
       CASE WHEN up.persona_id IS NOT NULL AND up.status != 'rejected'
            THEN 1 ELSE 0 END AS is_persona
FROM users u
LEFT JOIN user_personas up ON up.user_id = u.id
LEFT JOIN personas p ON p.id = up.persona_id;

-- Активность личности: сумма по её анкетам (accounts — сколько анкет слито).
CREATE VIEW v_persona_activity AS
SELECT i.identity,
       MAX(i.is_persona) AS is_persona,
       MAX(i.label)      AS label,
       COUNT(*)          AS accounts,
       SUM(a.comments)   AS comments,
       SUM(a.notes)      AS notes,
       MIN(a.first_seen) AS first_seen,
       MAX(a.last_seen)  AS last_seen
FROM v_identity i
JOIN v_user_activity a ON a.id = i.user_id
GROUP BY i.identity;

-- Направленные рёбра «кто кому отвечал» в терминах личностей (само-петли —
-- ответы между альтами одной личности — сохранены как реальный сигнал).
CREATE VIEW v_persona_reply_edges AS
SELECT fi.identity AS from_identity, ti.identity AS to_identity, COUNT(*) AS replies
FROM comments c
JOIN comments pc ON pc.id = c.parent_id
JOIN v_identity fi ON fi.user_id = c.author_id
JOIN v_identity ti ON ti.user_id = pc.author_id
WHERE c.parent_id != 0
GROUP BY fi.identity, ti.identity;
`

// migrateV7SQL — реальные даты активности из comments.published_at вместо
// first/last_seen (время выгрузки: весь бэкофилл шёл одним днём, спан был
// вырожден). Пересоздаёт v_user_activity (добавляя active_from/active_to) и
// v_persona_activity (её first_seen/last_seen теперь = реальный спан активности).
// Порядок DROP важен: v_persona_activity зависит от v_user_activity.
const migrateV7SQL = `
DROP VIEW IF EXISTS v_persona_activity;
DROP VIEW IF EXISTS v_user_activity;

CREATE VIEW v_user_activity AS
SELECT u.id, u.name, u.age,
       COUNT(c.id) AS comments,
       COUNT(DISTINCT c.note_id) AS notes,
       u.first_seen, u.last_seen, u.profile_url, u.avatar_url,
       MIN(c.published_at) AS active_from,  -- реальная активность (NULL — 0 комм.)
       MAX(c.published_at) AS active_to
FROM users u
LEFT JOIN comments c ON c.author_id = u.id
GROUP BY u.id;

CREATE VIEW v_persona_activity AS
SELECT i.identity,
       MAX(i.is_persona) AS is_persona,
       MAX(i.label)      AS label,
       COUNT(*)          AS accounts,
       SUM(a.comments)   AS comments,
       SUM(a.notes)      AS notes,
       COALESCE(MIN(a.active_from), '') AS first_seen,  -- = реальный спан активности
       COALESCE(MAX(a.active_to), '')   AS last_seen
FROM v_identity i
JOIN v_user_activity a ON a.id = i.user_id
GROUP BY i.identity;
`

// migrateV10SQL — индексы по дате публикации. Без них любой временной срез
// (MaxPublishedAt, ActiveCountsSince, недельная когорта отчёта) — полный скан
// ~10.7 млн комментариев (~2с). Индекс превращает срез в диапазонный поиск.
// archive.db наполняется батч-граббером разово, так что разовая сборка индекса
// при первой миграции и лёгкое замедление вставок несущественны.
const migrateV10SQL = `
CREATE INDEX IF NOT EXISTS idx_comments_published ON comments(published_at);
CREATE INDEX IF NOT EXISTS idx_notes_published    ON notes(published_at);
`

// migrateV11SQL — расширяет idx_comments_author до покрывающего
// (author_id, note_id, published_at). CohortNodes агрегирует по автору
// COUNT(*)/COUNT(DISTINCT note_id)/MIN/MAX(published_at); при узком индексе
// (author_id) note_id и published_at читаются из основной таблицы построчно —
// для «тяжёлых» авторов это сотни тысяч случайных чтений по rowid (на холодном
// кэше — минуты). Покрывающий индекс отдаёт все три поля прямо из среза автора,
// превращая случайные чтения в последовательный скан. author_id остаётся
// префиксом, поэтому все прежние запросы по автору работают как раньше. На
// пустой БД (свежая миграция до наполнения) пересоздание мгновенно.
const migrateV11SQL = `
DROP INDEX IF EXISTS idx_comments_author;
CREATE INDEX idx_comments_author ON comments(author_id, note_id, published_at);
`

// migrateV12SQL — расширяет published-индексы до (published_at, author_id).
// ActiveCountsSince сканирует окно (published_at >= ?) и группирует по личности,
// а личность выводится из author_id; при узком индексе (published_at) author_id
// на каждый комментарий окна читается из основной таблицы по rowid — десятки
// тысяч случайных чтений (холодно — минуты). С author_id прямо в индексе скан
// окна становится покрывающим (обращение к comments/notes уходит), остаются
// лишь join'ы в малые users/personas. published_at остаётся префиксом — срез по
// диапазону и MaxPublishedAt работают как раньше. На пустой БД пересоздание
// мгновенно.
const migrateV12SQL = `
DROP INDEX IF EXISTS idx_comments_published;
CREATE INDEX idx_comments_published ON comments(published_at, author_id);
DROP INDEX IF EXISTS idx_notes_published;
CREATE INDEX idx_notes_published ON notes(published_at, author_id);
`

// GraphNode — узел соцграфа (личность или одиночная анкета).
type GraphNode struct {
	Identity  string
	Label     string
	IsPersona bool
	Accounts  int
	Comments  int
	Notes     int
	FirstSeen string
	LastSeen  string
}

// GraphEdge — ребро «отвечал» с весом.
type GraphEdge struct {
	From    string
	To      string
	Replies int
}

// GraphNodes загружает все узлы (по v_persona_activity) в карту по identity.
func (s *Store) GraphNodes(ctx context.Context) (map[string]GraphNode, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity, is_persona, label, accounts, comments, notes, first_seen, last_seen
		FROM v_persona_activity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]GraphNode{}
	for rows.Next() {
		var n GraphNode
		var isP int
		if err := rows.Scan(&n.Identity, &isP, &n.Label, &n.Accounts, &n.Comments,
			&n.Notes, &n.FirstSeen, &n.LastSeen); err != nil {
			return nil, err
		}
		n.IsPersona = isP == 1
		out[n.Identity] = n
	}
	return out, rows.Err()
}

// CohortNodes собирает узлы (all-time итоги) ТОЛЬКО для перечисленных личностей —
// напрямую по comments через индекс author_id, минуя полный проход
// v_persona_activity (та агрегирует все ~22 тыс. личностей ≈25с, а отчёту нужны
// десятки активных за окно). Семантика полей совпадает с v_persona_activity:
// notes = число РАЗНЫХ заметок, где личность комментировала (COUNT DISTINCT
// note_id), спан first/last_seen — по published_at её комментариев.
func (s *Store) CohortNodes(ctx context.Context, identities []string) (map[string]GraphNode, error) {
	out := map[string]GraphNode{}
	if len(identities) == 0 {
		return out, nil
	}
	// 1) личность → её анкеты (user_id) + метка/признак персоны.
	idArgs := make([]any, len(identities))
	for i, id := range identities {
		idArgs[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity, user_id, label, is_persona
		FROM v_identity WHERE identity IN (`+placeholders(len(identities))+`)`, idArgs...)
	if err != nil {
		return nil, err
	}
	uid2ident := map[int64]string{}
	var uids []int64
	for rows.Next() {
		var ident, label string
		var uid int64
		var isP int
		if err := rows.Scan(&ident, &uid, &label, &isP); err != nil {
			rows.Close()
			return nil, err
		}
		n := out[ident]
		n.Identity = ident
		n.Label = label
		n.IsPersona = isP == 1
		n.Accounts++
		out[ident] = n
		uid2ident[uid] = ident
		uids = append(uids, uid)
	}
	cerr := rows.Err()
	rows.Close()
	if cerr != nil {
		return nil, cerr
	}
	if len(uids) == 0 {
		return out, nil
	}
	// 2) all-time итоги только по этим анкетам — индексный проход author_id.
	arows, err := s.db.QueryContext(ctx, `
		SELECT author_id, COUNT(*), COUNT(DISTINCT note_id),
		       COALESCE(MIN(published_at), ''), COALESCE(MAX(published_at), '')
		FROM comments WHERE author_id IN (`+intList(uids)+`) GROUP BY author_id`)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var uid int64
		var comments, notes int
		var first, last string
		if err := arows.Scan(&uid, &comments, &notes, &first, &last); err != nil {
			return nil, err
		}
		ident := uid2ident[uid]
		n := out[ident]
		n.Comments += comments
		n.Notes += notes
		n.FirstSeen = minNonEmpty(n.FirstSeen, first)
		n.LastSeen = maxStr(n.LastSeen, last)
		out[ident] = n
	}
	return out, arows.Err()
}

// MaxPublishedAt возвращает самую свежую дату публикации комментария (ISO-8601
// UTC) — конец окна для отбора недавней активности. Пусто — комментариев нет.
// Скан без индекса по published_at (минуты на полном corpus).
func (s *Store) MaxPublishedAt(ctx context.Context) (string, error) {
	var mx sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(published_at) FROM comments`).Scan(&mx); err != nil {
		return "", err
	}
	return mx.String, nil
}

// ActiveCountsSince возвращает по каждой identity число комментариев и заметок
// с published_at >= since (ISO-8601) — мера недавней активности для отбора и
// ранжирования когорты. Скан без индекса по published_at — минуты на полном
// corpus, разово для отчёта.
func (s *Store) ActiveCountsSince(ctx context.Context, since string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity, SUM(cnt) FROM (
			SELECT i.identity AS identity, COUNT(*) AS cnt
			FROM comments c JOIN v_identity i ON i.user_id = c.author_id
			WHERE c.published_at >= ? GROUP BY i.identity
			UNION ALL
			SELECT i.identity AS identity, COUNT(*) AS cnt
			FROM notes n JOIN v_identity i ON i.user_id = n.author_id
			WHERE n.author_id IS NOT NULL AND n.published_at >= ? GROUP BY i.identity
		) GROUP BY identity`, since, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var ident string
		var cnt int
		if err := rows.Scan(&ident, &cnt); err != nil {
			return nil, err
		}
		out[ident] = cnt
	}
	return out, rows.Err()
}

// GraphEdges загружает рёбра с весом ≥ minReplies (dropSelf убирает само-петли).
func (s *Store) GraphEdges(ctx context.Context, minReplies int, dropSelf bool) ([]GraphEdge, error) {
	q := `SELECT from_identity, to_identity, replies FROM v_persona_reply_edges WHERE replies >= ?`
	if dropSelf {
		q += ` AND from_identity != to_identity`
	}
	rows, err := s.db.QueryContext(ctx, q, minReplies)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GraphEdge
	for rows.Next() {
		var e GraphEdge
		if err := rows.Scan(&e.From, &e.To, &e.Replies); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- портрет одной личности ---

// PortraitAccount — анкета в составе личности.
type PortraitAccount struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Age      string `json:"age,omitempty"`
	Comments int    `json:"comments"`
}

// PortraitEdge — сосед по графу ответов с весом.
type PortraitEdge struct {
	Identity string `json:"identity"`
	Label    string `json:"label"`
	Replies  int    `json:"replies"`
}

// Portrait — досье личности: слитые анкеты, активность, ключевые собеседники,
// интересы (identity_facts) и отношения (v_relations).
type Portrait struct {
	Identity  string            `json:"identity"`
	Label     string            `json:"label"`
	IsPersona bool              `json:"is_persona"`
	Accounts  []PortraitAccount `json:"accounts"`
	Comments  int               `json:"comments"`
	Notes     int               `json:"notes"`
	FirstSeen string            `json:"first_seen,omitempty"`
	LastSeen  string            `json:"last_seen,omitempty"`
	RepliesTo []PortraitEdge    `json:"replies_to"` // кому отвечает чаще всего
	RepliedBy []PortraitEdge    `json:"replied_by"` // кто чаще всего отвечает ему
	Facts     []IdentityFact    `json:"facts,omitempty"`
	Relations []RelationRow     `json:"relations,omitempty"`
}

// Portrait собирает досье по идентификатору узла ('p<id>' | 'u<id>'; можно и
// голый user-id — приведём к канонической личности). top — сколько собеседников
// в каждую сторону.
func (s *Store) Portrait(ctx context.Context, token string, top int) (Portrait, error) {
	identity, err := s.canonIdentity(ctx, token)
	if err != nil {
		return Portrait{}, err
	}
	var p Portrait
	p.Identity = identity

	if err := s.db.QueryRowContext(ctx, `
		SELECT is_persona, label, comments, notes, first_seen, last_seen
		FROM v_persona_activity WHERE identity = ?`, identity).Scan(
		&p.IsPersona, &p.Label, &p.Comments, &p.Notes, &p.FirstSeen, &p.LastSeen); err != nil {
		return Portrait{}, fmt.Errorf("личность %s не найдена: %w", identity, err)
	}

	accIDs, err := s.identityAccounts(ctx, identity, &p)
	if err != nil {
		return Portrait{}, err
	}
	if p.RepliesTo, err = s.portraitEdges(ctx, accIDs, identity, top, true); err != nil {
		return Portrait{}, err
	}
	if p.RepliedBy, err = s.portraitEdges(ctx, accIDs, identity, top, false); err != nil {
		return Portrait{}, err
	}
	if p.Facts, err = s.IdentityFacts(ctx, identity); err != nil {
		return Portrait{}, err
	}
	if p.Relations, err = s.IdentityRelations(ctx, identity, top); err != nil {
		return Portrait{}, err
	}
	return p, nil
}

// canonIdentity приводит токен к каноническому identity через v_identity
// (голый user-id, входящий в персону, станет 'p<persona_id>').
func (s *Store) canonIdentity(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	switch {
	case strings.HasPrefix(token, "p"):
		if _, err := strconv.ParseInt(token[1:], 10, 64); err != nil {
			return "", fmt.Errorf("некорректный id личности: %q", token)
		}
		return token, nil
	case strings.HasPrefix(token, "u"):
		token = token[1:]
	}
	uid, err := strconv.ParseInt(token, 10, 64)
	if err != nil {
		return "", fmt.Errorf("некорректный id: %q (ожидалось p<id>|u<id>|<user_id>)", token)
	}
	var identity string
	if err := s.db.QueryRowContext(ctx,
		`SELECT identity FROM v_identity WHERE user_id = ?`, uid).Scan(&identity); err != nil {
		return "", fmt.Errorf("пользователь %d не найден: %w", uid, err)
	}
	return identity, nil
}

// identityAccounts заполняет p.Accounts и возвращает их id.
func (s *Store) identityAccounts(ctx context.Context, identity string, p *Portrait) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.name, a.age, a.comments
		FROM v_identity i JOIN v_user_activity a ON a.id = i.user_id
		WHERE i.identity = ? ORDER BY a.comments DESC`, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var ac PortraitAccount
		if err := rows.Scan(&ac.ID, &ac.Name, &ac.Age, &ac.Comments); err != nil {
			return nil, err
		}
		p.Accounts = append(p.Accounts, ac)
		ids = append(ids, ac.ID)
	}
	return ids, rows.Err()
}

// portraitEdges — топ собеседников: outgoing=true «кому отвечает» (ответы ОТ
// анкет личности), иначе «кто отвечает ему» (ответы НА комментарии личности).
// Считается напрямую по comments (idx по author_id/parent_id), без полного
// прохода вью. Само-петли исключаются.
func (s *Store) portraitEdges(ctx context.Context, accIDs []int64, self string, top int, outgoing bool) ([]PortraitEdge, error) {
	if len(accIDs) == 0 {
		return nil, nil
	}
	in := intList(accIDs)
	var q string
	if outgoing {
		q = `
			SELECT ti.identity, MAX(ti.label), COUNT(*) AS n
			FROM comments c
			JOIN comments pc ON pc.id = c.parent_id
			JOIN v_identity ti ON ti.user_id = pc.author_id
			WHERE c.parent_id != 0 AND c.author_id IN (` + in + `)
			GROUP BY ti.identity HAVING ti.identity != ? ORDER BY n DESC LIMIT ?`
	} else {
		q = `
			SELECT fi.identity, MAX(fi.label), COUNT(*) AS n
			FROM comments child
			JOIN v_identity fi ON fi.user_id = child.author_id
			WHERE child.parent_id IN (SELECT id FROM comments WHERE author_id IN (` + in + `))
			GROUP BY fi.identity HAVING fi.identity != ? ORDER BY n DESC LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, q, self, top)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortraitEdge
	for rows.Next() {
		var e PortraitEdge
		if err := rows.Scan(&e.Identity, &e.Label, &e.Replies); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// intList — "1,2,3" для IN (...). Значения числовые, инъекция невозможна.
func intList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// placeholders — "?,?,?" из n знаков для IN (...) с параметрами.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// minNonEmpty — лексикографически меньшая непустая строка (min по датам ISO-8601,
// где пустая = «нет данных»).
func minNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "" || a < b:
		return a
	default:
		return b
	}
}

// maxStr — лексикографически большая строка (max по датам ISO-8601).
func maxStr(a, b string) string {
	if b > a {
		return b
	}
	return a
}
