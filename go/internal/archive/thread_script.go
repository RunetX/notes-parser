package archive

// Архивный тред как СЦЕНАРИЙ реплея: кто, когда, кому и что сказал.
//
// Нужен затем, что эмуляцию не с чем сравнивать. Реплей поднимает настоящий
// разговор многолетней давности, прогоняет на его месте жителей и смотрит,
// сошлось ли; значит оригинал обязан приехать в том виде, в каком его видел
// участник — по времени, с настоящими рёбрами и с теми никами, которыми людей
// звали ТОГДА.
//
// От `lovegw export` отличается ровно этим. Тот отдаёт дерево по parent_id, а
// parent_id указывает на корень ветки и адресата угадывает в 34,8 % случаев
// (шапка addressee.go): сценарий, собранный по нему, был бы разговором, которого
// не было, и всякая метрика против него врала бы в нашу пользу.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"lovegw/internal/love"
)

// Источники ребра «кому отвечают», в порядке убывания достоверности. Источник
// едет в сценарий не для отчётности: реплика, чьё ребро всего лишь угадано по
// корню ветки, не годится в мерку — по ней нельзя судить, верно ли выбрал
// адресата житель.
const (
	EdgeTree      = "tree"      // настоящее ребро мобильного дерева (comment_reply)
	EdgeAddressee = "addressee" // разрешён адресат-человек → его последняя реплика до этой
	EdgeParent    = "parent"    // корень ветки (parent_id) — догадка
	EdgeRoot      = "root"      // ответ самой заметке
)

// ThreadScript — тред, разложенный по времени.
type ThreadScript struct {
	NoteID       int64
	Note         ScriptNote
	Comments     []ScriptComment // по возрастанию времени
	Participants []ScriptActor   // по времени первой реплики
	Undated      int             // реплик без времени: в сценарий не попали
	Edges        map[string]int  // сколько рёбер каким источником
}

// ScriptNote — заметка, с которой всё началось.
type ScriptNote struct {
	AuthorID    int64 // 0 — аноним
	AuthorNick  string
	Text        string
	PublishedAt time.Time
}

// ScriptComment — одна реплика оригинала.
type ScriptComment struct {
	ID          int64
	AuthorID    int64
	AuthorNick  string // ник НА ТУ ДАТУ, а не сегодняшний
	Text        string // тело без обращения
	Address     string // срезанное обращение, пусто — обращения не было
	PublishedAt time.Time
	ReplyTo     int64         // id реплики, которой отвечают; 0 — заметке
	TargetID    int64         // author_id адресата; 0 — автор заметки либо никто
	Edge        string        // откуда ребро (Edge*)
	Delay       time.Duration // от реплики-адресата, а у корневых — от заметки
}

// ScriptActor — участник треда.
type ScriptActor struct {
	UserID   int64
	Nick     string
	Comments int
	First    time.Time // когда впервые заговорил в этом треде
	// Gender — "male" | "female" | "" (неизвестен). Нужен кубику: разговор у
	// живых структурно разнополый, и мужчина отвечает мужчине вдвое реже
	// случайного (замер 29.08.2026). Пустой пол рычага не даёт вовсе.
	Gender string
}

// scriptRow — строка треда до разрешения рёбер.
type scriptRow struct {
	ScriptComment
	parentID    int64
	treeTo      int64
	addresseeID int64
}

// LoadThreadScript собирает сценарий одного треда.
//
// Реплики без времени в сценарий НЕ попадают, но считаются (Undated): часы
// реплея дискретно-событийные, и реплику, которую некуда поставить, пришлось бы
// либо выдумать, либо уронить прогон. Решает вызывающий — для сравнения с
// оригиналом дырявый сценарий не годится, а для стенда промпта сгодится.
func (s *Store) LoadThreadScript(ctx context.Context, noteID int64) (*ThreadScript, error) {
	sc := &ThreadScript{NoteID: noteID, Edges: map[string]int{}}
	if err := s.loadScriptNote(ctx, sc); err != nil {
		return nil, err
	}
	rows, err := s.loadScriptRows(ctx, sc)
	if err != nil {
		return nil, err
	}
	resolveEdges(sc, rows)
	if err := s.fillScriptNicks(ctx, sc); err != nil {
		return nil, err
	}
	sc.Participants = participantsOf(sc.Comments)
	if err := s.fillScriptGenders(ctx, sc); err != nil {
		return nil, err
	}
	return sc, nil
}

// loadScriptNote читает саму заметку.
func (s *Store) loadScriptNote(ctx context.Context, sc *ThreadScript) error {
	var (
		authorID sql.NullInt64
		pub      sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT author_id, text, published_at FROM notes WHERE id = ?`, sc.NoteID).
		Scan(&authorID, &sc.Note.Text, &pub)
	if err == sql.ErrNoRows {
		return fmt.Errorf("заметки %d нет в архиве", sc.NoteID)
	}
	if err != nil {
		return fmt.Errorf("чтение заметки %d: %w", sc.NoteID, err)
	}
	// Без времени заметки у сценария нет нуля: задержка первой реплики — это
	// «через сколько человек пришёл», и подменять её первой попавшейся отметкой
	// значит выдумать ровно ту величину, ради которой реплей и затевается.
	if !pub.Valid || pub.String == "" {
		return fmt.Errorf("у заметки %d нет времени публикации — сценарий не построить", sc.NoteID)
	}
	if sc.Note.PublishedAt, err = parseArchiveTime(pub.String); err != nil {
		return fmt.Errorf("время заметки %d: %w", sc.NoteID, err)
	}
	sc.Note.AuthorID = authorID.Int64
	return nil
}

// loadScriptRows читает реплики треда по возрастанию времени.
func (s *Store) loadScriptRows(ctx context.Context, sc *ThreadScript) ([]scriptRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.author_id, c.text, c.published_at, c.parent_id,
		       coalesce(r.reply_to, 0), coalesce(ad.addressee_id, 0)
		  FROM comments c
		  LEFT JOIN comment_reply     r  ON r.comment_id  = c.id
		  LEFT JOIN comment_addressee ad ON ad.comment_id = c.id
		 WHERE c.note_id = ?
		 ORDER BY c.published_at, c.id`, sc.NoteID)
	if err != nil {
		return nil, fmt.Errorf("чтение треда %d: %w", sc.NoteID, err)
	}
	defer rows.Close()

	var list []scriptRow
	for rows.Next() {
		var (
			r    scriptRow
			when sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.AuthorID, &r.Text, &when, &r.parentID,
			&r.treeTo, &r.addresseeID); err != nil {
			return nil, err
		}
		if !when.Valid || when.String == "" {
			sc.Undated++
			continue
		}
		if r.PublishedAt, err = parseArchiveTime(when.String); err != nil {
			sc.Undated++
			continue
		}
		orig := r.Text
		r.Text = love.TrimAddressPrefix(orig)
		r.Address = rawAddress(orig, r.Text)
		list = append(list, r)
	}
	return list, rows.Err()
}

// rawAddress — обращение так, как его НАПИСАЛ автор, с регистром.
//
// love.AddressPrefix для этого не годится: он приводит ник к нижнему регистру,
// и правильно делает — там обращение служит ключом поиска, а ключ обязан быть
// нечувствительным к регистру. Здесь же обращение это ИМЯ, которое увидит
// модель; «ягода» вместо «Ягода» она приняла бы за манеру писать людей со
// строчной и переняла бы её.
//
// Считается вычитанием: что осталось от тела до срезанного остатка, то и было
// обращением вместе с запятой.
func rawAddress(orig, trimmed string) string {
	if len(trimmed) >= len(orig) {
		return ""
	}
	head := strings.TrimSpace(orig[:len(orig)-len(trimmed)])
	return strings.TrimSpace(strings.TrimSuffix(head, ","))
}

// resolveEdges проставляет каждой реплике адресата и задержку.
func resolveEdges(sc *ThreadScript, list []scriptRow) {
	pos := make(map[int64]int, len(list))
	for i, r := range list {
		pos[r.ID] = i
	}
	said := make(map[int64][]int, len(list)) // author_id → позиции его реплик
	for i, r := range list {
		said[r.AuthorID] = append(said[r.AuthorID], i)
	}

	sc.Comments = make([]ScriptComment, 0, len(list))
	for i := range list {
		r := &list[i]
		idx, edge := edgeOf(r, i, pos, said)
		r.Edge = edge
		if idx >= 0 {
			t := list[idx]
			r.ReplyTo, r.TargetID = t.ID, t.AuthorID
			r.Delay = r.PublishedAt.Sub(t.PublishedAt)
		} else {
			r.TargetID = sc.Note.AuthorID
			r.Delay = r.PublishedAt.Sub(sc.Note.PublishedAt)
		}
		sc.Edges[edge]++
		sc.Comments = append(sc.Comments, r.ScriptComment)
	}
}

// edgeOf выбирает адресата реплики, стоящей на позиции i, и возвращает его
// ПОЗИЦИЮ в треде (-1 — ответ заметке).
//
// Порядок источников — по достоверности, и у каждого своя проверка на то, что
// адресат вообще МОГ быть адресатом: реплика обязана стоять в этом треде и
// РАНЬШЕ. Ответ на ещё не сказанное — не редкость данных, а признак, что ребро
// приехало не из этого разговора (перенумерация, добор, чужая заметка), и
// принимать его нельзя ни от какого источника.
func edgeOf(r *scriptRow, i int, pos map[int64]int, said map[int64][]int) (int, string) {
	if idx, ok := before(r.treeTo, i, pos); ok {
		return idx, EdgeTree
	}
	// Адресат разрешён как ЧЕЛОВЕК, а не как реплика: берём его последнее слово
	// до этой минуты — это то же правило, по которому зеркало и площадка ищут
	// адресата (love.Addressees), и другого у нас нет.
	if r.addresseeID != 0 && r.addresseeID != r.AuthorID {
		if idx, ok := lastBefore(said[r.addresseeID], i); ok {
			return idx, EdgeAddressee
		}
	}
	if idx, ok := before(r.parentID, i, pos); ok {
		return idx, EdgeParent
	}
	return -1, EdgeRoot
}

// before — позиция реплики id, если она есть в треде и стоит раньше i.
func before(id int64, i int, pos map[int64]int) (int, bool) {
	p, ok := pos[id]
	return p, ok && p < i
}

// lastBefore — последняя позиция из отсортированного списка, меньшая i.
func lastBefore(idxs []int, i int) (int, bool) {
	n := sort.SearchInts(idxs, i)
	if n == 0 {
		return 0, false
	}
	return idxs[n-1], true
}

// fillScriptNicks проставляет никам ТУ дату, а не сегодняшнюю.
//
// Правило нужно потому, что users.name хранит только НЫНЕШНИЙ ник, а люди на
// сайте переименовываются постоянно (у одной анкеты архива их два десятка).
// Сценарий 2016 года, подписанный никами 2026-го, показал бы модели разговор, в
// котором обращения в телах реплик не совпадают ни с одним участником, — и
// первое, чему бы она научилась, это звать людей не по имени.
//
// Источников три, и кладутся они друг на друга по возрастанию точности:
// нынешний ник (регистр верный, имя, возможно, уже другое), затем nick_history
// на месяц заметки (имя верное, но хранится в нижнем регистре), затем — как
// человека звали В ЭТОМ ТРЕДЕ, из обращений с разрешённым адресатом: там и
// написание, и регистр те самые.
func (s *Store) fillScriptNicks(ctx context.Context, sc *ThreadScript) error {
	list := scriptUserIDs(sc)
	if len(list) == 0 {
		return nil
	}
	nick := map[int64]string{}
	if err := s.eachNick(ctx, nick,
		`SELECT id, name FROM users WHERE id IN (`+intList(list)+`)`); err != nil {
		return fmt.Errorf("ники участников: %w", err)
	}
	ym := sc.Note.PublishedAt.Format("2006-01")
	// ORDER BY hits: последним ложится самый обоснованный вариант месяца.
	if err := s.eachNick(ctx, nick,
		`SELECT user_id, nick FROM nick_history
		  WHERE user_id IN (`+intList(list)+`) AND from_ym <= ? AND to_ym >= ?
		  ORDER BY hits`, ym, ym); err != nil {
		return fmt.Errorf("история ников: %w", err)
	}
	for id, form := range nicksInThread(sc.Comments) {
		nick[id] = form
	}

	sc.Note.AuthorNick = nick[sc.Note.AuthorID]
	for i := range sc.Comments {
		sc.Comments[i].AuthorNick = nick[sc.Comments[i].AuthorID]
	}
	return nil
}

// scriptUserIDs — все анкеты треда, включая автора заметки, по возрастанию.
func scriptUserIDs(sc *ThreadScript) []int64 {
	ids := map[int64]bool{}
	for _, c := range sc.Comments {
		ids[c.AuthorID] = true
	}
	if sc.Note.AuthorID != 0 {
		ids[sc.Note.AuthorID] = true
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return list[i] < list[j] })
	return list
}

// eachNick — прочитать пары «id, ник» и положить их поверх уже собранного.
func (s *Store) eachNick(ctx context.Context, dst map[int64]string, query string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			n  string
		)
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if n != "" {
			dst[id] = n
		}
	}
	return rows.Err()
}

// nicksInThread — как участников звали в этом самом разговоре. Считается по
// частоте: разовая описка соседа не должна переименовывать человека на весь
// сценарий.
func nicksInThread(cs []ScriptComment) map[int64]string {
	seen := map[int64]map[string]int{}
	for _, c := range cs {
		if c.Address == "" || c.TargetID == 0 {
			continue
		}
		if seen[c.TargetID] == nil {
			seen[c.TargetID] = map[string]int{}
		}
		seen[c.TargetID][c.Address]++
	}
	out := make(map[int64]string, len(seen))
	for id, forms := range seen {
		best, bestN := "", 0
		for form, n := range forms {
			if n > bestN || (n == bestN && form < best) {
				best, bestN = form, n
			}
		}
		if best != "" {
			out[id] = best
		}
	}
	return out
}

// participantsOf — участники по времени первой реплики.
func participantsOf(cs []ScriptComment) []ScriptActor {
	byID := map[int64]*ScriptActor{}
	var order []int64
	for _, c := range cs {
		a, ok := byID[c.AuthorID]
		if !ok {
			a = &ScriptActor{UserID: c.AuthorID, Nick: c.AuthorNick, First: c.PublishedAt}
			byID[c.AuthorID] = a
			order = append(order, c.AuthorID)
		}
		a.Comments++
	}
	out := make([]ScriptActor, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// ThreadPick — тред, годный в калибровку.
type ThreadPick struct {
	NoteID int64
	Said   int // сколько раз в нём говорил донор
	Total  int // всего реплик
}

// PickCalibrationThreads — треды, где донор сказал не меньше minSaid раз,
// РАВНОМЕРНО по всему их размаху разговорчивости.
//
// Порог minSaid здесь про одно: в треде должны быть точки, где правда известна,
// то есть донор в нём говорил. А вот отбирать среди подходящих САМЫЕ
// разговорчивые нельзя, и это оплачено замером 28.08.2026. Прежняя редакция
// брала верх списка, и на всех трёх слепках мерилась не догадка кубика, а его
// потолок: у Полынь-Травы потолок 5 реплик на тред, а в отобранных тредах она
// написала 68–107, то есть выше 7 % полнота не поднялась бы ни при какой
// настройке. Порядок измеренной полноты у трёх слепков в точности повторил
// отношение «потолок к числу реплик в отобранном треде» — то есть мерилась
// выборка, а не поведение.
//
// Равномерно — значит от самого скромного подходящего треда до самого людного
// одинаковыми шагами. Детерминированно, без жребия: два прогона одного слепка
// обязаны идти по одним и тем же тредам, иначе их не сравнить.
func (s *Store) PickCalibrationThreads(ctx context.Context, userIDs []int64, minSaid, limit int) ([]ThreadPick, error) {
	if len(userIDs) == 0 {
		return nil, fmt.Errorf("не заданы анкеты донора")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.note_id,
		       SUM(CASE WHEN c.author_id IN (`+intList(userIDs)+`) THEN 1 ELSE 0 END) said,
		       COUNT(*) total
		  FROM comments c
		 WHERE c.note_id IN (SELECT DISTINCT note_id FROM comments
		                      WHERE author_id IN (`+intList(userIDs)+`))
		 GROUP BY c.note_id
		HAVING said >= ?
		 ORDER BY said, total, c.note_id`, minSaid)
	if err != nil {
		return nil, fmt.Errorf("подбор тредов: %w", err)
	}
	defer rows.Close()
	var all []ThreadPick
	for rows.Next() {
		var p ThreadPick
		if err := rows.Scan(&p.NoteID, &p.Said, &p.Total); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return spreadPicks(all, limit), nil
}

// spreadPicks берёт limit штук равномерно по отсортированному списку, вместе с
// обоими краями: край скромных показывает, работает ли кубик там, где потолок не
// мешает, край людных — не захлёбывается ли он там, где мешает.
func spreadPicks(all []ThreadPick, limit int) []ThreadPick {
	if limit <= 0 || len(all) <= limit {
		return all
	}
	if limit == 1 {
		return all[len(all)/2 : len(all)/2+1]
	}
	out := make([]ThreadPick, 0, limit)
	for i := range limit {
		out = append(out, all[i*(len(all)-1)/(limit-1)])
	}
	return out
}

// parseArchiveTime — время архива (ISO-8601 UTC). Отдельной функцией потому,
// что архив писался годами и в старых строках встречается форма без зоны.
func parseArchiveTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("не разобрано время %q", s)
}

// fillScriptGenders проставляет пол участникам.
//
// Отдельным запросом после участников, а не полем в общем SELECT: пол есть
// свойство ЧЕЛОВЕКА, а не реплики, и спрашивать его на каждую из сотен строк
// треда значило бы повторять один ответ. Неизвестный пол остаётся пустым — в
// архиве он снят у 801 анкеты, и это не пробел, а граница обхода: те же 801
// дают 91 % всех реплик.
func (s *Store) fillScriptGenders(ctx context.Context, sc *ThreadScript) error {
	if len(sc.Participants) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(sc.Participants))
	for _, a := range sc.Participants {
		ids = append(ids, a.UserID)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, gender FROM users
		 WHERE id IN (`+intList(ids)+`) AND gender IN ('male','female')`)
	if err != nil {
		return fmt.Errorf("пол участников треда %d: %w", sc.NoteID, err)
	}
	defer rows.Close()
	byID := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var g string
		if err := rows.Scan(&id, &g); err != nil {
			return err
		}
		byID[id] = g
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range sc.Participants {
		sc.Participants[i].Gender = byID[sc.Participants[i].UserID]
	}
	return nil
}
