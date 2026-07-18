// Пакет mirror — зеркалирование сайта в мессенджер: наблюдатель ленты,
// менеджер воркеров и воркер-на-заметку с адаптивным интервалом опроса.
// Пакет не знает про Telegram: всё мессенджер-специфичное — за Sink
// (сегодня tgx; задел под MAX и другие).
package mirror

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// SiteClient — то, что mirror требует от клиента сайта.
type SiteClient interface {
	FetchNotes(ctx context.Context) ([]love.Note, error)
	FetchComments(ctx context.Context, noteID string) ([]love.Comment, error)
}

// Sink — мессенджер-приёмник событий зеркала.
type Sink interface {
	PostNote(ctx context.Context, n store.Note) (int64, error)
	PostComment(ctx context.Context, n store.Note, c store.Comment) (int64, error)
	NotifySubscriber(ctx context.Context, userID int64, n store.Note, c store.Comment) error
}

type Mirror struct {
	st           *store.Store
	site         SiteClient
	sink         Sink
	log          *slog.Logger
	notesLimit   int
	feedInterval time.Duration
	seedFirst    bool

	events chan string // id заметок для запуска воркеров
}

func New(st *store.Store, site SiteClient, sink Sink, notesLimit int,
	feedInterval time.Duration, seedFirst bool, log *slog.Logger) *Mirror {
	if log == nil {
		log = slog.Default()
	}
	return &Mirror{
		st:           st,
		site:         site,
		sink:         sink,
		log:          log,
		notesLimit:   notesLimit,
		feedInterval: feedInterval,
		seedFirst:    seedFirst,
		events:       make(chan string, 16),
	}
}

// Run запускает наблюдатель ленты и менеджер воркеров; блокируется до
// отмены контекста.
func (m *Mirror) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.watchFeed(ctx)
	}()
	go func() {
		defer wg.Done()
		m.managePollers(ctx)
	}()
	wg.Wait()
	return ctx.Err()
}

// watchFeed периодически обходит ленту заметок.
func (m *Mirror) watchFeed(ctx context.Context) {
	seed := m.seedFirst
	ticker := time.NewTicker(m.feedInterval)
	defer ticker.Stop()
	for {
		if m.feedCycle(ctx, seed) {
			seed = false // seed-режим действует до первого успешного обхода
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// feedCycle — один обход ленты. Возвращает true при успешной загрузке.
func (m *Mirror) feedCycle(ctx context.Context, seed bool) bool {
	// Сначала дожимаем застрявшие pending (упали между INSERT и постом).
	m.retryPending(ctx)

	notes, err := m.site.FetchNotes(ctx)
	if err != nil {
		m.log.Warn("лента недоступна", "err", err)
		return false
	}
	if len(notes) > m.notesLimit {
		notes = notes[:m.notesLimit]
	}
	known, err := m.st.KnownNoteIDs(ctx)
	if err != nil {
		m.log.Error("чтение известных заметок", "err", err)
		return false
	}

	seeded, posted := 0, 0
	// Лента отдаёт новые сверху — обрабатываем в обратном порядке,
	// чтобы старые заметки попадали в канал первыми.
	for i := len(notes) - 1; i >= 0; i-- {
		n := notes[i]
		if known[n.ID] {
			continue
		}
		rec := store.Note{
			ID:          n.ID,
			AuthorID:    n.AuthorID,
			AuthorName:  n.AuthorName,
			Text:        n.Text,
			Status:      store.StatusPending,
			FirstSeenAt: time.Now(),
		}
		if seed {
			rec.Status = store.StatusSeeded
			if _, err := m.st.InsertNote(ctx, rec); err != nil {
				m.log.Error("seed заметки", "note", n.ID, "err", err)
				continue
			}
			seeded++
			continue
		}
		added, err := m.st.InsertNote(ctx, rec)
		if err != nil || !added {
			if err != nil {
				m.log.Error("сохранение заметки", "note", n.ID, "err", err)
			}
			continue
		}
		if m.postNote(ctx, rec) {
			posted++
		}
	}
	if seed {
		m.log.Info("seed-обход ленты завершён", "seeded", seeded, "posted", 0)
	} else if posted > 0 {
		m.log.Info("обход ленты", "новых", posted)
	}
	return true
}

// retryPending допощивает заметки, застрявшие в pending после сбоя.
func (m *Mirror) retryPending(ctx context.Context) {
	pending, err := m.st.NotesByStatus(ctx, store.StatusPending)
	if err != nil {
		m.log.Error("чтение pending-заметок", "err", err)
		return
	}
	for _, n := range pending {
		m.postNote(ctx, n)
	}
}

// postNote отправляет заметку в канал и помечает её posted.
func (m *Mirror) postNote(ctx context.Context, n store.Note) bool {
	tgID, err := m.sink.PostNote(ctx, n)
	if err != nil {
		m.log.Warn("пост заметки не удался, останется pending", "note", n.ID, "err", err)
		return false
	}
	if err := m.st.SetNotePosted(ctx, n.ID, tgID); err != nil {
		m.log.Error("фиксация поста заметки", "note", n.ID, "err", err)
		return false
	}
	m.log.Info("заметка запощена", "note", n.ID, "tg_message_id", tgID)
	select {
	case m.events <- n.ID:
	default:
		// Менеджер подберёт заметку при следующем старте — не блокируемся.
		m.log.Warn("очередь событий переполнена", "note", n.ID)
	}
	return true
}

// managePollers запускает воркер на каждую отслеживаемую заметку.
// Единоличный владелец списка запущенных воркеров.
func (m *Mirror) managePollers(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()

	running := make(map[string]bool)
	start := func(id string) {
		if running[id] {
			return
		}
		running[id] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.pollNote(ctx, id)
		}()
	}

	posted, err := m.st.NotesByStatus(ctx, store.StatusPosted)
	if err != nil {
		m.log.Error("чтение отслеживаемых заметок", "err", err)
	}
	for _, n := range posted {
		start(n.ID)
	}
	m.log.Info("воркеры комментариев запущены", "count", len(posted))

	for {
		select {
		case id := <-m.events:
			start(id)
		case <-ctx.Done():
			return
		}
	}
}

// pollNote — воркер одной заметки: опрашивает комментарии с адаптивным
// интервалом, пока заметка не уйдёт в архив.
func (m *Mirror) pollNote(ctx context.Context, noteID string) {
	for {
		n, err := m.st.NoteByID(ctx, noteID)
		if err != nil {
			m.log.Error("воркер: чтение заметки", "note", noteID, "err", err)
			return
		}
		if n.Status == store.StatusArchived {
			return
		}
		now := time.Now()
		if ShouldArchive(now, n.FirstSeenAt, n.LastCommentAt) {
			if err := m.st.SetNoteArchived(ctx, noteID, now); err != nil {
				m.log.Error("архивирование", "note", noteID, "err", err)
			}
			m.log.Info("заметка ушла в архив", "note", noteID)
			return
		}

		m.pollComments(ctx, n)

		select {
		case <-time.After(PollInterval(now, n.FirstSeenAt, n.LastCommentAt)):
		case <-ctx.Done():
			return
		}
	}
}

// pollComments — один цикл опроса комментариев заметки.
func (m *Mirror) pollComments(ctx context.Context, n store.Note) {
	comments, err := m.site.FetchComments(ctx, n.ID)
	if err != nil {
		m.log.Warn("комментарии недоступны", "note", n.ID, "err", err)
		return
	}
	known, err := m.st.CommentIDs(ctx, n.ID)
	if err != nil {
		m.log.Error("чтение известных комментариев", "note", n.ID, "err", err)
		return
	}

	// Страница отдаёт новые сверху — сохраняем от старых к новым.
	sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
	fresh := 0
	for _, c := range comments {
		if known[c.ID] {
			continue
		}
		if _, err := m.st.InsertComment(ctx, store.Comment{
			ID:          c.ID,
			NoteID:      n.ID,
			AuthorName:  c.AuthorName,
			AuthorAge:   c.AuthorAge,
			AuthorLink:  c.AuthorLink,
			AvatarURL:   c.AvatarURL,
			PublishedAt: c.PublishedAt,
			Text:        c.Text,
			CreatedAt:   time.Now(),
		}); err != nil {
			m.log.Error("сохранение комментария", "comment", c.ID, "err", err)
			continue
		}
		fresh++
	}
	if fresh > 0 {
		if err := m.st.SetNoteLastCommentAt(ctx, n.ID, time.Now()); err != nil {
			m.log.Error("обновление last_comment_at", "note", n.ID, "err", err)
		}
	}

	if n.TGThreadID == 0 {
		// Автофорвард ещё не пойман: комментарии сохранены и уйдут
		// следующим циклом, когда появится корень треда.
		if fresh > 0 {
			m.log.Info("тред ещё не пойман, комментарии в очереди", "note", n.ID, "queued", fresh)
		}
		return
	}
	m.sendUnsent(ctx, n)
}

// sendUnsent отправляет все неотправленные комментарии заметки.
func (m *Mirror) sendUnsent(ctx context.Context, n store.Note) {
	unsent, err := m.st.UnsentComments(ctx, n.ID)
	if err != nil {
		m.log.Error("чтение неотправленных комментариев", "note", n.ID, "err", err)
		return
	}
	if len(unsent) == 0 {
		return
	}
	subs, err := m.st.Subscriptions(ctx)
	if err != nil {
		m.log.Error("чтение подписок", "err", err)
	}
	for _, c := range unsent {
		tgID, err := m.sink.PostComment(ctx, n, c)
		if err != nil {
			// Порядок важен: не перескакиваем через неотправленный.
			m.log.Warn("пост комментария не удался", "comment", c.ID, "err", err)
			return
		}
		if err := m.st.SetCommentTGMessageID(ctx, c.ID, tgID); err != nil {
			m.log.Error("фиксация поста комментария", "comment", c.ID, "err", err)
			return
		}
		c.TGMessageID = tgID
		m.notifySubscribers(ctx, subs, n, c)
	}
}

// notifySubscribers шлёт ЛС подписчикам, чьё ключевое слово встретилось
// в тексте комментария.
func (m *Mirror) notifySubscribers(ctx context.Context, subs []store.Subscription, n store.Note, c store.Comment) {
	for _, sub := range subs {
		if !strings.Contains(c.Text, sub.Keyword) {
			continue
		}
		if err := m.sink.NotifySubscriber(ctx, sub.TGUserID, n, c); err != nil {
			m.log.Warn("уведомление подписчика не удалось", "user", sub.TGUserID, "err", err)
		}
	}
}

// PollInterval — адаптивный интервал опроса комментариев: свежие и живые
// заметки опрашиваются часто, старые и тихие — редко.
func PollInterval(now, firstSeen, lastComment time.Time) time.Duration {
	age := now.Sub(firstSeen)
	active := !lastComment.IsZero() && now.Sub(lastComment) < 15*time.Minute
	switch {
	case age < 2*time.Hour || active:
		return 30 * time.Second
	case age < 24*time.Hour:
		return 3 * time.Minute
	case age < 72*time.Hour:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// ShouldArchive — заметка старше недели без комментариев за неделю
// перестаёт отслеживаться.
func ShouldArchive(now, firstSeen, lastComment time.Time) bool {
	const week = 7 * 24 * time.Hour
	if now.Sub(firstSeen) < week {
		return false
	}
	return lastComment.IsZero() || now.Sub(lastComment) >= week
}
