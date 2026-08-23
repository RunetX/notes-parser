package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// commentViewColumns — комментарии, замаскированные там же, где читаются.
//
// Последние четыре поля — адресат: соединение с самим собой по reply_to_id даёт
// ТЕКУЩИЙ ник того, кому отвечали. Именно поэтому префикс «Ник, » не хранится в
// теле: переименование и обезличивание меняют подпись сразу везде, включая чужие
// ответы, а обезличить человека, размазанного по чужим телам, было бы нечем.
const commentViewColumns = `
	c.id, c.note_id, c.anonymous, c.body, c.path, c.depth, c.status, c.published_at, c.edited_at,
	CASE WHEN c.anonymous THEN NULL ELSE c.author_id      END,
	CASE WHEN c.anonymous THEN NULL ELSE u.nick           END,
	CASE WHEN c.anonymous THEN NULL ELSE u.avatar_sha     END,
	CASE WHEN c.anonymous THEN NULL ELSE m.mime           END,
	CASE WHEN c.anonymous THEN NULL ELSE c.author_display END,
	CASE WHEN c.anonymous THEN 0     ELSE coalesce(u.gender, 0) END,
	CASE WHEN c.anonymous THEN false ELSE coalesce(u.kind, 0) = 0 END,
	coalesce(c.author_id = $1, false),
	rc.id, rc.anonymous,
	CASE WHEN rc.anonymous THEN NULL
	     ELSE coalesce(nullif(ru.nick, ''), nullif(rc.author_display, '')) END`

const commentViewFrom = `
	FROM comments c
	LEFT JOIN users    u  ON u.id  = c.author_id
	LEFT JOIN media    m  ON m.sha256 = u.avatar_sha
	LEFT JOIN comments rc ON rc.id = c.reply_to_id
	LEFT JOIN users    ru ON ru.id = rc.author_id`

func scanCommentView(row pgx.Row) (CommentView, error) {
	var (
		c         CommentView
		depth     int16
		author    *int64
		nick      *string
		sha       []byte
		mime      *string
		display   *string
		gender    Gender
		shadow    bool
		replyID   *int64
		replyAnon *bool
		replyNick *string
	)
	err := row.Scan(&c.ID, &c.NoteID, &c.Anonymous, &c.Body, &c.Path, &depth, &c.Status,
		&c.PublishedAt, &c.EditedAt,
		&author, &nick, &sha, &mime, &display, &gender, &shadow, &c.Own,
		&replyID, &replyAnon, &replyNick)
	if err != nil {
		return CommentView{}, err
	}
	c.Depth = int(depth)
	c.Author = Author{
		ID: idOf(author), Nick: strOf(nick),
		AvatarURL: MediaURL(sha, strOf(mime)), Gender: gender, Shadow: shadow,
	}
	c.Display = strOf(display)
	// Адресат рисуется, только если строка адресата ещё существует: снесённого
	// модерацией родителя подписать нечем, и ветка просто теряет обращение.
	if replyID != nil {
		c.ReplyTo = &ReplyRef{CommentID: *replyID, Nick: strOf(replyNick)}
		if replyAnon != nil {
			c.ReplyTo.Anonymous = *replyAnon
		}
	}
	return c, nil
}

// threadQuery и flatQuery вынесены в константы по той же причине, что и
// feedQuery: тест спрашивает у Postgres план ровно этого SQL. Древовидный вид
// обязан идти по comments_tree, плоский — по comments_flat; молчаливый переезд
// любого из них на сортировку в памяти проваливает всю затею с путями.
//
// COLLATE "C" в условии и в порядке написан явно и обязан совпадать с индексом:
// без него сравнение пойдёт по локали базы, где «.» и цифры упорядочены иначе,
// а индекс перестанет подходить — тихо, с правильным ответом и полным перебором.
const threadQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.status = 0
	 ORDER BY c.path COLLATE "C"
	 LIMIT $3`

const flatQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.status = 0
	 ORDER BY c.published_at DESC, c.id DESC
	 LIMIT $3 OFFSET $4`

// Те же два вида, но глазами МОДЕРАТОРА: к видимому добавляется скрытое
// модерацией (статус 2). Отдельными константами, а не параметром `OR $n`, по
// двум причинам сразу. Первая техническая: условие с OR планировщик не сводит к
// индексу, и лента с частичным индексом notes_feed уехала бы на полный перебор
// — то есть горячий путь чтения платил бы за редкую роль. Вторая по существу:
// скрытое АВТОРОМ (отзыв согласия, статус 1) и обезличенное (3) модератору не
// показываются вовсе — это исполнение права субъекта, а не спрятанный текст,
// который он вправе пересмотреть.
const threadModQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.status IN (0, 2)
	 ORDER BY c.path COLLATE "C"
	 LIMIT $3`

const flatModQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.status IN (0, 2)
	 ORDER BY c.published_at DESC, c.id DESC
	 LIMIT $3 OFFSET $4`

// commentsSinceQuery — добор реплик, появившихся ПОСЛЕ того, как страница была
// нарисована. Это живой канал (web/fresh.go), и запрос у него свой, а не выборка
// из дерева: тред отдаётся целиком до 5000 строк, и перечитывать его на каждую
// новую реплику у каждого открытого окна — верный способ отнять ядро у зеркала.
//
// Порядок ПО ВОЗРАСТАНИЮ id, в отличие от линейного вида: страница дописывается
// в том порядке, в каком реплики появлялись, иначе три пришедшие разом встали бы
// в тред задом наперёд. Индекс тот же, comments_flat (note_id, id).
//
// Границ ЗДЕСЬ ДВЕ, по одной на полосу идентификаторов, и это не педантизм.
// Тред у нас смешанный по устройству: своё и зеркальное живут в одном разговоре,
// а нативный id больше любого ngs'ного — то есть одна общая граница после первой
// же реплики, написанной ЗДЕСЬ, уезжает в нативную полосу, и ни один пришедший
// следом комментарий НГС в неё уже не попадает. На боевой заметке 313056 это
// выглядело так: в мессенджере реплики идут, на странице их нет до обновления.
// У ленты такой ошибки не было: там граница с самого начала пара «время, id».
//
// UNION ALL, а не OR по двум диапазонам, по той же причине, по какой у вида
// модератора отдельная константа: условие с OR планировщик не сводит к индексу и
// уводит добор в перебор всех реплик заметки — на каждый такт каждого открытого
// окна. Здесь же обе ветки — обычный range-scan по comments_flat.
//
// ORDER BY по НОМЕРУ колонки: после UNION имена столбцов не годятся — `id` тут
// два, свой и адресата.
var (
	commentsSinceQuery    = commentsSinceSQL("= 0")
	commentsSinceModQuery = commentsSinceSQL("IN (0, 2)")
)

// commentsSinceSQL собирает добор для одного набора статусов. Числа полос берутся
// из тех же констант, что и IsNGS/IsNative: написанные здесь руками, они однажды
// разошлись бы с ними молча.
//
// Восстановленное (полоса с 2e11) добор не носит вовсе, и границы у него нет:
// эпоха 2010 года приезжает разовой командой в пустые треды, «появиться» на
// открытой странице она не может по устройству.
func commentsSinceSQL(status string) string {
	band := func(after string, top int64) string {
		return `(SELECT ` + commentViewColumns + commentViewFrom + `
		 WHERE c.note_id = $2 AND c.status ` + status + `
		   AND c.id > ` + after + ` AND c.id < ` + strconv.FormatInt(top, 10) + `
		 ORDER BY c.id
		 LIMIT $5)`
	}
	return band("$3", NativeIDBase) + `
UNION ALL
` + band("$4", RestoredIDBase) + `
 ORDER BY 1 LIMIT $5`
}

// commentQuery — ОДНА реплика заметки, со всем, что нужно её показу. Спрашивает
// её форма ответа, открывающаяся без перезагрузки (web/replyform.go): страница
// приходит за готовой строкой формы, а строке нужен адресат — ник (текущий, а не
// снимок), тень он или участник и на какой глубине стоит.
//
// Идёт по comments_flat (note_id, id), как и добор: заметка в условии не для
// красоты — без неё запрос ходил бы по первичному ключу через всю таблицу на
// 10,7 млн строк, а заодно позволял бы попросить чужую реплику под видом своей
// заметки.
const commentQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.id = $3 AND c.status = 0`

const commentModQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.id = $3 AND c.status IN (0, 2)`

// CommentViewByID — одна реплика заметки глазами читателя. ErrNotFound значит
// «её здесь нет»: снесена, скрыта или принадлежит другой заметке — для показа
// это один и тот же ответ.
func (p *Platform) CommentViewByID(ctx context.Context, v Viewer, noteID, id int64) (CommentView, error) {
	q := commentQuery
	if v.CanModerate() {
		q = commentModQuery
	}
	c, err := scanCommentView(p.pool.QueryRow(ctx, q, v.UserID, noteID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return CommentView{}, fmt.Errorf("комментарий %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return CommentView{}, fmt.Errorf("чтение комментария %d: %w", id, err)
	}
	return c, nil
}

// FreshLimit — сколько реплик отдаётся за один добор. Не постраничка: если за
// такт пришло больше, следующий запрос донесёт хвост, а курсор сдвинется сам.
const FreshLimit = 50

// FreshAfter — граница живого добора: докуда тред на странице уже нарисован.
//
// Число на ПОЛОСУ, а не одно на всех. Полосы упорядочены между собой по
// происхождению, а не по времени: реплика, написанная здесь минуту назад, имеет
// номер больше любой ngs'ной, включая ту, что придёт завтра. Общая граница
// поэтому означала бы «показывать только ту полосу, в которой оказался
// максимум», и смешанный тред терял бы половину разговора.
type FreshAfter struct {
	NGS    int64 // последняя показанная реплика НГС
	Native int64 // последняя показанная реплика, написанная здесь
	Moved  MovedAfter
}

// MovedAfter — граница по ПЕРЕЕЗДАМ: докуда странице известны правки строк,
// которые она уже нарисовала.
//
// Полосы отвечают на вопрос «что появилось», и на «что переехало» ответить не
// могут: у переехавшей реплики id прежний, и мимо границы по id она проходит
// молча. А переезжают они постоянно — зеркало знает адресата лишь по обращению
// «Ник, …» и угадывает его примерно с половинной точностью, настоящее ребро
// приносит обход мобильной версии (см. миграцию 0015).
//
// Пара «время, id», а не одно время: одна транзакция обхода штампует все свои
// строки одним now(), и порция, обрезанная потолком, обязана продолжиться с
// середины этой отметки — иначе хвост переезда не приедет никогда.
//
// Нулевое значение значит «переездов не носим»: так выглядит граница со
// страницы, открытой до выкатки этой правки. Заметка, где не переезжало ничего,
// получает не ноль, а время запроса, — иначе ПЕРВЫЙ переезд в ней прошёл бы
// мимо открытых окон.
type MovedAfter struct {
	At time.Time
	ID int64
}

// On — носит ли эта граница переезды.
func (m MovedAfter) On() bool { return !m.At.IsZero() }

// Seen двигает границу: реплика с этим id на странице уже стоит.
//
// Восстановленное (2010 год) не двигает ничего: своей границы у него нет, потому
// что появиться на открытой странице оно не может — см. commentsSinceSQL.
func (f *FreshAfter) Seen(id int64) {
	switch {
	case IsNGS(id) && id > f.NGS:
		f.NGS = id
	case IsNative(id) && id > f.Native:
		f.Native = id
	}
}

// bounds — границы для запроса. Нижний край нативной полосы поднимается ЗДЕСЬ, а
// не в SQL: пустая граница значит «своих реплик страница ещё не показывала», а не
// «неси всё с нуля», — иначе нативная ветка добора забрала бы себе и реплики НГС,
// и каждая приехала бы дважды. Вторым условием в ветке это не решается: у
// range-scan нижний край один, и лишнее сравнение стало бы фильтром после чтения.
func (f FreshAfter) bounds() (ngs, native int64) {
	ngs, native = f.NGS, f.Native
	if ngs < 0 {
		ngs = 0
	}
	if native < NativeIDBase {
		native = NativeIDBase - 1
	}
	return ngs, native
}

// ThreadFreshAfter — граница живого добора для только что нарисованной страницы
// заметки: самая свежая реплика В КАЖДОЙ полосе.
//
// Спрашивается ОТДЕЛЬНЫМ запросом, а не считается по показанным строкам, и
// причина в линейном виде: там на странице ОКНО из тридцати самых свежих реплик.
// Полосы в окно попадают не обе — тред, где сегодня отвечали только с НГС,
// покажет тридцать ngs'ных строк и ни одной своей, — а «максимум показанного» в
// отсутствующей полосе есть ноль, то есть «неси с начала». Первый же сигнал
// притащил бы наверх страницы САМЫЕ СТАРЫЕ реплики этой полосы. У дерева такого
// расхождения нет (оно показано целиком), но дорога здесь одна на оба вида:
// второй способ вычислить границу — это второе место, где о полосах забудут.
//
// Спрашивать её надо ДО чтения реплик: реплика, пришедшая между двумя запросами,
// при таком порядке попадёт и на страницу, и в добор — а повтор гасится на
// странице по id. Обратный порядок терял бы её совсем.
//
// Оба максимума — обычный проход с конца по comments_flat, по одной строке на
// полосу.
//
// Третьим и четвёртым идёт граница переездов — та же пара, что у MovedAfter, и
// берётся она с конца comments_moved. Пусто (в заметке не переезжало ничего) —
// это НЕ ноль, а время запроса: нулевая граница означала бы «переездов не
// носим», и первая же правка дерева прошла бы мимо страницы. Границу спрашивают
// ДО чтения реплик, поэтому переезд, случившийся между двумя запросами, попадёт
// и на страницу, и в добор — а повтор гасится на странице по id.
const freshAfterQuery = `
	SELECT coalesce((SELECT max(id) FROM comments
	                  WHERE note_id = $1 AND id < $2), 0),
	       coalesce((SELECT max(id) FROM comments
	                  WHERE note_id = $1 AND id >= $2 AND id < $3), 0),
	       coalesce((SELECT moved_at FROM comments
	                  WHERE note_id = $1 AND moved_at IS NOT NULL
	                  ORDER BY moved_at DESC, id DESC LIMIT 1), now()),
	       coalesce((SELECT id FROM comments
	                  WHERE note_id = $1 AND moved_at IS NOT NULL
	                  ORDER BY moved_at DESC, id DESC LIMIT 1), 0)`

func (p *Platform) ThreadFreshAfter(ctx context.Context, noteID int64) (FreshAfter, error) {
	var f FreshAfter
	err := p.pool.QueryRow(ctx, freshAfterQuery, noteID, NativeIDBase, RestoredIDBase).
		Scan(&f.NGS, &f.Native, &f.Moved.At, &f.Moved.ID)
	return f, wrapf(err, "граница добора заметки %d", noteID)
}

// commentsMovedQuery — ЧТО переехало: одни id с отметками, по возрастанию пары.
//
// Отдельный запрос, а не третья ветка добора, и причина в потолке порции.
// Порядок у переездов свой (по отметке, а не по id), а общий LIMIT режет
// склеенную выборку по ЧУЖОМУ порядку — то есть курсор было бы некуда двигать,
// и хвост переезда пропадал бы молча.
//
// Строк он не собирает вовсе: в обычный такт переездов нет, и платить за пять
// соединений ради пустого ответа незачем. Разметку добирает второй запрос —
// только когда есть что добирать.
//
// Статус здесь НЕ спрашивается намеренно: граница обязана переступить и через
// скрытую строку, иначе добор упёрся бы в неё навсегда. Разметки скрытой не
// дадут ниже — там статус на месте.
const commentsMovedQuery = `
	SELECT id, moved_at FROM comments
	 WHERE note_id = $1 AND moved_at IS NOT NULL AND (moved_at, id) > ($2, $3)
	 ORDER BY moved_at, id
	 LIMIT $4`

// Строки переехавших глазами читателя. Порядок задаёт не этот запрос, а первый:
// страница обязана получить родителя раньше ребёнка, иначе ребёнок встанет по
// старому месту родителя.
const commentsMovedViewQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.id = ANY($3) AND c.status = 0`

const commentsMovedViewModQuery = `
	SELECT ` + commentViewColumns + commentViewFrom + `
	 WHERE c.note_id = $2 AND c.id = ANY($3) AND c.status IN (0, 2)`

// CommentsMoved — реплики, переехавшие после границы, и новая граница.
//
// Возвращает их В ТОМ ЖЕ ВИДЕ, что и добор новых, потому что переезд меняет не
// только место: вместе с ребром меняются глубина, подпись адресата («Ник, »
// рисуется из ребра) и иногда тело — приём срезает обращение ровно тогда, когда
// ребро появилось. Двигать строку на странице, не перерисовав её, значило бы
// оставить на ней прежнего адресата.
//
// Граница возвращается ОТДЕЛЬНО, а не вычисляется вызывающим: отметка переезда
// не часть показа, и класть её в CommentView значило бы носить служебное поле
// по всем страницам площадки.
func (p *Platform) CommentsMoved(ctx context.Context, v Viewer, noteID int64, after MovedAfter, limit int) ([]CommentView, MovedAfter, error) {
	if !after.On() {
		return nil, after, nil
	}
	limit = clampLimit(limit)
	rows, err := p.pool.Query(ctx, commentsMovedQuery, noteID, after.At, after.ID, limit)
	if err != nil {
		return nil, after, fmt.Errorf("переезды заметки %d: %w", noteID, err)
	}
	next := after
	ids := make([]int64, 0, 16)
	order := make(map[int64]int, 16)
	for rows.Next() {
		var m MovedAfter
		if err := rows.Scan(&m.ID, &m.At); err != nil {
			rows.Close()
			return nil, after, fmt.Errorf("переезды заметки %d: %w", noteID, err)
		}
		order[m.ID] = len(ids)
		ids = append(ids, m.ID)
		next = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, after, fmt.Errorf("переезды заметки %d: %w", noteID, err)
	}
	if len(ids) == 0 {
		return nil, next, nil
	}

	q := commentsMovedViewQuery
	if v.CanModerate() {
		q = commentsMovedViewModQuery
	}
	vrows, err := p.pool.Query(ctx, q, v.UserID, noteID, ids)
	if err != nil {
		return nil, next, fmt.Errorf("строки переездов заметки %d: %w", noteID, err)
	}
	out, err := collectComments(vrows, len(ids))
	if err != nil {
		return nil, next, fmt.Errorf("строки переездов заметки %d: %w", noteID, err)
	}
	// Порядок первого запроса: скрытые строки из него выпали, и сортировка идёт
	// по их прежним местам — родитель всё равно остаётся раньше ребёнка.
	sort.Slice(out, func(i, j int) bool { return order[out[i].ID] < order[out[j].ID] })
	return out, next, nil
}

// CommentsSince — реплики заметки новее границы. Виды здесь нет вовсе: и
// дерево, и линейный добираются одним запросом, а куда встанет строка, решает
// уже показ (у дерева — по ребру ответа, у линейного — сверху).
func (p *Platform) CommentsSince(ctx context.Context, v Viewer, noteID int64, after FreshAfter, limit int) ([]CommentView, error) {
	limit = clampLimit(limit)
	q := commentsSinceQuery
	if v.CanModerate() {
		q = commentsSinceModQuery
	}
	ngs, native := after.bounds()
	rows, err := p.pool.Query(ctx, q, v.UserID, noteID, ngs, native, limit)
	if err != nil {
		return nil, fmt.Errorf("новые реплики заметки %d: %w", noteID, err)
	}
	out, err := collectComments(rows, limit)
	if err != nil {
		return nil, fmt.Errorf("новые реплики заметки %d: %w", noteID, err)
	}
	return out, nil
}

// MaxThreadRows — потолок строк древовидного вида. Не постраничка, а
// предохранитель: дерево показывается ЦЕЛИКОМ, как на НГС, — ветка, обрезанная
// на середине, перестаёт быть деревом, и «дальше» в ней означало бы «продолжите
// разговор на следующей странице». Самый длинный тред зеркала — 891 реплика,
// так что потолок в 5000 отделяет нас от аварии, а не от людей.
const MaxThreadRows = 5000

// Thread — древовидный вид: ВСЕ комментарии заметки одним range-scan по
// (note_id, path). Сортировки в памяти нет — порядок даёт сам индекс, потому что
// путь устроен так, что побайтовое сравнение и есть обход дерева.
func (p *Platform) Thread(ctx context.Context, v Viewer, noteID int64) ([]CommentView, error) {
	q := threadQuery
	if v.CanModerate() {
		q = threadModQuery
	}
	rows, err := p.pool.Query(ctx, q, v.UserID, noteID, MaxThreadRows)
	if err != nil {
		return nil, fmt.Errorf("тред заметки %d: %w", noteID, err)
	}
	out, err := collectComments(rows, 256)
	if err != nil {
		return nil, fmt.Errorf("тред заметки %d: %w", noteID, err)
	}
	return out, nil
}

// Flat — линейный вид: страница комментариев от НОВЫХ к старым.
//
// Порядок именно такой, как на НГС, и это не мелочь: линейный вид там читают
// как ленту свежих реплик — открыл заметку, увидел последнее. Восходящий
// порядок заставлял бы листать в конец, чтобы узнать, чем всё кончилось.
// Отдельный индекс, а не сортировка выборки дерева: переключатель «дерево /
// линейный» на сайте живой, им пользуются.
//
// «Новые» считаются ПО ВРЕМЕНИ (индекс comments_flat_time, миграция 0014), а не
// по убыванию id, как было до 23.08.2026. Порядок по id верен внутри одной
// полосы идентификаторов и неверен между ними: нативная реплика позапрошлой
// недели имеет номер больше пришедшей с НГС сегодня, и всё написанное здесь
// вставало наверх страницы независимо от того, когда это было сказано.
func (p *Platform) Flat(ctx context.Context, v Viewer, noteID int64, offset, limit int) ([]CommentView, error) {
	limit = clampLimit(limit)
	if offset < 0 {
		offset = 0
	}
	q := flatQuery
	if v.CanModerate() {
		q = flatModQuery
	}
	rows, err := p.pool.Query(ctx, q, v.UserID, noteID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("комментарии заметки %d: %w", noteID, err)
	}
	out, err := collectComments(rows, limit)
	if err != nil {
		return nil, fmt.Errorf("комментарии заметки %d: %w", noteID, err)
	}
	return out, nil
}

func collectComments(rows pgx.Rows, limit int) ([]CommentView, error) {
	defer rows.Close()
	out := make([]CommentView, 0, limit)
	for rows.Next() {
		c, err := scanCommentView(rows)
		if err != nil {
			return nil, fmt.Errorf("разбор комментария: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CommentRow — сырая строка комментария, с настоящим автором анонимки. Для служб.
func (p *Platform) CommentRow(ctx context.Context, id int64) (Comment, error) {
	var (
		c          Comment
		author     *int64
		branchRoot *int64
		replyTo    *int64
	)
	err := p.pool.QueryRow(ctx, `
		SELECT id, note_id, author_id, author_display, anonymous, body,
		       branch_root_id, reply_to_id, reply_source, path, depth, status,
		       published_at, edited_at, created_at
		  FROM comments WHERE id = $1`, id).
		Scan(&c.ID, &c.NoteID, &author, &c.AuthorDisplay, &c.Anonymous, &c.Body,
			&branchRoot, &replyTo, &c.ReplySource, &c.Path, &c.Depth, &c.Status,
			&c.PublishedAt, &c.EditedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Comment{}, fmt.Errorf("комментарий %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Comment{}, fmt.Errorf("чтение комментария %d: %w", id, err)
	}
	c.AuthorID, c.BranchRootID, c.ReplyToID = idOf(author), idOf(branchRoot), idOf(replyTo)
	return c, nil
}

// CommentTally — сколько зеркальных комментариев у заметки и какой у них
// максимальный id.
type CommentTally struct {
	Count int
	MaxID int64
}

// MirroredCommentTallies — счётчики по всем заметкам разом. Сверке этого хватает,
// чтобы найти расхождения, не читая ни одного комментария: пара «сколько и до
// какого id» ловит и обычный догон (пришли новые), и дотянутый задним числом
// старый тред (`pull -full`), где max не сдвинулся, а count вырос.
func (p *Platform) MirroredCommentTallies(ctx context.Context) (map[int64]CommentTally, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT note_id, count(*), max(id) FROM comments WHERE id < $1 GROUP BY note_id`, NativeIDBase)
	if err != nil {
		return nil, fmt.Errorf("счётчики комментариев: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]CommentTally)
	for rows.Next() {
		var (
			id int64
			t  CommentTally
		)
		if err := rows.Scan(&id, &t.Count, &t.MaxID); err != nil {
			return nil, fmt.Errorf("счётчики комментариев: %w", err)
		}
		out[id] = t
	}
	return out, rows.Err()
}

// CommentIDs — id зеркальных комментариев заметки. Сверка спрашивает их у той
// заметки, где счётчики разошлись, и досылает недостающие.
func (p *Platform) CommentIDs(ctx context.Context, noteID int64) (map[int64]bool, error) {
	return p.idSet(ctx, fmt.Sprintf("комментарии заметки %d", noteID),
		`SELECT id FROM comments WHERE note_id = $1 AND id < $2`, noteID, NativeIDBase)
}

// IngestComment принимает комментарий с НГС. Идемпотентен по id.
//
// Автор без анкеты (Author.ID == 0) сохраняется снимком ника в author_display —
// единственное отступление от правила «истории ников нет»: показать такого
// комментатора больше нечем.
func (p *Platform) IngestComment(ctx context.Context, in MirroredComment) (bool, error) {
	if !IsNGS(in.ID) {
		return false, fmt.Errorf("комментарий НГС ожидается с id из полосы сайта, получен %d", in.ID)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("приём комментария %d: %w", in.ID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	author, err := ensureShadow(ctx, tx, in.Author)
	if err != nil {
		return false, err
	}
	display := ""
	if author == 0 {
		display = in.Author.Nick
	}
	path, branchRoot, err := placeComment(ctx, tx, in.NoteID, in.ReplyToID, in.ID)
	if err != nil {
		return false, fmt.Errorf("приём комментария %d: %w", in.ID, err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO comments (id, note_id, author_id, author_display, body,
		                      branch_root_id, reply_to_id, reply_source, path, depth, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO NOTHING`,
		in.ID, in.NoteID, nullID(author), display, in.Body,
		nullID(branchRoot), nullID(in.ReplyToID), in.ReplySource, path, PathDepth(path), in.PublishedAt)
	if err != nil {
		return false, fmt.Errorf("приём комментария %d: %w", in.ID, err)
	}
	inserted := tag.RowsAffected() > 0
	if inserted {
		if err := bumpNote(ctx, tx, in.NoteID, in.PublishedAt); err != nil {
			return false, err
		}
		// Реплика с НГС — такой же повод, как своя: человеку, которому ответили,
		// всё равно, с какой стороны пришёл ответ. Но только СВЕЖАЯ (см.
		// EventHorizon): этой же дорогой идут догоняющая сверка и перенос всего
		// зеркала на пустую площадку, а рассылать поводы по репликам 2014 года
		// значит завалить ими всех разом.
		if worthTelling(in.PublishedAt) {
			if err := recordEvent(ctx, tx, newEvent{
				Kind: EventComment, ActorID: author, NoteID: in.NoteID, CommentID: in.ID,
			}); err != nil {
				return false, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("приём комментария %d: %w", in.ID, err)
	}
	return inserted, nil
}

// CreateComment публикует нативный комментарий и возвращает его id.
//
// Заметка при этом может быть любой — и нашей, и зеркальной: своё и пришедшее с
// НГС живут в ОДНОМ треде, и это не компромисс, а смысл всей затеи. Своих
// заметок у площадки на старте ноль, а 285 зеркальных с их 61 177 репликами —
// весь материал, под которым сегодня можно разговаривать.
//
// Писать запрещает только НАШ замок (locked). Чужая отметка НГС «не актуальна»
// (comments_closed) остаётся надписью: она стоит у 62 % заметок зеркала, а
// ставит её сайт через минуты после публикации, пока реплики ещё идут.
func (p *Platform) CreateComment(ctx context.Context, in NewComment) (int64, error) {
	body, err := cleanBody(in.Body)
	if err != nil {
		return 0, err
	}
	if in.AuthorID == 0 {
		return 0, errors.New("нативный комментарий без автора")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("публикация комментария: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	if err := publishGuard(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	now := time.Now()
	if err := enforceRate(ctx, tx, commentsRecentQuery, in.AuthorID, now, commentRates); err != nil {
		return 0, err
	}

	var (
		locked bool
		status Status
	)
	err = tx.QueryRow(ctx, `SELECT locked, status FROM notes WHERE id = $1`, in.NoteID).
		Scan(&locked, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("заметка %d: %w", in.NoteID, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("публикация комментария к заметке %d: %w", in.NoteID, err)
	}
	if status != StatusVisible {
		// Скрытая заметка для пишущего просто отсутствует: рассказывать, что она
		// есть, но спрятана, — значит показывать работу модерации посторонним.
		return 0, fmt.Errorf("заметка %d: %w", in.NoteID, ErrNotFound)
	}
	if locked {
		return 0, ErrThreadLocked
	}

	var id int64
	if err := tx.QueryRow(ctx, `SELECT nextval('comments_native_seq')`).Scan(&id); err != nil {
		return 0, fmt.Errorf("выдача id комментария: %w", err)
	}
	path, branchRoot, err := placeComment(ctx, tx, in.NoteID, in.ReplyToID, id)
	if err != nil {
		return 0, fmt.Errorf("публикация комментария: %w", err)
	}
	source := ReplyNone
	if in.ReplyToID != 0 {
		source = ReplyNative // отвечали у нас: адресат известен точно, а не угадан
	}
	// anonymous не задаётся вовсе: комментарий на площадке подписан всегда
	// (см. NewComment). Колонка остаётся ради зеркала — на НГС анонимные
	// комментарии в старых тредах встречаются.
	if _, err := tx.Exec(ctx, `
		INSERT INTO comments (id, note_id, author_id, body,
		                      branch_root_id, reply_to_id, reply_source, path, depth, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, in.NoteID, in.AuthorID, body,
		nullID(branchRoot), nullID(in.ReplyToID), source, path, PathDepth(path), now); err != nil {
		return 0, fmt.Errorf("публикация комментария: %w", err)
	}
	if err := bumpNote(ctx, tx, in.NoteID, now); err != nil {
		return 0, err
	}
	if err := enqueueCheck(ctx, tx, SubjectComment, id, in.NoteID, in.AuthorID); err != nil {
		return 0, err
	}
	// Факт — той же транзакцией, что и сама реплика: «ответили, а повода нет»
	// это то же состояние, что и «опубликовано, но в очередь не попало». Кому
	// это повод, решается уже потом и фоном (см. events.go).
	if err := recordEvent(ctx, tx, newEvent{
		Kind: EventComment, ActorID: in.AuthorID, NoteID: in.NoteID, CommentID: id,
	}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("публикация комментария: %w", err)
	}
	return id, nil
}

// placeComment считает место комментария в дереве: путь и корень ветки.
//
// Отсутствующий адресат — рабочий случай, а не ошибка: родителя могла снести
// модерация, и комментарий просто становится корневым. Слишком глубокую ветку
// схлопываем (ClampParent), но ребро reply_to_id при этом остаётся настоящим:
// путь — раскладка, адресат — факт, и терять факт из-за раскладки нельзя.
func placeComment(ctx context.Context, q querier, noteID, replyToID, id int64) (path string, branchRoot int64, err error) {
	parent := ""
	if replyToID != 0 {
		err := q.QueryRow(ctx, `SELECT path FROM comments WHERE id = $1 AND note_id = $2`,
			replyToID, noteID).Scan(&parent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, fmt.Errorf("путь адресата %d: %w", replyToID, err)
		}
	}
	path, err = ChildPath(ClampParent(parent), id)
	if err != nil {
		return "", 0, err
	}
	// Корень ветки — первый сегмент пути. У корневого комментария это он сам,
	// и писать в branch_root_id ссылку на себя незачем: ноль читается как «я и
	// есть корень». Значение здесь выведено из НАШЕГО дерева, то есть ровно
	// настолько верно, насколько верен адресат; reply_scan по мобильной версии
	// (Ш6) уточняет и то, и другое.
	if root, err := BranchRootID(path); err == nil && root != id {
		branchRoot = root
	}
	return path, branchRoot, nil
}

// bumpNote двигает денормализованные счётчики заметки. Явно, а не триггером:
// триггер невидим при чтении кода, и через год никто не вспомнит, почему число
// меняется само.
//
// Счётчик считает ВСЕ принятые комментарии, включая позже скрытые: скрытие
// обязано его поправить (иначе лента врёт), и для починки расхождений есть
// RecountComments.
func bumpNote(ctx context.Context, q querier, noteID int64, at time.Time) error {
	_, err := q.Exec(ctx, `
		UPDATE notes
		   SET comment_count = comment_count + 1,
		       last_comment_at = greatest(coalesce(last_comment_at, 'epoch'::timestamptz), $2)
		 WHERE id = $1`, noteID, at)
	return wrapf(err, "счётчики заметки %d", noteID)
}

// RecountComments пересчитывает счётчик заметки по факту. Починка расхождений,
// а не рабочий путь: на 61 тыс. комментариев это дешёвая операция, но в ленте
// она бы стоила COUNT(*) на каждую строку.
func (p *Platform) RecountComments(ctx context.Context, noteID int64) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `
		UPDATE notes SET comment_count = (
		    SELECT count(*) FROM comments WHERE note_id = $1 AND status = 0)
		 WHERE id = $1
		 RETURNING comment_count`, noteID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("заметка %d: %w", noteID, ErrNotFound)
	}
	return n, wrapf(err, "пересчёт комментариев заметки %d", noteID)
}
