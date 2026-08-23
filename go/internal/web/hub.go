package web

// Хаб живого канала: один читатель журнала на процесс, разводка по темам в
// памяти.
//
// Устройство выбрано ради одного числа — ЧЕТЫРЕХ соединений к Postgres у
// веб-морды (platform.WebOpts). Всё, что морда отберёт у базы, отбирается у
// зеркала, которое живёт на том же ядре, поэтому живой канал обязан стоить
// столько же при одном открытом окне и при сотне.
//
// Отсюда два правила, и оба обязательны:
//
//  1. В базу ходит ТОЛЬКО хаб — одна горутина, два запроса за проход, независимо
//     от числа слушателей. Обработчик SSE не касается базы вовсе (см. live.go).
//  2. Звонок вместо ожидания такта, но такт остаётся. О новом хаб узнаёт по
//     LISTEN/NOTIFY (platform/live.go) и идёт читать журнал НЕМЕДЛЕННО — теми же
//     курсорами и тем же кодом, что и по такту. Звонок несёт пустой payload, и
//     это не экономия: доверять данным из уведомления значило бы завести второй
//     источник правды рядом с журналом и потерять то, ради чего курсоры и
//     существуют.
//
//     Такт при этом НЕ УБРАН, а разрежен. Он ловит пропущенный звонок (разрыв
//     подписки, рестарт базы) и остаётся единственным путём, когда подписки нет
//     вовсе. Отсюда две скорости: при живой подписке такт — страховка раз в
//     двадцать секунд, без неё он снова частый, как был. Переключение
//     автоматическое и с одной строкой в логе на каждую сторону: канал, тихо
//     деградировавший до минутных задержек, хуже канала, который об этом сказал.
//
// Почему звонок идёт ЧЕРЕЗ БАЗУ, а не прямым вызовом. Процессов два: морда
// (`lovegw web`, свой контейнер) и демон (`lovegw run`) — а факты рождаются в
// обоих, нативная реплика здесь, зеркальная и повод там. Позвать хаб из памяти
// можно было бы только в первой половине случаев, и вторая осталась бы на
// такте: два пути с разной задержкой вместо одного. Подробнее — в шапке
// platform/live.go.
//
// Курсоров два, и это не дублирование: факт появляется в events сразу, а повод в
// notifications — когда его раздаст platbus, то есть на такт-другой позже. Один
// курсор на двоих проскакивал бы мимо поводов по уже увиденным фактам.

import (
	"context"
	"log/slog"
	"slices"
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

// LiveWatcher — способность ядра ПОЗВАТЬ вместо того, чтобы ждать вопроса.
//
// Необязательная и подключается type-assertion'ом от Live, как сам Live — от
// Events: её отсутствие означает ровно «страница обновляется по такту», то есть
// поведение до этой правки, а не поломку. Так же выглядит и отказ подписки в
// бою, и это не совпадение — деградация обязана быть одним и тем же состоянием,
// а не отдельной веткой, которую никто никогда не проверял.
//
// Договор: одна попытка на вызов; своё соединение, вне пула; ready — подписка
// встала (можно и НУЖНО немедленно перечитать журнал: за время без подписки
// могли пропустить что угодно); ring — звонок; возврат nil означает «нас
// остановили», ошибка — «подписка кончилась».
type LiveWatcher interface {
	ListenLive(ctx context.Context, ready, ring func()) error
}

const (
	// hubTickPoll — как часто спрашиваем базу, когда звонить некому. Две
	// секунды: быстрее человек не заметит, а медленнее страница перестаёт
	// казаться живой. Это прежний, единственный такт канала — и он же его
	// нижняя планка, ниже которой деградация опускаться не вправе.
	hubTickPoll = 2 * time.Second
	// hubTickNotify — такт при живой подписке. Здесь он уже не способ узнать
	// новое, а страховка: ловит звонок, потерянный вместе с соединением, и
	// закрывает щель между разрывом подписки и тем, как мы это заметим.
	// Двадцать секунд — дешевле опроса вдесятеро и всё ещё быстрее, чем человек
	// успеет пожаловаться.
	hubTickNotify = 20 * time.Second
	// hubRingFloor — как часто звонок вправе поднимать хаб. Ноль здесь был бы
	// приглашением: перенос архива или догоняющая сверка пишут события пачками,
	// и каждая транзакция — свой звонок. Десятая доля секунды держит расход в
	// двадцати запросах к базе в секунду при любом шторме — и она же
	// единственное, что стоит между событием и вкладкой.
	//
	// Замер сквозного пути «реплика записана → сигнал у слушателя» (локальный
	// Postgres, 40 событий, 23.08.2026): на разреженном потоке, каким площадка
	// и живёт, медиана 10 мс, p95 15 мс — пол не задевается вовсе; на сплошном,
	// когда каждая следующая запись приходит сразу после прохода, медиана 95 мс
	// и p95 96 мс, то есть ровно этот пол. Обещано было 300 мс: втрое хуже
	// худшего случая.
	hubRingFloor = 100 * time.Millisecond
	// hubBatch — потолок выборки за такт. Упирается в него только шторм, и тогда
	// хвост приедет следующим тактом.
	hubBatch = 200
	// listenRetryMin / listenRetryMax — бэкофф переподписки. Начинаем через
	// секунду (разрыв бывает и мгновенным — рестарт базы при выкатке), дальше
	// удвоением до полуминуты: канал в это время работает, просто по такту.
	listenRetryMin = time.Second
	listenRetryMax = 30 * time.Second
	// latencyReport — как часто задержка попадает в лог. Раз в минуту сводкой, а
	// не строкой на сигнал: тем же приёмом и по той же причине, что отказы по
	// потолкам (см. reportEvery в guard.go).
	latencyReport = time.Minute
	// latencySamples — сколько замеров держим на сводку. Двухсот хватает на
	// честный p95 при нынешнем потоке, а перебор старших отбрасывает самые
	// свежие: сводка о том, что происходит СЕЙЧАС, важнее полной истории минуты.
	latencySamples = 200
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
	// at — когда факт записан. На провод НЕ уходит и уйти не может: liveData
	// собирает тело из трёх полей руками. Нужно оно ровно для замера, который
	// закрывает эту работу, — «событие записано → сигнал ушёл в сокет».
	at time.Time
}

// listener — одно открытое соединение.
type listener struct {
	ch     chan liveMsg
	topics []string
}

// liveMode — чем канал живёт прямо сейчас. Наружу это отдаёт healthz, и не из
// любопытства: «notify» и «poll» отличаются задержкой на порядок, и вопрос
// «почему страница стала неживой» без этого слова разбирается догадками.
type liveMode string

const (
	modeNotify liveMode = "notify"
	modePoll   liveMode = "poll"
)

// hub — состояние живого канала.
type hub struct {
	src Live
	log *slog.Logger
	// ring — звонок из подписки в горутину run. Ёмкость единица и отправка
	// неблокирующая: звонки не считают, они значат одно и то же — «сходи
	// посмотри», — и второй, пришедший до того, как первый разобрали, лишний.
	ring chan struct{}

	mu   sync.Mutex
	subs map[string]map[*listener]struct{}
	n    int

	// Курсоры трогает только горутина run, но лежат они под тем же мьютексом:
	// их читает диагностика, а класть рядом два способа синхронизации дороже,
	// чем взять мьютекс раз в две секунды.
	events int64
	pokes  int64
	inited bool

	// mode — режим канала, его читает healthz. Пока подписка не встала, это
	// честный «poll»: так канал и работает.
	mode liveMode
	// lat — замеры задержки до сводки (см. latencyReport).
	lat      []time.Duration
	latOver  int // сколько замеров не поместилось
	reported time.Time
}

func newHub(src Live, log *slog.Logger) *hub {
	return &hub{
		src:  src,
		log:  log,
		ring: make(chan struct{}, 1),
		mode: modePoll,
		subs: make(map[string]map[*listener]struct{}),
	}
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

// run крутит канал до отмены контекста: подписка звонит, такт страхует.
//
// Читает журнал ТОЛЬКО эта горутина — и по звонку, и по такту. Это не мелочь
// исполнения: курсоры не под мьютексом ради безопасности, а односоставные по
// устройству, и второй читатель немедленно завёл бы гонку «оба прочитали одно и
// то же, оба разослали».
func (h *hub) run(ctx context.Context) {
	go h.listen(ctx)

	// Таймер, а не тикер: период зависит от режима, а режим меняет соседняя
	// горутина, — и перезаводить срок на каждом круге честнее, чем ловить
	// момент, когда Reset безопасен.
	t := time.NewTimer(h.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.ring:
		}
		h.tick(ctx)
		h.report(time.Now())
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		// Пол частоты звонков — см. hubRingFloor. Стоит он ПОСЛЕ прохода, а не
		// перед ним: первый звонок обязан подниматься без задержки, платит
		// только шторм.
		select {
		case <-ctx.Done():
			return
		case <-time.After(hubRingFloor):
		}
		t.Reset(h.every())
	}
}

// listen держит подписку и переподписывается, пока не остановят.
//
// Здесь же и вся политика деградации. Правило про лог одно: ОДНА строка на
// падение и одна на возвращение, а не строка на попытку, — иначе получасовой
// отказ базы утопит в предупреждениях то самое, ради чего их читают.
func (h *hub) listen(ctx context.Context) {
	w, ok := h.src.(LiveWatcher)
	if !ok {
		// Не отказ: канал работает опросом ровно как работал. Info, а не Warn,
		// потому что чинить нечего.
		h.log.Info("живой канал: работаем опросом", "почему", "ядро не умеет звонить")
		return
	}
	wait := listenRetryMin
	for ctx.Err() == nil {
		err := w.ListenLive(ctx, func() {
			if h.setMode(modeNotify) {
				h.log.Info("живой канал: подписка встала, такт стал страховкой",
					"такт", hubTickNotify)
			}
			// Немедленный проход: пока подписки не было, звонки уходили в
			// никуда, и пропущенное лежит в журнале за курсором.
			h.wake()
		}, h.wake)
		if ctx.Err() != nil {
			return
		}
		// Смена режима отвечает сразу на два вопроса, и это не экономия строки:
		// «была ли подписка живой» — это и повод сказать о падении вслух, и
		// повод начать бэкофф заново, потому что прошлая неудача к нынешней
		// отношения не имеет. Сбрасывать счётчик из ready нельзя: замыкание
		// зовёт чужой код, и разрешать ему трогать переменную этой горутины
		// значит держать гонку, которую не покажет ни один тест.
		if h.setMode(modePoll) {
			wait = listenRetryMin
			h.log.Warn("живой канал: LISTEN отвалился, работаем опросом",
				"err", err, "такт", hubTickPoll)
		} else {
			// Не первая неудача подряд — режим уже poll, и говорить об этом
			// снова значит только шуметь.
			h.log.Debug("живой канал: переподписка не удалась", "err", err, "повтор", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, listenRetryMax)
	}
}

// wake — звонок горутине run. Неблокирующий: см. поле ring.
func (h *hub) wake() {
	select {
	case h.ring <- struct{}{}:
	default:
	}
}

// every — период такта в нынешнем режиме.
func (h *hub) every() time.Duration {
	if h.Mode() == modeNotify {
		return hubTickNotify
	}
	return hubTickPoll
}

// Mode — чем канал живёт сейчас. Читает healthz и тесты.
func (h *hub) Mode() liveMode {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mode
}

// setMode переводит канал в режим и отвечает, была ли это СМЕНА. Ответ и есть
// разрешение писать в лог: строку заслуживает переход, а не состояние.
func (h *hub) setMode(m liveMode) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mode == m {
		return false
	}
	h.mode = m
	return true
}

// observe принимает замер «факт записан → сигнал ушёл в сокет». Зовёт его
// обработчик потока (live.go) — только он знает, что запись удалась.
//
// Замеры за пределом сотни просто считаются: сводке нужен порядок величины и
// p95, а не полный список, и разрастаться под штормом ей незачем.
func (h *hub) observe(d time.Duration) {
	if d < 0 {
		// Часы морды и часы Postgres — разные; отрицательная задержка означает
		// расхождение, а не мгновенный ответ, и в сводке ей делать нечего.
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lat) < latencySamples {
		h.lat = append(h.lat, d)
		return
	}
	h.latOver++
}

// report кладёт сводку задержки в лог раз в минуту.
//
// Это и есть приёмка работы: обещано «событие доезжает до вкладки не позже чем
// через 300 мс», и проверяется обещание не на стенде, а тем, что бой сам про
// себя рассказывает.
func (h *hub) report(now time.Time) {
	h.mu.Lock()
	if h.reported.IsZero() {
		h.reported = now
		h.mu.Unlock()
		return
	}
	if now.Sub(h.reported) < latencyReport || len(h.lat) == 0 {
		h.mu.Unlock()
		return
	}
	lat, over, since := h.lat, h.latOver, h.reported
	h.lat, h.latOver, h.reported = nil, 0, now
	mode := h.mode
	h.mu.Unlock()

	slices.Sort(lat)
	h.log.Info("живой канал: задержка сигнала",
		"режим", string(mode),
		"сигналов", len(lat)+over,
		"медиана", pct(lat, 50).Round(time.Millisecond),
		"p95", pct(lat, 95).Round(time.Millisecond),
		"худший", lat[len(lat)-1].Round(time.Millisecond),
		"за", now.Sub(since).Round(time.Second))
}

// pct — процентиль отсортированного среза. Ближайший ранг, без интерполяции:
// на двух сотнях замеров разница между способами меньше, чем разброс самих
// замеров, а объяснять её пришлось бы каждому, кто прочтёт сводку.
func pct(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p + 99) / 100
	return sorted[min(i, len(sorted))-1]
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
		h.publish(noteTopic(e.NoteID), liveMsg{ID: e.ID, Kind: kind, Note: e.NoteID, at: e.At})
		if kind == "note" {
			h.publish(feedTopic, liveMsg{ID: e.ID, Kind: kind, Note: e.NoteID, at: e.At})
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
		h.publish(userTopic(p.UserID), liveMsg{ID: p.EventID, Kind: "poke", at: p.At})
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
