package morning

// Решение «писать ли сейчас» — чистой функцией, как у амвона (`pulpit.decide`).
// Порядок проверок и есть приоритет: он читается сверху вниз и проверяется
// таблицей, а не боевым утром.

import (
	"time"

	"lovegw/internal/store"
)

type action int

const (
	actIdle action = iota // ничего не делать и ничего не записывать
	actMark               // записать день сразу в конечном состоянии
	actPost               // собирать поводы, писать и публиковать
)

// Причины — строками, а не кодами: они уезжают в БД, в лог и в отчёт `/morning`,
// и человек должен понимать их без сверки с исходником.
const (
	reasonDisabled  = "disabled"
	reasonDone      = "done"
	reasonEarly     = "early"
	reasonTooLate   = "too_late"
	reasonSomeone   = "someone_else"
	reasonNoSession = "no_session"
	reasonNoLLM     = "no_llm"
	reasonNoFacts   = "no_facts"
	reasonBadDraft  = "bad_draft"
)

type decideInput struct {
	Enabled bool
	Now     time.Time
	Slot    time.Time
	Grace   time.Duration
	// Done — строка этого дня уже есть в любом состоянии. Однократность держит
	// первичный ключ, но спрашивать заранее дешевле, чем собирать поводы и
	// генерировать текст, чтобы упереться в него на вставке.
	Done bool
	// Foreign — id чужой заметки с приветствием, если она сегодня уже есть.
	Foreign string
	// HasSession — есть ли живая сессия владельца: без неё публиковать нечем.
	HasSession bool
}

// verdict — что делать и как это назвать.
type verdict struct {
	Action action
	State  string // для actMark
	Reason string
	Detail string // подробность для ЛС: id чужой заметки с приветствием
}

func decide(in decideInput) verdict {
	switch {
	case !in.Enabled:
		return verdict{Action: actIdle, Reason: reasonDisabled}
	case in.Done:
		return verdict{Action: actIdle, Reason: reasonDone}
	case in.Now.Before(in.Slot):
		return verdict{Action: actIdle, Reason: reasonEarly}
	case in.Now.Sub(in.Slot) > in.Grace:
		// Доброе утро в полдень — уже не доброе утро. Догонять сутками, как
		// это делает дайджест, здесь нельзя: догон перекрыл бы следующий слот.
		return verdict{Action: actMark, State: store.MorningSkipped, Reason: reasonTooLate}
	case in.Foreign != "":
		// Хозяин утра — человек. Своё приветствие рядом с чужим выглядит не
		// как ритуал, а как соревнование, поэтому молчим и говорим владельцу.
		return verdict{Action: actMark, State: store.MorningSkipped, Reason: reasonSomeone, Detail: in.Foreign}
	case !in.HasSession:
		return verdict{Action: actMark, State: store.MorningFailed, Reason: reasonNoSession}
	default:
		return verdict{Action: actPost}
	}
}
