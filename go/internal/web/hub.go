package web

// Хаб живого канала: один опрос базы на процесс, разводка по темам в памяти.
//
// Устройство выбрано ради одного числа — ЧЕТЫРЕХ соединений к Postgres у
// веб-морды (platform.WebOpts). Всё, что морда отберёт у базы, отбирается у
// зеркала, которое живёт на том же ядре, поэтому живой канал обязан стоить
// столько же при одном открытом окне и при сотне.
//
// Отсюда два правила, и оба обязательны:
//
//  1. В базу ходит ТОЛЬКО хаб — одна горутина, два запроса в такт, независимо от
//     числа слушателей. Обработчик SSE не касается базы вовсе (см. live.go).
//  2. Опрос, а не LISTEN/NOTIFY. Стоимость опроса не зависит от числа клиентов,
//     он переживает разрыв соединения к базе без переподписки, а «отстать на две
//     секунды» для живой страницы незаметно. Заменить его на LISTEN/NOTIFY можно
//     позже, ничего не переписывая: наружу хаб отдаёт те же сообщения.
//
// Курсоров два, и это не дублирование: факт появляется в events сразу, а повод в
// notifications — когда его раздаст platbus, то есть на такт-другой позже. Один
// курсор на двоих проскакивал бы мимо поводов по уже увиденным фактам.

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"lovegw/internal/platform"
)

// Live — что живому каналу нужно от шины. Способность НЕОБЯЗАТЕЛЬНАЯ:
// подключается она type-assertion'ом от Events (как SiteMessenger от Site), и её
// отсутствие означает ровно «страница не дописывается сама» — всё остальное
// работает по-прежнему.
type Live interface {
	LiveSince(ctx context.Context, afterID int64, limit int) ([]platform.LiveEvent, error)
	PokesSince(ctx context.Context, afterID int64, limit int) ([]platform.Poke, error)
	LastEventID(ctx context.Context) (int64, error)
}

const (
	// hubTick — как часто спрашиваем базу. Две секунды: быстрее человек не
	// заметит, а медленнее страница перестаёт казаться живой.
	hubTick = 2 * time.Second
	// hubBatch — потолок выборки за такт. Упирается в него только шторм, и тогда
	// хвост приедет следующим тактом.
	hubBatch = 200
	// listenerBuffer — сколько сигналов копим слушателю, который не читает.
	// Переполнение — рабочий случай, и сигнал в нём ТЕРЯЕТСЯ намеренно: все
	// сигналы одного рода значат одно и то же («в этом треде новое»), и восьми
	// накопленных человеку хватит, чтобы это понять.
	listenerBuffer = 8
)

// liveMsg — сигнал слушателю. Ни текста, ни имени: страница узнаёт, что
// появилось новое, и идёт читать обычным переходом (см. шапку live.go).
type liveMsg struct {
	ID   int64
	Kind string // comment | note | poke
	Note int64
}

// listener — одно открытое соединение.
type listener struct {
	ch     chan liveMsg
	topics []string
}

// hub — состояние живого канала.
type hub struct {
	src Live
	log *slog.Logger

	mu   sync.Mutex
	subs map[string]map[*listener]struct{}
	n    int

	// Курсоры трогает только горутина run, но лежат они под тем же мьютексом:
	// их читает диагностика, а класть рядом два способа синхронизации дороже,
	// чем взять мьютекс раз в две секунды.
	events int64
	pokes  int64
	inited bool
}

func newHub(src Live, log *slog.Logger) *hub {
	return &hub{src: src, log: log, subs: make(map[string]map[*listener]struct{})}
}

// noteTopic и userTopic — единственные две темы. Строками, потому что ключ карты
// всё равно строка, а «note:312811» в логе читается без расшифровки.
func noteTopic(id int64) string { return "note:" + strconv.FormatInt(id, 10) }
func userTopic(id int64) string { return "user:" + strconv.FormatInt(id, 10) }

// feedTopic — общая лента. Тема одна на всех, потому что и вопрос у ленты один:
// «появилось ли что-то новое». Подписаться на «все треды сразу» так нельзя, и не
// нужно: в ленте видно заметки, а не реплики.
const feedTopic = "feed"

// subscribe заводит слушателя. Ошибка означает «столько соединений мы не держим»
// — не поломку, а честный отказ: см. потолки в live.go.
func (h *hub) subscribe(l *listener, maxTotal, maxPerUser int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n >= maxTotal {
		return false
	}
	// Личная тема есть у каждого слушателя, поэтому её множество и есть счётчик
	// соединений человека — отдельной карты под это не нужно.
	for _, t := range l.topics {
		if strings.HasPrefix(t, "user:") && len(h.subs[t]) >= maxPerUser {
			return false
		}
	}
	for _, t := range l.topics {
		if h.subs[t] == nil {
			h.subs[t] = make(map[*listener]struct{})
		}
		h.subs[t][l] = struct{}{}
	}
	h.n++
	return true
}

func (h *hub) unsubscribe(l *listener) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range l.topics {
		delete(h.subs[t], l)
		if len(h.subs[t]) == 0 {
			delete(h.subs, t)
		}
	}
	h.n--
}

// publish разносит сигнал подписчикам темы. Отправка неблокирующая: медленный
// слушатель не имеет права задержать хаб, а потерянный сигнал ничего не значит
// (см. listenerBuffer).
func (h *hub) publish(topic string, m liveMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for l := range h.subs[topic] {
		select {
		case l.ch <- m:
		default:
		}
	}
}

// run крутит опрос до отмены контекста.
func (h *hub) run(ctx context.Context) {
	t := time.NewTicker(hubTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tick(ctx)
		}
	}
}

func (h *hub) tick(ctx context.Context) {
	if !h.start(ctx) {
		return
	}
	h.pumpEvents(ctx)
	h.pumpPokes(ctx)
}

// start ставит курсоры на верхний край журнала. Прошлое не рассылается: человек,
// открывший страницу, ждёт того, что произойдёт ДАЛЬШЕ, а не пересказа недели.
//
// Отказ базы здесь не фатален — пробуем следующим тактом. Важно только не
// начать с нуля: это разослало бы всю историю разом.
func (h *hub) start(ctx context.Context) bool {
	h.mu.Lock()
	inited := h.inited
	h.mu.Unlock()
	if inited {
		return true
	}
	last, err := h.src.LastEventID(ctx)
	if err != nil {
		h.log.Warn("живой канал: верх журнала", "err", err)
		return false
	}
	h.mu.Lock()
	h.events, h.pokes, h.inited = last, last, true
	h.mu.Unlock()
	return true
}

func (h *hub) pumpEvents(ctx context.Context) {
	h.mu.Lock()
	from := h.events
	h.mu.Unlock()

	list, err := h.src.LiveSince(ctx, from, hubBatch)
	if err != nil {
		h.log.Warn("живой канал: события", "err", err)
		return
	}
	for _, e := range list {
		// Курсор двигается по ВСЕМ фактам, включая те, которые странице
		// показать нечем: пропущенный вид — не повод перечитывать его вечно.
		from = max(from, e.ID)
		kind := liveKind(e.Kind)
		if kind == "" || e.NoteID == 0 {
			continue
		}
		h.publish(noteTopic(e.NoteID), liveMsg{ID: e.ID, Kind: kind, Note: e.NoteID})
		if kind == "note" {
			h.publish(feedTopic, liveMsg{ID: e.ID, Kind: kind, Note: e.NoteID})
		}
	}
	h.mu.Lock()
	h.events = from
	h.mu.Unlock()
}

func (h *hub) pumpPokes(ctx context.Context) {
	h.mu.Lock()
	from := h.pokes
	h.mu.Unlock()

	list, err := h.src.PokesSince(ctx, from, hubBatch)
	if err != nil {
		h.log.Warn("живой канал: поводы", "err", err)
		return
	}
	for _, p := range list {
		h.publish(userTopic(p.UserID), liveMsg{ID: p.EventID, Kind: "poke"})
		from = max(from, p.EventID)
	}
	h.mu.Lock()
	h.pokes = from
	h.mu.Unlock()
}

// liveKind — как назвать факт на проводе. Наружу уходят только те виды, которые
// странице есть чем показать; остальное (скрытие, бан) приезжает поводом в
// личную тему, а не сигналом в тред.
func liveKind(k platform.EventKind) string {
	switch k {
	case platform.EventComment:
		return "comment"
	case platform.EventNote:
		return "note"
	default:
		return ""
	}
}
