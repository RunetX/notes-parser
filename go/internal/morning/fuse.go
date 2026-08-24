package morning

// Верификация и предохранитель.
//
// Запрет писать в «Заметки» НГС не оставляет следа: анкета жива, сайт отвечает
// 200, а заметка просто не появляется в ленте. Единственный способ узнать —
// пойти и посмотреть, вышла ли она. Амвон научен этому дорого (первую же его
// реплику сняли за 15 минут и выдали владельцу сутки запрета), и здесь заведён
// тот же механизм в уменьшенном виде.
//
// Проверок ДВЕ, и вторая не стоит ни одного лишнего запроса: свежую заметку
// ищем в ленте через несколько минут после отправки, а вчерашнюю — попутно, в
// той же ленте, перед публикацией сегодняшней. Снос заметки виден так же
// хорошо, как и её отсутствие, а отдельного таймера не нужно.
//
// Правило охвата обязательно (`notePresence`): лента отдаёт последние тридцать
// заметок, и уехавшую за нижний край нельзя считать снесённой — иначе к концу
// суток предохранитель срабатывал бы сам собой.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// fuseHorizon — за какой срок считается полоса промахов. Урок ложного
// выключения амвона 23.08.2026: строк у такой службы мало, и «два промаха
// подряд» без горизонта однажды сложатся из событий, между которыми неделя.
const fuseHorizon = 48 * time.Hour

// outcome — исход дня глазами предохранителя.
type outcome int

const (
	outcomeNeutral outcome = iota // пропуск, отказ модели, не дошедший POST
	outcomeGood                   // заметка вышла и найдена
	outcomeMiss                   // заметка не появилась или исчезла
)

// outcomeOf — как день считается предохранителем. Не долетевший POST
// НЕЙТРАЛЕН: 17.08.2026 сайт отвечал 500 на любую публикацию, и вина была не
// наша — полосу такое не растит и не разрывает.
func outcomeOf(n store.MorningNote) outcome {
	switch n.State {
	case store.MorningConfirmed:
		return outcomeGood
	case store.MorningMissing:
		if n.Reason == store.MorningReasonSendFailed {
			return outcomeNeutral
		}
		return outcomeMiss
	default:
		return outcomeNeutral
	}
}

// fuseVerdict — гасить ли фичу. Чистая функция: полоса считается ИЗ БД (rows от
// новых к старым), поэтому краш-луп её не сбрасывает, а горизонт не даёт
// сложить в полосу давние промахи.
func fuseVerdict(rows []store.MorningNote, now time.Time, maxMisses int) (off bool, reason string) {
	misses := 0
	var days []string
	for _, n := range rows {
		if now.Sub(n.CreatedAt) > fuseHorizon {
			break
		}
		switch outcomeOf(n) {
		case outcomeGood:
			return false, "" // свежая удача разрывает полосу
		case outcomeMiss:
			misses++
			days = append(days, n.Day)
			if misses >= maxMisses {
				return true, "заметка не появилась в ленте " + strings.Join(days, ", ")
			}
		}
	}
	return false, ""
}

// verifyPending доводит до ясности дни, чей исход неизвестен: посланные, но
// ещё не найденные в ленте. Один поход в ленту на такт — и только если есть
// что проверять.
func (s *Service) verifyPending(ctx context.Context) {
	rows, err := s.st.MorningRecent(ctx, 3)
	if err != nil {
		s.log.Error("утренняя заметка: чтение последних дней", "err", err)
		return
	}
	var pending []store.MorningNote
	now := s.now()
	for _, n := range rows {
		if n.State != store.MorningPosted && n.State != store.MorningPosting {
			continue
		}
		if now.Sub(n.PostedAt) < verifyDelay {
			continue // сайту нужна фора: заметка появляется не мгновенно
		}
		if !n.CheckedAt.IsZero() && now.Sub(n.CheckedAt) < verifyEvery {
			continue
		}
		pending = append(pending, n)
	}
	if len(pending) == 0 {
		return
	}
	feed, err := s.site.FetchNotes(ctx)
	if err != nil {
		s.log.Warn("утренняя заметка: лента не прочиталась при проверке", "err", err)
		return
	}
	for _, n := range pending {
		s.verifyOne(ctx, n, feed, now)
	}
	s.checkFuse(ctx)
}

// verifyOne ищет заметку дня в ленте по автору и началу текста: id своей
// заметки сайт при публикации не возвращает вовсе.
func (s *Service) verifyOne(ctx context.Context, n store.MorningNote, feed []love.Note, now time.Time) {
	if id := findOwnNote(feed, s.cfg.OwnerProfileID, n.Text); id != "" {
		if err := s.st.ConfirmMorning(ctx, n.Day, id, now); err != nil {
			s.log.Error("утренняя заметка: подтверждение", "день", n.Day, "err", err)
			return
		}
		s.log.Info("утренняя заметка на месте", "день", n.Day, "заметка", id)
		return
	}
	checks, err := s.st.BumpMorningCheck(ctx, n.Day, now)
	if err != nil {
		s.log.Error("утренняя заметка: счёт проверок", "день", n.Day, "err", err)
		return
	}
	if checks < maxChecks {
		return
	}
	reason := "не появилась в ленте"
	if n.Reason == store.MorningReasonSendFailed {
		reason = store.MorningReasonSendFailed
	}
	if err := s.st.SetMorningState(ctx, n.Day, store.MorningMissing, reason); err != nil {
		s.log.Error("утренняя заметка: отметка пропажи", "день", n.Day, "err", err)
		return
	}
	s.log.Warn("утренняя заметка не найдена в ленте", "день", n.Day, "проверок", checks)
}

// checkOwnPresence — жива ли вчерашняя заметка. Зовётся из общего обхода ленты,
// поэтому своих запросов не стоит.
func (s *Service) checkOwnPresence(ctx context.Context, feed []love.Note, day string) {
	prev := PrevDay(day)
	if prev == "" {
		return
	}
	n, err := s.st.MorningByDay(ctx, prev)
	if err != nil || n.State != store.MorningConfirmed || n.NoteID == "" {
		return
	}
	if notePresence(feed, n.NoteID) != presenceGone {
		return
	}
	if err := s.st.SetMorningState(ctx, prev, store.MorningMissing, "исчезла из ленты"); err != nil {
		s.log.Error("утренняя заметка: отметка пропажи", "день", prev, "err", err)
		return
	}
	s.log.Warn("вчерашняя утренняя заметка исчезла", "день", prev, "заметка", n.NoteID)
	s.checkFuse(ctx)
}

// checkFuse смотрит полосу и, если пора, гасит фичу.
func (s *Service) checkFuse(ctx context.Context) {
	rows, err := s.st.MorningRecent(ctx, 10)
	if err != nil {
		s.log.Error("утренняя заметка: чтение полосы", "err", err)
		return
	}
	off, reason := fuseVerdict(rows, s.now(), s.cfg.FuseMisses)
	if !off {
		return
	}
	s.disable(ctx, reason)
}

// disable гасит тумблер и зовёт владельца. Автовосстановления нет: срок запрета
// неизвестен, и включать обратно — только руками, посмотрев глазами.
func (s *Service) disable(ctx context.Context, reason string) {
	enabled, err := s.Enabled(ctx)
	if err != nil || !enabled {
		return // уже выключено — второй раз не сообщаем
	}
	if err := s.SetEnabled(ctx, false, "fuse"); err != nil {
		s.log.Error("утренняя заметка: выключение", "err", err)
		return
	}
	if err := s.st.SetFlag(ctx, store.FlagMorningOffReason, reason, "fuse", s.now()); err != nil {
		s.log.Error("утренняя заметка: причина выключения", "err", err)
	}
	if err := s.st.SetFlag(ctx, store.FlagMorningOffAt,
		s.now().Format("02.01.2006 15:04"), "fuse", s.now()); err != nil {
		s.log.Error("утренняя заметка: время выключения", "err", err)
	}
	detail := s.diagnose(ctx)
	s.log.Error("утренняя заметка выключена предохранителем", "причина", reason, "анкета", detail)
	s.alert.Fail(ctx, alertKey, fmt.Sprintf(
		"🌅 Утренняя заметка выключилась сама: %s. %s\nВключить обратно — /morning.", reason, detail))
}

// diagnose спрашивает у сайта, что с анкетой. Ответ не меняет решения (фича уже
// выключена), но именно он отличает «запретили писать» от «сайт лежит», а
// владельцу с этим идти разбираться.
func (s *Service) diagnose(ctx context.Context) string {
	cookies, err := s.cookies(ctx)
	if err != nil {
		return "Сессия анкеты недействительна — нужен /login."
	}
	ctrl, err := s.site.ProfileControl(ctx, cookies)
	switch {
	case err != nil:
		return "Состояние анкеты выяснить не вышло: " + err.Error()
	case ctrl.Blocked:
		return "Анкета заблокирована."
	default:
		return "Анкета жива — похоже, запрет только на раздел «Заметки»."
	}
}

// findOwnNote — своя заметка в ленте: автор наш и текст начинается так же.
// Сравниваем по началу, а не целиком: сайт схлопывает пробелы и подменяет
// эмодзи картинками, поэтому дословного совпадения не будет никогда.
func findOwnNote(feed []love.Note, ownerID, text string) string {
	head := noteHead(text)
	if head == "" {
		return ""
	}
	for _, n := range feed {
		if n.AuthorID != ownerID {
			continue
		}
		if strings.HasPrefix(noteHead(n.Text), head) {
			return n.ID
		}
	}
	return ""
}

// headRunes — по скольким первым рунам опознаём свою заметку. Сорока хватает:
// два разных утра так не начинаются, а больше — значит упереться в обрезку,
// которую лента делает у длинных заметок.
const headRunes = 40

func noteHead(text string) string {
	fields := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	r := []rune(fields)
	if len(r) > headRunes {
		r = r[:headRunes]
	}
	return string(r)
}
