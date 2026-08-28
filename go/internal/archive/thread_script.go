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
