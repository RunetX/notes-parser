// Пакет platnarod — площадка как СЦЕНА, на которой играет народ.
//
// Отдельный пакет по той же причине, что platsink, platmod, platdigest и
// platimport: ядро площадки не обязано знать, что у кого-то есть эпик с
// персонажами. Здесь же держится и обратный инвариант — `narod` не импортирует
// `platform` (см. шапку narod): без него тот же оркестратор не гонялся бы
// реплеем на машине разработчика, где Postgres не поднят вовсе.
//
// Читает он ТОЛЬКО песочницы (`notes.stage`), и это не фильтр ради опрятности, а
// граница ответственности: житель не должен видеть обычных заметок площадки —
// иначе однажды он в них и заговорит. Правило это стоит вторым забором к
// ядерному (platform.stageGuard, «житель пишет только в песочнице»): ядро не
// пустит, а сцена не покажет.
package platnarod

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"lovegw/internal/narod"
	"lovegw/internal/platform"
)

// Stage — площадка глазами народа (реализует narod.Stage).
type Stage struct {
	pool *pgxpool.Pool
	core *platform.Platform
}

// New создаёт сцену.
func New(p *platform.Platform) *Stage {
	return &Stage{pool: p.Pool(), core: p}
}

// stageNotesQuery — заметки-песочницы после afterID.
//
// Скрытые не отдаются: для пишущего скрытая заметка просто отсутствует, и то же
// самое ответило бы ядро при попытке в неё написать. Аноним здесь невозможен по
// построению (песочницу заводит администратор или житель), но LEFT JOIN оставлен
// — автор может быть и снесён.
const stageNotesQuery = `
	SELECT n.id, coalesce(n.author_id, 0), coalesce(u.nick, ''), n.body,
	       n.published_at, n.locked
	  FROM notes n
	  LEFT JOIN users u ON u.id = n.author_id
	 WHERE n.stage AND n.status = 0 AND n.id > $1
	 ORDER BY n.id
	 LIMIT $2`

// StageNotesSince — заметки-песочницы с номером больше afterID.
func (s *Stage) StageNotesSince(ctx context.Context, afterID int64, limit int) ([]narod.StageNote, error) {
	rows, err := s.pool.Query(ctx, stageNotesQuery, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("заметки песочницы: %w", err)
	}
	defer rows.Close()
	var out []narod.StageNote
	for rows.Next() {
		var n narod.StageNote
		if err := rows.Scan(&n.ID, &n.AuthorID, &n.AuthorNick, &n.Body,
			&n.PublishedAt, &n.Locked); err != nil {
			return nil, fmt.Errorf("заметки песочницы: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// stageThreadQuery — тред целиком, ПО ВРЕМЕНИ.
//
// По времени, а не по материализованному пути, и это не мелочь: кубику нужен
// номер реплики ПО ПОРЯДКУ РАЗГОВОРА — замер отклика снят именно по нему
// (archive.MineReplyRate, корзины по позиции в треде). Обход дерева дал бы
// порядок веток, в котором двадцатая по времени реплика оказывается второй, и
// затухание считалось бы не по тому.
//
// Индекс тот же, что у линейного вида морды (comments_flat_time, миграция 0014),
// — второго ради песочницы не заводится.
const stageThreadQuery = `
	SELECT c.id, c.note_id, coalesce(c.author_id, 0), coalesce(u.nick, ''),
	       c.body, coalesce(c.reply_to_id, 0), c.published_at
	  FROM comments c
	  LEFT JOIN users u ON u.id = c.author_id
	 WHERE c.note_id = $1 AND c.status = 0
	 ORDER BY c.published_at, c.id`

// StageThread — весь тред заметки, от старых реплик к новым.
func (s *Stage) StageThread(ctx context.Context, noteID int64) ([]narod.StageReply, error) {
	rows, err := s.pool.Query(ctx, stageThreadQuery, noteID)
	if err != nil {
		return nil, fmt.Errorf("тред песочницы %d: %w", noteID, err)
	}
	defer rows.Close()
	var out []narod.StageReply
	for rows.Next() {
		var c narod.StageReply
		if err := rows.Scan(&c.ID, &c.NoteID, &c.AuthorID, &c.AuthorNick,
			&c.Body, &c.ReplyTo, &c.PublishedAt); err != nil {
			return nil, fmt.Errorf("тред песочницы %d: %w", noteID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StagePost публикует реплику жителя.
//
// Идёт она ЧЕРЕЗ ЯДРО, тем же вызовом, что и реплика человека из формы, — и это
// требование, а не удобство. Реплика жителя обязана пройти всё то же самое:
// право писать, потолок частоты, замок треда, гейт песочницы, очередь модерации
// той же транзакцией и событие в шину. Пиши мы INSERT'ом мимо ядра, у площадки
// завёлся бы автор, которого никто не проверяет, — а это ровно та поблажка, от
// которой отказались, оставив жителей под автоматом модерации наравне со всеми.
func (s *Stage) StagePost(ctx context.Context, userID, noteID, replyTo int64, body string) (int64, error) {
	return s.core.CreateComment(ctx, platform.NewComment{
		NoteID: noteID, AuthorID: userID, ReplyToID: replyTo, Body: body,
	})
}
