package platsink

// Сверка lovegw.db → Postgres: вторая нога приёма.
//
// Она обязательна, а не «на всякий случай». Зеркало помечает заметку posted,
// только когда отработали ВСЕ приёмники, а воркер комментариев стартует по
// posted, — значит лежащий Postgres тормозил бы и телеграм-контур. Живой
// приёмник поэтому имеет право честно отказать, а догоняет сверка.
//
// Направление одностороннее: SQLite остаётся источником правды для всего, что
// пришло с сайта, и durable-буфером на время любого простоя площадки. Первый же
// проход на пустой базе и есть бэкфилл — отдельной команды под него нет.
//
// Чего сверка НЕ делает: не ходит на сайт. Байты аватаров и иллюстраций
// приезжают только живым потоком (их качает зеркало с RU-IP); историческим
// строкам достаётся ссылка в ngs_avatar_url / note_images.url, а забрать их
// оптом — отдельная операция, и она нужна не раньше, чем появятся страницы.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

// Interval — период сверки. Пять минут — это потолок отставания площадки от
// зеркала при неработающем живом приёмнике.
const Interval = 5 * time.Minute

// passBudget — потолок ОДНОГО прохода сверки в демоне. Двадцать минут — это не
// оценка работы (обычный проход укладывается в секунды, а первый, он же
// бэкфилл, идёт часами и продолжится следующим тактом), а признак того, что
// база занята намертво: без потолка горутина сверки висит в одном запросе до
// перезапуска демона, и площадка перестаёт догонять вовсе. Руками
// (`platform reconcile`) потолка нет — там есть кому нажать Ctrl-C.
const passBudget = 20 * time.Minute

// notesChunk — сколько заметок запрашивать у SQLite за раз. Предел там на число
// параметров запроса (999), а не на память.
const notesChunk = 400

// Stats — что сверка сделала за проход.
type Stats struct {
	Notes    int // заметок принято
	Comments int // комментариев принято
	Images   int // иллюстраций привязано
	Closed   int // заметок закрыто для комментариев
	Scanned  int // заметок пришлось сверять поимённо
}

// Empty — делать было нечего (обычное состояние работающего зеркала).
func (s Stats) Empty() bool { return s.Notes+s.Comments+s.Images+s.Closed == 0 }

// Reconciler сверяет зеркальную базу с площадкой.
type Reconciler struct {
	st  *store.Store
	p   *platform.Platform
	log *slog.Logger
}

// NewReconciler создаёт сверку.
func NewReconciler(st *store.Store, p *platform.Platform, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{st: st, p: p, log: log}
}

// Run крутит сверку до отмены контекста. Ошибка прохода не возвращается наружу
// намеренно: сверка живёт в общем errgroup демона, и её падение утащило бы за
// собой зеркало и ботов — а причина у неё ровно одна, недоступный Postgres,
// и лечится он сам.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		st, err := r.pass(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			// Не поломка: проход идёмпотентен и возобновляем, поэтому недоделку
			// доберёт следующий такт. Срок нужен на случай, когда база занята
			// намертво: без него горутина сверки висит в одном запросе до
			// перезапуска демона, и площадка не догоняет уже никогда.
			r.log.Warn("сверка площадки не уложилась в срок, продолжим следующим тактом",
				"budget", passBudget, "notes", st.Notes, "comments", st.Comments)
		case err != nil:
			r.log.Warn("сверка площадки не удалась", "err", err)
		case !st.Empty():
			r.log.Info("сверка площадки", "notes", st.Notes, "comments", st.Comments,
				"images", st.Images, "closed", st.Closed, "scanned", st.Scanned)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// pass — проход под потолком времени (см. passBudget). Отдельно от Once, чтобы
// у разовой команды остался проход без срока.
func (r *Reconciler) pass(ctx context.Context) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, passBudget)
	defer cancel()
	return r.Once(ctx)
}

// Once — один проход сверки.
func (r *Reconciler) Once(ctx context.Context) (Stats, error) {
	var st Stats
	// Порядок обязателен: заметка старше своих комментариев и картинок (на неё
	// стоит внешний ключ), а отметка «закрыто» — старше некуда, она про уже
	// существующую строку.
	if err := r.syncNotes(ctx, &st); err != nil {
		return st, err
	}
	if err := r.syncComments(ctx, &st); err != nil {
		return st, err
	}
	if err := r.syncImages(ctx, &st); err != nil {
		return st, err
	}
	if err := r.syncClosed(ctx, &st); err != nil {
		return st, err
	}
	return st, nil
}

// syncNotes досылает заметки, которых на площадке нет.
func (r *Reconciler) syncNotes(ctx context.Context, st *Stats) error {
	missing, err := r.missingNotes(ctx)
	if err != nil {
		return err
	}
	for len(missing) > 0 {
		size := min(notesChunk, len(missing))
		if err := r.ingestNotes(ctx, missing[:size], st); err != nil {
			return err
		}
		missing = missing[size:]
	}
	return nil
}

// missingNotes — id заметок зеркала, которых у площадки нет, от старых к новым.
func (r *Reconciler) missingNotes(ctx context.Context) ([]string, error) {
	known, err := r.st.KnownNoteIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("список заметок зеркала: %w", err)
	}
	have, err := r.p.MirroredNoteIDs(ctx)
	if err != nil {
		return nil, err
	}
	missing := make([]string, 0, len(known))
	for id := range known {
		n, err := siteID(id)
		if err != nil {
			r.log.Warn("заметка зеркала с нечисловым id пропущена", "note", id, "err", err)
			continue
		}
		if !have[n] {
			missing = append(missing, id)
		}
	}
	sortIDs(missing)
	return missing, nil
}

// ingestNotes принимает одну порцию заметок.
func (r *Reconciler) ingestNotes(ctx context.Context, chunk []string, st *Stats) error {
	rows, err := r.st.NotesByIDs(ctx, chunk)
	if err != nil {
		return fmt.Errorf("чтение заметок зеркала: %w", err)
	}
	for _, id := range chunk {
		n, ok := rows[id]
		if !ok {
			continue // заметку удалили между двумя запросами — не наше дело
		}
		in, err := noteFrom(n)
		if err != nil {
			r.log.Warn("заметка зеркала не переводится в приём", "note", id, "err", err)
			continue
		}
		fresh, err := r.p.IngestNote(ctx, in)
		if err != nil {
			return err
		}
		if fresh {
			st.Notes++
		}
	}
	return nil
}

// syncComments сверяет комментарии по счётчикам и досылает недостающие.
func (r *Reconciler) syncComments(ctx context.Context, st *Stats) error {
	mirrored, err := r.st.CommentTallies(ctx)
	if err != nil {
		return fmt.Errorf("счётчики комментариев зеркала: %w", err)
	}
	have, err := r.p.MirroredCommentTallies(ctx)
	if err != nil {
		return err
	}
	stale := make([]string, 0, 8)
	for noteID, t := range mirrored {
		id, err := siteID(noteID)
		if err != nil {
			continue // про такую заметку уже предупредили в syncNotes
		}
		if cur, ok := have[id]; ok && cur.Count == t.Count && cur.MaxID == t.MaxID {
			continue
		}
		stale = append(stale, noteID)
	}
	sortIDs(stale)

	for _, noteID := range stale {
		id, err := siteID(noteID)
		if err != nil {
			continue
		}
		n, err := r.syncNoteComments(ctx, noteID, id)
		st.Comments += n
		st.Scanned++
		if err != nil {
			return err
		}
	}
	return nil
}

// syncNoteComments досылает недостающие комментарии одной заметки.
//
// Адресата считаем сами, а не берём у зеркала: message_targets знает лишь то,
// что УЖЕ ушло в этот приёмник, а сверке нужна вся заметка целиком (и на
// бэкфилле там нет ни одной строки). Правило то же самое — «последний, кто
// писал в этой заметке под этим ником», — поэтому живой приём и сверка сходятся
// на одном ответе, а кто из них успел первым, неважно: приём идемпотентен.
//
// Обход строго по возрастанию id: адресат всегда старше ответа, значит к моменту
// вставки его путь в дереве уже есть.
func (r *Reconciler) syncNoteComments(ctx context.Context, noteID string, id int64) (int, error) {
	comments, err := r.st.CommentsForNote(ctx, noteID)
	if err != nil {
		return 0, fmt.Errorf("комментарии заметки %s: %w", noteID, err)
	}
	have, err := r.p.CommentIDs(ctx, id)
	if err != nil {
		return 0, err
	}
	var (
		seen = make(map[string]int64, len(comments)) // ник → последняя его реплика
		n    int
	)
	for _, c := range comments {
		var replyTo int64
		if nick := love.AddressPrefix(c.Text); nick != "" {
			replyTo = seen[nick]
		}
		if !have[c.ID] {
			if _, err := r.p.IngestComment(ctx, commentFrom(id, c, replyTo)); err != nil {
				return n, fmt.Errorf("заметка %s: %w", noteID, err)
			}
			n++
		}
		seen[strings.ToLower(c.AuthorName)] = c.ID
	}
	return n, nil
}

// syncImages привязывает иллюстрации, которых у площадки нет. Байты не качаются:
// на сайт сверка не ходит (см. шапку файла), и ссылка без байтов — рабочее
// состояние: страница такую картинку просто не рисует.
func (r *Reconciler) syncImages(ctx context.Context, st *Stats) error {
	mirrored, err := r.st.NoteImageCounts(ctx)
	if err != nil {
		return fmt.Errorf("счётчики иллюстраций зеркала: %w", err)
	}
	have, err := r.p.NoteImageCounts(ctx)
	if err != nil {
		return err
	}
	for noteID, count := range mirrored {
		id, err := siteID(noteID)
		if err != nil || have[id] >= count {
			continue
		}
		imgs, err := r.st.NoteImagesFor(ctx, noteID)
		if err != nil {
			return fmt.Errorf("иллюстрации заметки %s: %w", noteID, err)
		}
		for _, img := range imgs {
			if err := r.p.AttachNoteImage(ctx, id, nil, img.URL); err != nil {
				return err
			}
		}
		st.Images += count - have[id]
	}
	return nil
}

// syncClosed переносит отметку «не актуальна». Сравниваем закрытые в зеркале с
// открытыми у нас: обе выборки — короткие списки id, и на каждую заметку
// приходится не более одного UPDATE за всю её жизнь.
func (r *Reconciler) syncClosed(ctx context.Context, st *Stats) error {
	closed, err := r.st.ClosedNoteIDs(ctx)
	if err != nil {
		return fmt.Errorf("закрытые заметки зеркала: %w", err)
	}
	open, err := r.p.OpenNoteIDs(ctx)
	if err != nil {
		return err
	}
	for noteID := range closed {
		id, err := siteID(noteID)
		if err != nil || !open[id] {
			continue
		}
		changed, err := r.p.SetCommentsClosed(ctx, id, true)
		if err != nil {
			return err
		}
		if changed {
			st.Closed++
		}
	}
	return nil
}

// sortIDs упорядочивает id заметок по числовому значению, то есть по времени
// появления на сайте. Нужно ради предсказуемости: бэкфилл идёт от старых к
// новым, и прерванный проход виден по логу, а не гадается.
func sortIDs(ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.ParseInt(ids[i], 10, 64)
		b, _ := strconv.ParseInt(ids[j], 10, 64)
		return a < b
	})
}
