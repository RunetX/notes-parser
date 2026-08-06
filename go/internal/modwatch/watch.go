package modwatch

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"time"

	"lovegw/internal/love"
)

// Site — то, что наблюдателю нужно от сайта. Отдельный интерфейс (а не
// *love.Client) ради тестов на подставном сайте.
type Site interface {
	// Feed — свежая страница ленты (30 заметок, новые первыми).
	Feed(ctx context.Context) ([]love.Note, error)
	// Thread — страница комментариев (page ≥ 1, новые первыми) и шапка заметки
	// (nil — шапка не разобралась).
	Thread(ctx context.Context, noteID string, page int) ([]love.Comment, *love.Note, error)
}

// Значения по умолчанию: сайт опрашивается щадяще, но так, чтобы удаление
// ловилось с точностью до пары минут — иначе окно присутствия размывается и
// статистика теряет силу.
// Охват выставлен по замеру: при 5 мин / 6 тредов / 3 страницах наблюдатель
// ловил 63 % реплик сайта, а удаление комментария видно только внутри охвата.
// Текущие значения дают ~90 % при ~15 запросах в минуту.
const (
	DefaultFeedInterval   = 90 * time.Second
	DefaultThreadInterval = 3 * time.Minute
	DefaultWindow         = 48 * time.Hour
	DefaultDepth          = 12 * time.Hour
	DefaultMaxThreads     = 10
	DefaultMaxPages       = 4
)

// Watcher — цикл наблюдения.
type Watcher struct {
	Site  Site
	Store *Store
	Log   *slog.Logger

	FeedInterval   time.Duration // период опроса ленты
	ThreadInterval time.Duration // минимальный период опроса одного треда
	Window         time.Duration // сколько заметка считается активной после первой встречи
	Depth          time.Duration // насколько вглубь треда листать по времени комментариев
	MaxThreads     int           // сколько тредов опрашивать за один тик
	MaxPages       int           // предел страниц комментариев на тред

	Now func() time.Time // подмена времени в тестах
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) log() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Watcher) defaults() {
	if w.FeedInterval <= 0 {
		w.FeedInterval = DefaultFeedInterval
	}
	if w.ThreadInterval <= 0 {
		w.ThreadInterval = DefaultThreadInterval
	}
	if w.Window <= 0 {
		w.Window = DefaultWindow
	}
	if w.Depth <= 0 {
		w.Depth = DefaultDepth
	}
	if w.MaxThreads <= 0 {
		w.MaxThreads = DefaultMaxThreads
	}
	if w.MaxPages <= 0 {
		w.MaxPages = DefaultMaxPages
	}
}

// Run крутит наблюдение до отмены контекста. Ошибка одного тика не роняет цикл:
// сайт за DDoS-Guard иногда отвечает 403, и пропущенный опрос — не повод терять
// наблюдение (пропуск лишь расширяет окно неопределённости следующего события).
func (w *Watcher) Run(ctx context.Context) error {
	w.defaults()
	ticker := time.NewTicker(w.FeedInterval)
	defer ticker.Stop()
	for {
		if err := w.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.log().Warn("опрос не удался", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Poll — один проход: лента, затем очередь тредов.
func (w *Watcher) Poll(ctx context.Context) error {
	w.defaults()
	now := w.now()
	if err := w.pollFeed(ctx, now); err != nil {
		return err
	}
	return w.pollThreads(ctx, now)
}

// pollFeed сверяет ленту с прошлым состоянием.
//
// Правило охвата: лента отдаёт 30 свежих заметок, старые уходят за край
// страницы сами. Поэтому исчезновение считается удалением только у заметок с
// id больше самого старого из присутствующих сейчас — то есть внутри охвата.
func (w *Watcher) pollFeed(ctx context.Context, now time.Time) error {
	notes, err := w.Site.Feed(ctx)
	if err != nil {
		return err
	}
	if len(notes) == 0 {
		// Пустая лента — почти наверняка сбой разбора; считать всё удалённым нельзя.
		w.log().Warn("лента пуста, пропускаю сверку")
		return nil
	}
	known, err := w.Store.LiveNotes(ctx)
	if err != nil {
		return err
	}
	present := make(map[int64]bool, len(notes))
	var minID int64 = math.MaxInt64
	for _, n := range notes {
		id, err := strconv.ParseInt(n.ID, 10, 64)
		if err != nil {
			continue
		}
		present[id] = true
		if id < minID {
			minID = id
		}
		authorID, _ := strconv.ParseInt(n.AuthorID, 10, 64)
		st := NoteState{
			ID:             id,
			AuthorID:       authorID,
			AuthorName:     n.AuthorName,
			TextHead:       n.Text,
			Images:         len(n.Images),
			CommentsClosed: n.CommentsClosed,
			PublishedAt:    n.PublishedAt,
		}
		prev, seen := known[id]
		if err := w.applyNote(ctx, now, st, prev, seen); err != nil {
			return err
		}
	}
	for id, row := range known {
		if present[id] || id < minID {
			continue
		}
		age, idle := w.eventContext(ctx, id, row.PublishedAt, now)
		if err := w.Store.AddEvent(ctx, Event{
			Kind:       KindNoteGone,
			RefID:      id,
			NoteID:     id,
			PrevSeen:   row.LastSeen,
			DetectedAt: now,
			Details:    describeNote(row),
			Age:        age,
			Idle:       idle,
		}); err != nil {
			return err
		}
		if err := w.Store.MarkNoteGone(ctx, id, now); err != nil {
			return err
		}
		w.log().Info("заметка исчезла", "note", id, "автор", row.AuthorName)
	}
	return nil
}

// applyNote записывает состояние заметки и заводит события по изменениям шапки.
func (w *Watcher) applyNote(ctx context.Context, now time.Time, st NoteState, prev NoteRow, seen bool) error {
	kinds := headerChanges(st, prev, seen)
	for _, kind := range kinds {
		prevSeen := prev.LastSeen
		if kind == KindNotePublished {
			prevSeen = now
		}
		age, idle := w.eventContext(ctx, st.ID, st.PublishedAt, now)
		if kind == KindNotePublished {
			idle = Unknown // тред ещё пуст — «тишина» бессмысленна
		}
		if err := w.Store.AddEvent(ctx, Event{
			Kind: kind, RefID: st.ID, NoteID: st.ID,
			PrevSeen: prevSeen, DetectedAt: now,
			Details: describeNote(NoteRow{NoteState: st}),
			Age:     age, Idle: idle,
		}); err != nil {
			return err
		}
	}
	if err := w.Store.SaveNote(ctx, now, st); err != nil {
		return err
	}
	return w.saveAuthor(ctx, now, st.AuthorID, st.AuthorName)
}

// headerChanges — что изменилось в шапке заметки с прошлого опроса.
func headerChanges(st NoteState, prev NoteRow, seen bool) []string {
	if !seen {
		return []string{KindNotePublished}
	}
	var kinds []string
	if prev.Images == 0 && st.Images > 0 {
		kinds = append(kinds, KindImageAdded)
	}
	if !prev.CommentsClosed && st.CommentsClosed {
		kinds = append(kinds, KindCommentsClosed)
	}
	if prev.CommentsClosed && !st.CommentsClosed {
		kinds = append(kinds, KindCommentsOpened)
	}
	return kinds
}

// eventContext — возраст объекта к моменту действия и тишина в треде перед ним.
// Обе величины нужны, чтобы отличать руку от автоматики: у сайтового таймера
// возраст один и тот же, у человека — какой придётся.
func (w *Watcher) eventContext(ctx context.Context, noteID int64, published, now time.Time) (age, idle time.Duration) {
	age, idle = Unknown, Unknown
	if !published.IsZero() && now.After(published) {
		age = now.Sub(published)
	}
	if last, ok := w.Store.LastCommentAt(ctx, noteID); ok && now.After(last) {
		idle = now.Sub(last)
	}
	return age, idle
}

// saveAuthor обновляет ник и заводит событие при переименовании.
func (w *Watcher) saveAuthor(ctx context.Context, now time.Time, id int64, name string) error {
	if id == 0 || name == "" {
		return nil
	}
	prev, err := w.Store.SaveUser(ctx, now, id, name)
	if err != nil {
		return err
	}
	if prev == "" || prev == name {
		return nil
	}
	return w.Store.AddEvent(ctx, Event{
		Kind: KindNickChanged, RefID: id, PrevSeen: now, DetectedAt: now,
		Details: prev + " → " + name,
		Age:     Unknown, Idle: Unknown,
	})
}

// pollThreads опрашивает очередь активных тредов.
func (w *Watcher) pollThreads(ctx context.Context, now time.Time) error {
	due, err := w.Store.NotesDue(ctx, now, w.ThreadInterval, w.Window, w.MaxThreads)
	if err != nil {
		return err
	}
	for _, note := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.pollThread(ctx, note, now); err != nil {
			w.log().Warn("тред не опрошен", "note", note.ID, "err", err)
			continue
		}
	}
	return nil
}

// pollThread сверяет свежую часть треда: страницы листаются, пока комментарии
// не станут старше Depth или не кончатся страницы. Исчезнувшим считается только
// комментарий с id больше самого старого из увиденных сейчас — то есть внутри
// охвата (иначе за удаление принимался бы выход за нижний край страниц).
func (w *Watcher) pollThread(ctx context.Context, note NoteRow, now time.Time) error {
	known, err := w.Store.LiveComments(ctx, note.ID)
	if err != nil {
		return err
	}
	seen, minID, err := w.fetchThread(ctx, note, now)
	if err != nil {
		return err
	}
	for id, row := range known {
		if seen[id] || id < minID {
			continue
		}
		age, idle := w.eventContext(ctx, note.ID, row.PublishedAt, now)
		if err := w.Store.AddEvent(ctx, Event{
			Kind:       KindCommentGone,
			RefID:      id,
			NoteID:     note.ID,
			PrevSeen:   row.LastSeen,
			DetectedAt: now,
			Details:    row.AuthorName + ": " + row.TextHead,
			Age:        age,
			Idle:       idle,
		}); err != nil {
			return err
		}
		if err := w.Store.MarkCommentGone(ctx, id, now); err != nil {
			return err
		}
		w.log().Info("комментарий исчез", "note", note.ID, "comment", id, "автор", row.AuthorName)
	}
	return w.Store.SetNotePolled(ctx, note.ID, now)
}

// fetchThread листает свежие страницы треда, записывая всё увиденное, и
// возвращает множество увиденных id вместе с самым старым из них — границей
// охвата, глубже которой судить об исчезновении нельзя.
func (w *Watcher) fetchThread(ctx context.Context, note NoteRow, now time.Time) (map[int64]bool, int64, error) {
	idStr := strconv.FormatInt(note.ID, 10)
	seen := make(map[int64]bool)
	var minID int64 = math.MaxInt64
	var oldest time.Time
	for page := 1; page <= w.MaxPages; page++ {
		comments, header, err := w.Site.Thread(ctx, idStr, page)
		if err != nil {
			if page == 1 {
				return nil, minID, err
			}
			break // конец пейджера — не ошибка
		}
		if page == 1 && header != nil {
			if err := w.applyHeader(ctx, note, header, now); err != nil {
				return nil, minID, err
			}
		}
		if len(comments) == 0 {
			break
		}
		for _, c := range comments {
			seen[c.ID] = true
			if c.ID < minID {
				minID = c.ID
			}
			if oldest.IsZero() || c.PublishedAt.Before(oldest) {
				oldest = c.PublishedAt
			}
			if err := w.saveComment(ctx, note.ID, c, now); err != nil {
				return nil, minID, err
			}
		}
		if !oldest.IsZero() && now.Sub(oldest) > w.Depth {
			break
		}
	}
	return seen, minID, nil
}

// applyHeader обновляет заметку по шапке страницы комментариев: только там
// видна дата публикации, без которой не посчитать возраст события.
func (w *Watcher) applyHeader(ctx context.Context, note NoteRow, header *love.Note, now time.Time) error {
	st := NoteState{
		ID:             note.ID,
		AuthorID:       note.AuthorID,
		AuthorName:     note.AuthorName,
		TextHead:       header.Text,
		Images:         len(header.Images),
		CommentsClosed: header.CommentsClosed,
		PublishedAt:    header.PublishedAt,
	}
	if id, err := strconv.ParseInt(header.AuthorID, 10, 64); err == nil && id != 0 {
		st.AuthorID = id
		st.AuthorName = header.AuthorName
	}
	return w.applyNote(ctx, now, st, note, true)
}

func (w *Watcher) saveComment(ctx context.Context, noteID int64, c love.Comment, now time.Time) error {
	authorID, _ := strconv.ParseInt(c.AuthorID, 10, 64)
	if err := w.Store.SaveComment(ctx, now, CommentState{
		ID:          c.ID,
		NoteID:      noteID,
		AuthorID:    authorID,
		AuthorName:  c.AuthorName,
		TextHead:    c.Text,
		PublishedAt: c.PublishedAt,
	}); err != nil {
		return err
	}
	return w.saveAuthor(ctx, now, authorID, c.AuthorName)
}

func describeNote(r NoteRow) string {
	author := r.AuthorName
	if author == "" {
		author = "аноним"
	}
	return author + ": " + Head(r.TextHead)
}
