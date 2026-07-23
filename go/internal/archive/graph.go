package archive

import (
	"context"
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

// Portrait — досье личности: слитые анкеты, активность, ключевые собеседники.
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
