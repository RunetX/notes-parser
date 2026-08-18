package platform

import (
	"context"
	"errors"
	"fmt"
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
	 ORDER BY c.id DESC
	 LIMIT $3 OFFSET $4`

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
	rows, err := p.pool.Query(ctx, threadQuery, v.UserID, noteID, MaxThreadRows)
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
// Отдельный индекс (note_id, id), а не сортировка выборки дерева: переключатель
// «дерево / линейный» на сайте живой, им пользуются.
func (p *Platform) Flat(ctx context.Context, v Viewer, noteID int64, offset, limit int) ([]CommentView, error) {
	limit = clampLimit(limit)
	if offset < 0 {
		offset = 0
	}
	rows, err := p.pool.Query(ctx, flatQuery, v.UserID, noteID, limit, offset)
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

	if err := writeGuard(ctx, tx, in.AuthorID); err != nil {
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
	if err := enqueueCheck(ctx, tx, SubjectComment, id, in.AuthorID); err != nil {
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
