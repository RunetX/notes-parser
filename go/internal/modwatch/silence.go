package modwatch

// Разбор молчания: кого закрыли, а кто ушёл сам.
//
// Слепой поиск запретов по ритму (bans.go) не работает — естественных пауз
// столько, что подпись в них тонет. Здесь вопрос решается не статистикой, а
// вторым, независимым каналом: молчание берётся из реплик, присутствие — из
// анкеты. Совпадение «замолчал, но ходит» и есть запрет; «замолчал и не
// заходит» — уход, наказание тут ни при чём (запрет в «Заметки» ходить по
// сайту не мешает — проверено на живом бане 12.08.2026).
//
// Чего метод не умеет. Он не отличает запрет от «человек читает, но не пишет»:
// признак один и тот же. Отделяет их возвращение — наказание кончается ровно
// на сроке (сутки, неделя, месяц), поэтому окончательный ответ даёт не первый
// отчёт, а второй, после возврата.

import (
	"sort"
	"time"
)

// Commenter — человек в круге наблюдения: сколько написал за окно и когда
// замолчал.
type Commenter struct {
	UserID      int64
	Nick        string
	Comments    int
	LastComment time.Time
}

// Пороги разбора по умолчанию.
//
// Silence — с какого молчания вообще смотреть. Трое суток: суточный запрет
// (самый частый) к этому времени уже кончился и человек вернулся, а кто не
// вернулся — либо под более долгим сроком, либо ушёл.
//
// Fresh — насколько свежим должен быть последний заход, чтобы считать, что
// человек ЕЩЁ на площадке. Это главное условие запрета, и его пришлось вводить
// отдельно: на первом же живом прогоне (13.08.2026) в кандидаты попал Игорь —
// 460 реплик, молчит восемь суток, «ходил после» 12 часов. Но последний его
// заход был тогда же, восемь суток назад: человек ушёл, просто на полдня
// позже, чем замолчал.
//
// Margin — насколько заход должен пережить последнюю реплику. Полсуток
// отсекают обычный хвост вечера: дописал последнюю реплику и ещё побродил.
//
// MinMissed — сколько реплик человек не написал против СВОЕГО же темпа. Без
// этого порога список забивают редкие комментаторы: четверо суток молчания у
// пишущего раз в неделю — не событие.
//
// MinSeenDays — на скольких РАЗНЫХ сутках молчания человек замечен на сайте.
// Порог введён после разбора Актрисы u1431505 (13.08.2026): её признали
// закрытой по одной свежей отметке, а она в тот же вечер вернулась — отметка
// была не «ходит молча тринадцать суток», а «первый день на площадке после
// отсутствия». Владелец анкеты потом объяснил: уезжала без связи. Одна точка
// эти два случая не различает в принципе, различает только серия.
const (
	DefaultSilence     = 72 * time.Hour
	DefaultFresh       = 24 * time.Hour
	DefaultMargin      = 12 * time.Hour
	DefaultMinMissed   = 20
	DefaultMinSeenDays = 2
)

// Вердикты разбора.
const (
	VerdictBan       = "запрет?"     // молчит, но ходит — и ходил не только сегодня
	VerdictThin      = "мало данных" // на площадке, но отметок внутри молчания мало: мог только вернуться
	VerdictLeftLater = "ушёл позже"  // ещё ходил, замолчав, но потом пропал совсем
	VerdictLeft      = "ушёл"        // перестал и писать, и заходить разом
	VerdictUnknown   = "не опрошен"  // анкету ещё не смотрели
	VerdictMissing   = "анкеты нет"  // 404: удалена или закрыта администрацией
)

// SilenceOptions — параметры разбора.
type SilenceOptions struct {
	Now         time.Time
	Silence     time.Duration
	Fresh       time.Duration
	Margin      time.Duration
	Window      time.Duration // окно, за которое посчитаны реплики круга
	MinMissed   float64       // 0 — не отсеивать (у остальных полей 0 означает «по умолчанию»)
	MinSeenDays int
}

func (o *SilenceOptions) defaults() {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.Silence <= 0 {
		o.Silence = DefaultSilence
	}
	if o.Fresh <= 0 {
		o.Fresh = DefaultFresh
	}
	if o.Margin <= 0 {
		o.Margin = DefaultMargin
	}
	if o.Window <= 0 {
		o.Window = DefaultActivityWindow
	}
	if o.MinSeenDays <= 0 {
		o.MinSeenDays = DefaultMinSeenDays
	}
}

// SilenceRow — строка разбора.
type SilenceRow struct {
	Commenter
	LastActivity time.Time     // последняя известная отметка присутствия
	Silence      time.Duration // сколько молчит
	Away         time.Duration // сколько не заходит
	After        time.Duration // сколько ещё ходил после последней реплики
	Stale        time.Duration // как давно опрашивали анкету
	Missed       float64       // сколько реплик не написал против своего темпа
	SeenDays     int           // на скольких разных сутках молчания замечен на сайте
	HideMe       bool
	VIP          bool
	Verdict      string
}

// ClassifySilence разбирает замолчавших по данным обхода анкет. На вход — круг
// с последней репликой каждого, снимки анкет и след присутствия (`trail`:
// отметки по анкетам, обычно из Store.ActivityIn); на выход — только молчащие
// дольше порога, кандидаты на запрет впереди.
//
// След обязателен по существу, а не для удобства: без него «ходит молча» и
// «вернулся сегодня» — одна и та же картинка (см. MinSeenDays). Пустой след
// не ошибка, но вердикты тогда честно вырождаются в «мало данных».
func ClassifySilence(people []Commenter, profiles map[int64]ProfileRow,
	trail map[int64][]time.Time, opt SilenceOptions) []SilenceRow {
	opt.defaults()
	var out []SilenceRow
	for _, c := range people {
		row, ok := silenceRow(c, profiles[c.UserID], trail[c.UserID], opt)
		if !ok {
			continue
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := verdictRank(out[i].Verdict), verdictRank(out[j].Verdict); a != b {
			return a < b
		}
		return out[i].Missed > out[j].Missed // громче молчит тот, кто больше недописал
	})
	return out
}

// silenceRow строит строку разбора по одному человеку. ok=false — он не молчит
// достаточно долго либо молчание не идёт вразрез с его собственным темпом.
func silenceRow(c Commenter, p ProfileRow, stamps []time.Time, opt SilenceOptions) (SilenceRow, bool) {
	if c.LastComment.IsZero() {
		return SilenceRow{}, false
	}
	silence := opt.Now.Sub(c.LastComment)
	if silence < opt.Silence {
		return SilenceRow{}, false
	}
	row := SilenceRow{Commenter: c, Silence: silence, Verdict: VerdictUnknown}
	if days := opt.Window.Hours() / 24; days > 0 {
		row.Missed = float64(c.Comments) / days * (silence.Hours() / 24)
	}
	if row.Missed < opt.MinMissed {
		return SilenceRow{}, false
	}
	switch {
	case p.CheckedAt.IsZero():
		// анкету ещё не смотрели — вердикта нет
	case p.Missing:
		row.Verdict = VerdictMissing
	default:
		row.LastActivity, row.HideMe, row.VIP = p.LastAt, p.HideMe, p.VIP
		row.Stale = opt.Now.Sub(p.CheckedAt)
		row.After = p.LastAt.Sub(c.LastComment)
		row.Away = opt.Now.Sub(p.LastAt)
		row.SeenDays = seenDays(stamps, c.LastComment)
		row.Verdict = verdictOf(row, opt)
	}
	if row.Nick == "" {
		row.Nick = p.Nick
	}
	return row, true
}

// seenDays — на скольких разных сутках молчания человек замечен на сайте.
// Сутки считаются от последней реплики, а не по календарю: так не нужен пояс
// площадки, и мера прямо отвечает на вопрос «насколько плотно след покрывает
// молчание». Отметки раньше последней реплики не в счёт — это ещё не молчание.
func seenDays(stamps []time.Time, lastComment time.Time) int {
	if lastComment.IsZero() {
		return 0
	}
	buckets := map[int64]bool{}
	for _, at := range stamps {
		d := at.Sub(lastComment)
		if d <= 0 {
			continue
		}
		buckets[int64(d/(24*time.Hour))] = true
	}
	return len(buckets)
}

// verdictOf — правило: запрет это «молчит, но ходит СЕЙЧАС, и ходил не только
// сегодня». Перестал ходить — значит ушёл, и неважно, сколько ещё ходил после
// последней реплики; разница лишь в том, разом он это сделал или в два приёма.
//
// Оговорка про «мало данных» и есть главный урок Актрисы: свежая отметка у
// вернувшегося и у закрытого выглядит одинаково, поэтому пока след не покрыл
// хотя бы MinSeenDays разных суток молчания, называть это запретом нельзя.
func verdictOf(r SilenceRow, opt SilenceOptions) string {
	switch {
	case r.LastActivity.IsZero():
		return VerdictUnknown // анкету видели, но времени сайт не дал
	case r.Away > opt.Fresh && r.After >= opt.Margin:
		return VerdictLeftLater
	case r.Away > opt.Fresh:
		return VerdictLeft
	case r.SeenDays >= opt.MinSeenDays:
		return VerdictBan
	default:
		return VerdictThin
	}
}

func verdictRank(v string) int {
	switch v {
	case VerdictBan:
		return 0
	case VerdictThin:
		return 1
	case VerdictLeftLater:
		return 2
	case VerdictUnknown:
		return 3
	case VerdictMissing:
		return 4
	default:
		return 5
	}
}
