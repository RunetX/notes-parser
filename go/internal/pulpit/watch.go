package pulpit

// Цикл ленты: что делать с заметкой и когда её уже поздно трогать. Решение
// вынесено в чистую decide — от него зависит, появится ли на сайте необратимая
// реплика, и проверять его надо таблицей, а не глазами по логам.

import (
	"context"
	"fmt"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// action — что делать с заметкой.
type action int

const (
	actIdle action = iota // ничего не писать в БД (фича выключена, заметка занята)
	actMark               // записать строку skipped с причиной и не трогать заметку
	actPost               // писать реплику
)

// Причины пропуска. Строки уходят в БД и в логи — читаются глазами при разборе
// «почему под этой заметкой нас нет».
const (
	reasonColdStart = "cold_start" // первый обход после старта/включения
	reasonStale     = "stale"      // заметка успела обжиться, первыми уже не быть
	reasonQuota     = "quota"      // суточный предохранитель
	reasonTooLate   = "too_late"   // генерация не уложилась в бюджет
	reasonDisabled  = "disabled"
	reasonTaken     = "taken"
	reasonNoLLM     = "no_llm"
	reasonNoSession = "no_session"
	reasonNoteGone  = "note_gone"
	reasonNoReply   = "no_reply"
	reasonDeleted   = "deleted"
	reasonCoin      = "coin"    // монетка легла «не отвечать»
	reasonNoJoke    = "no_joke" // в заметке настоящая беда: шутить нечем
)

// decideInput — всё, от чего зависит решение по заметке.
type decideInput struct {
	Enabled   bool          // рантайм-тумблер
	Cold      bool          // первый обход после старта или включения
	Taken     bool          // заметку уже занял другой вход (claim не прошёл)
	Age       time.Duration // сколько прошло с момента, когда мы её увидели
	Freshness time.Duration
	Today     int // сколько реплик уже ушло за сутки
	MaxPerDay int
}

// decide — писать ли реплику под этой заметкой. Порядок проверок и есть
// приоритет: выключенная фича молчит вовсе (в БД ни строчки), занятая заметка
// не наша, холодный старт помечает и молчит, дальше свежесть и суточный
// потолок.
func decide(in decideInput) (action, string) {
	switch {
	case !in.Enabled:
		return actIdle, reasonDisabled
	case in.Taken:
		return actIdle, reasonTaken
	case in.Cold:
		return actMark, reasonColdStart
	case in.Freshness > 0 && in.Age > in.Freshness:
		return actMark, reasonStale
	case in.MaxPerDay > 0 && in.Today >= in.MaxPerDay:
		return actMark, reasonQuota
	default:
		return actPost, ""
	}
}

// feedCycle — один обход своей ленты. false — лента не прочиталась (тогда
// холодный старт остаётся взведённым: пометить заметки было нечем).
func (s *Service) feedCycle(ctx context.Context, cold bool) bool {
	notes, err := s.site.FetchNotes(ctx)
	if err != nil {
		s.log.Warn("амвон: лента недоступна", "err", err)
		return false
	}
	today, err := s.st.PulpitSentSince(ctx, dayStart(time.Now()))
	if err != nil {
		s.log.Error("амвон: суточный счёт", "err", err)
		return false
	}
	// Лента отдаёт новые сверху: идём с конца, чтобы старые заметки получали
	// реплику первыми — иначе в редком догоне порядок реплик разъедется с
	// порядком заметок.
	for i := len(notes) - 1; i >= 0; i-- {
		if s.handleNote(ctx, notes[i], cold, today) {
			today++
		}
	}
	return true
}

// handleNote занимает заметку и, если она наша и свежая, пишет под ней реплику.
// Возвращает true, если реплика ушла на сайт (для суточного счёта в этом обходе).
func (s *Service) handleNote(ctx context.Context, n love.Note, cold bool, today int) bool {
	if today < 0 {
		var err error
		if today, err = s.st.PulpitSentSince(ctx, dayStart(time.Now())); err != nil {
			s.log.Error("амвон: суточный счёт", "err", err)
			return false
		}
	}
	enabled, err := s.Enabled(ctx)
	if err != nil {
		s.log.Error("амвон: чтение тумблера", "err", err)
		return false
	}
	act, reason := decide(decideInput{
		Enabled: enabled, Cold: cold, Freshness: s.cfg.Freshness,
		Today: today, MaxPerDay: s.cfg.MaxPerDay,
	})
	if act == actIdle {
		return false
	}
	state := store.PulpitQueued
	if act == actMark {
		state = store.PulpitSkipped
	}
	now := time.Now()
	claimed, err := s.st.TryClaimPulpitNote(ctx, n.ID, state, reason, now)
	if err != nil {
		s.log.Error("амвон: занять заметку", "note", n.ID, "err", err)
		return false
	}
	if !claimed || act != actPost {
		// Не занята нами — значит уже разобрана (в том числе прошлым обходом
		// или колбэком зеркала); помеченную заметку трогать больше незачем.
		return false
	}
	return s.postQuip(ctx, n, now)
}

// resumeQueued дожимает строки, застрявшие в queued: демон упал между claim'ом
// и отправкой. Свежесть считается от момента claim'а, поэтому старьё сюда не
// проскочит — оно уедет в skipped:stale.
func (s *Service) resumeQueued(ctx context.Context) {
	rows, err := s.st.PulpitByState(ctx, store.PulpitQueued)
	if err != nil {
		s.log.Error("амвон: чтение очереди", "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	today, err := s.st.PulpitSentSince(ctx, dayStart(time.Now()))
	if err != nil {
		s.log.Error("амвон: суточный счёт", "err", err)
		return
	}
	for _, row := range rows {
		act, reason := decide(decideInput{
			Enabled: true, Age: time.Since(row.SeenAt), Freshness: s.cfg.Freshness,
			Today: today, MaxPerDay: s.cfg.MaxPerDay,
		})
		if act != actPost {
			if _, err := s.st.CASPulpitState(ctx, row.NoteID, store.PulpitQueued,
				store.PulpitSkipped, reason); err != nil {
				s.log.Error("амвон: снятие с очереди", "note", row.NoteID, "err", err)
			}
			continue
		}
		n, err := s.noteByID(ctx, row.NoteID)
		if err != nil {
			s.log.Warn("амвон: заметка из очереди не читается", "note", row.NoteID, "err", err)
			continue
		}
		if s.postQuip(ctx, n, row.SeenAt) {
			today++
		}
	}
}

// NoteByID — то же, что noteByID, но наружу: им пользуется CLI-черновик
// (`lovegw pulpit draft <id>`), которому заметку надо где-то взять.
func (s *Service) NoteByID(ctx context.Context, id string) (love.Note, error) {
	return s.noteByID(ctx, id)
}

// noteByID — текст заметки: сперва из своей БД (её кладёт зеркало), иначе с
// сайта. Нужен догону: в очереди лежит только id.
func (s *Service) noteByID(ctx context.Context, id string) (love.Note, error) {
	if n, err := s.st.NoteByID(ctx, id); err == nil {
		return love.Note{
			ID: n.ID, AuthorID: n.AuthorID, AuthorName: n.AuthorName, Text: n.Text,
		}, nil
	}
	page, err := s.site.FetchCommentsPage(ctx, id)
	if err != nil {
		return love.Note{}, err
	}
	if page.Note == nil {
		return love.Note{}, fmt.Errorf("заметка %s: шапка не разобрана", id)
	}
	n := *page.Note
	n.ID = id // ParseNoteFromCommentsPage id не заполняет: его знает вызывающий
	return n, nil
}
