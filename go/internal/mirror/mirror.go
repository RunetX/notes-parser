// Пакет mirror — зеркалирование сайта в мессенджер: наблюдатель ленты,
// менеджер воркеров и воркер-на-заметку с адаптивным интервалом опроса.
// Пакет не знает про Telegram: всё мессенджер-специфичное — за Sink
// (сегодня tgx; задел под MAX и другие).
package mirror

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"lovegw/internal/alerts"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// SiteClient — то, что mirror требует от клиента сайта.
type SiteClient interface {
	FetchNotes(ctx context.Context) ([]love.Note, error)
	FetchCommentsPage(ctx context.Context, noteID string) (love.CommentsPage, error)
	FetchMedia(ctx context.Context, url string) ([]byte, error)
}

// Sink — мессенджер-приёмник событий зеркала. avatar/image — уже скачанные
// байты медиа (могут быть nil: тогда заметка/комментарий уходит текстом).
// Id сообщений — строки: Telegram отдаёт числа (десятичной строкой), MAX —
// mid; mirror трактует их как непрозрачные значения и хранит per-messenger
// в message_targets. threadID — корень треда обсуждения в этом мессенджере.
// replyToID (может быть пустым) — сообщение адресата реплики: комментарий
// уходит ответом на него, и мессенджер сам рисует цитату исходного.
type Sink interface {
	Name() string // имя мессенджера: store.MessengerTelegram / store.MessengerMax
	PostNote(ctx context.Context, n store.Note, avatar []byte) (string, error)
	PostComment(ctx context.Context, n store.Note, threadID, replyToID string, c store.Comment, avatar []byte) (string, error)
	PostNoteImage(ctx context.Context, threadID, imageURL string, image []byte) (string, error)
	// Уведомления подписчикам приёмник не шлёт: ЛС пишет только тот бот,
	// которого пользователь сам запускал (SubNotify). Постер канала первым
	// написать не может, так что запасного пути тут не бывает.
}

// SubEvent — повод для ЛС подписчику. Комментарий нулевой (Comment.ID == 0) у
// повода «новая заметка автора»: цитировать ещё нечего, и ссылка ведёт на пост
// заметки. Id сообщений — в том мессенджере, чья подписка сработала.
type SubEvent struct {
	Sub       store.Subscription // ID — для кнопки «Отписаться», Kind/Target — для повода
	Note      store.Note
	Comment   store.Comment
	ThreadID  string // корень треда заметки ("" — ещё не пойман)
	MsgID     string // сообщение комментария ("" у новой заметки)
	PostMsgID string // пост заметки в канале ("" у комментария)
}

// IsComment — повод про комментарий (иначе про новую заметку автора).
func (e SubEvent) IsComment() bool { return e.Comment.ID != 0 }

// Reason — первая строка уведомления: почему оно пришло. Текст, а не HTML:
// разметку накладывает композер мессенджера, он же экранирует.
func (e SubEvent) Reason() string {
	switch e.Sub.Kind {
	case store.SubAuthorNotes:
		return "✍️ Новая заметка автора " + e.Note.AuthorName
	case store.SubNoteComments:
		return "💬 Новый комментарий к заметке, на которую вы подписаны"
	default:
		return "🔔 Ключевое слово «" + e.Sub.Target + "»"
	}
}

// SubNotify шлёт подписчику ЛС о событии. Задаётся по имени мессенджера;
// мессенджер без записи подписчиков не уведомляет вовсе (о чём зеркало
// предупреждает на старте).
type SubNotify func(ctx context.Context, userID int64, ev SubEvent)

// ThreadStarter — приёмник, умеющий сам открыть тред обсуждения заметки
// (MAX: «ручной автофорвард» — копия заметки в чат обсуждения). Если тред
// не пойман и приёмник реализует интерфейс, mirror зовёт StartThread на
// каждом цикле опроса до успеха. В Telegram тред ловит bridge по
// автофорварду — там интерфейс не реализуется.
type ThreadStarter interface {
	StartThread(ctx context.Context, n store.Note, postMsgID string) (threadID string, err error)
}

type Mirror struct {
	st           *store.Store
	site         SiteClient
	sinks        []Sink
	log          *slog.Logger
	alert        *alerts.Alerter
	notesLimit   int
	feedInterval time.Duration
	seedFirst    bool
	subNotify    map[string]SubNotify

	events chan string // id заметок для запуска воркеров
}

// Config — параметры зеркала. AlertSend (может быть nil) шлёт админу
// уведомления о дрейфе вёрстки и блокировке сайта. SubNotify задаёт по имени
// мессенджера отправку ЛС подписчику — им всегда занимается бот, которого
// пользователь запускал сам, а не постер канала.
type Config struct {
	// NotesLimit — сколько заметок брать из ленты на холодном старте (seed
	// или пустая БД) и сколько максимум подхватывать за один обход при
	// догоне после простоя. Глубину просмотра ленты он не ограничивает.
	NotesLimit   int
	FeedInterval time.Duration
	SeedFirst    bool
	AlertSend    func(ctx context.Context, text string)
	SubNotify    map[string]SubNotify
}

// New создаёт зеркало, публикующее во все переданные приёмники (fan-out).
func New(st *store.Store, site SiteClient, sinks []Sink, cfg Config, log *slog.Logger) *Mirror {
	if log == nil {
		log = slog.Default()
	}
	for _, sink := range sinks {
		if cfg.SubNotify[sink.Name()] == nil {
			log.Warn("подписчиков этого мессенджера уведомлять некому: нет ЛС-бота",
				"sink", sink.Name())
		}
	}
	return &Mirror{
		st:           st,
		site:         site,
		sinks:        sinks,
		log:          log,
		alert:        alerts.New(cfg.AlertSend, alertThreshold),
		notesLimit:   cfg.NotesLimit,
		feedInterval: cfg.FeedInterval,
		seedFirst:    cfg.SeedFirst,
		subNotify:    cfg.SubNotify,
		events:       make(chan string, 16),
	}
}

// reportSiteError классифицирует ошибку загрузки и при необходимости
// беспокоит админа: дрейф вёрстки (MarkupError) или блокировка (403).
// driftKey — ключ дрейфа для конкретного источника (лента/комментарии).
func (m *Mirror) reportSiteError(ctx context.Context, driftKey string, err error) {
	var me *love.MarkupError
	switch {
	case errors.As(err, &me):
		m.alert.Fail(ctx, driftKey, "селектор «"+me.Selector+"» — "+me.Context)
	case errors.Is(err, love.ErrForbidden):
		m.alert.Fail(ctx, keyForbidden, "сайт вернул 403 (геоблок или бан IP)")
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
		m.reportSiteError(ctx, keyFeedDrift, err)
		return false
	}
	m.alert.OK(ctx, keyFeedDrift)
	m.alert.OK(ctx, keyForbidden)
	known, err := m.st.KnownNoteIDs(ctx)
	if err != nil {
		m.log.Error("чтение известных заметок", "err", err)
		return false
	}
	// Холодный старт (seed или пустая БД): берём только верх ленты — незачем
	// вываливать в канал всю историю. Дальше окно не режем: заметка, успевшая
	// уехать ниже notes_limit за время простоя демона или всплеска публикаций,
	// иначе терялась бы навсегда.
	if (seed || len(known) == 0) && len(notes) > m.notesLimit {
		notes = notes[:m.notesLimit]
	}

	// Лента отдаёт новые сверху — обрабатываем в обратном порядке,
	// чтобы старые заметки попадали в канал первыми.
	var fresh []love.Note
	for i := len(notes) - 1; i >= 0; i-- {
		if !known[notes[i].ID] {
			fresh = append(fresh, notes[i])
		}
	}
	// Догоняем не быстрее notes_limit заметок за обход: после долгого простоя
	// вся лента разом не улетит в канал, хвост уйдёт следующими циклами.
	if !seed && m.notesLimit > 0 && len(fresh) > m.notesLimit {
		m.log.Info("догоняем пропущенные заметки",
			"всего", len(fresh), "в этом обходе", m.notesLimit)
		fresh = fresh[:m.notesLimit]
	}

	seeded, posted := 0, 0
	for _, n := range fresh {
		switch m.ingestNewNote(ctx, n, seed) {
		case ingestSeeded:
			seeded++
		case ingestPosted:
			posted++
		}
	}
	if !seed {
		m.markClosedNotes(ctx, notes)
	}
	if seed {
		m.log.Info("seed-обход ленты завершён", "seeded", seeded, "posted", 0)
	} else if posted > 0 {
		m.log.Info("обход ленты", "новых", posted)
	}
	return true
}

// ingestResult — итог обработки одной новой заметки из ленты.
type ingestResult int

const (
	ingestSkipped ingestResult = iota // не сохранили/не запостили (ошибка или гонка)
	ingestSeeded                      // зафиксировали без постинга (seed-режим)
	ingestPosted                      // запостили в канал
)

// ingestNewNote сохраняет новую заметку и, вне seed-режима, постит её в канал
// (с иллюстрациями). В seed-режиме заметка только фиксируется как seeded.
func (m *Mirror) ingestNewNote(ctx context.Context, n love.Note, seed bool) ingestResult {
	rec := store.Note{
		ID:              n.ID,
		AuthorID:        n.AuthorID,
		AuthorName:      n.AuthorName,
		Text:            n.Text,
		AuthorAvatarURL: n.AuthorAvatarURL,
		Status:          store.StatusPending,
		FirstSeenAt:     time.Now(),
	}
	if seed {
		rec.Status = store.StatusSeeded
		if _, err := m.st.InsertNote(ctx, rec); err != nil {
			m.log.Error("seed заметки", "note", n.ID, "err", err)
			return ingestSkipped
		}
		m.storeNoteImages(ctx, n.ID, n.Images)
		return ingestSeeded
	}
	added, err := m.st.InsertNote(ctx, rec)
	if err != nil || !added {
		if err != nil {
			m.log.Error("сохранение заметки", "note", n.ID, "err", err)
		}
		return ingestSkipped
	}
	m.storeNoteImages(ctx, n.ID, n.Images)
	if m.postNote(ctx, rec) {
		return ingestPosted
	}
	return ingestSkipped
}

// markClosedNotes фиксирует в БД пометку сайта «не актуальна». Это справочный
// признак и только: сайт вешает его почти на все заметки, кроме самой свежей,
// а комментарии при этом продолжают приходить — поэтому снимать заметку с
// отслеживания по нему нельзя (проверено на 312811: помечена через 4 минуты
// после публикации, комментарии шли ещё сутки). Архивация — только по
// недельному правилу ShouldArchive.
func (m *Mirror) markClosedNotes(ctx context.Context, notes []love.Note) {
	for _, n := range notes {
		if !n.CommentsClosed {
			continue
		}
		changed, err := m.st.MarkNoteCommentsClosed(ctx, n.ID)
		if err != nil {
			m.log.Error("отметка «не актуальна»", "note", n.ID, "err", err)
			continue
		}
		if changed {
			m.log.Info("заметка закрыта для комментариев на сайте", "note", n.ID)
		}
	}
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

// postNote отправляет заметку в каналы всех приёмников (с аватаром живого
// автора, если есть) и, когда все приёмники отработали, помечает её posted.
// Идемпотентность per-messenger — по message_targets: приёмник, уже
// получивший пост, при ретрае пропускается.
func (m *Mirror) postNote(ctx context.Context, n store.Note) bool {
	avatar := m.fetchRealAvatar(ctx, n.AuthorAvatarURL)
	allPosted := true
	for _, sink := range m.sinks {
		_, _, found, err := m.st.Target(ctx, sink.Name(), store.TargetNotePost, n.ID)
		if err != nil {
			m.log.Error("чтение цели поста", "note", n.ID, "sink", sink.Name(), "err", err)
			allPosted = false
			continue
		}
		if found {
			continue
		}
		// Приёмник, открывающий тред сам (MAX), делает это ДО поста в канал:
		// тогда пост уже может вести кнопкой прямо в ветку заметки. Не вышло —
		// не страшно, тред дожмётся на цикле опроса (кнопка будет на чат).
		m.startThreadEarly(ctx, sink, n)
		msgID, err := sink.PostNote(ctx, n, avatar)
		if err != nil {
			m.log.Warn("пост заметки не удался, останется pending",
				"note", n.ID, "sink", sink.Name(), "err", err)
			allPosted = false
			continue
		}
		if err := m.st.SetTarget(ctx, sink.Name(), store.TargetNotePost, n.ID, msgID, ""); err != nil {
			m.log.Error("фиксация поста заметки", "note", n.ID, "sink", sink.Name(), "err", err)
			allPosted = false
			continue
		}
		m.log.Info("заметка запощена", "note", n.ID, "sink", sink.Name(), "message_id", msgID)
		// Ровно здесь, а не в ingestNewNote: ссылка на пост нужна своя в каждом
		// мессенджере. Приёмник, уже получивший пост, сюда не доходит (проверка
		// message_targets выше), так что ретрай дублей не шлёт, а seed молчит —
		// он до postNote не добирается вовсе.
		m.notifyAuthorSubscribers(ctx, sink, n, msgID)
	}
	if !allPosted {
		return false
	}
	if err := m.st.SetNoteStatusPosted(ctx, n.ID); err != nil {
		m.log.Error("фиксация статуса posted", "note", n.ID, "err", err)
		return false
	}
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
			m.archiveNote(ctx, noteID, now, "заметка ушла в архив")
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

// archiveNote переводит заметку в архив и логирует причину; воркер завершается.
func (m *Mirror) archiveNote(ctx context.Context, noteID string, now time.Time, reason string) {
	if err := m.st.SetNoteArchived(ctx, noteID, now); err != nil {
		m.log.Error("архивирование", "note", noteID, "err", err)
	}
	// Подписки на комментарии архивной заметки снимаем молча: новых
	// комментариев по ней не будет никогда, а строка вечно ела бы предел и
	// висела бы в /mysubs обманкой (о чём предупреждаем при подписке).
	if n, err := m.st.RemoveNoteSubscriptions(ctx, noteID); err != nil {
		m.log.Error("снятие подписок на архивную заметку", "note", noteID, "err", err)
	} else if n > 0 {
		m.log.Info("подписки на комментарии архивной заметки сняты", "note", noteID, "count", n)
	}
	m.log.Info(reason, "note", noteID)
}

// pollComments — один цикл опроса комментариев заметки.
func (m *Mirror) pollComments(ctx context.Context, n store.Note) {
	page, err := m.site.FetchCommentsPage(ctx, n.ID)
	if err != nil {
		m.log.Warn("комментарии недоступны", "note", n.ID, "err", err)
		m.reportSiteError(ctx, keyCommentsDrift, err)
		return
	}
	comments := page.Comments
	m.alert.OK(ctx, keyCommentsDrift)
	m.alert.OK(ctx, keyForbidden)
	m.applyNoteHeader(ctx, n, page.Note)
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

	m.sendUnsent(ctx, n, fresh)
}

// applyNoteHeader применяет необязательные обновления из шапки заметки на
// странице комментариев (nil — шапка не разобралась): дописывает свежие
// иллюстрации (модераторы добавляют/заменяют картинки после публикации —
// новая уйдёт в треды обычным путём sendUnsentImages) и ловит признак
// «комментарии запрещены» для заметок, уже выпавших из окна ленты.
func (m *Mirror) applyNoteHeader(ctx context.Context, n store.Note, header *love.Note) {
	if header == nil {
		return
	}
	m.storeNoteImages(ctx, n.ID, header.Images)
	if header.CommentsClosed && !n.CommentsClosed {
		changed, err := m.st.MarkNoteCommentsClosed(ctx, n.ID)
		if err != nil {
			m.log.Error("отметка «комментарии запрещены»", "note", n.ID, "err", err)
		} else if changed {
			m.log.Info("заметка закрыта для комментариев на сайте (страница заметки)", "note", n.ID)
		}
	}
}

// startThread просит приёмник-ThreadStarter открыть тред заметки и
// фиксирует пойманный корень. "" — приёмник не умеет или не смог (повторим
// следующим циклом).
func (m *Mirror) startThread(ctx context.Context, sink Sink, n store.Note) string {
	ts, ok := sink.(ThreadStarter)
	if !ok {
		return ""
	}
	postID, _, found, err := m.st.Target(ctx, sink.Name(), store.TargetNotePost, n.ID)
	if err != nil || !found || postID == "" {
		return ""
	}
	thread, err := ts.StartThread(ctx, n, postID)
	if err != nil {
		m.log.Warn("тред не открыт", "note", n.ID, "sink", sink.Name(), "err", err)
		return ""
	}
	if err := m.st.SetTarget(ctx, sink.Name(), store.TargetNoteThread, n.ID, "", thread); err != nil {
		m.log.Error("фиксация треда", "note", n.ID, "sink", sink.Name(), "err", err)
		return ""
	}
	m.log.Info("тред открыт приёмником", "note", n.ID, "sink", sink.Name(), "thread", thread)
	return thread
}

// startThreadEarly открывает тред до поста в канал — у приёмника, который
// умеет это сам и ещё не имеет корня. Нужен MAX: кнопка «Обсудить» на посте
// канала ведёт в ветку заметки, а её id известен только после копии в чате.
// Все ошибки мягкие: без корня пост уйдёт обычным путём.
func (m *Mirror) startThreadEarly(ctx context.Context, sink Sink, n store.Note) {
	ts, ok := sink.(ThreadStarter)
	if !ok {
		return
	}
	if _, thread, found, err := m.st.Target(ctx, sink.Name(), store.TargetNoteThread, n.ID); err != nil ||
		(found && thread != "") {
		return
	}
	thread, err := ts.StartThread(ctx, n, "")
	if err != nil {
		m.log.Warn("тред не открыт до поста", "note", n.ID, "sink", sink.Name(), "err", err)
		return
	}
	if err := m.st.SetTarget(ctx, sink.Name(), store.TargetNoteThread, n.ID, "", thread); err != nil {
		m.log.Error("фиксация треда", "note", n.ID, "sink", sink.Name(), "err", err)
		return
	}
	m.log.Info("тред открыт до поста", "note", n.ID, "sink", sink.Name(), "thread", thread)
}

// sendUnsent отправляет неотправленное содержимое заметки в тред каждого
// приёмника: сперва иллюстрации (они должны быть раньше комментариев), затем
// комментарии. Приёмник без пойманного треда пропускается — его содержимое
// уйдёт следующим циклом, когда появится корень треда.
func (m *Mirror) sendUnsent(ctx context.Context, n store.Note, fresh int) {
	var subs []store.Subscription
	subsLoaded := false
	for _, sink := range m.sinks {
		// Заметка, не зеркалившаяся в этот приёмник (запощена до его
		// включения), в его цикле не участвует: комментарии не очередятся.
		_, _, posted, err := m.st.Target(ctx, sink.Name(), store.TargetNotePost, n.ID)
		if err != nil {
			m.log.Error("чтение цели поста", "note", n.ID, "sink", sink.Name(), "err", err)
			continue
		}
		if !posted {
			continue
		}
		_, thread, found, err := m.st.Target(ctx, sink.Name(), store.TargetNoteThread, n.ID)
		if err != nil {
			m.log.Error("чтение цели треда", "note", n.ID, "sink", sink.Name(), "err", err)
			continue
		}
		if !found || thread == "" {
			thread = m.startThread(ctx, sink, n)
			if thread == "" {
				if fresh > 0 {
					m.log.Info("тред ещё не пойман, комментарии в очереди",
						"note", n.ID, "sink", sink.Name(), "queued", fresh)
				}
				continue
			}
		}
		if !m.sendUnsentImages(ctx, sink, thread, n) {
			continue // иллюстрацию не отправили — комментарии подождут (порядок)
		}
		unsent, err := m.st.UnsentCommentsFor(ctx, sink.Name(), n.ID)
		if err != nil {
			m.log.Error("чтение неотправленных комментариев", "note", n.ID, "sink", sink.Name(), "err", err)
			continue
		}
		if len(unsent) == 0 {
			continue
		}
		if !subsLoaded {
			subsLoaded = true
			if subs, err = m.st.Subscriptions(ctx); err != nil {
				m.log.Error("чтение подписок", "err", err)
			}
		}
		for _, c := range unsent {
			avatar := m.fetchMedia(ctx, c.AvatarURL, "аватар комментария")
			msgID, err := sink.PostComment(ctx, n, thread, m.addresseeMessage(ctx, sink, n, c), c, avatar)
			if err != nil {
				// Порядок важен: не перескакиваем через неотправленный.
				m.log.Warn("пост комментария не удался", "comment", c.ID, "sink", sink.Name(), "err", err)
				break
			}
			if err := m.st.SetTarget(ctx, sink.Name(), store.TargetComment,
				strconv.FormatInt(c.ID, 10), msgID, ""); err != nil {
				m.log.Error("фиксация поста комментария", "comment", c.ID, "sink", sink.Name(), "err", err)
				break
			}
			m.notifySubscribers(ctx, subs, sink, n, c, thread, msgID)
		}
	}
}

// addresseeMessage определяет, какому сообщению треда отвечает комментарий:
// обращение «Ник, …» в начале текста — это адресат (parent_id сайта указывает
// на корень ветки и сходится с адресатом лишь в трети случаев, см.
// love.AddressPrefix), ник ищется среди уже отзеркаленных комментаторов этой
// заметки. Пусто — обращения нет, автор не комментировал (адресуются и автору
// самой заметки) или ник не разошёлся: комментарий уйдёт на корень треда.
func (m *Mirror) addresseeMessage(ctx context.Context, sink Sink, n store.Note, c store.Comment) string {
	nick := love.AddressPrefix(c.Text)
	if nick == "" {
		return ""
	}
	msgID, err := m.st.AddresseeMessage(ctx, sink.Name(), n.ID, c.ID, nick)
	if err != nil {
		m.log.Error("поиск адресата реплики", "comment", c.ID, "sink", sink.Name(), "err", err)
		return ""
	}
	return msgID
}

// sendUnsentImages шлёт неотправленные иллюстрации заметки в тред приёмника.
// Возвращает false, если отправка сорвалась (мессенджер/фиксация) — цикл
// прерывается, чтобы иллюстрация ушла раньше комментариев. Нескачавшуюся
// картинку пропускаем, чтобы битый URL не блокировал комментарии навсегда.
func (m *Mirror) sendUnsentImages(ctx context.Context, sink Sink, thread string, n store.Note) bool {
	imgs, err := m.st.UnsentNoteImagesFor(ctx, sink.Name(), n.ID)
	if err != nil {
		m.log.Error("чтение иллюстраций", "note", n.ID, "sink", sink.Name(), "err", err)
		return true
	}
	for _, img := range imgs {
		data, err := m.site.FetchMedia(ctx, img.URL)
		if err != nil {
			m.log.Warn("иллюстрация не скачана, пропускаем", "note", n.ID, "url", img.URL, "err", err)
			continue
		}
		msgID, err := sink.PostNoteImage(ctx, thread, img.URL, data)
		if err != nil {
			m.log.Warn("иллюстрация не отправлена в тред", "note", n.ID, "sink", sink.Name(), "err", err)
			return false
		}
		if err := m.st.SetTarget(ctx, sink.Name(), store.TargetNoteImage,
			strconv.FormatInt(img.ID, 10), msgID, ""); err != nil {
			m.log.Error("фиксация иллюстрации", "note", n.ID, "sink", sink.Name(), "err", err)
			return false
		}
		m.log.Info("иллюстрация отправлена в тред", "note", n.ID, "sink", sink.Name(), "message_id", msgID)
	}
	return true
}

// storeNoteImages идемпотентно сохраняет иллюстрации заметки (по URL);
// уже известные пропускаются, новые получат посты в тредах.
func (m *Mirror) storeNoteImages(ctx context.Context, noteID string, images []string) {
	for i, url := range images {
		if err := m.st.InsertNoteImage(ctx, noteID, i, url); err != nil {
			m.log.Error("сохранение иллюстрации", "note", noteID, "url", url, "err", err)
		}
	}
}

// fetchMedia качает медиа с RU-IP. При ошибке — nil: сообщение уйдёт без
// картинки (Telegram не может забрать медиа love.ngs.ru со своих серверов).
func (m *Mirror) fetchMedia(ctx context.Context, url, what string) []byte {
	if url == "" {
		return nil
	}
	b, err := m.site.FetchMedia(ctx, url)
	if err != nil {
		m.log.Warn(what+" не скачан", "url", url, "err", err)
		return nil
	}
	return b
}

// fetchRealAvatar качает аватар автора заметки только если это фото живого
// человека; для дефолтного силуэта — nil (заметка уходит без аватара).
func (m *Mirror) fetchRealAvatar(ctx context.Context, url string) []byte {
	if !isRealAvatar(url) {
		return nil
	}
	return m.fetchMedia(ctx, url, "аватар автора")
}

// isRealAvatar отличает загруженное фото от дефолтного силуэта: плейсхолдеры
// лежат в /static/ на самом сайте, реальные фото — на CDN (абсолютный URL).
func isRealAvatar(url string) bool {
	return strings.HasPrefix(url, "http") && !strings.Contains(url, "/static/")
}

// notifySubscribers шлёт ЛС подписчикам этого мессенджера, которых касается
// новый комментарий: подписка на комментарии этой заметки и подписка на слово
// в её тексте. Один комментарий — одно ЛС на человека: сработавшие подписки
// схлопываются по пользователю, и при совпадении выигрывает подписка на
// заметку. Она точнее: заметку человек выбрал сам, слово могло совпасть
// случайно, и отписываться из уведомления он захочет именно от заметки.
func (m *Mirror) notifySubscribers(ctx context.Context, subs []store.Subscription,
	sink Sink, n store.Note, c store.Comment, threadID, commentMsgID string) {
	sent := make(map[int64]bool, len(subs))
	for _, kind := range []string{store.SubNoteComments, store.SubKeyword} {
		for _, sub := range subs {
			if sub.Messenger != sink.Name() || sub.Kind != kind || sent[sub.UserID] {
				continue
			}
			if kind == store.SubNoteComments && sub.Target != n.ID {
				continue
			}
			if kind == store.SubKeyword && !strings.Contains(c.Text, sub.Target) {
				continue
			}
			sent[sub.UserID] = true
			m.deliver(ctx, sink, sub.UserID, SubEvent{
				Sub: sub, Note: n, Comment: c, ThreadID: threadID, MsgID: commentMsgID,
			})
		}
	}
}

// notifyAuthorSubscribers — новая заметка автора ушла в канал этого приёмника:
// шлём ЛС подписчикам автора. У анонимной заметки (author_id «0») подписчиков
// нет по построению — вариант «на автора» под ней не предлагается.
func (m *Mirror) notifyAuthorSubscribers(ctx context.Context, sink Sink, n store.Note, postMsgID string) {
	if n.AuthorID == "" || n.AuthorID == "0" {
		return
	}
	subs, err := m.st.SubscribersByTarget(ctx, sink.Name(), store.SubAuthorNotes, n.AuthorID)
	if err != nil {
		m.log.Error("чтение подписок на автора", "note", n.ID, "err", err)
		return
	}
	for _, sub := range subs {
		m.deliver(ctx, sink, sub.UserID, SubEvent{Sub: sub, Note: n, PostMsgID: postMsgID})
	}
}

// deliver отдаёт событие ЛС-боту мессенджера.
func (m *Mirror) deliver(ctx context.Context, sink Sink, userID int64, ev SubEvent) {
	notify := m.subNotify[sink.Name()]
	if notify == nil {
		m.log.Debug("уведомление подписчика некому отдать", "sink", sink.Name(), "user", userID)
		return
	}
	notify(ctx, userID, ev)
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
