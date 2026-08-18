package platform

// Настоящее дерево ответов.
//
// Живое зеркало знает адресата только по обращению «Ник, …» и разрешает его в
// ПОСЛЕДНЮЮ реплику этого человека в заметке. Это угадывание, и точность у него
// примерно половина: 16.08.2026 в заметке 313000 реплика ПростоТы 07:26:54
// привязалась к комментарию Livesey 07:20:16 вместо настоящего 07:07:15 — ветка
// выросла не там, а у второго комментария пропали ответы.
//
// Настоящее дерево отдаёт мобильная версия сайта, и точность там 92 % (замер по
// архиву 14.08.2026 — 98,7 % с учётом восстановленных пар). Здесь оно
// применяется: рёбра переставляются, пути перекладываются, а обращение
// срезается из тела ровно там, где ребро появилось впервые, — иначе показ
// дорисует ник поверх уже написанного и выйдет «Ник, Ник, …».

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// ReplyTreeStats — что сделал один проход по заметке.
type ReplyTreeStats struct {
	Total   int // комментариев в заметке
	Known   int // из них найдено в мобильном дереве
	Edges   int // переставлено рёбер
	Paths   int // переложено путей
	Trimmed int // снято обращений из тела
}

type treeRow struct {
	id      int64
	replyTo int64
	source  ReplySource
	body    string
	path    string
	nick    string
}

// ApplyReplyTree переставляет рёбра заметки по дереву мобильной версии.
//
// tree — «id комментария → id того, кому он отвечает», 0 означает реплику
// верхнего уровня. Комментарии, которых в дереве нет (сайт их уже удалил, а у
// нас они сохранились), остаются как есть: молчание мобильной страницы не
// повод рвать ребро.
//
// Всё в одной транзакции: дерево — это связная структура, и заметка, у которой
// половина путей новая, а половина старая, показывается неправильно вся.
func (p *Platform) ApplyReplyTree(ctx context.Context, noteID int64, tree map[int64]int64) (ReplyTreeStats, error) {
	var st ReplyTreeStats
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	rows, err := tx.Query(ctx, `
		SELECT c.id, coalesce(c.reply_to_id, 0), c.reply_source, c.body, c.path,
		       coalesce(nullif(u.nick, ''), nullif(c.author_display, ''), '')
		  FROM comments c
		  LEFT JOIN users u ON u.id = c.author_id
		 WHERE c.note_id = $1
		 ORDER BY c.id`, noteID)
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	var list []treeRow
	for rows.Next() {
		var r treeRow
		if err := rows.Scan(&r.id, &r.replyTo, &r.source, &r.body, &r.path, &r.nick); err != nil {
			rows.Close()
			return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	st.Total = len(list)

	byID := make(map[int64]*treeRow, len(list))
	parent := make(map[int64]int64, len(list))
	for i := range list {
		byID[list[i].id] = &list[i]
		parent[list[i].id] = list[i].replyTo
	}
	for id, p := range tree {
		if _, ok := byID[id]; !ok {
			continue // комментарий есть на сайте, но не у нас — не наше дело
		}
		st.Known++
		parent[id] = p
	}

	// Пути перекладываются одним проходом по возрастанию id: ответ всегда
	// новее того, кому отвечает, поэтому путь родителя к этому моменту готов.
	paths := make(map[int64]string, len(list))
	for i := range list {
		r := &list[i]
		pp := ""
		if pid := parent[r.id]; pid != 0 && pid < r.id {
			pp = paths[pid]
		}
		np, err := ChildPath(ClampParent(pp), r.id)
		if err != nil {
			return st, fmt.Errorf("путь комментария %d: %w", r.id, err)
		}
		paths[r.id] = np
	}

	for i := range list {
		r := &list[i]
		newParent := parent[r.id]
		newPath := paths[r.id]
		newBody := r.body
		newSource := r.source
		if _, inTree := tree[r.id]; inTree {
			newSource = ReplyMobileTree
		}
		// Обращение срезается ровно тогда, когда ребро появилось: у строк, где
		// зеркало адресата не нашло, «Ник, » так и остался в теле. Ник берём у
		// НОВОГО адресата — по нему и проверяем, что срезаем именно обращение,
		// а не начало фразы.
		if newParent != 0 && r.replyTo == 0 {
			if target, ok := byID[newParent]; ok {
				if cut, done := TrimAddress(newBody, target.nick); done {
					newBody = cut
					st.Trimmed++
				}
			}
		}
		if newParent == r.replyTo && newPath == r.path && newBody == r.body && newSource == r.source {
			continue
		}
		if newParent != r.replyTo {
			st.Edges++
		}
		if newPath != r.path {
			st.Paths++
		}
		branchRoot := int64(0)
		if root, err := BranchRootID(newPath); err == nil && root != r.id {
			branchRoot = root
		}
		if _, err := tx.Exec(ctx, `
			UPDATE comments
			   SET reply_to_id = $2, reply_source = $3, path = $4, depth = $5,
			       branch_root_id = $6, body = $7
			 WHERE id = $1`,
			r.id, nullID(newParent), newSource, newPath, PathDepth(newPath),
			nullID(branchRoot), newBody); err != nil {
			return st, fmt.Errorf("правка комментария %d: %w", r.id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	return st, nil
}

// TrimAddress срезает обращение «Ник, » с начала тела, если оно там есть.
//
// Ник сверяется с настоящим ником адресата: без этого «Кстати, я тоже» лишилось
// бы первого слова. Сравнение без учёта регистра — люди пишут обращение как
// придётся, а сайт подставляет его с заглавной.
//
// Экспортируется ради второго писателя — импорта архива: там ребро проставляется
// не по одной заметке, а сразу по всем, и правило снятия обращения обязано быть
// тем же самым. Разъехавшись, они дали бы на одной странице «Ник, Ник, …» рядом
// с чистыми репликами.
func TrimAddress(body, nick string) (string, bool) {
	nick = strings.TrimSpace(nick)
	if nick == "" || body == "" {
		return body, false
	}
	if !strings.HasPrefix(strings.ToLower(body), strings.ToLower(nick)) {
		return body, false
	}
	rest := body[len(nick):]
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, ",") {
		return body, false
	}
	rest = strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
	if rest == "" {
		// «Ник,» и больше ничего: пустая реплика хуже реплики с обращением.
		return body, false
	}
	return rest, true
}

// ---------------------------------------------------------------- дисциплина обхода

// ReplyScanDue — заметки, которым пора уточнить дерево: сперва не смотренные
// ни разу, дальше самые давние. Отброшенные (reply_scan_skip) не возвращаются
// никогда — их решение принимает человек.
func (p *Platform) ReplyScanDue(ctx context.Context, limit int) ([]int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT n.id
		  FROM notes n
		  LEFT JOIN ingest_state s ON s.note_id = n.id
		 WHERE n.id < $1 AND n.comment_count > 0
		   AND coalesce(s.reply_scan_skip, false) = false
		 ORDER BY s.reply_scan_at NULLS FIRST, n.id DESC
		 LIMIT $2`, NativeIDBase, limit)
	if err != nil {
		return nil, fmt.Errorf("очередь обхода дерева: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("очередь обхода дерева: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkReplyScan отмечает проход по заметке. Три подряд неудачи гасят заметку
// насовсем: страницу, которую сайт не отдаёт (на 848 репликах он отвечал 500),
// незачем дёргать вечно — это плата за каждый обход и повод для тревоги.
func (p *Platform) MarkReplyScan(ctx context.Context, noteID int64, ok bool) error {
	if ok {
		_, err := p.pool.Exec(ctx, `
			INSERT INTO ingest_state (note_id, reply_scan_at, reply_scan_fails)
			VALUES ($1, now(), 0)
			ON CONFLICT (note_id) DO UPDATE SET reply_scan_at = now(), reply_scan_fails = 0`, noteID)
		return wrapf(err, "отметка обхода заметки %d", noteID)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO ingest_state (note_id, reply_scan_at, reply_scan_fails)
		VALUES ($1, now(), 1)
		ON CONFLICT (note_id) DO UPDATE
		   SET reply_scan_at = now(),
		       reply_scan_fails = ingest_state.reply_scan_fails + 1,
		       reply_scan_skip = ingest_state.reply_scan_fails + 1 >= 3`, noteID)
	return wrapf(err, "отметка неудачи обхода заметки %d", noteID)
}
