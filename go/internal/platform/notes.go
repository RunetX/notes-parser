package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// MaxBodyRunes — потолок длины текста. Это санитарная граница, а не продуктовое
// решение: она существует, чтобы случайный или злонамеренный мегабайт не стал
// строкой в базе. Самые длинные заметки НГС — единицы тысяч знаков.
const MaxBodyRunes = 20000

// ErrEmptyBody — пустой текст.
var ErrEmptyBody = errors.New("пустой текст")

// ErrTooLong — текст длиннее допустимого.
var ErrTooLong = fmt.Errorf("текст длиннее %d знаков", MaxBodyRunes)

const noteColumns = `id, author_id, anonymous, body, status, comments_closed, locked,
	comment_count, published_at, published_exact, last_comment_at, edited_at, created_at`

func scanNote(row pgx.Row) (Note, error) {
	var (
		n      Note
		author *int64
	)
	err := row.Scan(&n.ID, &author, &n.Anonymous, &n.Body, &n.Status, &n.CommentsClosed, &n.Locked,
		&n.CommentCount, &n.PublishedAt, &n.PublishedExact, &n.LastCommentAt, &n.EditedAt, &n.CreatedAt)
	n.AuthorID = idOf(author)
	return n, err
}

// noteViewColumns — те же заметки, но уже замаскированные.
//
// Маскирование стоит в SELECT, а не в Go, намеренно: так автор анонимной
// публикации не покидает базу вовсе, и никакой забывчивый шаблон выше его не
// покажет. Аватар отдаётся нашим путём (sha + mime), ссылка на hsmedia.ru
// наружу не уходит никогда — иначе смерть НГС забирает с собой наши страницы, а
// до тех пор сообщает ему каждого нашего читателя.
const noteViewColumns = `
	n.id, n.anonymous, n.body, n.status, n.comments_closed, n.locked, n.comment_count,
	n.published_at, n.published_exact, n.last_comment_at, n.edited_at,
	CASE WHEN n.anonymous THEN NULL ELSE n.author_id     END,
	CASE WHEN n.anonymous THEN NULL ELSE u.nick          END,
	CASE WHEN n.anonymous THEN NULL ELSE u.avatar_sha    END,
	CASE WHEN n.anonymous THEN NULL ELSE m.mime          END,
	CASE WHEN n.anonymous THEN 0     ELSE coalesce(u.gender, 0) END,
	CASE WHEN n.anonymous THEN false ELSE coalesce(u.kind, 0) = 0 END,
	coalesce(n.author_id = $1, false),
	n.pinned_at IS NOT NULL`

const noteViewFrom = `
	FROM notes n
	LEFT JOIN users u ON u.id = n.author_id
	LEFT JOIN media m ON m.sha256 = u.avatar_sha`

func scanNoteView(row pgx.Row) (NoteView, error) {
	var (
		v      NoteView
		author *int64
		nick   *string
		sha    []byte
		mime   *string
		gender Gender
		shadow bool
	)
	err := row.Scan(&v.ID, &v.Anonymous, &v.Body, &v.Status, &v.CommentsClosed, &v.Locked, &v.CommentCount,
		&v.PublishedAt, &v.PublishedExact, &v.LastCommentAt, &v.EditedAt,
		&author, &nick, &sha, &mime, &gender, &shadow, &v.Own, &v.Pinned)
	v.Author = Author{
		ID: idOf(author), Nick: strOf(nick),
		AvatarURL: MediaURL(sha, strOf(mime)), Gender: gender, Shadow: shadow,
	}
	return v, err
}

// feedQuery — лента. Запрос вынесен в константу, чтобы тест мог спросить у
// Postgres план РОВНО того SQL, который выполняется, и убедиться, что берётся
// частичный индекс notes_feed, а не seq scan: переезд ленты на полный перебор —
// это отказ, который не виден ни в одном тесте на поведение.
//
// Пролистывание — по НОМЕРУ страницы, то есть через OFFSET. Ключевой курсор был
// бы устойчивее (OFFSET на живой ленте умеет и дублировать строку, и терять её,
// если во время чтения пришла новая заметка), но нумерованная постраничка
// «1 2 3 … 5933» без OFFSET не строится, а она — часть узнаваемого вида: на НГС
// это `/notes/page~2/limit~20/`, и человек привык знать, на какой он странице и
// уметь вернуться. Цена размена невелика: сдвиг случается только при публикации
// новой заметки ровно в момент листания, и стоит он одной задвоенной строки.
const feedQuery = `
	SELECT ` + noteViewColumns + noteViewFrom + `
	 WHERE n.status = 0 AND n.pinned_at IS NULL
	 ORDER BY n.published_at DESC, n.id DESC
	 LIMIT $2 OFFSET $3`

// pinnedQuery — закреплённые заметки, те самые, что лента показывает поверх
// хронологии. Отдельным запросом, а не сортировкой внутри ленты, намеренно:
// «ORDER BY pinned DESC, published_at DESC» отняло бы у ленты индекс
// notes_feed и превратило бы КАЖДЫЙ её показ в сортировку 117 тысяч строк ради
// одной-двух наверху. Здесь же свой частичный индекс и заведомо короткий ответ.
const pinnedQuery = `
	SELECT ` + noteViewColumns + noteViewFrom + `
	 WHERE n.status = 0 AND n.pinned_at IS NOT NULL
	 ORDER BY n.pinned_at DESC, n.id DESC
	 LIMIT $2`

// Feed — страница ленты от новых к старым.
//
// Скрытые публикации отсекаются по notes.status, а не соединением с users:
// рубильник «скрыть все мои публикации» (users.hide_all) обязан исполняться
// записью статусов, а не проверкой на чтении. Иначе отзыв согласия стоил бы
// join'а на каждой странице ленты — и однажды его бы оттуда убрали. Тому, кто
// будет делать hide_all: менять надо статусы публикаций, а не этот запрос.
func (p *Platform) Feed(ctx context.Context, v Viewer, offset, limit int) ([]NoteView, error) {
	limit = clampLimit(limit)
	if offset < 0 {
		offset = 0
	}
	rows, err := p.pool.Query(ctx, feedQuery, v.UserID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("лента: %w", err)
	}
	defer rows.Close()

	out := make([]NoteView, 0, limit)
	for rows.Next() {
		n, err := scanNoteView(rows)
		if err != nil {
			return nil, fmt.Errorf("лента, разбор строки: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("лента: %w", err)
	}
	return out, nil
}

// MaxPinned — потолок закреплённых. Он не про базу, а про ленту: закрепление
// имеет смысл ровно до тех пор, пока закреплённое можно окинуть взглядом, —
// десять «важных» заметок наверху это просто вторая лента, которую никто не
// читает.
const MaxPinned = 5

// PinnedNotes — закреплённые заметки, самое свежее закрепление первым.
//
// Лента их у себя НЕ показывает (feedQuery отсекает по pinned_at), поэтому
// одна и та же заметка не выходит дважды. Цена размена записана честно: пока
// заметка закреплена, её нет на своём хронологическом месте, и последняя
// страница ленты короче на число закреплённых. Обратное — «показывать и там, и
// там» — читается как ошибка показа, а не как решение.
func (p *Platform) PinnedNotes(ctx context.Context, v Viewer) ([]NoteView, error) {
	rows, err := p.pool.Query(ctx, pinnedQuery, v.UserID, MaxPinned)
	if err != nil {
		return nil, fmt.Errorf("закреплённые: %w", err)
	}
	defer rows.Close()

	out := make([]NoteView, 0, MaxPinned)
	for rows.Next() {
		n, err := scanNoteView(rows)
		if err != nil {
			return nil, fmt.Errorf("закреплённые, разбор строки: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("закреплённые: %w", err)
	}
	return out, nil
}

// CountNotes — сколько заметок в ленте. Нужен постраничке: без общего числа
// нельзя нарисовать ни номера страниц, ни последнюю.
//
// COUNT(*) здесь единственный на всю площадку и живёт он ровно ради этого. На
// нынешних 300 строках он бесплатен; когда приедет архив (10,7 млн), его
// придётся заменить оценкой из pg_class — считать точное число заметок 2011
// года на каждый показ ленты незачем.
func (p *Platform) CountNotes(ctx context.Context) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM notes WHERE status = 0`).Scan(&n)
	return n, wrapf(err, "счётчик ленты")
}

// NoteViewByID — заметка для показа.
func (p *Platform) NoteViewByID(ctx context.Context, v Viewer, id int64) (NoteView, error) {
	n, err := scanNoteView(p.pool.QueryRow(ctx,
		`SELECT `+noteViewColumns+noteViewFrom+` WHERE n.id = $2`, v.UserID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return NoteView{}, fmt.Errorf("заметка %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return NoteView{}, fmt.Errorf("чтение заметки %d: %w", id, err)
	}
	return n, nil
}

// NoteRow — сырая строка заметки, вместе с настоящим автором анонимки. Для служб
// (приём, модерация, права субъекта), не для показа: показывает NoteViewByID.
func (p *Platform) NoteRow(ctx context.Context, id int64) (Note, error) {
	n, err := scanNote(p.pool.QueryRow(ctx, `SELECT `+noteColumns+` FROM notes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Note{}, fmt.Errorf("заметка %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Note{}, fmt.Errorf("чтение заметки %d: %w", id, err)
	}
	return n, nil
}

// IngestNote принимает заметку с НГС. Идемпотентна по id: повтор ничего не
// меняет и возвращает false. Тень автора заводится в той же транзакции, иначе
// внешний ключ поймал бы гонку приёма заметки и первого комментария.
func (p *Platform) IngestNote(ctx context.Context, in MirroredNote) (bool, error) {
	if !IsNGS(in.ID) {
		return false, fmt.Errorf("заметка НГС ожидается с id из полосы сайта, получен %d", in.ID)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("приём заметки %d: %w", in.ID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	author, err := ensureShadow(ctx, tx, in.Author)
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO notes (id, author_id, anonymous, body, comments_closed, published_at, published_exact)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		in.ID, nullID(author), in.Anonymous, in.Body, in.CommentsClosed, in.PublishedAt, in.PublishedExact)
	if err != nil {
		return false, fmt.Errorf("приём заметки %d: %w", in.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("приём заметки %d: %w", in.ID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// MirroredNoteIDs — id всех заметок, пришедших с НГС. Нужен сверке: разница с
// тем же множеством в lovegw.db и есть список того, что до площадки не доехало.
// Нативные заметки в ответ не попадают — их в зеркале нет и быть не может.
func (p *Platform) MirroredNoteIDs(ctx context.Context) (map[int64]bool, error) {
	return p.idSet(ctx, "список зеркальных заметок",
		`SELECT id FROM notes WHERE id < $1`, NativeIDBase)
}

// OpenNoteIDs — зеркальные заметки, у которых комментарии ещё открыты. Сверке
// нужна именно эта сторона: отметку «не актуальна» сайт ставит уже после
// публикации, у приёмника зеркала события про неё нет вовсе, и переносить её
// может только сверка. Обратный переход (закрыли — открыли) на НГС не бывает.
func (p *Platform) OpenNoteIDs(ctx context.Context) (map[int64]bool, error) {
	return p.idSet(ctx, "список открытых заметок",
		`SELECT id FROM notes WHERE id < $1 AND NOT comments_closed`, NativeIDBase)
}

// SetCommentsClosed переносит отметку сайта «не актуальна». Возвращает true при
// первом переходе — чтобы событие логировалось один раз, а не каждый обход.
//
// На архивацию эта отметка НЕ влияет и влиять не должна: сайт ставит её через
// минуты после публикации, пока комментарии продолжают приходить.
func (p *Platform) SetCommentsClosed(ctx context.Context, id int64, closed bool) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
		UPDATE notes SET comments_closed = $2 WHERE id = $1 AND comments_closed <> $2`, id, closed)
	if err != nil {
		return false, fmt.Errorf("отметка «комментарии закрыты» у заметки %d: %w", id, err)
	}
	return tag.RowsAffected() > 0, nil
}

// CreateNote публикует нативную заметку и возвращает её id.
//
// У анонимной заметки автор сохраняется НАСТОЯЩИЙ: он должен видеть её среди
// своих и мочь удалить, модерация — забанить анонимного флудера, а ограничение
// частоты — считать по человеку. Скрытие живёт в границе показа (см. types.go),
// и это осознанный размен: компрометация базы деанонимизирует автора, зато
// анонимность не отменяет ни прав субъекта, ни модерации.
// Проверки, частота и постановка в очередь модерации идут ОДНОЙ транзакцией с
// вставкой: между «можно ли ему писать» и самой записью иначе успевает пройти
// бан или отзыв согласия, а «опубликовано, но в очередь не попало» — состояние,
// которого не должно быть вовсе.
func (p *Platform) CreateNote(ctx context.Context, in NewNote) (int64, error) {
	body, err := cleanBody(in.Body)
	if err != nil {
		return 0, err
	}
	if in.AuthorID == 0 {
		return 0, errors.New("нативная заметка без автора")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("публикация заметки: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if err := writeGuard(ctx, tx, in.AuthorID); err != nil {
		return 0, err
	}
	if err := enforceRate(ctx, tx, notesRecentQuery, in.AuthorID, time.Now(), noteRates); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO notes (id, author_id, anonymous, body, published_at, published_exact)
		VALUES (nextval('notes_native_seq'), $1, $2, $3, now(), true)
		RETURNING id`, in.AuthorID, in.Anonymous, body).Scan(&id); err != nil {
		return 0, fmt.Errorf("публикация заметки: %w", err)
	}
	if err := enqueueCheck(ctx, tx, SubjectNote, id, id, in.AuthorID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("публикация заметки: %w", err)
	}
	return id, nil
}

// cleanBody приводит текст к виду, в котором он хранится: без пробельного мусора
// по краям и без нулевых байтов (Postgres не хранит \x00 в text вовсе, и лучше
// сказать об этом отказом, чем получить ошибку драйвера в середине транзакции).
func cleanBody(s string) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if s == "" {
		return "", ErrEmptyBody
	}
	if utf8.RuneCountInString(s) > MaxBodyRunes {
		return "", ErrTooLong
	}
	return s, nil
}

const (
	defaultLimit = 50
	maxLimit     = 100
)

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return defaultLimit
	case n > maxLimit:
		return maxLimit
	default:
		return n
	}
}
