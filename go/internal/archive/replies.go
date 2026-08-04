package archive

// Слой обогащения: настоящая цель ответа с мобильной версии сайта.
//
// Что не так с уже выкачанным. Граббер ходит по десктопной странице, где
// `comments.parent_id` — это КОРЕНЬ ВЕТКИ, а не реплика, которой отвечают
// (дерево там двухуровневое). Слой адресатов (v16) восстанавливает адресата по
// обращению «Ник, …» — но только человека, а не конкретную его реплику, и
// только у тех 93,6 % ответов, где обращение вообще есть.
//
// Что даёт мобильная. Она рендерит настоящее дерево вложенными <ul> (глубина до
// семи) — там сразу точная пара «реплика → та, которой отвечают», без
// обращения и без догадок. Обогащение НЕ трогает механизм выгрузки: заметки
// уже в архиве, оттуда берутся только пары id, а тексты, авторы и даты
// остаются десктопными (на мобильной даты относительные — «Сегодня в 11:43»).
//
// Почему проход отдельный и резюмируемый. Мобильная отдаёт тред целиком, и на
// длинных ветках сайт отвечает 500 (заметка 312866, 848 комментариев —
// воспроизводимо, как и десктопный ?view=tree). Поэтому неудача — это
// нормальный исход, он пишется в reply_scan и не повторяется на каждом прогоне.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// migrateV17SQL — слой настоящих целей ответа.
//
// comment_reply.reply_to намеренно без внешнего ключа (как и comments.parent_id):
// при частичной выгрузке адресат может быть ещё не сохранён, висячая ссылка —
// не нарушение целостности, а неполнота архива. У comment_id ключ есть:
// обогащаем только то, что уже выкачано.
const migrateV17SQL = `
CREATE TABLE comment_reply (
    comment_id INTEGER PRIMARY KEY REFERENCES comments(id),
    reply_to   INTEGER NOT NULL          -- id реплики, которой отвечают (никогда 0)
);
CREATE INDEX idx_comment_reply_to ON comment_reply(reply_to);

CREATE TABLE reply_scan (
    note_id    INTEGER PRIMARY KEY REFERENCES notes(id),
    scanned_at TEXT    NOT NULL,
    status     TEXT    NOT NULL,         -- ok | failed
    seen       INTEGER NOT NULL,         -- комментариев на странице
    stored     INTEGER NOT NULL          -- из них записано пар (адресат не сам себе корень)
);
`

// Итоги обхода заметки.
const (
	ReplyScanOK     = "ok"
	ReplyScanFailed = "failed"
)

// ReplyTreeResult — итог сохранения дерева одной заметки.
type ReplyTreeResult struct {
	Seen    int // комментариев на странице
	Stored  int // записано пар «реплика → адресат»
	Unknown int // комментариев страницы, которых нет в архиве (заметку пора обновить)
}

// SaveReplyTree записывает дерево ответов заметки: пары «реплика → та, которой
// отвечают». tree — id комментария → id родителя (0 — верхний уровень, такие
// пары не хранятся: это ответ самой заметке, он и так виден по parent_id).
// Идемпотентна: повторный проход перезаписывает пары заметки и отметку обхода.
// Комментарии, которых нет в архиве, молча пропускаются и считаются отдельно —
// внешний ключ бы на них упал, а причина не в дереве, а в устаревшей выгрузке.
func (s *Store) SaveReplyTree(ctx context.Context, noteID int64, tree map[int64]int64) (ReplyTreeResult, error) {
	res := ReplyTreeResult{Seen: len(tree)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM comment_reply
		WHERE comment_id IN (SELECT id FROM comments WHERE note_id = ?)`, noteID); err != nil {
		return res, fmt.Errorf("очистка дерева заметки %d: %w", noteID, err)
	}

	for id, parent := range tree {
		var known int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM comments WHERE id = ?`, id).Scan(&known); err != nil {
			return res, err
		}
		if known == 0 {
			res.Unknown++
			continue
		}
		if parent == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO comment_reply(comment_id, reply_to) VALUES (?, ?)`,
			id, parent); err != nil {
			return res, fmt.Errorf("запись пары %d→%d: %w", id, parent, err)
		}
		res.Stored++
	}

	if err := markScan(ctx, tx, noteID, ReplyScanOK, res.Seen, res.Stored); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// MarkReplyScanFailed запоминает, что страница заметки не отдалась: на длинных
// тредах сайт стабильно отвечает 500, и без отметки каждый прогон упирался бы
// в одни и те же заметки. Повторить можно прогоном с -retry.
func (s *Store) MarkReplyScanFailed(ctx context.Context, noteID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := markScan(ctx, tx, noteID, ReplyScanFailed, 0, 0); err != nil {
		return err
	}
	return tx.Commit()
}

func markScan(ctx context.Context, tx *sql.Tx, noteID int64, status string, seen, stored int) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO reply_scan(note_id, scanned_at, status, seen, stored)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(note_id) DO UPDATE SET
			scanned_at = excluded.scanned_at, status = excluded.status,
			seen = excluded.seen, stored = excluded.stored`,
		noteID, time.Now().UTC().Format(time.RFC3339), status, seen, stored)
	if err != nil {
		return fmt.Errorf("отметка обхода заметки %d: %w", noteID, err)
	}
	return nil
}

// ReplyScanTargets — заметки, которые ещё стоит обойти: сначала самые
// многолюдные (там больше всего рёбер чинится за один запрос). maxComments > 0
// отсекает заведомо неподъёмные треды — страница со всем тредом на них падает
// в 500. retryFailed — вернуть и те, что уже падали. limit 0 — без предела.
func (s *Store) ReplyScanTargets(ctx context.Context, limit, maxComments int, retryFailed bool) ([]int64, error) {
	q := `
		SELECT n.id FROM notes n
		JOIN (SELECT note_id, COUNT(*) AS cnt FROM comments GROUP BY note_id) c ON c.note_id = n.id
		LEFT JOIN reply_scan r ON r.note_id = n.id
		WHERE (r.note_id IS NULL`
	if retryFailed {
		q += ` OR r.status = '` + ReplyScanFailed + `'`
	}
	q += `)`
	args := []any{}
	if maxComments > 0 {
		q += ` AND c.cnt <= ?`
		args = append(args, maxComments)
	}
	q += ` ORDER BY c.cnt DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

// ReplyScanStats — покрытие архива слоем настоящих целей ответа.
type ReplyScanStats struct {
	Notes     int // заметок в архиве
	ScannedOK int
	Failed    int
	Pairs     int // пар «реплика → адресат»
	Replies   int // ответов в архиве (parent_id != 0)
}

func (s *Store) ReplyScanStats(ctx context.Context) (ReplyScanStats, error) {
	var st ReplyScanStats
	err := s.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM notes),
		       (SELECT COUNT(*) FROM reply_scan WHERE status = ?),
		       (SELECT COUNT(*) FROM reply_scan WHERE status = ?),
		       (SELECT COUNT(*) FROM comment_reply),
		       (SELECT COUNT(*) FROM comments WHERE parent_id != 0)`,
		ReplyScanOK, ReplyScanFailed).
		Scan(&st.Notes, &st.ScannedOK, &st.Failed, &st.Pairs, &st.Replies)
	if err != nil {
		return st, fmt.Errorf("покрытие слоя ответов: %w", err)
	}
	return st, nil
}
