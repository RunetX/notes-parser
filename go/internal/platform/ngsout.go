package platform

// Отправка написанного здесь на love.ngs.ru — обратное направление зеркала.
//
// Зачем: сообщество раскололось. Часть людей осталась на НГС и сюда не
// переехала, и человек, написавший на площадке, хочет быть прочитанным обеими
// половинами.
//
// ПО УМОЛЧАНИЮ ВЫКЛЮЧЕНО, и это про согласие, а не про осторожность: публикуя
// здесь, человек соглашался на публикацию ЗДЕСЬ. Унести его слова на чужой сайт
// под его именем можно только по отдельной, осознанно нажатой галочке (/me).
//
// ЯДРО ТОЛЬКО СТАВИТ СТРОКУ и ничего не отправляет. Ключ шифрования кук есть
// лишь у демона — в docker-compose это записано прямо: конфиг веб-морды
// отдельный, потому что она смотрит в интернет. Ходит на сайт демон, ровно как
// platout носит заметки в мессенджеры.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"lovegw/internal/sitetext"
)

// Состояния строки очереди.
const (
	NGSQueued  = "queued"
	NGSSent    = "sent"
	NGSFailed  = "failed"
	NGSSkipped = "skipped"
)

// Виды уносимого.
const (
	NGSNote    = "note"
	NGSComment = "comment"
)

// NGSMaxAttempts — сколько раз пробуем. Три: у сайта бывают короткие
// 5xx-штормы (замер 17.08.2026 — он отвечал 500 на любой комментарий), и одна
// попытка теряла бы текст на ровном месте. Больше трёх незачем: «сайт лежит
// час» и «нас не пускают» отсюда неотличимы, а повторять вечно значит однажды
// опубликовать недельной давности реплику в остывший тред.
const NGSMaxAttempts = 3

// NGSJob — что демону предстоит унести.
type NGSJob struct {
	ID       int64
	Kind     string
	ObjectID int64
	AuthorID int64
	// NoteID — куда нести комментарий: id заметки НГС. У заметки не заполняется.
	NoteID int64
	// ReplyToNGSID — id реплики НГС, которой отвечаем. Ноль — корень треда.
	ReplyToNGSID int64
	Body         string
	Attempts     int
}

// SetNGSSend переключает галочку «Отправлять».
//
// Только участнику и только себе — вызывающий передаёт СВОЙ id. Жителю не даём
// вовсе: у персонажа нет анкеты НГС, а значит и сессии, от чьего имени писать.
func (p *Platform) SetNGSSend(ctx context.Context, userID int64, on bool) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE users SET ngs_send = $2
		 WHERE id = $1 AND kind = $3 AND NOT persona AND anonymized_at IS NULL`,
		userID, on, KindMember)
	if err != nil {
		return fmt.Errorf("галочка отправки на НГС у %d: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// NGSSendOn — стоит ли галочка. Отдельным запросом, а не полем User:
// спрашивают её двое (страница /me и постановка в очередь), и оба раза точечно.
func (p *Platform) NGSSendOn(ctx context.Context, userID int64) (bool, error) {
	var on bool
	err := p.pool.QueryRow(ctx, `SELECT ngs_send FROM users WHERE id = $1`, userID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("галочка отправки на НГС у %d: %w", userID, err)
	}
	return on, nil
}

// enqueueNGS ставит публикацию в очередь на вынос, если её ЕСТЬ куда нести и
// автор этого хотел. Зовётся ТОЙ ЖЕ транзакцией, что и сама публикация: «опубликовано,
// но в очередь не попало» не должно существовать — тем же доводом, по которому
// рядом стоит moderation_queue.
//
// Три причины промолчать, и все три — норма, а не отказ:
//   - галочка не стоит (умолчание);
//   - анонимная заметка: на НГС её пришлось бы публиковать от имени автора, а он
//     именно этого и не хотел; анонимность у нас и анонимность там — разные
//     обещания, и подменять одно другим нельзя;
//   - комментарий под НАТИВНОЙ заметкой: на НГС такой заметки нет вовсе, нести
//     реплику некуда. Тред двойника и песочницы сюда же — их на сайте нет.
func enqueueNGS(ctx context.Context, q querier, kind string, objectID, authorID, noteID int64, anonymous bool) error {
	if anonymous || authorID == 0 {
		return nil
	}
	if kind == NGSComment && !IsNGS(noteID) {
		return nil
	}
	var on bool
	err := q.QueryRow(ctx, `
		SELECT ngs_send FROM users
		 WHERE id = $1 AND kind = $2 AND NOT persona AND anonymized_at IS NULL`,
		authorID, KindMember).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("очередь на НГС для %d: %w", objectID, err)
	}
	if !on {
		return nil
	}
	// ON CONFLICT DO NOTHING, а не проверка перед вставкой: уникальный индекс по
	// (kind, object_id) и есть защита от дубля в чужом треде, и держать её должна
	// база, а не порядок вызовов.
	if _, err := q.Exec(ctx, `
		INSERT INTO ngs_outbox (kind, object_id, author_id)
		VALUES ($1, $2, $3) ON CONFLICT (kind, object_id) DO NOTHING`,
		kind, objectID, authorID); err != nil {
		return fmt.Errorf("очередь на НГС для %d: %w", objectID, err)
	}
	return nil
}

// NextNGSJobs выдаёт голову очереди и СРАЗУ засчитывает попытку.
//
// Попытка считается на выдаче, а не на исходе, и это то же правило, по которому
// амвон не считает промахом не дошедший POST: сайт отвечает 500 и на принятый
// комментарий, поэтому «отправляется» неотличимо от «отправлено». Считай мы
// попытку по результату — упавший посреди отправки демон брал бы ту же строку
// вечно и однажды опубликовал бы её дважды.
//
// Тело и адресат берутся здесь же, одной транзакцией: демону не нужно знать
// устройство наших таблиц.
func (p *Platform) NextNGSJobs(ctx context.Context, limit int) ([]NGSJob, error) {
	if limit <= 0 {
		limit = 1
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("очередь на НГС: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	rows, err := tx.Query(ctx, `
		WITH taken AS (
		    SELECT id FROM ngs_outbox
		     WHERE state = $1
		     ORDER BY created_at, id
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE ngs_outbox o
		   SET attempts = o.attempts + 1
		  FROM taken
		 WHERE o.id = taken.id
		RETURNING o.id, o.kind, o.object_id, o.author_id, o.attempts`, NGSQueued, limit)
	if err != nil {
		return nil, fmt.Errorf("очередь на НГС: %w", err)
	}
	var jobs []NGSJob
	for rows.Next() {
		var j NGSJob
		if err := rows.Scan(&j.ID, &j.Kind, &j.ObjectID, &j.AuthorID, &j.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("очередь на НГС: %w", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("очередь на НГС: %w", err)
	}
	for i := range jobs {
		if err := fillNGSJob(ctx, tx, &jobs[i]); err != nil {
			return nil, fmt.Errorf("очередь на НГС: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("очередь на НГС: %w", err)
	}
	return jobs, nil
}

// fillNGSJob дочитывает текст и место. Пустое тело означает, что публикацию
// скрыли или снесли, пока строка ждала: демон такую пропустит.
func fillNGSJob(ctx context.Context, q querier, j *NGSJob) error {
	switch j.Kind {
	case NGSNote:
		err := q.QueryRow(ctx, `
			SELECT body FROM notes WHERE id = $1 AND status = 0 AND NOT anonymous`,
			j.ObjectID).Scan(&j.Body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	case NGSComment:
		// reply_to_id уносим ТОЛЬКО если адресат сам с НГС: у нативной реплики
		// там номера нет, и ответ ей на сайте становится корневым — это честнее,
		// чем привязать его к чужой строке.
		var replyTo *int64
		err := q.QueryRow(ctx, `
			SELECT body, note_id, reply_to_id FROM comments
			 WHERE id = $1 AND status = 0`, j.ObjectID).Scan(&j.Body, &j.NoteID, &replyTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if replyTo != nil && IsNGS(*replyTo) {
			j.ReplyToNGSID = *replyTo
		}
		return nil
	}
	return nil
}

// FinishNGSJob записывает исход. Исчерпав попытки, строка становится failed:
// повторять вечно — значит однажды опубликовать недельной давности реплику в
// остывший тред.
func (p *Platform) FinishNGSJob(ctx context.Context, id int64, ngsID string, cause error) error {
	if cause == nil {
		_, err := p.pool.Exec(ctx, `
			UPDATE ngs_outbox SET state = $2, sent_at = now(), ngs_id = $3, last_error = ''
			 WHERE id = $1`, id, NGSSent, ngsID)
		return err
	}
	_, err := p.pool.Exec(ctx, `
		UPDATE ngs_outbox
		   SET state = CASE WHEN attempts >= $3 THEN $4 ELSE $2 END,
		       last_error = $5
		 WHERE id = $1`, id, NGSQueued, NGSMaxAttempts, NGSFailed, cause.Error())
	return err
}

// SkipNGSJob снимает строку без попытки: публикации больше нет (скрыли, снесли)
// либо нести её некому — у автора нет живой сессии сайта. Это рабочий исход, и
// отличать его от неудачи надо, чтобы не жечь попытки.
func (p *Platform) SkipNGSJob(ctx context.Context, id int64, why string) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE ngs_outbox SET state = $2, last_error = $3 WHERE id = $1`,
		id, NGSSkipped, why)
	return err
}

// NGSOutboxStats — сводка для doctor и для глаз владельца.
type NGSOutboxStats struct {
	Queued, Sent, Failed, Skipped int
	// Echoed — сколько ушедших строк вернулось с сайта и было опознано своими
	// (у строки закреплён id записи НГС). Показывается затем, что гашение эха
	// работает МОЛЧА: удачное опознание не оставляет следа ни на странице, ни в
	// канале, и отличить «эхо гасится» от «эха ещё не было» иначе нечем — а
	// разница между ними это дубли в трёх местах сразу.
	Echoed int
	Oldest time.Time
}

func (p *Platform) NGSOutboxStats(ctx context.Context) (NGSOutboxStats, error) {
	var s NGSOutboxStats
	var oldest *time.Time
	err := p.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = $1),
		       count(*) FILTER (WHERE state = $2),
		       count(*) FILTER (WHERE state = $3),
		       count(*) FILTER (WHERE state = $4),
		       count(*) FILTER (WHERE ngs_id <> ''),
		       min(created_at) FILTER (WHERE state = $1)
		  FROM ngs_outbox`, NGSQueued, NGSSent, NGSFailed, NGSSkipped).
		Scan(&s.Queued, &s.Sent, &s.Failed, &s.Skipped, &s.Echoed, &oldest)
	if err != nil {
		return s, fmt.Errorf("сводка очереди на НГС: %w", err)
	}
	if oldest != nil {
		s.Oldest = *oldest
	}
	return s, nil
}

// NGSEchoWindow — сколько строка очереди вправе узнавать себя в записи сайта.
//
// Сутки. Попытки кончаются за полторы минуты, и текст, всплывший на НГС днём
// позже, нашей копией уже не является: либо человек написал его там руками,
// либо это чужая цитата. Окно тут предохранитель, а не мерка, — главную защиту
// даёт ngs_id: одну ушедшую строку нельзя опознать дважды.
const NGSEchoWindow = 24 * time.Hour

// ClaimNGSEcho — узнать в записи НГС свою копию, ушедшую туда из Зазеркалья, и
// закрепить за ней id сайта.
//
// Зовёт это ЗЕРКАЛО, прежде чем принять запись, и по ответу «да» не несёт её
// никуда: ни на площадку (там она уже лежит нативной строкой), ни в каналы
// мессенджеров (туда её отнёс platout). Второго прохода не будет — id
// запоминается на стороне зеркала.
//
// Опознаём по ТРОЙКЕ «автор, место, отпечаток текста», а не по одному тексту:
// побайтового совпадения не бывает (сайт схлопывает пробелы, делает nl2br и
// подменяет эмодзи картинками), зато совпадения всех трёх у чужой реплики не
// бывает тем более. Поверх стоит предохранитель, который и делает ошибку
// неопасной: у каждой строки очереди ngs_id заполняется РОВНО ОДИН РАЗ, то есть
// погасить эха больше, чем мы отправили, нельзя ни при какой сверке.
//
// Кандидатом считается строка, по которой была ХОТЯ БЫ ОДНА попытка, а не только
// удачная: сайт отвечает 500 и на принятый комментарий (замер 17.08.2026), и
// строка в состоянии failed сплошь и рядом лежит на НГС. Возьми мы одни лишь
// sent — именно эти реплики и вернулись бы дублем.
//
// noteID ноль означает заметку: у неё места на сайте нет, пока она туда не
// уехала.
func (p *Platform) ClaimNGSEcho(ctx context.Context, kind string, noteID, authorID int64,
	body, ngsID string, at time.Time) (bool, error) {
	if authorID == 0 || ngsID == "" || body == "" {
		return false, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	// Уже закреплённая за этой записью строка узнаёт её СНОВА, и это не
	// мелочь: зеркало помнит свой ответ в SQLite, а не записавшаяся отметка
	// иначе означала бы дубль на следующем такте. Опознание идемпотентно по id
	// записи — значит память зеркала остаётся оптимизацией.
	var seen int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM ngs_outbox WHERE kind = $1 AND author_id = $2 AND ngs_id = $3`,
		kind, authorID, ngsID).Scan(&seen); err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	if seen > 0 {
		return true, nil
	}

	var (
		rows pgx.Rows
		from = at.Add(-NGSEchoWindow)
	)
	switch kind {
	case NGSNote:
		rows, err = tx.Query(ctx, `
			SELECT o.id, n.body
			  FROM ngs_outbox o JOIN notes n ON n.id = o.object_id
			 WHERE o.kind = $1 AND o.author_id = $2
			   AND o.attempts > 0 AND o.ngs_id = ''
			   AND o.created_at > $3
			 ORDER BY o.created_at
			   FOR UPDATE OF o`, NGSNote, authorID, from)
	case NGSComment:
		rows, err = tx.Query(ctx, `
			SELECT o.id, c.body
			  FROM ngs_outbox o JOIN comments c ON c.id = o.object_id
			 WHERE o.kind = $1 AND o.author_id = $2 AND c.note_id = $3
			   AND o.attempts > 0 AND o.ngs_id = ''
			   AND o.created_at > $4
			 ORDER BY o.created_at
			   FOR UPDATE OF o`, NGSComment, authorID, noteID, from)
	default:
		return false, fmt.Errorf("опознание эха: неизвестный вид %q", kind)
	}
	if err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	var claim int64
	for rows.Next() {
		var (
			id   int64
			sent string
		)
		if err := rows.Scan(&id, &sent); err != nil {
			rows.Close()
			return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
		}
		// Правило сверки зависит от ВИДА, и это про форму данных, а не про
		// строгость: реплику сайт отдаёт целиком, а заметку лента показывает
		// началом.
		same := sitetext.SameText(sent, body)
		if kind == NGSNote {
			same = sitetext.SameStart(sent, body)
		}
		if claim == 0 && same {
			claim = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	if claim == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE ngs_outbox SET ngs_id = $2 WHERE id = $1`, claim, ngsID); err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	return true, nil
}
