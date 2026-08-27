package web

// Потолки наплыва: за ними морда отказывает быстро и дёшево.
//
// Стоят они здесь не ради самой морды. Площадка живёт на ОДНОМ ядре вместе с
// зеркалом: тот же хост крутит демона, который читает НГС и носит заметки в
// каналы, тот же Postgres обслуживает приём и сверку. Веб-морда — единственное,
// до чего дотягивается посторонний, поэтому её потолки это в первую очередь
// забота о зеркале: пусть страница скажет «слишком часто», чем замолчит канал.
//
// Потолка три, и один другого не заменяет:
//
//   - ЧАСТОТА (корзина по клиенту) — сколько запросов с одного адреса, с ЦЕНОЙ
//     по маршруту: дерево треда дороже ленты, а вход дороже всего — он ходит на
//     НГС и шлёт человеку личное сообщение. Стоит первой: это единственная
//     проверка, которая отсекает поток ДО того, как он что-нибудь займёт.
//   - ОДНОВРЕМЕННОСТЬ (семафор) — сколько запросов в работе разом. Бережёт
//     память: страница треда собирается в буфер целиком, и полтысячи таких
//     буферов это OOM на 1,6 ГиБ, где рядом Postgres и демон.
//   - СРОК (бюджет запроса) — сколько запрос вправе длиться. Без него медленный
//     запрос держит и слот семафора, и соединение к базе.
//
// Чего здесь нет намеренно: ни блокировок по адресу, ни капчи, ни счётчиков на
// диске. Сырых адресов площадка не хранит нигде — это персональные данные, и
// лог прокси от них специально очищен, — поэтому корзины живут в памяти, а
// ключом им служит хеш адреса на случайной соли ЭТОГО запуска: пережить
// рестарт такому счётчику незачем, а восстановить по нему адрес нечем.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxInFlight — сколько запросов держим в работе разом. Двенадцать при пуле
	// в четыре соединения: восемь сверх пула — это запас на отдачу статики и на
	// те доли секунды, что страница рисуется уже без базы.
	maxInFlight = 12

	// requestBudget — сколько живёт обычный запрос. Лента отдаётся за 86 мс,
	// тред за 96 — восемь секунд означают «что-то не так», а не «много данных».
	requestBudget = 8 * time.Second
	// loginBudget — вход ходит на НГС: читает анкету (потолок 15 с) и шлёт код
	// личным сообщением. Мерить его общей меркой значило бы обрывать людей на
	// медленном ответе чужого сайта. Той же меркой живёт обновление аватара —
	// оно ходит туда же (см. goesToNGS).
	loginBudget = 25 * time.Second

	// maxFormBytes — потолок тела формы. Формы у нас текстовые, поэтому
	// 64 КиБ — это с запасом на самую длинную заметку. Без потолка net/http
	// принял бы по 10 МиБ на каждый POST.
	maxFormBytes = 64 << 10

	// uploadMaxBytes — потолок тела маршрутов, принимающих файл: десять мегабайт
	// картинки плюс запас на текст заметки, границы multipart и заголовки частей.
	uploadMaxBytes = 11 << 20

	// uploadBudget — десять мегабайт с телефона по мобильной сети ползут минуту,
	// и восемь секунд общего бюджета отрезали бы честную закачку на середине.
	uploadBudget = 90 * time.Second

	// bucketBurst / bucketRefill — размер корзины и скорость наполнения. 120
	// токенов при цене треда в 4 — это три десятка страниц подряд, больше, чем
	// человек открывает залпом; 2 токена в секунду — 30 тредов в минуту
	// вдолгую. Ходящему по ссылкам потолок незаметен, а потоку одинаковых
	// запросов достаётся полпроцента ядра.
	bucketBurst  = 120.0
	bucketRefill = 2.0

	// Цены маршрутов. Считаем не запросы, а работу: страница треда отдаёт
	// дерево целиком (до 5000 реплик), а POST входа тянет за собой чужой сайт.
	costPage   = 1.0
	costThread = 4.0
	costWrite  = 4.0
	costLogin  = 20.0
	// costUpload — как у входа, и по той же логике: там мы ждём чужой сайт, тут
	// держим процессор, которого у морды шесть десятых ядра. Двадцать токенов
	// при корзине в 120 — это шесть картинок залпом и дальше по одной в десять
	// секунд, то есть вдвое щедрее правила ядра (одна заметка в пять минут).
	costUpload = 20.0

	// bucketIdle — через сколько корзина забывается. Больше времени полного
	// восстановления (минута), так что забываем только тех, кто ушёл.
	bucketIdle = 10 * time.Minute
	// maxBuckets — потолок числа корзин: backstop на случай наплыва с множества
	// адресов. Двадцать тысяч корзин — меньше двух мегабайт.
	maxBuckets = 20000
	sweepEvery = time.Minute
	// reportEvery — как часто отказы попадают в лог. Строкой на запрос их писать
	// нельзя (наплыв — это ровно тот случай, когда лог сам становится нагрузкой),
	// а молчать нельзя тем более: сработавший потолок это единственный признак,
	// по которому владелец узнает о наплыве раньше, чем по замолчавшему каналу.
	reportEvery = time.Minute
)

// bucket — корзина токенов одного клиента.
type bucket struct {
	tokens float64
	at     time.Time
}

// guard — общее состояние потолков, один на сервер.
type guard struct {
	sem chan struct{}
	now func() time.Time // подменяется тестом
	log *slog.Logger

	mu       sync.Mutex
	buckets  map[[8]byte]*bucket
	swept    time.Time
	reported time.Time
	limited  int // отказов по частоте с прошлой сводки
	busy     int // отказов по занятости
	salt     [16]byte
}

func newGuard(log *slog.Logger) *guard {
	if log == nil {
		log = slog.Default()
	}
	g := &guard{
		sem:     make(chan struct{}, maxInFlight),
		now:     time.Now,
		log:     log,
		buckets: make(map[[8]byte]*bucket),
	}
	// crypto/rand.Read с Go 1.24 ошибки не возвращает (при отказе системного
	// источника он паникует сам), поэтому проверять здесь нечего.
	rand.Read(g.salt[:]) //nolint:errcheck // см. выше
	return g
}

// withGuard — сам слой. Стоит СНАРУЖИ withViewer: тот читает сессию из базы, а
// пускать к базе того, кому мы уже решили отказать, значит платить за отказ.
func (s *Server) withGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Цена считается раньше ключа: у бесплатного маршрута (статика, проба
		// здоровья) хеш адреса не нужен вовсе.
		if cost := costOf(r); cost > 0 && !s.guard.allow(s.guard.key(r), cost) {
			retryLater(w, http.StatusTooManyRequests, 10,
				"Слишком много запросов подряд. Подождите несколько секунд и обновите страницу.")
			return
		}
		// Живой канал идёт мимо СЕМАФОРА и мимо СРОКА, но не мимо частоты: его
		// соединение живёт минутами, и обе оставшиеся проверки его убили бы —
		// семафор в двенадцать слотов десяток вкладок занял бы целиком, а
		// восьмисекундный бюджет обрывал бы поток на середине.
		//
		// Взамен у него СВОИ потолки, и они честнее общих: всего соединений,
		// соединений на человека и срок жизни потока (live.go). Работает это
		// только вместе с двумя его свойствами — поток открыт лишь вошедшим
		// (значит их число ограничено числом участников) и в базу он не ходит
		// ни разу (значит соединение к Postgres не держит).
		if longLived(r) {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.guard.sem <- struct{}{}:
			defer func() { <-s.guard.sem }()
		default:
			// Не поломка, а честная очередь: столько страниц разом площадка не
			// рисует. Человек обновит, робот получит то же самое.
			s.guard.refused(&s.guard.busy)
			retryLater(w, http.StatusServiceUnavailable, 5,
				"Площадка сейчас занята. Обновите страницу через несколько секунд.")
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyOf(r))
		}
		if isNoteUpload(r) {
			// Дедлайны ставятся ПОРУЧНО, потому что ReadTimeout и WriteTimeout —
			// поля http.Server, общие на все маршруты разом. Двадцать секунд
			// чтения стоят там против slowloris, и снимать их ради одной формы
			// нельзя; ResponseController — единственный инструмент, который
			// умеет различать маршруты. Работает он благодаря Unwrap у обёртки
			// лога: без него настоящий писатель не виден.
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(uploadBudget))
			_ = rc.SetWriteDeadline(time.Now().Add(uploadBudget + 30*time.Second))
		}
		ctx, cancel := context.WithTimeout(r.Context(), budgetOf(r))
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// allow списывает цену запроса с корзины клиента.
func (g *guard) allow(key [8]byte, cost float64) bool {
	if cost <= 0 {
		return true
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	g.report(now)

	b, ok := g.buckets[key]
	if !ok {
		if len(g.buckets) >= maxBuckets {
			// Корзин больше, чем бывает живых читателей, и чистка их не убрала:
			// значит адресов действительно много. Новым в этот момент отвечаем
			// «слишком часто» — память конечна, а отказ обратим.
			g.limited++
			return false
		}
		b = &bucket{tokens: bucketBurst, at: now}
		g.buckets[key] = b
	}
	b.tokens = min(bucketBurst, b.tokens+now.Sub(b.at).Seconds()*bucketRefill)
	b.at = now
	if b.tokens < cost {
		g.limited++
		return false
	}
	b.tokens -= cost
	return true
}

// sweep убирает корзины тех, кто ушёл. Отдельной горутины под это нет: чистка
// стоит одного прохода по карте раз в минуту и делается тем, кто и так держит
// мьютекс.
func (g *guard) sweep(now time.Time) {
	if now.Sub(g.swept) < sweepEvery && len(g.buckets) < maxBuckets {
		return
	}
	g.swept = now
	for k, b := range g.buckets {
		if now.Sub(b.at) > bucketIdle {
			delete(g.buckets, k)
		}
	}
}

// refused считает отказ. Отдельным методом, потому что «занято» решается вне
// мьютекса — в момент выбора слота семафора.
func (g *guard) refused(counter *int) {
	g.mu.Lock()
	*counter++
	g.mu.Unlock()
}

// report — сводка отказов раз в минуту. Зовётся из-под мьютекса.
func (g *guard) report(now time.Time) {
	if g.reported.IsZero() {
		g.reported = now
		return
	}
	if now.Sub(g.reported) < reportEvery {
		return
	}
	if g.limited > 0 || g.busy > 0 {
		g.log.Warn("морда отказывала по потолкам",
			"по_частоте", g.limited, "по_занятости", g.busy,
			"за", now.Sub(g.reported).Round(time.Second), "корзин", len(g.buckets))
	}
	g.reported, g.limited, g.busy = now, 0, 0
}

// key — кто стучится, в виде, который не хранит адрес. Соль случайна и живёт в
// памяти процесса, поэтому по ключу нельзя ни узнать адрес, ни проверить
// догадку о нём даже с этой картой в руках.
func (g *guard) key(r *http.Request) [8]byte {
	h := sha256.New()
	h.Write(g.salt[:])
	h.Write([]byte(clientIP(r)))
	var k [8]byte
	copy(k[:], h.Sum(nil))
	return k
}

// clientIP — адрес пришедшего.
//
// Берётся ПОСЛЕДНЯЯ запись X-Forwarded-For, а не первая, и это не мелочь:
// заголовок, присланный клиентом, прокси не выкидывает, а дописывает свой адрес
// в конец. Первая запись — то, что клиент написал сам, и любой потолок обходился
// бы одной строкой в заголовке. Последнюю ставит наш Caddy, который к тому же
// перетирает весь заголовок целиком (см. deploy/platform/Caddyfile).
//
// Наружу приложение не смотрит вовсе — портов у контейнера нет, — поэтому
// RemoteAddr здесь бывает только у запроса изнутри сети compose.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.LastIndexByte(xff, ','); i >= 0 {
			xff = xff[i+1:]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// costOf — во сколько обходится запрос. Ноль — отдаём из памяти (или, у медиа,
// вообще мимо нас: в бою этот путь перехватывает Caddy).
func costOf(r *http.Request) float64 {
	p := r.URL.Path
	switch {
	case p == "/healthz" || p == "/robots.txt" ||
		strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/media/"):
		return 0
	// Карта сайта — запрос робота, и стоит он как тред: пятьдесят тысяч строк из
	// базы. Ходят за ней редко, но потолок здесь не про честного робота, а про
	// того, кто решит брать её в цикле.
	case strings.HasPrefix(p, "/sitemap"):
		return costThread
	case goesToNGS(r):
		return costLogin
	// Публикация с картинкой стоит дороже всякой другой записи и потому стоит
	// ПЕРЕД общим правилом «любой POST — это запись».
	case isNoteUpload(r):
		return costUpload
	case r.Method == http.MethodPost:
		return costWrite
	// Живой добор дешевле страницы треда, и цену ему надо ставить ДО общего
	// правила «всё под /n/ — это тред»: он отдаёт не дерево до 5000 строк, а
	// порцию по индексу (note_id, id), и платить за него как за тред значило бы
	// выбирать корзину читателю, который просто держит вкладку открытой.
	case isFresh(p) || isReplyForm(r):
		return costPage
	case strings.HasPrefix(p, "/n/"):
		return costThread
	default:
		return costPage
	}
}

// isFresh — живой добор: «/fresh» у ленты и «/n/<id>/fresh» у треда.
func isFresh(p string) bool {
	return p == "/fresh" || (strings.HasPrefix(p, "/n/") && strings.HasSuffix(p, "/fresh"))
}

// isReplyForm — открытие формы ответа на месте. Ценой это страница, а не тред, и
// это не поблажка: до неё то же нажатие перерисовывало ВЕСЬ тред до 5000 строк,
// а теперь читается одна реплика по индексу. Только GET — POST по тому же адресу
// это публикация, и стоит она как запись.
func isReplyForm(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.HasPrefix(r.URL.Path, "/n/") && strings.HasSuffix(r.URL.Path, "/reply")
}

// goesToNGS — запрос, который тянет за собой ЧУЖОЙ сайт: вход (анкета плюс
// личное сообщение с кодом) и обновление аватара (анкета плюс файл с CDN). Цена
// и срок у них общие, потому что общее у них главное — ждём мы не себя, а НГС, и
// каждый такой запрос занимает наш слот всё это время.
func goesToNGS(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/login") || r.URL.Path == "/me/avatar"
}

// longLived — запрос, который по устройству живёт долго. Такой ровно один, и
// список здесь именно для того, чтобы второй нельзя было завести молча: снятые
// с него семафор и срок — это две из трёх защит морды, и каждый новый путь в
// этом списке обязан объяснить, чем он их заменяет.
func longLived(r *http.Request) bool {
	return r.URL.Path == "/live"
}

// isNoteUpload — пути площадки, принимающие файл.
//
// Список отдельной функцией, как longLived и goesToNGS, и ровно по той же
// причине: с него сняты три общих потолка сразу — размер тела, срок запроса и
// цена, — и каждый новый маршрут в этом списке обязан объяснить, чем он их
// заменяет. Здесь заменяют: свой потолок тела, свой семафор перекодирования и
// проверка права ДО чтения тела.
//
// Путей ДВА: публикация заметки участником и правка, где картинку ставит
// администратор (27.08.2026). Второй дешевле первого не потолками, а входом —
// нажать его может только администратор, — но потолки у него те же: чужой файл
// одинаково способен съесть память контейнера, кто бы его ни прислал.
func isNoteUpload(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == "/new" ||
		(strings.HasPrefix(r.URL.Path, "/n/") && strings.HasSuffix(r.URL.Path, "/edit"))
}

// maxBodyOf — потолок тела запроса.
func maxBodyOf(r *http.Request) int64 {
	if isNoteUpload(r) {
		return uploadMaxBytes
	}
	return maxFormBytes
}

// budgetOf — сколько запросу отпущено.
func budgetOf(r *http.Request) time.Duration {
	switch {
	case goesToNGS(r):
		return loginBudget
	case isNoteUpload(r):
		return uploadBudget
	}
	return requestBudget
}

// retryLater — отказ по потолку. Простым текстом, а не страницей: отказ обязан
// быть дешевле работы, иначе он и есть работа — а шаблон это разбор темы, шапка
// и сборка буфера. Retry-After читают и люди (браузер покажет текст), и роботы.
func retryLater(w http.ResponseWriter, status, after int, msg string) {
	h := w.Header()
	h.Set("Retry-After", strconv.Itoa(after))
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, msg+"\n") //nolint:errcheck // клиенту уже нечего сказать
}
