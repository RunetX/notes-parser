package narodsim

// Режим вакуума — «жители в пустой заметке».
//
// Из архива берётся ОДНА заметка, и больше ничего. Разговор растёт сам:
// заметка — точка решения каждому, каждая сказанная реплика — точка решения всем
// остальным, и так пока никто больше не захочет говорить. Ни одной чужой реплики
// в сценарий не подаётся, поэтому мерится здесь не «угадал ли житель чужой ход»,
// а ФОРМА РАЗГОВОРА, которая выходит из кубика сама: кто пришёл, сколько сказал,
// через сколько, кто кому и когда всё стихло.
//
// Разница с solo не в масштабе, а в том, ЧТО можно спросить. В solo правда
// известна в каждой точке, и цена этому — разговор, который всё равно идёт по
// рельсам оригинала: житель не может ни увести его, ни оживить, ни убить.
// Вакуум эти рельсы убирает, и потому только здесь видно, что кубик даёт на
// длинной дистанции — ошибка, незаметная на одном шаге, за сорок шагов
// превращается либо в мёртвую заметку, либо в шторм.
//
// СОСТАВ. Играют только те, у кого есть карточка, а в оригинале говорили ещё
// два-три десятка человек. Поэтому сравнивать «сколько вышло реплик» с
// оригиналом впрямую нельзя — и не сравниваем: оригинал СУЖАЕТСЯ до того же
// состава, и обе формы снимаются одной и той же функцией. Сужение при этом
// названо честно (доля состава в оригинале печатается рядом): в настоящем
// разговоре человек отвечал и тем, кого здесь нет, — то есть даже суженный
// оригинал остаётся целью приблизительной, а не эталоном.
//
// БЕСПЛАТНО ПО УМОЛЧАНИЮ. Модель нужна только на ТЕКСТ реплики; кто придёт,
// когда и кому — считает кубик, то есть формула. Значит вся форма разговора
// меряется без единого запроса, и цикл «правка кубика → прогон → отчёт» здесь
// такой же даровой, как в solo.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"lovegw/internal/archive"
)

// VacuumActor — житель на сцене.
type VacuumActor struct {
	UserID  int64
	Nick    string
	CardID  string
	Decider Decider
	// Speaker — чем пишется текст. nil означает разговор БЕЗ слов: реплики
	// встают на свои места и в свои минуты, а тела у них нет. Это рабочий режим,
	// а не заглушка — форму разговора он меряет целиком.
	Speaker Speaker
}

// vacuumHorizon — докуда крутить часы по умолчанию.
//
// Горизонт нужен не ради экономии: задержка берётся из распределения с длинным
// хвостом, и одна вытянутая десятая доля процента способна подвесить очередь на
// месяцы виртуального времени. Неделя выбрана как заведомо больше любого живого
// треда — то есть это СТОП, а не модель того, когда разговор кончается; когда он
// кончается, отвечает сам прогон, и ответ этот и есть измеряемое.
const vacuumHorizon = 7 * 24 * time.Hour

// vacuumBurst — окно, за которое разговор не имеет права уложиться целиком.
// Десять минут: столько же названо в брифе, и число это не про «быстро», а про
// признак машины — живые приходят россыпью в течение часов.
const vacuumBurst = 10 * time.Minute

// VacuumOpts — параметры прогона.
type VacuumOpts struct {
	Actors []VacuumActor
	// MaxReplies — потолок реплик треда. Не украшение: каскад «реплика → точка
	// решения всем» способен разогнаться, и потолок здесь тот же предохранитель,
	// что горизонт по времени. Ноль — умолчание.
	MaxReplies int
	Horizon    time.Duration
	// MaxSpeak — потолок обращений к модели за прогон; ноль — без потолка.
	// Пропущенное считается и называется в отчёте: реплика без текста остаётся
	// на своём месте в разговоре, и молча выдавать её за написанную нельзя.
	MaxSpeak int

	// Familiar — знакомство: «сколько раз я уже отвечал ему». Карта живёт ДОЛЬШЕ
	// одного треда и передаётся зовущим — в бою её помнит граф мира. Может быть
	// nil: тогда все друг другу незнакомы, и это законное начало мира, а не
	// отсутствие данных.
	Familiar map[int64]map[int64]int
}

const vacuumMaxReplies = 300

// VacuumRun — итог прогона в вакууме.
type VacuumRun struct {
	Seed   uint64  `json:"seed"`
	NoteID int64   `json:"note_id"`
	Cast   []int64 `json:"cast"`
	// Stopped — почему разговор кончился. Это НЕ служебная строка: «очередь
	// пуста» значит, что тред затих сам, а «потолок реплик» — что мы его
	// оборвали, и метрики после обрыва читаются иначе.
	Stopped string `json:"stopped"`

	Got  VacuumShape `json:"got"`  // что вышло
	Want VacuumShape `json:"want"` // оригинал, суженный до того же состава

	// OrigReplies — сколько реплик было в оригинале ВСЕГО. Стоит рядом с Want.Replies
	// затем, что без него суженный оригинал читается как весь: доля состава в
	// настоящем разговоре — первое, что нужно знать, чтобы верить остальным числам.
	OrigReplies int `json:"orig_replies"`

	Thread   *archive.ThreadScript `json:"-"` // сгенерированный разговор целиком
	Speeches int                   `json:"speeches"`
	Rejected int                   `json:"rejected"`
	Skipped  int                   `json:"skipped"`
}

// VacuumShape — форма разговора, снятая по одному составу.
//
// Одной функцией на оба разговора, а не двумя похожими: сравнивают их между
// собой, и две реализации одной мерки разошлись бы молча — тот же довод, по
// которому площадка и зеркало ищут адресата парным тестом.
type VacuumShape struct {
	Replies int           `json:"replies"`
	Spoke   []int64       `json:"spoke"` // кто из состава заговорил
	ByActor map[int64]int `json:"by_actor"`
	Pairs   []string      `json:"pairs"` // «кто→кому» внутри состава
	First   archive.Dist  `json:"first_sec"`
	Gap     archive.Dist  `json:"gap_sec"`
	SpanSec int           `json:"span_sec"` // от заметки до последней реплики
	InBurst int           `json:"in_burst"` // реплик в первые vacuumBurst
	Depth   int           `json:"depth"`    // самая длинная цепочка ответов
}

// vacEvent — намеченная реплика.
type vacEvent struct {
	actor   int64
	replyTo int64
}

// RunVacuum разыгрывает тред с нуля: из оригинала берётся только заметка.
func RunVacuum(ctx context.Context, sc *archive.ThreadScript, o VacuumOpts) (*VacuumRun, error) {
	if len(o.Actors) == 0 {
		return nil, fmt.Errorf("вакуум: некому играть")
	}
	byActor := make(map[int64]VacuumActor, len(o.Actors))
	nick := map[int64]string{sc.Note.AuthorID: sc.Note.AuthorNick}
	cast := make([]int64, 0, len(o.Actors))
	for _, a := range o.Actors {
		if a.UserID == 0 {
			return nil, fmt.Errorf("вакуум: житель без анкеты")
		}
		if a.Decider == nil {
			return nil, fmt.Errorf("вакуум: жителю %d нечем решать «прийти или смолчать»", a.UserID)
		}
		byActor[a.UserID] = a
		nick[a.UserID] = a.Nick
		cast = append(cast, a.UserID)
	}
	sort.Slice(cast, func(i, j int) bool { return cast[i] < cast[j] })

	horizon, maxReplies := o.Horizon, o.MaxReplies
	if horizon <= 0 {
		horizon = vacuumHorizon
	}
	if maxReplies <= 0 {
		maxReplies = vacuumMaxReplies
	}
	familiar := o.Familiar
	if familiar == nil {
		familiar = map[int64]map[int64]int{}
	}

	t0 := sc.Note.PublishedAt
	run := &VacuumRun{NoteID: sc.NoteID, Cast: cast, OrigReplies: len(sc.Comments)}
	got := &archive.ThreadScript{NoteID: sc.NoteID, Note: sc.Note, Edges: map[string]int{}}
	run.Thread = got

	st := &vacState{
		run: run, got: got, o: o, byActor: byActor, nick: nick,
		familiar: familiar, said: map[int64]int{}, pending: map[int64]int{},
		byID: map[int64]archive.ScriptComment{},
	}

	// Первый круг — сама заметка. Автора её пропускаем: «прийти в новую заметку»
	// это про чужую, а под своей человек появляется, отвечая пришедшим.
	for _, id := range cast {
		if id == sc.Note.AuthorID {
			continue
		}
		if err := st.roll(ctx, byActor[id], DecisionPoint{
			Now: t0, Actor: id, NoteID: sc.NoteID, TriggerID: 0, Trigger: sc.Note.Text,
			Author: sc.Note.AuthorID, Nick: sc.Note.AuthorNick,
		}, t0); err != nil {
			return nil, err
		}
	}

	deadline := t0.Add(horizon)
	for {
		at, ok := st.q.Peek()
		if !ok {
			run.Stopped = "очередь пуста — тред затих сам"
			break
		}
		if at.After(deadline) {
			run.Stopped = fmt.Sprintf("горизонт %s", horizon)
			break
		}
		if len(got.Comments) >= maxReplies {
			run.Stopped = fmt.Sprintf("потолок реплик (%d)", maxReplies)
			break
		}
		_, ev, _ := st.q.Pop()
		if err := st.say(ctx, at, ev, t0); err != nil {
			return nil, err
		}
	}

	inCast := map[int64]bool{}
	for _, id := range cast {
		inCast[id] = true
	}
	run.Got = shapeOf(got.Comments, inCast, t0)
	run.Want = shapeOf(sc.Comments, inCast, t0)
	return run, nil
}

// vacState — всё, что меняется по ходу разговора.
type vacState struct {
	run      *VacuumRun
	got      *archive.ThreadScript
	o        VacuumOpts
	byActor  map[int64]VacuumActor
	nick     map[int64]string
	familiar map[int64]map[int64]int
	said     map[int64]int
	// pending — намеченное, но ещё не сказанное. Считается вместе со сказанным,
	// иначе потолок реплик на тред не ограничивает НИЧЕГО: житель успевает
	// наметить десяток ответов раньше, чем произнесёт первый, и все десять
	// проходят проверку «сколько я уже сказал».
	pending map[int64]int
	byID    map[int64]archive.ScriptComment
	q       Queue[vacEvent]
	nextID  int64
}

// roll — одна точка решения: бросок кубика и, если выпало говорить, намерение в
// очередь.
func (st *vacState) roll(ctx context.Context, a VacuumActor, p DecisionPoint, at time.Time) error {
	d, err := a.Decider.Decide(ctx, p)
	if err != nil {
		return fmt.Errorf("решение %d на %d: %w", p.Actor, p.TriggerID, err)
	}
	if !d.Speak {
		return nil
	}
	st.pending[p.Actor]++
	st.q.Push(at.Add(d.After), vacEvent{actor: p.Actor, replyTo: p.TriggerID})
	return nil
}

// say — намеченная реплика прозвучала: встала в тред и стала точкой решения для
// всех остальных.
func (st *vacState) say(ctx context.Context, at time.Time, ev vacEvent, t0 time.Time) error {
	st.nextID++
	c := archive.ScriptComment{
		ID: st.nextID, AuthorID: ev.actor, AuthorNick: st.nick[ev.actor],
		PublishedAt: at, ReplyTo: ev.replyTo,
		// Ребро здесь не разрешается, а ЗАДАЁТСЯ: житель отвечал именно на эту
		// реплику, и знаем мы это точно. Источник назван настоящим деревом,
		// потому что по достоверности он ровно таков.
		Edge: archive.EdgeTree,
	}
	if ev.replyTo == 0 {
		c.TargetID = st.got.Note.AuthorID
		c.Delay = at.Sub(t0)
	} else {
		t := st.byID[ev.replyTo]
		c.TargetID = t.AuthorID
		c.Delay = at.Sub(t.PublishedAt)
		c.Address = st.nick[t.AuthorID]
	}
	if err := st.speak(ctx, &c, at); err != nil {
		return err
	}

	st.got.Comments = append(st.got.Comments, c)
	st.got.Edges[c.Edge]++
	st.byID[c.ID] = c
	st.said[ev.actor]++
	st.pending[ev.actor]--
	if c.TargetID != 0 {
		if st.familiar[ev.actor] == nil {
			st.familiar[ev.actor] = map[int64]int{}
		}
		st.familiar[ev.actor][c.TargetID]++
	}

	// Каскад: сказанное слышат все, кроме сказавшего.
	for _, id := range st.run.Cast {
		if id == ev.actor {
			continue
		}
		if err := st.roll(ctx, st.byActor[id], DecisionPoint{
			Now: at, Actor: id, NoteID: st.got.NoteID, TriggerID: c.ID, Trigger: c.Text,
			Author: c.AuthorID, Nick: c.AuthorNick,
			Addressed:   c.TargetID == id,
			Seen:        len(st.got.Comments),
			Said:        st.said[id] + st.pending[id],
			Familiarity: st.familiar[id][c.AuthorID],
		}, at); err != nil {
			return err
		}
	}
	return nil
}

// speak — тело реплики. Молчащий Speaker оставляет её без слов, и это законно:
// форма разговора меряется и так, а деньги тратятся по отдельному нажатию.
func (st *vacState) speak(ctx context.Context, c *archive.ScriptComment, at time.Time) error {
	sp := st.byActor[c.AuthorID].Speaker
	if sp == nil {
		return nil
	}
	if st.o.MaxSpeak > 0 && st.run.Speeches+st.run.Rejected >= st.o.MaxSpeak {
		st.run.Skipped++
		return nil
	}
	s, err := sp.Speak(ctx, SpeechPoint{
		Now: at, Actor: c.AuthorID, Script: st.got, Upto: len(st.got.Comments),
		Slot: c.ID, ReplyTo: c.ReplyTo,
	})
	if err != nil {
		return fmt.Errorf("реплика %d жителя %d: %w", c.ID, c.AuthorID, err)
	}
	if s.Rejected != "" || s.Got == "" {
		st.run.Rejected++
		return nil
	}
	c.Text = s.Got
	st.run.Speeches++
	return nil
}

// shapeOf снимает форму разговора по составу cast.
//
// Реплики не из состава пропускаются целиком — и в этом весь смысл сужения:
// оригинал, где рядом говорили ещё тридцать человек, сравнивается с нашим по
// той его части, которую мы вообще могли воспроизвести.
func shapeOf(cs []archive.ScriptComment, cast map[int64]bool, t0 time.Time) VacuumShape {
	sh := VacuumShape{ByActor: map[int64]int{}}
	var (
		first        = map[int64]time.Time{}
		firsts, gaps []int
		prev         time.Time
		pairs        = map[string]bool{}
		depth        = map[int64]int{}
		last         time.Time
	)
	for _, c := range cs {
		if !cast[c.AuthorID] {
			continue
		}
		sh.Replies++
		sh.ByActor[c.AuthorID]++
		if _, ok := first[c.AuthorID]; !ok {
			first[c.AuthorID] = c.PublishedAt
			firsts = append(firsts, int(c.PublishedAt.Sub(t0).Seconds()))
		}
		if !prev.IsZero() {
			gaps = append(gaps, int(c.PublishedAt.Sub(prev).Seconds()))
		}
		prev, last = c.PublishedAt, c.PublishedAt
		if c.PublishedAt.Sub(t0) <= vacuumBurst {
			sh.InBurst++
		}
		if cast[c.TargetID] && c.TargetID != c.AuthorID {
			pairs[fmt.Sprintf("%d>%d", c.AuthorID, c.TargetID)] = true
		}
		// Глубина считается по цепочке, ОСТАВШЕЙСЯ в составе: ответ человеку,
		// которого здесь нет, начинает ветку заново. Иначе суженный оригинал
		// показывал бы глубину разговора, половины которого в нём нет.
		d := 1
		if p, ok := depth[c.ReplyTo]; ok {
			d = p + 1
		}
		depth[c.ID] = d
		sh.Depth = max(sh.Depth, d)
	}
	for id := range first {
		sh.Spoke = append(sh.Spoke, id)
	}
	sort.Slice(sh.Spoke, func(i, j int) bool { return sh.Spoke[i] < sh.Spoke[j] })
	for p := range pairs {
		sh.Pairs = append(sh.Pairs, p)
	}
	sort.Strings(sh.Pairs)
	sh.First, sh.Gap = archive.NewDist(firsts), archive.NewDist(gaps)
	if !last.IsZero() {
		sh.SpanSec = int(last.Sub(t0).Seconds())
	}
	return sh
}

// BurstOnly — уложился ли ВЕСЬ разговор в окно шторма. Обязательный признак
// провала из брифа: живые приходят россыпью в течение часов, и тред, целиком
// написанный за десять минут, выдаёт машину раньше любого текста.
func (s VacuumShape) BurstOnly() bool {
	return s.Replies > 1 && s.InBurst == s.Replies
}

// JaccardSpoke — насколько сошёлся СОСТАВ заговоривших.
//
// Жаккар, а не точность с полнотой: у «кто пришёл» нет правильной стороны —
// лишний пришедший и проспавший одинаково портят разговор, и одно число,
// симметричное к обоим, честнее пары, которую придётся читать вместе.
func JaccardSpoke(got, want VacuumShape) float64 { return jaccardInt(got.Spoke, want.Spoke) }

// JaccardPairs — насколько сошёлся граф «кто кому отвечал».
func JaccardPairs(got, want VacuumShape) float64 { return jaccardStr(got.Pairs, want.Pairs) }

func jaccardInt(a, b []int64) float64 {
	as := make([]string, 0, len(a))
	for _, x := range a {
		as = append(as, fmt.Sprint(x))
	}
	bs := make([]string, 0, len(b))
	for _, x := range b {
		bs = append(bs, fmt.Sprint(x))
	}
	return jaccardStr(as, bs)
}

// jaccardStr — доля общего в объединении. Пустое против пустого — единица: два
// разговора, в которых из состава не заговорил никто, сошлись полностью, и ноль
// здесь читался бы как расхождение.
func jaccardStr(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	in := map[string]bool{}
	for _, x := range a {
		in[x] = true
	}
	both, union := 0, len(a)
	for _, x := range b {
		if in[x] {
			both++
			continue
		}
		union++
	}
	if union == 0 {
		return 1
	}
	return float64(both) / float64(union)
}
