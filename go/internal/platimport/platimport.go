// Пакет platimport — разовая раскатка архива (archive.db) в Postgres площадки.
//
// Архив это 117 588 заметок и 10,8 млн комментариев с 2004 года, снятых с
// love.ngs.ru; зеркало же держит только последние три недели. Полоса
// идентификаторов делает перенос почти тождественным: id строки в archive.db уже
// РАВЕН id на сайте, то есть переразметки ключей не требуется вовсе и внешние
// ключи сходятся сами (см. шапку migrations/0001_init.sql).
//
// Почему это отдельный пакет, а не метод ядра. Во-первых, `platform` не вправе
// знать ни про НГС, ни про архив (deps_test стережёт запрет импорта
// internal/archive). Во-вторых, работа здесь администраторская: ради скорости
// снимаются и заново строятся индексы `comments`, а такому месту в ядре не быть.
//
// Что переносится и что нет:
//
//   - заметки, которых в Postgres ЕЩЁ НЕТ. Уже зеркалённые пропускаются целиком
//     вместе со своими комментариями: там живут reply_scan, медиа и настоящее
//     дерево, а копия архива беднее. Дописывать в такой тред задним числом
//     значило бы считать пути от строк, которых мы не видим.
//   - строки с ПОЛОЖИТЕЛЬНЫМИ id. Дамп theloser.ru 2010 принёс 11 577
//     комментариев и 351 анкету, у которых настоящих id сайта нет, и грабер
//     выдал им отрицательные. Полоса идентификаторов такого не принимает
//     (CHECK id > 0), а придумывать им третью полосу — значит ломать обещание
//     «id строки равен id на сайте» ради 0,1 % корпуса.
//
// Дерево ответов собирается из ТРЁХ источников по убыванию достоверности:
// мобильное дерево (comment_reply, 76 % строк), обращение «Ник, …»
// (comment_addressee, разрешается в последнюю реплику адресата в этой заметке)
// и parent_id десктопа — он указывает лишь на корень ветки. Обращение при
// найденном ребре срезается из тела тем же правилом, что у живого обхода
// (platform.TrimAddress): иначе показ дорисует ник поверх написанного.
package platimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lovegw/internal/love"
	"lovegw/internal/platform"

	_ "modernc.org/sqlite"
)

// commentIndexes — индексы `comments`, снимаемые на время раскатки.
//
// Сняты они не ради красоты цифр: 10,8 млн вставок на 1 vCPU обслуживают четыре
// btree размером больше shared_buffers, то есть каждая строка стоит случайной
// записи в четыре дерева. Пересборка после наполнения читает таблицу подряд.
// Определения продублированы из миграции сознательно — это ровно тот текст,
// который надо выполнить руками, если раскатка оборвалась (он и печатается).
var commentIndexes = []struct{ name, ddl string }{
	{"comments_tree", `CREATE INDEX IF NOT EXISTS comments_tree ON comments (note_id, path COLLATE "C")`},
	{"comments_flat", `CREATE INDEX IF NOT EXISTS comments_flat ON comments (note_id, id)`},
	{"comments_author", `CREATE INDEX IF NOT EXISTS comments_author ON comments (author_id, id DESC)`},
	{"comments_reply_to", `CREATE INDEX IF NOT EXISTS comments_reply_to ON comments (reply_to_id) WHERE reply_to_id IS NOT NULL`},
}

// Options — что и как раскатывать.
type Options struct {
	Archive     string // путь к archive.db
	Batch       int    // комментариев в одной транзакции (0 — 50 000)
	Notes       int    // взять только столько заметок (0 — все); отладка
	OnlyNote    int64  // взять только эту заметку; стенд для проверки треда
	KeepIndexes bool   // не снимать индексы comments
	DryRun      bool   // всё посчитать, но ничего не писать
}

// Stats — итог раскатки.
type Stats struct {
	Users, Notes, Comments, Images int
	SkipNotes, SkipComments        int // уже в базе / без настоящего id сайта
	Trimmed                        int // снято обращений «Ник, » из тела
	EdgeTree, EdgeAddr             int
	EdgeParent, EdgeNone           int
}

// Run переносит архив в Postgres. Идемпотентна: повтор досылает недостающее,
// поэтому обрыв на любой транзакции чинится повторным запуском.
func Run(ctx context.Context, p *platform.Platform, opt Options, log *slog.Logger) (Stats, error) {
	var st Stats
	if log == nil {
		log = slog.Default()
	}
	if opt.Batch <= 0 {
		opt.Batch = 50_000
	}
	pool := p.Pool()
	if inDB, want, err := p.Version(ctx); err != nil {
		return st, err
	} else if inDB != want {
		return st, fmt.Errorf("схема площадки версии %d, ожидается %d: сначала platform migrate", inDB, want)
	}

	src, err := openArchive(ctx, opt.Archive)
	if err != nil {
		return st, err
	}
	defer src.Close()

	a, err := readArchiveIndex(ctx, src)
	if err != nil {
		return st, err
	}
	log.Info("архив прочитан", "анкет", len(a.nicks), "заметок", len(a.noteIDs),
		"приблизительных дат", len(a.inexact))

	have, err := readExisting(ctx, pool)
	if err != nil {
		return st, err
	}
	log.Info("в базе площадки", "анкет", len(have.users), "заметок", len(have.notes))
	if opt.DryRun {
		log.Info("сухой прогон: записи не будет")
	}

	if err := copyUsers(ctx, src, pool, have, opt, &st, log); err != nil {
		return st, err
	}

	if !opt.KeepIndexes && !opt.DryRun {
		if err := dropIndexes(ctx, pool, log); err != nil {
			return st, err
		}
		defer func() {
			if err := createIndexes(context.WithoutCancel(ctx), pool, log); err != nil {
				log.Error("ИНДЕКСЫ НЕ ВОССТАНОВЛЕНЫ — выполнить руками", "err", err)
				for _, ix := range commentIndexes {
					log.Error("нужен индекс", "sql", ix.ddl)
				}
			}
		}()
	}

	if err := copyNotes(ctx, src, pool, a, have, opt, &st, log); err != nil {
		return st, err
	}
	return st, nil
}

// ---------------------------------------------------------------- чтение сторон

// openArchive открывает архив только на чтение.
//
// immutable=1 здесь не ускорение, а единственный способ прочитать WAL-базу,
// лежащую read-only: без него SQLite заводит рядом -shm, и открытие падает с
// «attempt to write a readonly database». Обещание «файл не меняется» поэтому
// проверяется, а не даётся на слово: непустой -wal означает незакрытую
// транзакцию, и прочитанное мимо неё было бы тихо неполным.
func openArchive(ctx context.Context, path string) (*sql.DB, error) {
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return nil, fmt.Errorf("архив %s: рядом непустой -wal (%d Б) — сначала закрыть базу "+
			"или снять копию через .backup, иначе часть данных не видна", path, fi.Size())
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		return nil, fmt.Errorf("архив %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("архив %s: %w", path, err)
	}
	return db, nil
}

// archiveIndex — то из архива, что держится в памяти целиком: ники (нужны для
// снятия обращения) и список заметок. На 23 857 анкет и 117 588 заметок это
// единицы мегабайт, зато позволяет не ходить в SQLite за каждым ником.
type archiveIndex struct {
	nicks   map[int64]string
	noteIDs []int64
	inexact map[int64]bool // заметки с восстановленным, а не снятым временем
}

func readArchiveIndex(ctx context.Context, src *sql.DB) (*archiveIndex, error) {
	a := &archiveIndex{nicks: map[int64]string{}, inexact: map[int64]bool{}}
	if err := scanIDs(ctx, src, `SELECT id, name FROM users WHERE id > 0`, "анкеты архива",
		func(id int64, s string) { a.nicks[id] = s }); err != nil {
		return nil, err
	}
	if err := scanIDs(ctx, src, `SELECT id, '' FROM notes WHERE id > 0 ORDER BY id`, "заметки архива",
		func(id int64, _ string) { a.noteIDs = append(a.noteIDs, id) }); err != nil {
		return nil, err
	}
	// Заметки, чьё время не снято с сайта, а восстановлено при импорте чужих
	// дампов (theloser.ru — сдвиг на 6 часов, elovenotes — верхняя граница).
	// Такие едут с published_exact = false: колонка ровно про это.
	err := scanIDs(ctx, src,
		`SELECT row_id, '' FROM import_provenance WHERE kind = 'note' AND time_kind IS NOT NULL`,
		"происхождение заметок", func(id int64, _ string) { a.inexact[id] = true })
	if err != nil {
		// Таблицы происхождения в архиве постарше может не быть — не отказ.
		a.inexact = map[int64]bool{}
	}
	return a, nil
}

func scanIDs(ctx context.Context, src *sql.DB, query, what string, fn func(int64, string)) error {
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		fn(id, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// existing — что уже лежит в Postgres. Множества, а не COUNT: раскатка обязана
// быть идемпотентной, а проверять наличие построчным SELECT на 10,8 млн строк —
// это 10,8 млн обращений к серверу.
type existing struct {
	users map[int64]bool
	notes map[int64]bool
}

func readExisting(ctx context.Context, pool *pgxpool.Pool) (*existing, error) {
	e := &existing{users: map[int64]bool{}, notes: map[int64]bool{}}
	for _, q := range []struct {
		sql  string
		dst  map[int64]bool
		what string
	}{
		{`SELECT id FROM users WHERE id < $1`, e.users, "анкеты"},
		{`SELECT id FROM notes WHERE id < $1`, e.notes, "заметки"},
	} {
		rows, err := pool.Query(ctx, q.sql, platform.NativeIDBase)
		if err != nil {
			return nil, fmt.Errorf("что уже есть (%s): %w", q.what, err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("что уже есть (%s): %w", q.what, err)
			}
			q.dst[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("что уже есть (%s): %w", q.what, err)
		}
	}
	return e, nil
}

// ---------------------------------------------------------------- анкеты

// copyUsers переносит анкеты, которых в базе ещё нет.
//
// Существующие строки не трогаются вовсе: там уже могут быть вошедший участник
// со своим ником, обезличенный по требованию субъекта и привязанный аватар.
// Архив старше живого зеркала, и «latest-wins» здесь означал бы откат ника на
// годы назад.
func copyUsers(ctx context.Context, src *sql.DB, pool *pgxpool.Pool, have *existing,
	opt Options, st *Stats, log *slog.Logger) error {
	rows, err := src.QueryContext(ctx,
		`SELECT id, name, avatar_url, gender FROM users WHERE id > 0 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("анкеты архива: %w", err)
	}
	defer rows.Close()

	batch := make([][]any, 0, 20_000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if opt.DryRun {
			st.Users += len(batch)
			batch = batch[:0]
			return nil
		}
		n, err := pool.CopyFrom(ctx, pgx.Identifier{"users"},
			[]string{"id", "nick", "ngs_avatar_url", "kind", "gender"}, pgx.CopyFromRows(batch))
		if err != nil {
			return fmt.Errorf("перенос анкет: %w", err)
		}
		st.Users += int(n)
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var id int64
		var nick, avatar, gender string
		if err := rows.Scan(&id, &nick, &avatar, &gender); err != nil {
			return fmt.Errorf("анкеты архива: %w", err)
		}
		if have.users[id] {
			continue
		}
		// Силуэт по умолчанию — не аватар, а фон: ссылку на него хранить незачем
		// (то же правило у живого приёмника).
		if !love.IsRealAvatar(avatar) {
			avatar = ""
		}
		batch = append(batch, []any{id, nick, avatar, int16(platform.KindShadow), int16(genderOf(gender))})
		if len(batch) >= 20_000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("анкеты архива: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	log.Info("анкеты перенесены", "новых", st.Users)
	return nil
}

// genderOf переводит значение архива в наше. Перевод живёт здесь, а не в ядре:
// `platform` про НГС не знает.
func genderOf(s string) platform.Gender {
	switch s {
	case "male":
		return platform.GenderMale
	case "female":
		return platform.GenderFemale
	}
	return platform.GenderUnknown
}

// ---------------------------------------------------------------- заметки и треды

type noteRow struct {
	id             int64
	author         int64 // 0 — аноним либо автор без анкеты
	anonymous      bool
	body           string
	images         []string
	commentsClosed bool
	publishedAt    time.Time
	exact          bool
	count          int
	last           time.Time
}

// copyNotes переносит заметки вместе с их тредами, порциями в одной транзакции.
//
// Порция — заметки целиком, а не строки: тред это связная структура, и заметка,
// у которой половина комментариев доехала, показывается неправильно вся. Отсюда
// же устойчивость к обрыву: незавершённая порция откатывается, а повтор
// раскатки видит заметку отсутствующей и переносит её заново.
func copyNotes(ctx context.Context, src *sql.DB, pool *pgxpool.Pool, a *archiveIndex, have *existing,
	opt Options, st *Stats, log *slog.Logger) error {
	getNote, err := src.PrepareContext(ctx, `
		SELECT author_id, text, images, comments_closed, published_at FROM notes WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("подготовка запроса заметки: %w", err)
	}
	defer getNote.Close()
	getComments, err := src.PrepareContext(ctx, `
		SELECT c.id, c.author_id, c.text, c.published_at, c.parent_id,
		       coalesce(r.reply_to, 0), coalesce(ad.addressee_id, 0)
		  FROM comments c
		  LEFT JOIN comment_reply     r  ON r.comment_id  = c.id
		  LEFT JOIN comment_addressee ad ON ad.comment_id = c.id
		 WHERE c.note_id = ?
		 ORDER BY c.id`)
	if err != nil {
		return fmt.Errorf("подготовка запроса треда: %w", err)
	}
	defer getComments.Close()

	var (
		notes    []noteRow
		comments [][]any
		images   [][]any
		seen     int
		started  = time.Now()
	)
	flush := func() error {
		if len(notes) == 0 {
			return nil
		}
		if !opt.DryRun {
			if err := writeChunk(ctx, pool, notes, comments, images); err != nil {
				return err
			}
		}
		st.Notes += len(notes)
		st.Comments += len(comments)
		st.Images += len(images)
		notes, comments, images = notes[:0], comments[:0], images[:0]
		log.Info("перенесено", "заметок", st.Notes, "комментариев", st.Comments,
			"просмотрено", seen, "из", len(a.noteIDs), "за", time.Since(started).Truncate(time.Second))
		return nil
	}

	for _, id := range a.noteIDs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if opt.OnlyNote != 0 && id != opt.OnlyNote {
			continue
		}
		seen++
		if have.notes[id] {
			st.SkipNotes++
			continue
		}
		if opt.Notes > 0 && st.Notes+len(notes) >= opt.Notes {
			break
		}
		n, err := readNote(ctx, getNote, id, a)
		if err != nil {
			return err
		}
		tree, err := readThread(ctx, getComments, n, a, st)
		if err != nil {
			return err
		}
		n.count = len(tree)
		for _, c := range tree {
			if at, ok := c[10].(time.Time); ok && at.After(n.last) {
				n.last = at
			}
		}
		notes = append(notes, *n)
		comments = append(comments, tree...)
		for i, u := range n.images {
			images = append(images, []any{n.id, int16(i), nil, u})
		}
		if len(comments) >= opt.Batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func readNote(ctx context.Context, stmt *sql.Stmt, id int64, a *archiveIndex) (*noteRow, error) {
	var (
		author sql.NullInt64
		body   string
		imgs   string
		closed int
		at     string
	)
	if err := stmt.QueryRowContext(ctx, id).Scan(&author, &body, &imgs, &closed, &at); err != nil {
		return nil, fmt.Errorf("заметка %d: %w", id, err)
	}
	n := &noteRow{id: id, body: body, commentsClosed: closed != 0, exact: !a.inexact[id]}
	// Автор без анкеты (отрицательный id из чужого дампа) неотличим для нас от
	// анонима: ссылаться не на кого, а заводить ему строку в `users` значило бы
	// придумать человека, которого на сайте нет.
	if author.Valid && author.Int64 > 0 {
		n.author = author.Int64
	}
	n.anonymous = n.author == 0
	var err error
	if n.publishedAt, err = parseTime(at); err != nil {
		return nil, fmt.Errorf("заметка %d: %w", id, err)
	}
	if imgs != "" && imgs != "[]" {
		if err := json.Unmarshal([]byte(imgs), &n.images); err != nil {
			return nil, fmt.Errorf("заметка %d, иллюстрации: %w", id, err)
		}
	}
	return n, nil
}

// readThread собирает тред одной заметки: рёбра, пути и снятые обращения.
// Возвращает готовые строки COPY в порядке возрастания id.
func readThread(ctx context.Context, stmt *sql.Stmt, n *noteRow, a *archiveIndex, st *Stats) ([][]any, error) {
	rows, err := stmt.QueryContext(ctx, n.id)
	if err != nil {
		return nil, fmt.Errorf("тред заметки %d: %w", n.id, err)
	}
	defer rows.Close()

	type raw struct {
		id, author, parent, replyTo, addressee int64
		body                                   string
		at                                     time.Time
	}
	var list []raw
	for rows.Next() {
		var r raw
		var at string
		if err := rows.Scan(&r.id, &r.author, &r.body, &at, &r.parent, &r.replyTo, &r.addressee); err != nil {
			return nil, fmt.Errorf("тред заметки %d: %w", n.id, err)
		}
		if r.id <= 0 {
			st.SkipComments++
			continue
		}
		if r.at, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("комментарий %d: %w", r.id, err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("тред заметки %d: %w", n.id, err)
	}

	var (
		out      = make([][]any, 0, len(list))
		paths    = make(map[int64]string, len(list))
		known    = make(map[int64]bool, len(list))
		lastOf   = make(map[int64]int64, len(list)) // автор → его последняя реплика
		authorOf = make(map[int64]int64, len(list))
	)
	for _, r := range list {
		known[r.id] = true
	}
	for _, r := range list {
		parent, source := edgeOf(r.id, r.replyTo, r.addressee, r.parent, known, lastOf)
		switch source {
		case platform.ReplyMobileTree:
			st.EdgeTree++
		case platform.ReplyPrefix:
			st.EdgeAddr++
		case platform.ReplyDesktopParent:
			st.EdgeParent++
		default:
			st.EdgeNone++
		}
		body := r.body
		if parent != 0 {
			// Обращение срезается тем же правилом, что у живого обхода: ник
			// берётся у НАЙДЕННОГО адресата, и по нему же проверяется, что
			// срезается обращение, а не начало фразы.
			if cut, done := platform.TrimAddress(body, a.nicks[authorOf[parent]]); done {
				body = cut
				st.Trimmed++
			}
		}
		path, err := platform.ChildPath(platform.ClampParent(paths[parent]), r.id)
		if err != nil {
			return nil, fmt.Errorf("путь комментария %d: %w", r.id, err)
		}
		paths[r.id] = path
		branch := int64(0)
		if root, err := platform.BranchRootID(path); err == nil && root != r.id {
			branch = root
		}
		out = append(out, []any{
			r.id, n.id, nullID(r.author), "", body,
			nullID(branch), nullID(parent), int16(source), path,
			int16(platform.PathDepth(path)), r.at,
		})
		lastOf[r.author] = r.id
		authorOf[r.id] = r.author
	}
	return out, nil
}

// edgeOf выбирает ребро ответа из трёх источников по убыванию достоверности.
//
// Мобильное дерево — настоящий адресат (92 % совпадения при замере). Обращение
// «Ник, …» разрешается в ПОСЛЕДНЮЮ реплику этого человека в треде, и это
// угадывание примерно с половинной точностью, — но оно лучше, чем ничего.
// parent_id десктопа указывает лишь на корень ветки: как адресат он почти
// всегда неверен, зато как раскладка ветки верен всегда, и ветка вырастает там,
// где росла на сайте.
func edgeOf(id, replyTo, addressee, parent int64, known map[int64]bool,
	lastOf map[int64]int64) (int64, platform.ReplySource) {
	if replyTo > 0 && replyTo < id && known[replyTo] {
		return replyTo, platform.ReplyMobileTree
	}
	if addressee > 0 {
		if last, ok := lastOf[addressee]; ok && last < id {
			return last, platform.ReplyPrefix
		}
	}
	if parent > 0 && parent < id && known[parent] {
		return parent, platform.ReplyDesktopParent
	}
	return 0, platform.ReplyNone
}

// writeChunk пишет порцию одной транзакцией: заметки, их треды, их иллюстрации.
func writeChunk(ctx context.Context, pool *pgxpool.Pool, notes []noteRow, comments, images [][]any) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("порция: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	nrows := make([][]any, 0, len(notes))
	for _, n := range notes {
		var last any
		if !n.last.IsZero() {
			last = n.last
		}
		nrows = append(nrows, []any{
			n.id, nullID(n.author), n.anonymous, n.body, n.commentsClosed,
			n.count, n.publishedAt, n.exact, last,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"notes"},
		[]string{"id", "author_id", "anonymous", "body", "comments_closed",
			"comment_count", "published_at", "published_exact", "last_comment_at"},
		pgx.CopyFromRows(nrows)); err != nil {
		return fmt.Errorf("перенос заметок: %w", err)
	}
	if len(comments) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"comments"},
			[]string{"id", "note_id", "author_id", "author_display", "body",
				"branch_root_id", "reply_to_id", "reply_source", "path", "depth", "published_at"},
			pgx.CopyFromRows(comments)); err != nil {
			return fmt.Errorf("перенос комментариев: %w", err)
		}
	}
	if len(images) > 0 {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"note_images"},
			[]string{"note_id", "position", "sha256", "url"},
			pgx.CopyFromRows(images)); err != nil {
			return fmt.Errorf("перенос иллюстраций: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- индексы

func dropIndexes(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	for _, ix := range commentIndexes {
		if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+ix.name); err != nil {
			return fmt.Errorf("снятие индекса %s: %w", ix.name, err)
		}
	}
	log.Info("индексы comments сняты на время раскатки — страницы треда пока идут перебором")
	return nil
}

func createIndexes(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	for _, ix := range commentIndexes {
		started := time.Now()
		if _, err := pool.Exec(ctx, ix.ddl); err != nil {
			return fmt.Errorf("сборка индекса %s: %w", ix.name, err)
		}
		log.Info("индекс собран", "имя", ix.name, "за", time.Since(started).Truncate(time.Second))
	}
	return nil
}

// Analyze обновляет статистику планировщика. Отдельным вызовом, а не внутри
// Run: без неё планы остаются рассчитанными на 61 тыс. комментариев, а планы
// запросов — часть контракта площадки (тест требует notes_feed / comments_tree /
// comments_flat).
func Analyze(ctx context.Context, pool *pgxpool.Pool) error {
	for _, t := range []string{"users", "notes", "comments", "note_images"} {
		if _, err := pool.Exec(ctx, "ANALYZE "+t); err != nil {
			return fmt.Errorf("статистика %s: %w", t, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- мелочи

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// parseTime разбирает время архива. Оно везде UTC (ISO-8601 с «Z»), но ранние
// импорты писали его без зоны — читаем и так, всё равно как UTC.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("пустое время")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("время %q не разобрано", s)
}
