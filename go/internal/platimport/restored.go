package platimport

// Добор ВОССТАНОВЛЕННОГО: комментарии, которых нет ни на сайте, ни в первой
// раскатке архива.
//
// Что это за строки. Сам НГС стёр комментарии до конца 2013 года: страница треда
// заметки 2010 года отдаёт ноль реплик, и живым обходом эпоху не вернуть ничем.
// Уцелела она в чужой копии — дампе WordPress-зеркала theloser.ru, работавшего в
// 2010–2012: 11 577 комментариев за 4–9 августа 2010 года, 337 тредов. В
// archive.db они лежат с 13.08.2026 и помечены в `import_provenance`, а
// идентификаторов сайта у них нет вовсе — грабер выдал им отрицательные,
// детерминированно выведенные из номера заметки. Раскатка архива такие строки
// пропускает (см. шапку platimport.go), и на площадке 326 заметок 2010 года
// стоят с пустыми тредами.
//
// Три решения этого файла.
//
// ПОЛОСА. Ключ считается из ключа архива: id = RestoredIDBase + |id архива|.
// Отображение детерминированное, поэтому добор идемпотентен без таблицы
// соответствий: повтор вычисляет те же самые идентификаторы. Полоса третья, а не
// нативная, потому что нативное — это написанное ЗДЕСЬ и уходящее в каналы
// мессенджеров (см. platout); реплика 2010 года в нативной полосе была бы
// запощена в канал через пятнадцать секунд.
//
// ТРЕД ДОПИСЫВАЕТСЯ, А НЕ СОЗДАЁТСЯ, и это ровно обратное правилу раскатки
// архива («заметка уже есть — пропустить целиком»). То правило было про
// зеркалённые треды: там живут reply_scan, медиа и настоящее дерево, а копия
// архива беднее. Здесь противоположный случай — тред ПУСТ и другим уже не
// станет, потому что источника для него не существует. Поэтому непустой тред
// по-прежнему не трогается: это признак, что заметку принесло зеркало.
//
// ОБРАЩЕНИЕ ЭПОХИ — «Для Ник (Имя) текст», третий формат помимо «Ник, …» и
// «Для [b][i]Ник[/i][/b]». Разбирается он не по виду, а сопоставлением с
// УЧАСТНИКАМИ ТРЕДА, и это принципиально: в 2010-м рядом жили «Димон» и
// «Димон_Таибычев», а глобальный набор ников на этих же строках даёт «Me» вместо
// «Mellissa» и «Н» вместо «НюШ@».
//
// Замер по всем 11 577 строкам: ребро находится у 6 945 (60 %), у 2 957 обращения
// нет вовсе, у 1 606 ник не принадлежит участнику треда (дамп покрывает шесть
// суток, а треды начинались раньше — реплик адресата в нём просто нет), 52 состоят
// из одного обращения, и 17 адресованы автору заметки, который в своём треде не
// отвечал. Разрешилось — обращение уходит в ребро и срезается из тела; не
// разрешилось — остаётся текстом, потому что снятое обращение без ребра исчезло
// бы совсем.
//
// Заодно чинится авторство самих заметок: 181 заметка 2010 года подписана ником,
// который ни в одну анкету не разрешается, и первая раскатка записала их
// анонимными — автора без анкеты она приравнивает к анониму. Теперь такому
// автору есть где жить (та же третья полоса), и подпись возвращается на место.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lovegw/internal/platform"
)

// RestoredOptions — что и как доносить.
type RestoredOptions struct {
	Archive  string // путь к archive.db
	Notes    int    // взять только столько заметок (0 — все); отладка
	OnlyNote int64  // взять только эту заметку; стенд для проверки треда
	DryRun   bool   // всё посчитать, но ничего не писать
}

// RestoredStats — итог добора.
type RestoredStats struct {
	Users      int // заведено анкет третьей полосы
	Notes      int // заметок, в которые дописан тред
	Comments   int
	Signed     int // заметок, которым вернули подписанного автора
	SkipAbsent int // заметки нет на площадке: её приносит раскатка архива
	SkipFilled int // тред уже непуст — значит, заметку вело зеркало
	Trimmed    int // снято обращений «Для Ник (Имя)» в ребро
	EdgeAddr   int // рёбер из обращения
	EdgeNone   int // реплик без ребра
}

// RunRestored доносит на площадку комментарии без идентификатора сайта.
//
// Идемпотентна: повтор пропускает заметки с непустым тредом, а анкеты пишутся
// ON CONFLICT DO NOTHING по вычисленному ключу.
func RunRestored(ctx context.Context, p *platform.Platform, opt RestoredOptions, log *slog.Logger) (RestoredStats, error) {
	var st RestoredStats
	if log == nil {
		log = slog.Default()
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

	nicks, err := readNicks(ctx, src)
	if err != nil {
		return st, err
	}
	noteIDs, err := restoredNoteIDs(ctx, src, opt)
	if err != nil {
		return st, err
	}
	log.Info("восстановленное в архиве", "тредов", len(noteIDs), "анкет всего", len(nicks))
	if opt.DryRun {
		log.Info("сухой прогон: записи не будет")
	}

	if err := copyRestoredUsers(ctx, src, pool, opt, &st, log); err != nil {
		return st, err
	}

	target, err := readTargets(ctx, pool, noteIDs)
	if err != nil {
		return st, err
	}

	// ORDER BY id DESC — это и есть хронология. Ключ архива у восстановленной
	// строки отрицательный и выведен из номера в треде: -(note_id*1000 + seq),
	// поэтому «первая реплика» — самое БОЛЬШОЕ число. Порядок здесь не мелочь:
	// по нему считаются рёбра («последняя реплика этого человека») и пути, то
	// есть перевёрнутый тред дал бы дерево, выросшее задом наперёд.
	getComments, err := src.PrepareContext(ctx, `
		SELECT id, author_id, text, published_at FROM comments
		 WHERE note_id = ? AND id < 0 ORDER BY id DESC`)
	if err != nil {
		return st, fmt.Errorf("подготовка запроса треда: %w", err)
	}
	defer getComments.Close()
	getAuthor, err := src.PrepareContext(ctx, `SELECT author_id FROM notes WHERE id = ?`)
	if err != nil {
		return st, fmt.Errorf("подготовка запроса заметки: %w", err)
	}
	defer getAuthor.Close()

	started := time.Now()
	for _, id := range noteIDs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		tgt, ok := target[id]
		if !ok {
			// Заметки нет на площадке: её приносит раскатка архива, и решать за
			// неё этот добор не вправе — у него нет ни тела заметки, ни медиа.
			st.SkipAbsent++
			continue
		}
		if tgt.comments > 0 {
			// Непустой тред — признак, что заметку вело зеркало, а не дамп.
			// Такие называются поимённо: их единицы, и молча пропущенная заметка
			// выглядит одинаково с заметкой, которую забыли.
			st.SkipFilled++
			log.Info("тред уже непуст, дамп не дописываем",
				"заметка", id, "комментариев", tgt.comments)
			continue
		}
		if opt.Notes > 0 && st.Notes >= opt.Notes {
			break
		}
		var author sql.NullInt64
		switch err := getAuthor.QueryRowContext(ctx, id).Scan(&author); {
		case errors.Is(err, sql.ErrNoRows):
			// Реплики в архиве есть, а самой заметки нет: подписывать нечего,
			// но тред донести можно — он от этого не становится хуже.
			author = sql.NullInt64{}
		case err != nil:
			return st, fmt.Errorf("заметка %d: %w", id, err)
		}
		rows, last, err := readRestoredThread(ctx, getComments, id, author.Int64, nicks, &st)
		if err != nil {
			return st, err
		}
		// Подпись возвращается только заметке, которую первая раскатка записала
		// анонимной именно из-за отсутствия анкеты: чужого решения (аноним на
		// самом сайте, скрытие модератором) добор не трогает.
		signAs := int64(0)
		if author.Valid && author.Int64 < 0 && tgt.anonymous && tgt.author == 0 {
			signAs = restoredID(author.Int64)
		}
		if !opt.DryRun {
			if err := writeRestored(ctx, pool, id, rows, last, signAs); err != nil {
				return st, err
			}
		}
		st.Notes++
		st.Comments += len(rows)
		if signAs != 0 {
			st.Signed++
		}
		if st.Notes%50 == 0 {
			log.Info("донесено", "заметок", st.Notes, "комментариев", st.Comments,
				"за", time.Since(started).Truncate(time.Second))
		}
	}
	log.Info("добор закончен", "заметок", st.Notes, "комментариев", st.Comments,
		"подписей возвращено", st.Signed,
		"нет на площадке", st.SkipAbsent, "тред непуст", st.SkipFilled,
		"обращений в ребро", st.EdgeAddr, "без ребра", st.EdgeNone,
		"за", time.Since(started).Truncate(time.Second))
	return st, nil
}

// restoredID — ключ площадки для строки архива без идентификатора сайта.
//
// Отображение детерминированное и обратимое: |id архива| у комментария это
// note_id*1000 + номер в треде, поэтому реплики одной заметки идут подряд и в
// хронологии — ровно то, на чём держится материализованный путь.
func restoredID(archiveID int64) int64 {
	if archiveID >= 0 {
		return archiveID
	}
	return platform.RestoredIDBase - archiveID
}

// ---------------------------------------------------------------- чтение архива

func readNicks(ctx context.Context, src *sql.DB) (map[int64]string, error) {
	nicks := map[int64]string{}
	err := scanIDs(ctx, src, `SELECT id, name FROM users`, "анкеты архива",
		func(id int64, s string) { nicks[id] = s })
	return nicks, err
}

func restoredNoteIDs(ctx context.Context, src *sql.DB, opt RestoredOptions) ([]int64, error) {
	var out []int64
	err := scanIDs(ctx, src,
		`SELECT DISTINCT note_id, '' FROM comments WHERE id < 0 ORDER BY note_id`,
		"заметки восстановленного", func(id int64, _ string) {
			if opt.OnlyNote == 0 || id == opt.OnlyNote {
				out = append(out, id)
			}
		})
	return out, err
}

// copyRestoredUsers заводит анкеты третьей полосы — ники 2010 года, не
// разрешившиеся ни в одну анкету сайта.
//
// Заводятся они все сразу, а не по мере надобности: строка стоит десятков байт,
// зато у заметки и у комментария появляется настоящий автор, а не снимок ника в
// колонке. Отсюда же берётся то, ради чего это делается по-человечески:
// ветерана, узнавшего свой ник, можно привязать к такой тени приглашением
// (`platform invite -bind`), и весь его след 2010 года станет своим.
func copyRestoredUsers(ctx context.Context, src *sql.DB, pool *pgxpool.Pool,
	opt RestoredOptions, st *RestoredStats, log *slog.Logger) error {
	rows, err := src.QueryContext(ctx, `SELECT id, name FROM users WHERE id < 0 ORDER BY id DESC`)
	if err != nil {
		return fmt.Errorf("анкеты восстановленного: %w", err)
	}
	defer rows.Close()

	var batch [][]any
	for rows.Next() {
		var id int64
		var nick string
		if err := rows.Scan(&id, &nick); err != nil {
			return fmt.Errorf("анкеты восстановленного: %w", err)
		}
		if nick = strings.TrimSpace(nick); nick == "" {
			continue
		}
		batch = append(batch, []any{restoredID(id), nick, int16(platform.KindShadow)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("анкеты восстановленного: %w", err)
	}
	if opt.DryRun {
		st.Users = len(batch)
		return nil
	}
	for _, u := range batch {
		tag, err := pool.Exec(ctx,
			`INSERT INTO users (id, nick, kind) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
			u...)
		if err != nil {
			return fmt.Errorf("перенос анкеты %v: %w", u[0], err)
		}
		st.Users += int(tag.RowsAffected())
	}
	log.Info("анкеты восстановленного", "новых", st.Users, "всего в дампе", len(batch))
	return nil
}

// target — то, что площадка уже знает о заметке.
type target struct {
	comments  int
	author    int64
	anonymous bool
}

func readTargets(ctx context.Context, pool *pgxpool.Pool, ids []int64) (map[int64]target, error) {
	out := make(map[int64]target, len(ids))
	rows, err := pool.Query(ctx,
		`SELECT id, comment_count, coalesce(author_id, 0), anonymous FROM notes WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("заметки площадки: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var t target
		if err := rows.Scan(&id, &t.comments, &t.author, &t.anonymous); err != nil {
			return nil, fmt.Errorf("заметки площадки: %w", err)
		}
		out[id] = t
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- тред

// readRestoredThread собирает тред одной заметки: ключи третьей полосы, рёбра из
// обращений, пути. Возвращает строки COPY в порядке возрастания id и время
// последней реплики.
func readRestoredThread(ctx context.Context, stmt *sql.Stmt, noteID, noteAuthor int64,
	nicks map[int64]string, st *RestoredStats) ([][]any, time.Time, error) {
	rows, err := stmt.QueryContext(ctx, noteID)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("тред заметки %d: %w", noteID, err)
	}
	defer rows.Close()

	var (
		out   [][]any
		last  time.Time
		paths = map[int64]string{}
		// Участники треда: нормализованный ник → его последняя реплика. Автор
		// заметки входит сюда с нулём — обращение к нему разбирается и опознаётся,
		// но ребра не даёт: ответить в корень треда и так значит ответить ему.
		seen = map[string]int64{}
	)
	if nick := nicks[noteAuthor]; nick != "" {
		seen[normNick(nick)] = 0
	}
	for rows.Next() {
		var (
			id, author int64
			body, at   string
		)
		if err := rows.Scan(&id, &author, &body, &at); err != nil {
			return nil, time.Time{}, fmt.Errorf("тред заметки %d: %w", noteID, err)
		}
		published, err := parseTime(at)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("комментарий %d: %w", id, err)
		}
		key := restoredID(id)
		if key >= platform.IDBandLimit {
			return nil, time.Time{}, fmt.Errorf("комментарий %d: ключ %d вышел за полосу восстановленного", id, key)
		}

		parent, source := int64(0), platform.ReplyNone
		if rest, nick, ok := cutAddress2010(body, seen); ok {
			if to := seen[normNick(nick)]; to != 0 {
				parent, source = to, platform.ReplyPrefix
				body = rest
				st.Trimmed++
			}
		}
		if source == platform.ReplyPrefix {
			st.EdgeAddr++
		} else {
			st.EdgeNone++
		}

		path, err := platform.ChildPath(platform.ClampParent(paths[parent]), key)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("путь комментария %d: %w", id, err)
		}
		paths[key] = path
		branch := int64(0)
		if root, err := platform.BranchRootID(path); err == nil && root != key {
			branch = root
		}
		out = append(out, []any{
			key, noteID, nullID(restoredID(author)), "", body,
			nullID(branch), nullID(parent), int16(source), path,
			int16(platform.PathDepth(path)), published,
		})
		if published.After(last) {
			last = published
		}
		if nick := nicks[author]; nick != "" {
			seen[normNick(nick)] = key
		}
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("тред заметки %d: %w", noteID, err)
	}
	return out, last, nil
}

// ---------------------------------------------------------------- обращение 2010

// normNick — ник для сравнения: регистр и «ё» в 2010-м писали как придётся.
// Отображение рунно однозначное (ToLower и ё→е дают ровно одну руну на руну),
// поэтому позиции в нормализованной строке совпадают с позициями в исходной, и
// срезать обращение можно по индексу нормализованной.
func normNick(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.TrimSpace(s) {
		r = unicode.ToLower(r)
		if r == 'ё' {
			r = 'е'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// addressWord — слово, с которого начиналось обращение в 2010 году.
const addressWord = "для"

// maxNameParens — потолок длины «(Имя)» в рунах. Скобки в теле реплики бывают и
// сами по себе («Для Мари (кстати, ты была права) …»), поэтому вырезается только
// короткая вставка сразу за ником — как её и писал сайт.
const maxNameParens = 40

// cutAddress2010 разбирает обращение эпохи 2010 — «Для Ник (Имя) текст» — и
// возвращает остаток без обращения, опознанный ник и признак разбора.
//
// Ник ищется не по виду («слово после Для»), а по списку УЧАСТНИКОВ треда, и
// берётся самый длинный подошедший. Иначе «Для Димон_Таибычев …» досталось бы
// «Димону», а «Для НюШ@ …» — участнику с ником «Н»: ники 2010 года сплошь
// приставки друг друга, и разбор по виду ошибается молча.
//
// Пустой остаток обращением не считается: реплика «Для Ник» целиком — это
// обращение и есть, и срезать из неё нечего.
func cutAddress2010(body string, seen map[string]int64) (rest, nick string, ok bool) {
	src := []rune(strings.TrimLeft(body, " \t\r\n"))
	low := []rune(normNick(string(src)))
	head := []rune(addressWord)
	if len(low) <= len(head) || string(low[:len(head)]) != addressWord {
		return body, "", false
	}
	i := len(head)
	if !unicode.IsSpace(low[i]) {
		return body, "", false
	}
	for i < len(low) && unicode.IsSpace(low[i]) {
		i++
	}
	tail := low[i:]

	var best []rune
	for key := range seen {
		k := []rune(key)
		if len(k) == 0 || len(k) <= len(best) || !hasPrefixRunes(tail, k) {
			continue
		}
		// Граница ника обязана быть границей слова, иначе ник «Кот» съел бы
		// начало «Котёнок» у соседа по треду.
		if r := tail[len(k):]; len(r) > 0 && (unicode.IsLetter(r[0]) || unicode.IsDigit(r[0])) {
			continue
		}
		best = k
	}
	if len(best) == 0 {
		return body, "", false
	}
	j := i + len(best)
	for j < len(src) && unicode.IsSpace(src[j]) {
		j++
	}
	// «(Имя)» — поле анкеты, которое сайт подставлял в обращение. Оно часть
	// обращения, а не текста, и уезжает вместе с ним.
	if j < len(src) && src[j] == '(' {
		if k := closingParen(src, j); k > 0 {
			j = k + 1
			for j < len(src) && unicode.IsSpace(src[j]) {
				j++
			}
		}
	}
	if j >= len(src) {
		return body, "", false
	}
	return string(src[j:]), string(best), true
}

func hasPrefixRunes(s, prefix []rune) bool {
	if len(prefix) > len(s) {
		return false
	}
	for i, r := range prefix {
		if s[i] != r {
			return false
		}
	}
	return true
}

// closingParen — позиция закрывающей скобки короткой вставки; -1, если её нет
// или вставка длиннее имени.
func closingParen(src []rune, open int) int {
	for k := open + 1; k < len(src) && k-open <= maxNameParens; k++ {
		if src[k] == ')' {
			return k
		}
	}
	return -1
}

// ---------------------------------------------------------------- запись

// writeRestored пишет тред одной заметки одной транзакцией.
//
// Порция — заметка целиком по той же причине, что и у раскатки архива: тред это
// связная структура, и наполовину доехавший тред показывается неправильно весь.
// Здесь к этому добавляются счётчик и подпись самой заметки — им тоже незачем
// расходиться с содержимым треда.
func writeRestored(ctx context.Context, pool *pgxpool.Pool, noteID int64,
	rows [][]any, last time.Time, signAs int64) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("заметка %d: %w", noteID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"comments"},
		[]string{"id", "note_id", "author_id", "author_display", "body",
			"branch_root_id", "reply_to_id", "reply_source", "path", "depth", "published_at"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("тред заметки %d: %w", noteID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE notes SET comment_count = $2, last_comment_at = $3 WHERE id = $1`,
		noteID, len(rows), last); err != nil {
		return fmt.Errorf("счётчик заметки %d: %w", noteID, err)
	}
	if signAs != 0 {
		// Условие повторяет решение вызывающего и держит его в той же
		// транзакции: чужую анонимность заметки добор не снимает.
		if _, err := tx.Exec(ctx,
			`UPDATE notes SET author_id = $2, anonymous = false
			  WHERE id = $1 AND author_id IS NULL AND anonymous`, noteID, signAs); err != nil {
			return fmt.Errorf("подпись заметки %d: %w", noteID, err)
		}
	}
	// Обход дерева по мобильной версии этим тредам противопоказан: на сайте их
	// нет вовсе, а очередь reply-scan берёт заметки с непустым тредом, сначала
	// ни разу не смотренные, — то есть вместо живых заметок ушла бы на триста
	// с лишним заведомо пустых страниц, в закрывающееся окно.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ingest_state (note_id, reply_scan_skip) VALUES ($1, true)
		ON CONFLICT (note_id) DO UPDATE SET reply_scan_skip = true`, noteID); err != nil {
		return fmt.Errorf("отметка обхода заметки %d: %w", noteID, err)
	}
	return tx.Commit(ctx)
}
