package pulpit

// Верификация: наша реплика в треде — или её там нет. Это единственный способ
// узнать и то, что POST дошёл, и то, что нас забанили: запрет писать в
// «Заметки» ничего не убирает с площадки, анкета остаётся живой, ответ сайта
// остаётся двухсотым — не появляется только сама реплика.
//
// Своя реплика опознаётся по AuthorID (он есть в разметке обоих видов
// страницы), а не эвристикой «чего не было»: заметка новая, мы под ней первые,
// поэтому наш комментарий — самый ранний из своих. Ответы на ответы приходят
// позже и имеют бо́льшие id.

import (
	"context"
	"errors"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// verifyAttempts — сколько раз перечитать тред, прежде чем счесть реплику
// пропавшей. Двух хватает: сайт показывает свежий комментарий сразу, а один
// запас — на случай кэша страницы.
const verifyAttempts = 2

// recheckAfter — через сколько после отправки перепроверяем подтверждённую
// реплику: модерация чистит треды не мгновенно.
const recheckAfter = 30 * time.Minute

// recheckWindow — как глубоко назад перепроверяем подтверждённые реплики.
// Ограничение по времени, а не «все confirmed»: строк там накапливается по
// десятку в сутки, и перечитывать их вечно незачем — модерация чистит треды в
// первые часы.
const recheckWindow = 24 * time.Hour

// verifyCycle проверяет отправленные реплики и перепроверяет подтверждённые.
func (s *Service) verifyCycle(ctx context.Context) {
	rows, err := s.st.PulpitByState(ctx, store.PulpitPosting, store.PulpitPosted)
	if err != nil {
		s.log.Error("амвон: чтение отправленных реплик", "err", err)
		return
	}
	for _, row := range rows {
		s.verifyNote(ctx, row)
	}

	confirmed, err := s.st.PulpitConfirmedSince(ctx, time.Now().Add(-recheckWindow))
	if err != nil {
		s.log.Error("амвон: чтение подтверждённых реплик", "err", err)
		return
	}
	for _, row := range confirmed {
		if needsRecheck(row, time.Now()) {
			s.recheckConfirmed(ctx, row)
		}
	}
}

// needsRecheck — пора ли перечитать тред подтверждённой реплики. Ровно один раз
// на реплику: после проверки checked_at уезжает за горизонт и условие гаснет.
func needsRecheck(row store.PulpitComment, now time.Time) bool {
	if row.PostedAt.IsZero() {
		return false
	}
	horizon := row.PostedAt.Add(recheckAfter)
	return now.After(horizon) && row.CheckedAt.Before(horizon)
}

// verifyNote ищет свою реплику в треде заметки.
func (s *Service) verifyNote(ctx context.Context, row store.PulpitComment) {
	// Окно САМЫХ НОВЫХ: реплику только что отправили, и она в треде последняя.
	page, ok := s.threadOf(ctx, row, newestWindow)
	if !ok {
		return
	}
	now := time.Now()
	if c, found := ownComment(page.Comments, s.cfg.OwnerProfileID); found {
		if err := s.st.ConfirmPulpitComment(ctx, row.NoteID, c.ID, now); err != nil {
			s.log.Error("амвон: подтверждение реплики", "note", row.NoteID, "err", err)
			return
		}
		// Бесплатный источник своего ника: под нашей же репликой сайт написал
		// её автора. По нику опознаются обращения к нам.
		s.setNick(c.AuthorName)
		s.log.Info("амвон: реплика на месте", "note", row.NoteID, "comment", c.ID)
		return
	}
	checks, err := s.st.BumpPulpitCheck(ctx, row.NoteID, now)
	if err != nil {
		s.log.Error("амвон: отметка проверки", "note", row.NoteID, "err", err)
		return
	}
	if checks < verifyAttempts {
		return
	}
	// Причину сбоя отправки сохраняем: реплики нет потому, что она не ушла, —
	// предохранителю такое отсутствие ни о чём не говорит (см. outcomeOf).
	reason := reasonNoReply
	if row.Reason == reasonSendFailed {
		reason = reasonSendFailed
	}
	if err := s.st.SetPulpitState(ctx, row.NoteID, store.PulpitMissing, reason, now); err != nil {
		s.log.Error("амвон: отметка пропажи", "note", row.NoteID, "err", err)
		return
	}
	s.log.Warn("амвон: реплики нет в треде", "note", row.NoteID, "проверок", checks, "причина", reason)
}

// recheckConfirmed смотрит, на месте ли уже подтверждённая реплика: её могла
// вычистить модерация, и это второй сигнал предохранителя.
func (s *Service) recheckConfirmed(ctx context.Context, row store.PulpitComment) {
	// Окно САМЫХ РАННИХ: своя реплика в треде старейшая — мы под заметкой первые.
	// В обычном окне «тридцать последних» её через полчаса уже нет, и раньше это
	// засчитывалось чисткой (боевой случай 313058, 23.08.2026: 88 реплик в треде,
	// наша самая первая, вердикт «вычистили» ровно через тридцать минут).
	page, ok := s.threadOf(ctx, row, oldestWindow)
	if !ok {
		return
	}
	now := time.Now()
	for _, c := range page.Comments {
		if c.ID == row.CommentID {
			// Тем же вызовом обновляем checked_at — второй перепроверки не будет.
			if err := s.st.ConfirmPulpitComment(ctx, row.NoteID, row.CommentID, now); err != nil {
				s.log.Error("амвон: отметка перепроверки", "note", row.NoteID, "err", err)
			}
			return
		}
	}
	// ПРАВИЛО ОХВАТА, то же, что у modwatch: пропажа считается удалением только у
	// объекта, которого страница вообще МОГЛА показать. За краем окна наша реплика
	// может оказаться и здесь — если мы влезли в заметку, где уже лежало тридцать
	// чужих. Тогда вердикта нет вовсе: «не видели» это не «удалили».
	if !pageCovers(page, row.CommentID) {
		if err := s.st.ConfirmPulpitComment(ctx, row.NoteID, row.CommentID, now); err != nil {
			s.log.Error("амвон: отметка перепроверки", "note", row.NoteID, "err", err)
		}
		s.log.Info("амвон: реплика вне окна страницы — вердикта нет",
			"note", row.NoteID, "comment", row.CommentID, "в треде", page.Total)
		return
	}
	if err := s.st.SetPulpitState(ctx, row.NoteID, store.PulpitMissing, reasonDeleted, now); err != nil {
		s.log.Error("амвон: отметка удаления", "note", row.NoteID, "err", err)
		return
	}
	s.log.Warn("амвон: подтверждённую реплику удалили", "note", row.NoteID, "comment", row.CommentID)
}

// threadOf читает тред заметки. ok == false — вердикта нет: сбой загрузки
// промахом НЕ считается (иначе известные короткие 5xx-штормы сайта или 403
// геоблока выключали бы фичу), а исчезнувшая заметка — это снесли её, а не нас.
// Снос сайт показывает страницей-заглушкой с кодом 200 (ErrNoteDeleted), а не
// 404: до того как парсер научился её узнавать, снесённая заметка приезжала
// сюда пустым тредом, реплика в нём не находилась — и снос засчитывался
// промахом, то есть три сноса подряд гасили фичу за чужое действие.
func (s *Service) threadOf(ctx context.Context, row store.PulpitComment, window threadWindow) (love.CommentsPage, bool) {
	fetch := s.site.FetchCommentsPage
	if window == oldestWindow {
		fetch = s.site.FetchOldestComments
	}
	page, err := fetch(ctx, row.NoteID)
	if errors.Is(err, love.ErrNotFound) || errors.Is(err, love.ErrNoteDeleted) {
		if err := s.st.SetPulpitState(ctx, row.NoteID, store.PulpitVanished, reasonNoteGone, time.Now()); err != nil {
			s.log.Error("амвон: отметка исчезнувшей заметки", "note", row.NoteID, "err", err)
		}
		s.log.Info("амвон: заметка исчезла с сайта", "note", row.NoteID)
		return love.CommentsPage{}, false
	}
	if err != nil {
		s.log.Warn("амвон: тред недоступен", "note", row.NoteID, "err", err)
		return love.CommentsPage{}, false
	}
	return page, true
}

// threadWindow — какой край треда читать.
//
// Разделение не педантизм: две проверки ищут РАЗНОЕ. Свежеотправленная реплика
// в треде последняя, подтверждённая полчаса назад — первая, а страница отдаёт
// окно из тридцати строк с одного конца. Спросив не тот край, мы не находим
// реплику, которая на месте, — и это ровно то, из-за чего амвон выключил себя
// сам 23.08.2026.
type threadWindow int

const (
	newestWindow threadWindow = iota // тридцать последних (обычная страница)
	oldestWindow                     // тридцать первых (asc)
)

// pageCovers — МОГЛА ли эта страница показать реплику с таким id.
//
// Окно покрывает тред целиком, когда счётчик треда сходится с числом строк на
// странице: тогда отсутствие реплики значит именно отсутствие. Иначе окно
// частичное, и asc-страница покрывает всё до самого позднего id на ней; что
// дальше — мы не смотрели. Счётчика может не быть вовсе (Total == 0), и тогда
// судить не о чем.
func pageCovers(page love.CommentsPage, commentID int64) bool {
	if page.Total > 0 && page.Total <= len(page.Comments) {
		return true
	}
	var newest int64
	for _, c := range page.Comments {
		if c.ID > newest {
			newest = c.ID
		}
	}
	return newest > 0 && commentID < newest
}

// ownComment — своя реплика первого уровня: самая ранняя из наших под этой
// заметкой (ответы на ответы приходят позже и имеют бо́льшие id).
func ownComment(comments []love.Comment, ownProfileID string) (love.Comment, bool) {
	if ownProfileID == "" {
		return love.Comment{}, false
	}
	var best love.Comment
	found := false
	for _, c := range comments {
		if c.AuthorID != ownProfileID {
			continue
		}
		if !found || c.ID < best.ID {
			best, found = c, true
		}
	}
	return best, found
}
