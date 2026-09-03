package platform

// Настоящее дерево ответов.
//
// Живое зеркало знает адресата только по обращению «Ник, …» и разрешает его в
// ПОСЛЕДНЮЮ реплику этого человека в заметке. Это угадывание, и точность у него
// примерно половина: 16.08.2026 в заметке 313000 реплика ПростоТы 07:26:54
// привязалась к комментарию Livesey 07:20:16 вместо настоящего 07:07:15 — ветка
// выросла не там, а у второго комментария пропали ответы.
//
// Настоящее дерево отдаёт мобильная версия сайта, и точность там 92 % (замер по
// архиву 14.08.2026 — 98,7 % с учётом восстановленных пар). Здесь оно
// применяется: рёбра переставляются, пути перекладываются, а обращение
// срезается из тела ровно там, где ребро появилось впервые, — иначе показ
// дорисует ник поверх уже написанного и выйдет «Ник, Ник, …».

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ReplyTreeStats — что сделал один проход по заметке.
type ReplyTreeStats struct {
	Total   int // комментариев в заметке
	Known   int // из них найдено в мобильном дереве
	Edges   int // переставлено рёбер
	Paths   int // переложено путей
	Trimmed int // снято обращений из тела
}

type treeRow struct {
	id      int64
	replyTo int64
	source  ReplySource
	body    string
	path    string
	nick    string
}

// ApplyReplyTree переставляет рёбра заметки по дереву мобильной версии.
//
// tree — «id комментария → id того, кому он отвечает», 0 означает реплику
// верхнего уровня. Комментарии, которых в дереве нет (сайт их уже удалил, а у
// нас они сохранились), остаются как есть: молчание мобильной страницы не
// повод рвать ребро.
//
// Всё в одной транзакции: дерево — это связная структура, и заметка, у которой
// половина путей новая, а половина старая, показывается неправильно вся.
func (p *Platform) ApplyReplyTree(ctx context.Context, noteID int64, tree map[int64]int64) (ReplyTreeStats, error) {
	var st ReplyTreeStats
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // после Commit это no-op

	rows, err := tx.Query(ctx, `
		SELECT c.id, coalesce(c.reply_to_id, 0), c.reply_source, c.body, c.path,
		       coalesce(nullif(u.nick, ''), nullif(c.author_display, ''), '')
		  FROM comments c
		  LEFT JOIN users u ON u.id = c.author_id
		 WHERE c.note_id = $1
		 ORDER BY c.id`, noteID)
	if err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	var list []treeRow
	for rows.Next() {
		var r treeRow
		if err := rows.Scan(&r.id, &r.replyTo, &r.source, &r.body, &r.path, &r.nick); err != nil {
			rows.Close()
			return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	st.Total = len(list)

	byID := make(map[int64]*treeRow, len(list))
	parent := make(map[int64]int64, len(list))
	for i := range list {
		byID[list[i].id] = &list[i]
		parent[list[i].id] = list[i].replyTo
	}
	// НАШИ РЕПЛИКИ НА САЙТЕ ЗОВУТСЯ ИНАЧЕ, и дерево называет именно те номера.
	//
	// Написанное здесь уезжает на НГС (platngs) и получает ТАМ свой номер, а у
	// нас лежит нативной строкой со своим. Дерево мобильной версии говорит
	// номерами САЙТА — значит ответ живого человека нашей реплике приезжает с
	// родителем, которого в этой заметке нет вовсе, и без перевода такой ответ
	// вставал в КОРЕНЬ, теряя адресата.
	//
	// Живой приём при этом отрабатывал ПРАВИЛЬНО: он ищет адресата по обращению
	// «Ник, …» (AddresseeInNote) и нашу строку находит, — а обход дерева следом
	// заменял верное ребро несуществующим номером. Со стороны это и выглядело
	// как «прикрепилось, а потом отвязалось» (жалоба владельца 03.09.2026;
	// замер по бою: пять реплик в заметках 313146 и 313158).
	own, err := ngsOwnComments(ctx, tx, list)
	if err != nil {
		return st, err
	}
	for id, p := range tree {
		if _, ok := byID[id]; !ok {
			// Комментарий есть на сайте, но не у нас — не наше дело. Сюда же
			// попадают НАШИ уехавшие реплики: у них номер сайта, и ребро им
			// сайтом не правится. Так и надо — оно поставлено ЗДЕСЬ и знает
			// больше: родителя, которого на НГС нет, вынос кладёт в корень
			// треда, а у нас эта реплика отвечает кому следует.
			continue
		}
		st.Known++
		if native, ok := own[p]; ok {
			p = native
		}
		parent[id] = p
	}

	// Пути перекладываются ОТ РОДИТЕЛЯ К РЕБЁНКУ, а не одним проходом по
	// возрастанию id.
	//
	// Прежде проход опирался на «ответ всегда новее того, кому отвечает» —
	// верное на НГС, где id растут по времени, и НЕВЕРНОЕ у нас: наша реплика
	// лежит в нативной полосе, то есть её номер больше номера любого ответа,
	// который придёт на неё с сайта. Родитель оказывался «в будущем», путь его
	// не брался — и переведённое строкой выше ребро всё равно легло бы в корень.
	// Тот же класс ошибки, что уже трижды ловили на курсорах: порядок по id
	// верен только ВНУТРИ полосы.
	paths := make(map[int64]string, len(list))
	laying := make(map[int64]bool, len(list))
	var layout func(id int64) (string, error)
	layout = func(id int64) (string, error) {
		if p, ok := paths[id]; ok {
			return p, nil
		}
		laying[id] = true
		defer delete(laying, id)
		pp := ""
		// laying[pid] — кольцо. В данных его быть не может (всякое ребро
		// указывает назад по времени, и сайтовое, и наше), но без проверки
		// цена ошибки — бесконечная рекурсия на боевом треде, то есть упавший
		// демон. Кольцо разрывается РАСКЛАДКОЙ: путь становится корневым, а
		// ребро остаётся настоящим — то же разделение, что у ClampParent.
		if pid := parent[id]; pid != 0 && !laying[pid] {
			if _, ok := byID[pid]; ok {
				got, err := layout(pid)
				if err != nil {
					return "", err
				}
				pp = got
			}
		}
		np, err := ChildPath(ClampParent(pp), id)
		if err != nil {
			return "", fmt.Errorf("путь комментария %d: %w", id, err)
		}
		paths[id] = np
		return np, nil
	}
	for i := range list {
		if _, err := layout(list[i].id); err != nil {
			return st, err
		}
	}

	// moved — тронул ли проход хоть одну ВИДИМУЮ строку. Нужен ради звонка в
	// конце: см. ringLive перед Commit.
	var moved bool
	for i := range list {
		r := &list[i]
		newParent := parent[r.id]
		newPath := paths[r.id]
		newBody := r.body
		newSource := r.source
		if _, inTree := tree[r.id]; inTree {
			newSource = ReplyMobileTree
		}
		// Обращение срезается ровно тогда, когда ребро появилось: у строк, где
		// зеркало адресата не нашло, «Ник, » так и остался в теле. Ник берём у
		// НОВОГО адресата — по нему и проверяем, что срезаем именно обращение,
		// а не начало фразы.
		if newParent != 0 && r.replyTo == 0 {
			if target, ok := byID[newParent]; ok {
				if cut, done := TrimAddress(newBody, target.nick); done {
					newBody = cut
					st.Trimmed++
				}
			}
		}
		if newParent == r.replyTo && newPath == r.path && newBody == r.body && newSource == r.source {
			continue
		}
		if newParent != r.replyTo {
			st.Edges++
		}
		if newPath != r.path {
			st.Paths++
		}
		branchRoot := int64(0)
		if root, err := BranchRootID(newPath); err == nil && root != r.id {
			branchRoot = root
		}
		// moved_at ставится ТОЙ ЖЕ правкой, и это не журнал, а адрес: по нему
		// живой добор досылает строку тем, у кого она уже нарисована на старом
		// месте (web/fresh.go). Без отметки открытая страница остаётся с
		// угаданным деревом до перезагрузки — с чего эта колонка и заведена
		// (миграция 0015). Отметка одна на всю транзакцию: now() в Postgres —
		// время НАЧАЛА транзакции, поэтому переезд, тронувший ветку целиком,
		// приезжает на страницу одной порцией и в правильном порядке.
		//
		// Ставится она только тогда, когда изменилось ВИДИМОЕ: место, адресат
		// или тело. Смена одного `reply_source` («ребро теперь настоящее»)
		// показу безразлична, а отмечать её значило бы прогнать через живой
		// канал весь тред при первом же обходе заметки — ради строк, которые на
		// экране не сдвинутся ни на пиксель.
		shown := newParent != r.replyTo || newPath != r.path || newBody != r.body
		moved = moved || shown
		if _, err := tx.Exec(ctx, `
			UPDATE comments
			   SET reply_to_id = $2, reply_source = $3, path = $4, depth = $5,
			       branch_root_id = $6, body = $7,
			       moved_at = CASE WHEN $8 THEN now() ELSE moved_at END
			 WHERE id = $1`,
			r.id, nullID(newParent), newSource, newPath, PathDepth(newPath),
			nullID(branchRoot), newBody, shown); err != nil {
			return st, fmt.Errorf("правка комментария %d: %w", r.id, err)
		}
	}
	// Звонок живому каналу — той же транзакцией, как и у записи факта (live.go).
	//
	// Без него отметка moved_at доходила до открытой страницы только ЗАОДНО со
	// следующей репликой: добор идёт по сигналу, а сигналы рождались лишь из
	// events. Разговор затихал — и человек до перезагрузки смотрел на ветку,
	// выросшую по догадке зеркала, хотя в базе она уже стояла на месте (жалоба
	// владельца 24.08.2026: обновил — все ветки на месте). Механика переезда
	// была доведена до страницы наполовину; звонок — вторая половина.
	//
	// Событием этот переезд НЕ становится, и это не экономия: events — журнал
	// того, о чём уместно сказать участнику, а «ветка встала на своё место» не
	// повод ни для повода, ни для колокольчика. Звонок здесь говорит ровно то,
	// что значит: «в этой заметке изменилось показываемое, сходи посмотри».
	if moved {
		ringLive(ctx, tx)
	}
	if err := tx.Commit(ctx); err != nil {
		return st, fmt.Errorf("дерево заметки %d: %w", noteID, err)
	}
	return st, nil
}

// ngsOwnComments — перевод «номер на НГС → наш номер» для реплик заметки,
// уехавших на сайт. Обратная сторона NGSReplyTarget: тот переводит наш номер в
// сайтовый перед отправкой, этот — сайтовый в наш при возврате дерева.
//
// Спрашиваются только НАТИВНЫЕ номера, как в NGSSentObjects, и по той же
// причине: зеркальной строки в очереди не бывает по построению — уносим мы
// своё, — а у заметки, где своих реплик нет вовсе, запроса не будет ни одного.
// Замер 03.09.2026: своё сказано в 422 заметках из 117 818, то есть обход
// дерева платит за перевод в одной заметке из трёхсот. Вход — уникальный
// индекс ngs_outbox_object по паре (kind, object_id).
//
// Номер берётся у ЛЮБОЙ строки, а не только у sent: он проставляется тогда,
// когда реплику удалось опознать на странице сайта (findPosted либо эхо), а
// сайт отвечает 500 и на ПРИНЯТУЮ реплику — то есть у строки с failed на НГС
// сплошь и рядом стоит живая копия, и ответы приходят именно ей.
func ngsOwnComments(ctx context.Context, q querier, list []treeRow) (map[int64]int64, error) {
	native := make([]int64, 0, len(list))
	for _, r := range list {
		if IsNative(r.id) {
			native = append(native, r.id)
		}
	}
	if len(native) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT ngs_id, object_id FROM ngs_outbox
		 WHERE kind = $1 AND object_id = ANY($2) AND ngs_id <> ''`, NGSComment, native)
	if err != nil {
		return nil, fmt.Errorf("наши реплики на НГС: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]int64, len(native))
	for rows.Next() {
		var (
			ngsID string
			ours  int64
		)
		if err := rows.Scan(&ngsID, &ours); err != nil {
			return nil, fmt.Errorf("наши реплики на НГС: %w", err)
		}
		// Номер сайта нечислом не бывает: его вычитывает findPosted из анкора
		// страницы. Но колонка — свободный текст, и падать всей заметкой из-за
		// одной странной строки незачем.
		id, err := strconv.ParseInt(ngsID, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		out[id] = ours
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("наши реплики на НГС: %w", err)
	}
	return out, nil
}

// legacyAddressRe — обращение, каким его рисовал сайт в первые месяцы архива:
// «Для [b][i]Ник[/i][/b] текст» (комментарии начинаются 31.10.2013). Это
// разметка САЙТА, а не человека, поэтому ник в проверке не участвует вовсе:
// форма однозначна сама по себе, а в теле стоит ник НА ТУ ДАТУ — сверять его с
// нынешним значило бы промахиваться на каждом переименовании.
var legacyAddressRe = regexp.MustCompile(`^\s*Для \[b\]\[i\].{1,64}?\[/i\]\[/b\][ \t]*`)

// TrimLegacyAddress срезает обращение образца 2013 года. Пара к TrimAddress:
// мысль одна («адресат — ребро, а не текст»), эпохи сайта разные.
func TrimLegacyAddress(body string) (string, bool) {
	loc := legacyAddressRe.FindStringIndex(body)
	if loc == nil {
		return body, false
	}
	rest := body[loc[1]:]
	if strings.TrimSpace(rest) == "" {
		// «Для Ник» и больше ничего: пустая реплика хуже реплики с обращением.
		return body, false
	}
	return rest, true
}

// TrimAddress срезает обращение «Ник, » с начала тела, если оно там есть.
//
// Ник сверяется с настоящим ником адресата: без этого «Кстати, я тоже» лишилось
// бы первого слова. Сравнение без учёта регистра — люди пишут обращение как
// придётся, а сайт подставляет его с заглавной.
//
// Экспортируется ради второго писателя — импорта архива: там ребро проставляется
// не по одной заметке, а сразу по всем, и правило снятия обращения обязано быть
// тем же самым. Разъехавшись, они дали бы на одной странице «Ник, Ник, …» рядом
// с чистыми репликами.
func TrimAddress(body, nick string) (string, bool) {
	nick = strings.TrimSpace(nick)
	if nick == "" || body == "" {
		return body, false
	}
	if !strings.HasPrefix(strings.ToLower(body), strings.ToLower(nick)) {
		return body, false
	}
	rest := body[len(nick):]
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if !strings.HasPrefix(rest, ",") {
		return body, false
	}
	rest = strings.TrimLeftFunc(rest[1:], unicode.IsSpace)
	if rest == "" {
		// «Ник,» и больше ничего: пустая реплика хуже реплики с обращением.
		return body, false
	}
	return rest, true
}

// ---------------------------------------------------------------- дисциплина обхода

// ReplyScanDue — заметки, которым пора уточнить дерево: сперва не смотренные
// ни разу, дальше самые давние. Отброшенные (reply_scan_skip) не возвращаются
// никогда — их решение принимает человек.
func (p *Platform) ReplyScanDue(ctx context.Context, limit int) ([]int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT n.id
		  FROM notes n
		  LEFT JOIN ingest_state s ON s.note_id = n.id
		 WHERE n.id < $1 AND n.comment_count > 0
		   AND coalesce(s.reply_scan_skip, false) = false
		 ORDER BY s.reply_scan_at NULLS FIRST, n.id DESC
		 LIMIT $2`, NativeIDBase, limit)
	if err != nil {
		return nil, fmt.Errorf("очередь обхода дерева: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("очередь обхода дерева: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ReplyScanFresh — вторая очередь: ЖИВЫЕ треды, у которых появились реплики
// после последнего обхода. Отдельная от ReplyScanDue, и это не удобство.
//
// Та очередь устроена под добор истории: «сперва не смотренные, дальше самые
// давние». Заметку из неё смотрят ОДИН раз, а дописанные позже реплики остаются
// с рёбрами, которые зеркало угадало по обращению (love.Addressees), — до
// второго круга по 117 тысячам заметок дело не дойдёт никогда. Пока НГС не
// принимал комментариев, это было неважно; с 20.08.2026 принимает.
//
// Три условия, и каждое отвечает за своё: fresh — тред ещё живой (мёртвый
// дообходит историческая очередь), reply_scan_at < last_comment_at — с прошлого
// раза что-то дописали, gap — не чаще чем раз в столько-то, иначе на бойком
// треде обход шёл бы на каждую реплику. Вход по индексу notes_fresh_scan
// (миграция 0013): без него это seq scan по всем заметкам раз в минуту.
func (p *Platform) ReplyScanFresh(ctx context.Context, limit int, fresh, gap time.Duration) ([]int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT n.id
		  FROM notes n
		  LEFT JOIN ingest_state s ON s.note_id = n.id
		 WHERE n.id < $1 AND n.comment_count > 0
		   AND n.last_comment_at > $2
		   AND coalesce(s.reply_scan_skip, false) = false
		   AND (s.reply_scan_at IS NULL
		        OR (s.reply_scan_at < n.last_comment_at AND s.reply_scan_at < $3))
		 ORDER BY n.last_comment_at DESC
		 LIMIT $4`, NativeIDBase, time.Now().Add(-fresh), time.Now().Add(-gap), limit)
	if err != nil {
		return nil, fmt.Errorf("очередь свежих тредов: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("очередь свежих тредов: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MarkReplyScan отмечает проход по заметке. Три подряд неудачи гасят заметку
// насовсем: страницу, которую сайт не отдаёт (на 848 репликах он отвечал 500),
// незачем дёргать вечно — это плата за каждый обход и повод для тревоги.
func (p *Platform) MarkReplyScan(ctx context.Context, noteID int64, ok bool) error {
	if ok {
		_, err := p.pool.Exec(ctx, `
			INSERT INTO ingest_state (note_id, reply_scan_at, reply_scan_fails)
			VALUES ($1, now(), 0)
			ON CONFLICT (note_id) DO UPDATE SET reply_scan_at = now(), reply_scan_fails = 0`, noteID)
		return wrapf(err, "отметка обхода заметки %d", noteID)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO ingest_state (note_id, reply_scan_at, reply_scan_fails)
		VALUES ($1, now(), 1)
		ON CONFLICT (note_id) DO UPDATE
		   SET reply_scan_at = now(),
		       reply_scan_fails = ingest_state.reply_scan_fails + 1,
		       reply_scan_skip = ingest_state.reply_scan_fails + 1 >= 3`, noteID)
	return wrapf(err, "отметка неудачи обхода заметки %d", noteID)
}
