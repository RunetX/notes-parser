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
	"strconv"
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

// Причины пропуска, названные ЗДЕСЬ, а не в самой службе выноса. Их читает не
// только лог: по ним страница /me говорит человеку, что его записи никуда не
// уходят, — а галочка, которая молча ничего не делает, хуже отсутствующей.
// Оплачено живым случаем 02.09.2026: у участницы семь ответов подряд легли в
// skipped «нет живой сессии сайта», и узнать об этом ей было неоткуда. Две
// копии одного текста (в службе и в запросе страницы) разъехались бы молча.
const (
	NGSNoSession      = "нет живой сессии сайта"
	NGSSessionInvalid = "сессия сайта недействительна"
	NGSSessionUnread  = "сессия сайта не читается"
)

// ngsSessionCauses — те причины, что лечатся ОДНИМ И ТЕМ ЖЕ: войти в РюмкинЪ
// заново. Человеку разница между ними не нужна, ему нужно действие.
var ngsSessionCauses = []string{NGSNoSession, NGSSessionInvalid, NGSSessionUnread}

// NGSStuck — сколько записей человека не ушло на НГС из-за сессии и с каких пор.
//
// Ноль означает «сейчас работает»: считаются строки, только пока ПОСЛЕДНЯЯ в
// очереди — такой же пропуск. Иначе строка на /me осталась бы навсегда: старые
// пропуски не переигрываются, и однажды не ушедшая заметка попрекала бы человека
// год спустя, когда всё давно наладилось. Как только следующая запись уезжает,
// предупреждение гаснет само.
//
// Индекса по author_id у ngs_outbox нет, и заводить его сюда незачем: страница
// «Моя страница» открывается редко и одним человеком, а тем же способом эту
// таблицу уже читает опознание эха.
func (p *Platform) NGSStuck(ctx context.Context, userID int64) (int, time.Time, error) {
	var (
		n     int
		since *time.Time
	)
	err := p.pool.QueryRow(ctx, `
		WITH last AS (
		    SELECT state, last_error FROM ngs_outbox
		     WHERE author_id = $1
		     ORDER BY created_at DESC, id DESC LIMIT 1
		)
		SELECT count(*), min(o.created_at)
		  FROM ngs_outbox o, last
		 WHERE o.author_id = $1 AND o.state = $2 AND o.last_error = ANY($3)
		   AND last.state = $2 AND last.last_error = ANY($3)`,
		userID, NGSSkipped, ngsSessionCauses).Scan(&n, &since)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("непрошедшее на НГС у %d: %w", userID, err)
	}
	if since == nil {
		return n, time.Time{}, nil
	}
	return n, *since, nil
}

// NGSJob — что демону предстоит унести.
type NGSJob struct {
	ID       int64
	Kind     string
	ObjectID int64
	AuthorID int64
	// NoteID — куда нести комментарий: id заметки НГС. У заметки не заполняется.
	NoteID int64
	// Адресата здесь НЕТ намеренно: он спрашивается перед самой отправкой
	// (NGSReplyTarget), потому что родитель мог уехать на сайт тем же проходом
	// очереди и получить номер в её середине.
	Body     string
	Attempts int
	// Anonymous — заметка была опубликована здесь анонимно, и на НГС она уходит
	// такой же (hideme=1). Читается на ВЫДАЧЕ строки, а не при постановке в
	// очередь: там это стоило бы лишнего условия в самой горячей транзакции
	// площадки, а здесь заметка и так читается ради тела. Разойтись с тем, что
	// было при публикации, признак не может — анонимность правкой не меняется
	// («автор и анонимность не редакторский вопрос», см. EditNoteAsAdmin).
	Anonymous bool
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
//   - комментарий под НАТИВНОЙ заметкой: на НГС такой заметки нет вовсе, нести
//     реплику некуда. Тред двойника и песочницы сюда же — их на сайте нет.
//   - ПЕСОЧНИЦА, и это ЧЕТВЁРТЫЙ её выход наружу.
//
// ЧЕТВЁРТОЙ ПРИЧИНОЙ БЫЛА АНОНИМНОСТЬ, и она снята 02.09.2026 — вместе с
// доводом, который оказался неверен. Стояло: «на НГС её пришлось бы публиковать
// от имени автора, а он именно этого и не хотел». На самом деле сайт принимает
// анонимную заметку (`action_note[hideme]=1`), love.Client.PostNote берёт этот
// признак параметром, и РюмкинЪ публикует так с самого начала
// (/add_anonymous_note). То есть анонимность переносится КАК ЕСТЬ: обещание,
// данное здесь, там не нарушается.
//
// Настоящая причина, по которой анонимную заметку нельзя было отпускать, лежала
// в ЭХЕ и держится теперь явно: своя копия опознаётся по тройке «автор, место,
// отпечаток текста», а у анонимной записи НГС автора нет вовсе — она вернулась
// бы зеркалом второй строкой в ленту и постами в оба канала. Поэтому вместе с
// этой правкой ClaimNGSEcho научен узнавать заметку БЕЗ автора (см. там же).
//
// Четвёртая причина оплачена боем 01.09.2026: двойник (заметка 100000000036,
// тело — служебная строка synthBody «Смежное обсуждение: о заметке говорят
// жители площадки, их реплики пишет машина») уехал на love.ngs.ru заметкой
// 313147 под именем администратора. Правило «наружу из песочницы не уходит
// ничего» держали каналы (platout), выпуск (platdigest) и поводы (fanOut) — а
// вынос на сайт оказался четвёртой дверью, и о ней забыли.
//
// Признак берётся у ВЫЗЫВАЮЩЕГО, а не спрашивается здесь запросом: у обеих
// публикаций он уже в руках (у заметки это in.Stage, у комментария — колонка,
// прочитанная для stageGuard), а лишний SELECT в самой горячей транзакции
// площадки платили бы все. У заметки признак закрывает и двойника: двойника без
// песочницы не бывает вовсе (см. CreateNote).
//
// У комментария случай СВОЙ, и правилом строкой выше он не закрывается: реплику
// в нативной песочнице не уносит «под нативной заметкой некуда», а вот у
// ЗЕРКАЛЬНОЙ заметки, переведённой в песочницу (narod stage), на сайте есть
// настоящий тред — туда машинная сцена и легла бы.
func enqueueNGS(ctx context.Context, q querier, kind string, objectID, authorID, noteID int64, stage bool) error {
	if authorID == 0 || stage {
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
			SELECT body, anonymous FROM notes WHERE id = $1 AND status = 0`,
			j.ObjectID).Scan(&j.Body, &j.Anonymous)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	case NGSComment:
		err := q.QueryRow(ctx, `
			SELECT body, note_id FROM comments
			 WHERE id = $1 AND status = 0`, j.ObjectID).Scan(&j.Body, &j.NoteID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	return nil
}

// FinishNGSJob записывает исход. Исчерпав попытки, строка становится failed:
// повторять вечно — значит однажды опубликовать недельной давности реплику в
// остывший тред.
func (p *Platform) FinishNGSJob(ctx context.Context, id int64, ngsID string, cause error) error {
	if cause == nil {
		// СТРОКА ПЕРЕРИСОВЫВАЕТСЯ, и отметка тут та же, что у переезда ветки.
		//
		// У реплики, ушедшей на НГС, меняется метка происхождения: «написано
		// здесь» становится «написано здесь, копия ушла». Меняется она через
		// полминуты после публикации — с таким тактом ходит очередь
		// выноса, — то есть ровно тогда, когда автор ещё смотрит на свою
		// реплику; без отметки он видит прежний значок до обновления страницы
		// (жалоба владельца 02.09.2026).
		//
		// Канал для этого уже есть и второго не заводится: moved_at и заголовок
		// X-Fresh-Moved несут строку ГОТОВОЙ разметкой, а страница ставит её
		// вместо прежней. Довод тот же, по которому переезд не шлют парой чисел:
		// второго способа превратить наш текст в HTML нет.
		//
		// Одним запросом с выносом, а не соседним: «ушло, но страница об этом не
		// узнает» — то же самое состояние, которого не должно существовать, что
		// и «опубликовано, но в очередь модерации не попало».
		_, err := p.pool.Exec(ctx, `
			WITH done AS (
				UPDATE ngs_outbox SET state = $2, sent_at = now(), ngs_id = $3, last_error = ''
				 WHERE id = $1
			 RETURNING kind, object_id
			)
			UPDATE comments c SET moved_at = now()
			  FROM done
			 WHERE done.kind = $4 AND c.id = done.object_id`,
			id, NGSSent, ngsID, NGSComment)
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
	// Автор неизвестен — это АНОНИМНАЯ запись сайта, и наша анонимная заметка
	// возвращается оттуда именно такой: масок у НГС две («аноним» в ленте и
	// пусто в разборе), а ниточки к анкете нет ни одной. Для заметки это
	// рабочий случай, для реплики — нет: анонимных комментариев не бывает ни
	// здесь, ни там, значит спрашивают не про то.
	if ngsID == "" || body == "" || (authorID == 0 && kind != NGSNote) {
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
	// У анонимной записи автора нет, и спрашивать по нему нечего: ngs_id сам по
	// себе однозначен — это номер записи на сайте.
	seenSQL := `SELECT count(*) FROM ngs_outbox WHERE kind = $1 AND ngs_id = $2`
	seenArgs := []any{kind, ngsID}
	if authorID != 0 {
		seenSQL += ` AND author_id = $3`
		seenArgs = append(seenArgs, authorID)
	}
	if err := tx.QueryRow(ctx, seenSQL, seenArgs...).Scan(&seen); err != nil {
		return false, fmt.Errorf("опознание эха %s %s: %w", kind, ngsID, err)
	}
	if seen > 0 {
		return true, nil
	}

	var (
		rows pgx.Rows
		from = at.Add(-NGSEchoWindow)
	)
	switch {
	case kind == NGSNote && authorID == 0:
		// АНОНИМНАЯ ЗАМЕТКА: автора у записи нет, и невод остаётся о двух
		// нитях — текст да окно. Грубее обычного, и это названо ценой, а не
		// недосмотром: арифметический предохранитель цел (ngs_id проставляется
		// строке РОВНО ОДИН РАЗ), поэтому погасить эха больше, чем мы отправили,
		// нельзя ни при какой сверке текстов, а худшее, что выйдет, — один раз
		// принять за своё чужую анонимку с тем же началом внутри суток.
		rows, err = tx.Query(ctx, `
			SELECT o.id, n.body
			  FROM ngs_outbox o JOIN notes n ON n.id = o.object_id
			 WHERE o.kind = $1 AND n.anonymous
			   AND o.attempts > 0 AND o.ngs_id = ''
			   AND o.created_at > $2
			 ORDER BY o.created_at
			   FOR UPDATE OF o`, NGSNote, from)
	case kind == NGSNote:
		rows, err = tx.Query(ctx, `
			SELECT o.id, n.body
			  FROM ngs_outbox o JOIN notes n ON n.id = o.object_id
			 WHERE o.kind = $1 AND o.author_id = $2 AND NOT n.anonymous
			   AND o.attempts > 0 AND o.ngs_id = ''
			   AND o.created_at > $3
			 ORDER BY o.created_at
			   FOR UPDATE OF o`, NGSNote, authorID, from)
	case kind == NGSComment:
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

// NGSReplyTarget — куда на НГС ложится ответ и кого он называет.
//
// Спрашивается ПЕРЕД отправкой, а не при выдаче строки: родитель мог уехать на
// сайт тем же проходом очереди, и его номер появляется в середине прохода.
//
// ok == false означает «родителя на НГС нет вовсе». Такую реплику уносить не
// надо: она отвечает собеседнику, которого на той стороне не видно, и читается
// там бессмыслицей под именем живого человека. То же правило, по которому не
// уносится ответ под нативной заметкой, — нести некуда.
//
// Ноль и пустой ник при ok == true — ответ САМОЙ ЗАМЕТКЕ: у него адресата нет,
// и обращения быть не должно.
//
// Ник нужен потому, что сайт и площадка хранят адресата ПО-РАЗНОМУ: у нас это
// ребро, дорисовываемое на показе из текущего ника, а на НГС — префикс «Ник, »
// в САМОМ ТЕЛЕ. Не подставив его, мы отправляем туда реплику, про которую
// нельзя понять, кому она отвечает, даже когда ветка указана верно (bridge это
// знает с самого начала, а вынос — с 01.09.2026, оплачено боем).
func (p *Platform) NGSReplyTarget(ctx context.Context, commentID int64) (int64, string, bool, error) {
	var (
		replyTo, parent *int64
		nick, parentNGS string
	)
	err := p.pool.QueryRow(ctx, `
		SELECT c.reply_to_id, p.id, COALESCE(u.nick, ''), COALESCE(o.ngs_id, '')
		  FROM comments c
		  LEFT JOIN comments p ON p.id = c.reply_to_id
		  LEFT JOIN users u ON u.id = p.author_id
		  LEFT JOIN ngs_outbox o ON o.kind = $2 AND o.object_id = p.id
		 WHERE c.id = $1`, commentID, NGSComment).
		Scan(&replyTo, &parent, &nick, &parentNGS)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("адресат ответа %d: %w", commentID, err)
	}
	if replyTo == nil {
		return 0, "", true, nil // ответ заметке: адресата нет
	}
	if parent == nil {
		// Родителя снесли, пока строка ждала очереди.
		return 0, "", false, nil
	}
	if IsNGS(*parent) {
		return *parent, nick, true, nil
	}
	// Родитель нативный: он существует на НГС ровно в том случае, если сам туда
	// уехал и вернулся опознанным — тогда за ним закреплён номер сайта.
	id, err := strconv.ParseInt(parentNGS, 10, 64)
	if err != nil || id == 0 {
		return 0, "", false, nil
	}
	return id, nick, true, nil
}

// NGSSentObjects — что из показанного уже унесено на НГС (kind: note | comment).
//
// Понадобилось на СТРАНИЦЕ (решение владельца 02.09.2026: «если из Зазеркалья
// успешно улетел на НГС, то наверное должен тоже помечаться»): метка
// происхождения отвечала двумя состояниями — «пришло с НГС» и «написано здесь»,
// — а с открытием выноса появилось третье, и оно самое интересное автору:
// написано здесь И там теперь тоже стоит.
//
// Пачкой на страницу, как NoteThumbs и SynthOrigins, а не колонкой в запросе
// треда: тред идёт range-scan'ом по comments_tree, и LEFT JOIN к очереди платили
// бы ВСЕ девятьсот строк ради тех немногих, что вообще могли уехать. Вход —
// уникальный индекс ngs_outbox_object по паре (kind, object_id).
//
// Спрашиваются только НАТИВНЫЕ номера: зеркальной строки в очереди не бывает по
// построению — уносим мы своё, — и отбор здесь бережёт не запрос, а массив
// параметров: в зеркальном треде своих реплик единицы из сотен.
//
// «Унесено» — это state = sent ЛИБО непустой ngs_id, и второе условие не
// лишнее: сайт отвечает 500 и на ПРИНЯТУЮ реплику (замер 17.08.2026), поэтому у
// строки бывает failed при живой копии на НГС. Узнаём мы об этом ровно тогда,
// когда её номер удалось вычитать со страницы, — и тогда метка честна.
func (p *Platform) NGSSentObjects(ctx context.Context, kind string, ids []int64) (map[int64]bool, error) {
	native := make([]int64, 0, len(ids))
	for _, id := range ids {
		if IsNative(id) {
			native = append(native, id)
		}
	}
	if len(native) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT object_id FROM ngs_outbox
		 WHERE kind = $1 AND object_id = ANY($2) AND (state = $3 OR ngs_id <> '')`,
		kind, native, NGSSent)
	if err != nil {
		return nil, fmt.Errorf("унесённое на НГС (%s): %w", kind, err)
	}
	defer rows.Close()

	sent := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("унесённое на НГС (%s): %w", kind, err)
		}
		sent[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("унесённое на НГС (%s): %w", kind, err)
	}
	return sent, nil
}
