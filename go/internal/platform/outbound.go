package platform

// Исходящее направление площадки: что написано ЗДЕСЬ — читается отсюда, чтобы
// уйти в каналы мессенджеров. В этом файле только чтение; доставку и её учёт
// ведёт пакет platout, потому что «где это сообщение в мессенджере» знает
// зеркальная SQLite, а не Postgres.
//
// Маскирование анонима стоит в SELECT ровно как у страниц, и по той же причине:
// у OutNote просто НЕТ поля, куда положить настоящего автора анонимной заметки.
// Канал — такой же посторонний, как читатель, и забывчивый композер сообщения
// не может выдать того, чего ему не дали.

import (
	"context"
	"fmt"
	"time"
)

// OutNote — нативная заметка, готовая к отправке в канал.
//
// AuthorID нулевой у анонимной заметки И у участника, пришедшего по
// приглашению: композер поста делает из него ссылку на анкету love.ngs.ru, а
// такой анкеты у нативного идентификатора нет. Ноль там значит «показать имя
// без ссылки» — ровно то, что нужно в обоих случаях.
type OutNote struct {
	ID          int64
	Anonymous   bool
	AuthorID    int64
	AuthorNick  string
	AvatarSHA   []byte
	AvatarMIME  string
	Body        string
	PublishedAt time.Time
}

// OutComment — нативный комментарий, готовый к отправке в тред.
//
// Поля «анонимно» здесь нет, потому что его нет и у NewComment: анонимной
// бывает заметка, реплика подписана всегда.
//
// Body идёт БЕЗ обращения «Ник, …» — на площадке оно хранится ребром
// ReplyToID, а не текстом. Дорисовывать его для мессенджера не нужно: реплика
// уходит ответом на сообщение адресата, и цитату рисует сам мессенджер — то же
// самое зеркало делает с комментариями НГС.
type OutComment struct {
	ID          int64
	NoteID      int64
	AuthorID    int64
	AuthorNick  string
	AvatarSHA   []byte
	AvatarMIME  string
	Body        string
	ReplyToID   int64
	PublishedAt time.Time
}

// AnonNick — имя автора анонимной заметки в канале.
const AnonNick = "Аноним"

const outNoteQuery = `
	SELECT n.id, n.anonymous, n.body, n.published_at,
	       CASE WHEN n.anonymous THEN NULL ELSE n.author_id  END,
	       CASE WHEN n.anonymous THEN NULL ELSE u.nick       END,
	       CASE WHEN n.anonymous THEN NULL ELSE u.avatar_sha END,
	       CASE WHEN n.anonymous THEN NULL ELSE m.mime       END
	  FROM notes n
	  LEFT JOIN users u ON u.id = n.author_id
	  LEFT JOIN media m ON m.sha256 = u.avatar_sha
	 WHERE n.id > $1 AND n.status = 0
	 ORDER BY n.id
	 LIMIT $2`

// OutboundNotes — нативные заметки после afterID, от старых к новым.
//
// Скрытые (status ≠ 0) не отдаются вовсе: заметка, снятая модератором или
// автором, в канал уходить не должна. Обратная сторона — заметка, снятая и
// возвращённая до того, как её забрал обход, в канал не попадёт уже никогда;
// это осознанный размен в пользу «снятое не всплывает».
func (p *Platform) OutboundNotes(ctx context.Context, afterID int64, limit int) ([]OutNote, error) {
	rows, err := p.pool.Query(ctx, outNoteQuery, floorNative(afterID), clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("исходящие заметки: %w", err)
	}
	defer rows.Close()

	var out []OutNote
	for rows.Next() {
		var (
			n      OutNote
			author *int64
			nick   *string
			mime   *string
		)
		if err := rows.Scan(&n.ID, &n.Anonymous, &n.Body, &n.PublishedAt,
			&author, &nick, &n.AvatarSHA, &mime); err != nil {
			return nil, fmt.Errorf("исходящие заметки: %w", err)
		}
		n.AuthorID, n.AuthorNick, n.AvatarMIME = ngsIDOf(author), strOf(nick), strOf(mime)
		if n.Anonymous {
			n.AuthorNick = AnonNick
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

const outCommentQuery = `
	SELECT c.id, c.note_id, c.body, c.reply_to_id, c.published_at,
	       c.author_id, u.nick, u.avatar_sha, m.mime
	  FROM comments c
	  LEFT JOIN users u ON u.id = c.author_id
	  LEFT JOIN media m ON m.sha256 = u.avatar_sha
	 WHERE c.id > $1 AND c.status = 0
	 ORDER BY c.id
	 LIMIT $2`

// OutboundComments — нативные комментарии после afterID, от старых к новым.
// Порядок здесь не украшение: в треде мессенджера реплика обязана появиться
// после того, на что отвечает, иначе цитировать будет нечего.
func (p *Platform) OutboundComments(ctx context.Context, afterID int64, limit int) ([]OutComment, error) {
	rows, err := p.pool.Query(ctx, outCommentQuery, floorNative(afterID), clampLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("исходящие комментарии: %w", err)
	}
	defer rows.Close()

	var out []OutComment
	for rows.Next() {
		var (
			c      OutComment
			author *int64
			reply  *int64
			nick   *string
			mime   *string
		)
		if err := rows.Scan(&c.ID, &c.NoteID, &c.Body, &reply, &c.PublishedAt,
			&author, &nick, &c.AvatarSHA, &mime); err != nil {
			return nil, fmt.Errorf("исходящие комментарии: %w", err)
		}
		c.AuthorID, c.AuthorNick = ngsIDOf(author), strOf(nick)
		c.ReplyToID, c.AvatarMIME = idOf(reply), strOf(mime)
		out = append(out, c)
	}
	return out, rows.Err()
}

// floorNative опускает курсор не ниже начала нативной полосы: зеркальные строки
// в канал уже отнесло само зеркало, и второй раз им туда не надо.
func floorNative(afterID int64) int64 {
	if afterID < NativeIDBase-1 {
		return NativeIDBase - 1
	}
	return afterID
}

// ngsIDOf — идентификатор автора, годный для ссылки на анкету НГС. У нативного
// участника (пришёл по приглашению, номера анкеты нет) — ноль: показать его
// именем без ссылки честнее, чем увести читателя на чужую анкету.
func ngsIDOf(p *int64) int64 {
	if id := idOf(p); IsNGS(id) {
		return id
	}
	return 0
}
