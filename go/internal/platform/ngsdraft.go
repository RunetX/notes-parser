package platform

// Заметка, которая публикуется НЕ ЗДЕСЬ.
//
// Решение владельца 02.09.2026: «если галка отправки стоит, то свою не создаём,
// отправляем на НГС, а с НГС забираем как обычно; если не стоит — публикуем
// только у нас».
//
// ЧЕМ ЭТО ЛУЧШЕ ПРЕЖНЕГО. До этого дня заметка человека с галочкой выходила
// ДВАЖДЫ — нативной строкой здесь и копией на сайте, — и обе половины
// приходилось разводить руками: эхо гасило вернувшуюся копию, чтобы она не
// встала в ленту второй раз. Платой был оборванный разговор: для зеркала
// заметки не существовало, тред её никто не опрашивал, и ответивший на НГС
// отвечал в пустоту. Теперь копия ОДНА, живёт на НГС, а сюда приезжает обычным
// зеркалом — со своим тредом, своими репликами и своей дорогой в каналы.
//
// Ничего нового ради этого не заведено: ровно так с первого дня работает
// /add_note у РюмкинЪа. Веб-форма просто перестала быть исключением.
//
// ЦЕНА НАЗВАНА: заметка появляется здесь не сразу — такт очереди выноса
// полминуты, обход ленты минуту, — и правки в авторском окне у неё не будет
// вовсе (зеркальную заметку править нельзя, ErrNotNative). Обе платы видны
// человеку на форме и на /me.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Состояния черновика. Третьего исхода нет намеренно: черновик обязан где-то
// опубликоваться. Не пустил сайт — публикуем здесь, и текст не пропадает;
// «отказ» как конечное состояние означал бы заметку, которую написали, а её
// нигде нет.
const (
	NGSDraftQueued = "queued"
	NGSDraftSent   = "sent"
	NGSDraftLocal  = "local"
)

// NGSDraftMaxAttempts — сколько раз стучимся на сайт, прежде чем опубликовать
// здесь. Три, как у ngs_outbox, и по той же причине: у НГС бывают короткие
// 5xx-штормы, а «сайт лежит час» и «нас не пускают» отсюда неотличимы.
const NGSDraftMaxAttempts = 3

// NGSDraft — заметка, ждущая отправки на сайт.
type NGSDraft struct {
	ID        int64
	AuthorID  int64
	Body      string
	Anonymous bool
	Attempts  int
}

// ngsDraftRecentQuery — потолок частоты для такого автора.
//
// Считать по одним лишь notes нельзя: его заметки приезжают сюда ЗЕРКАЛОМ, то
// есть в полосе НГС, а notesRecentQuery отсекает всё ниже NativeIDBase — и
// потолок у него не сработал бы ни разу. Поэтому складываем: нативные заметки
// (человек мог писать и до галочки, и после её снятия) плюс черновики.
const ngsDraftRecentQuery = `
	SELECT (SELECT count(*) FROM notes      WHERE author_id = $1 AND id >= $2 AND published_at > $3)
	     + (SELECT count(*) FROM ngs_drafts WHERE author_id = $1 AND created_at > $3)`

// QueueNGSNote принимает заметку, которая пойдёт на НГС вместо площадки.
//
// Проверки те же и в том же порядке, что у CreateNote: право писать по
// действующей редакции согласия и потолок частоты. Стоят они ЗДЕСЬ, а не в
// морде, по тому же доводу — писать можно не только из формы, а второй список
// правил однажды разошёлся бы с этим.
//
// Песочница, двойник и картинка сюда не идут: песочницу ведут жители и она
// наружу не выходит вовсе, а иллюстрацию наш клиент сайта отправить не умеет —
// love.Client.PostNote шлёт один текст. Заметка с картинкой поэтому остаётся
// НАТИВНОЙ (решение названо в web/write.go), и молча ронять файл мы не станем.
func (p *Platform) QueueNGSNote(ctx context.Context, in NewNote) (int64, error) {
	body, err := cleanBody(in.Body)
	if err != nil {
		return 0, err
	}
	if in.AuthorID == 0 {
		return 0, errors.New("заметка на НГС без автора")
	}
	if in.Stage || in.SynthOf != 0 || in.Image != nil {
		return 0, errors.New("на НГС уносится только обычная заметка без картинки")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("заметка на НГС: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := publishGuard(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	if err := enforceRate(ctx, tx, ngsDraftRecentQuery, in.AuthorID, time.Now(), noteRates); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO ngs_drafts (author_id, body, anonymous)
		VALUES ($1, $2, $3) RETURNING id`,
		in.AuthorID, body, in.Anonymous).Scan(&id); err != nil {
		return 0, fmt.Errorf("заметка на НГС: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("заметка на НГС: %w", err)
	}
	return id, nil
}

// NextNGSDrafts выдаёт голову очереди и СРАЗУ засчитывает попытку — то же
// правило, что у ngs_outbox: сайт отвечает 500 и на принятую заметку, значит
// «отправляется» неотличимо от «отправлено», и считать по исходу означало бы
// однажды поставить дубль.
func (p *Platform) NextNGSDrafts(ctx context.Context, limit int) ([]NGSDraft, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := p.pool.Query(ctx, `
		WITH taken AS (
		    SELECT id FROM ngs_drafts
		     WHERE state = $1
		     ORDER BY created_at, id
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE ngs_drafts d SET attempts = d.attempts + 1
		  FROM taken WHERE d.id = taken.id
		RETURNING d.id, d.author_id, d.body, d.anonymous, d.attempts`,
		NGSDraftQueued, limit)
	if err != nil {
		return nil, fmt.Errorf("очередь заметок на НГС: %w", err)
	}
	defer rows.Close()

	var out []NGSDraft
	for rows.Next() {
		var d NGSDraft
		if err := rows.Scan(&d.ID, &d.AuthorID, &d.Body, &d.Anonymous, &d.Attempts); err != nil {
			return nil, fmt.Errorf("очередь заметок на НГС: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("очередь заметок на НГС: %w", err)
	}
	return out, nil
}

// SentNGSDraft — заметка ушла на сайт. Больше о ней здесь не знают ничего:
// номер там нам не нужен, заметку принесёт зеркало обычным обходом ленты.
func (p *Platform) SentNGSDraft(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE ngs_drafts SET state = $2, sent_at = now(), last_error = ''
		 WHERE id = $1`, id, NGSDraftSent)
	if err != nil {
		return fmt.Errorf("исход заметки на НГС %d: %w", id, err)
	}
	return nil
}

// RetryNGSDraft — попытка не удалась, но они ещё есть.
func (p *Platform) RetryNGSDraft(ctx context.Context, id int64, cause error) error {
	_, err := p.pool.Exec(ctx, `
		UPDATE ngs_drafts SET last_error = $2 WHERE id = $1`, id, cause.Error())
	if err != nil {
		return fmt.Errorf("исход заметки на НГС %d: %w", id, err)
	}
	return nil
}

// PublishNGSDraftHere — сайт не взял, публикуем здесь. Текст человека не
// пропадает ни при каком отказе, и это главное свойство всей затеи.
//
// Заметка заводится ОБЫЧНЫМ путём (CreateNote), а не INSERT'ом мимо него:
// очередь модерации, событие шины и права должны достаться ей наравне со всеми.
// KeepHere при этом гасит вынос: три попытки уже потрачены, и заводить рядом
// вторую очередь на ту же заметку значит однажды опубликовать её дважды.
//
// Потолок частоты здесь НЕ считается второй раз: он уже сработал, когда
// черновик принимали, — а отказ на этом месте оставил бы человека вовсе без
// заметки, наказав его за то, что чужой сайт лежал.
func (p *Platform) PublishNGSDraftHere(ctx context.Context, id int64) (int64, error) {
	var d NGSDraft
	err := p.pool.QueryRow(ctx, `
		SELECT author_id, body, anonymous FROM ngs_drafts
		 WHERE id = $1 AND state = $2`, id, NGSDraftQueued).Scan(&d.AuthorID, &d.Body, &d.Anonymous)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("черновик %d: %w", id, err)
	}
	noteID, err := p.CreateNote(ctx, NewNote{
		AuthorID: d.AuthorID, Anonymous: d.Anonymous, Body: d.Body, KeepHere: true,
	})
	if err != nil {
		return 0, err
	}
	if _, err := p.pool.Exec(ctx, `
		UPDATE ngs_drafts SET state = $2, note_id = $3, sent_at = now()
		 WHERE id = $1`, id, NGSDraftLocal, noteID); err != nil {
		// Заметка уже опубликована, и это важнее строки очереди: скажем об
		// ошибке наружу, но чинить нечего — повтор возьмёт черновик снова и
		// заведёт вторую заметку, а этого допускать нельзя.
		return noteID, fmt.Errorf("отметка черновика %d: %w", id, err)
	}
	return noteID, nil
}

// NGSDraftsPending — сколько заметок человека ещё в пути.
//
// Спрашивает страница «Моя страница»: заметки нет ни в ленте, ни у автора, и
// без этой строки полторы минуты выглядят как пропажа текста.
func (p *Platform) NGSDraftsPending(ctx context.Context, userID int64) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, `
		SELECT count(*) FROM ngs_drafts WHERE author_id = $1 AND state = $2`,
		userID, NGSDraftQueued).Scan(&n); err != nil {
		return 0, fmt.Errorf("заметки в пути у %d: %w", userID, err)
	}
	return n, nil
}
