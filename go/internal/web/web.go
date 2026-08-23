// Пакет web — SSR-морда площадки: роутер, middleware, шаблоны. Ходит в
// internal/platform напрямую, а НЕ в собственный API по петле: лишний хоп на
// одном ядре стоит дороже, чем экономит, и разводит два пути чтения данных.
//
// Страницы собираются на сервере целиком и работают без JS. Это не аскеза:
// строгий CSP («ни inline-скриптов, ни чужих хостов») — единственная защита от
// XSS, которая держится структурой, а не бдительностью, и она же причина, по
// которой у площадки нет ни npm, ни CDN.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lovegw/internal/platform"
)

const (
	readHeaderTimeout = 10 * time.Second
	// readTimeout — потолок на ВЕСЬ запрос вместе с телом. Без него медленно
	// отдаваемое тело держит горутину и слот семафора сколько угодно: это
	// slowloris, и стоит он атакующему одного открытого сокета.
	readTimeout = 20 * time.Second
	// writeTimeout — с запасом над бюджетом входа (guard.loginBudget): вход
	// ходит на НГС, и обрывать его ответом «пусто» было бы хуже ожидания.
	writeTimeout  = 45 * time.Second
	idleTimeout   = 90 * time.Second
	shutdownGrace = 10 * time.Second
	// maxHeaderBytes — заголовков у нас на пару килобайт (кука сессии да
	// Accept-*); дефолтный мегабайт на запрос — это память ни за что.
	maxHeaderBytes = 16 << 10
)

// SiteName — как площадка зовётся на своих страницах. Имя рабочее: настоящее
// выбирает владелец, и оно же попадёт в тексты согласий и в уведомление РКН,
// поэтому меняется оно один раз и здесь.
const SiteName = "Зазеркалье"

// Config — что серверу нужно знать о себе.
type Config struct {
	Listen  string
	BaseURL string
	// MediaDir — каталог CAS. В бою файлы отдаёт Caddy, минуя Go; наш обработчик
	// нужен разработке и на случай запроса мимо прокси.
	MediaDir string
	// Operator — реквизиты того, кто обрабатывает данные. Подставляются в
	// тексты согласий ДО публикации: доказательством служит финальный текст, а
	// не шаблон, поэтому смена реквизитов — это новая версия документа.
	Operator platform.Operator
	Log      *slog.Logger
}

// Store — то, что морда спрашивает у ядра.
//
// Интерфейс, а не *platform.Platform, по двум причинам. Первая: он же и есть
// список того, что веб-морда умеет делать с данными, — здесь видно, что она
// только читает. Вторая практическая: страницы проверяются httptest'ом без
// Postgres, а интеграционные тесты ядра идут через ssh-туннель и стоят двух
// минут — гонять их ради вёрстки незачем.
type Store interface {
	Ping(ctx context.Context) error
	CountNotes(ctx context.Context) (int, error)
	Feed(ctx context.Context, v platform.Viewer, offset, limit int) ([]platform.NoteView, error)
	// PinnedNotes — закреплённые заметки, которые лента показывает поверх
	// хронологии. Отдельным методом, а не флагом у Feed: своего порядка,
	// своего потолка и своего индекса, а главное — они нужны только на первой
	// странице, и спрашивать их на каждой было бы запросом ни за чем.
	PinnedNotes(ctx context.Context, v platform.Viewer) ([]platform.NoteView, error)
	NoteViewByID(ctx context.Context, v platform.Viewer, id int64) (platform.NoteView, error)
	NoteImages(ctx context.Context, noteID int64) ([]platform.Media, error)
	// CommentViewByID — одна реплика. Нужна форме ответа, которая открывается
	// на месте, без перезагрузки (replyform.go): страница просит у сервера
	// готовую строку с формой, а строке нужен адресат — его ник, тень он или
	// участник и на какой глубине стоит. Читать ради этого весь тред заново
	// было бы дороже той перезагрузки, от которой мы уходим.
	CommentViewByID(ctx context.Context, v platform.Viewer, noteID, id int64) (platform.CommentView, error)
	Thread(ctx context.Context, v platform.Viewer, noteID int64) ([]platform.CommentView, error)
	Flat(ctx context.Context, v platform.Viewer, noteID int64, offset, limit int) ([]platform.CommentView, error)
	// CommentsSince и NotesSince — живой добор: что появилось ПОСЛЕ того, как
	// страница была нарисована (fresh.go). Отдельные методы, а не «дай тред
	// заново»: тред отдаётся целиком до 5000 строк, и перечитывать его на
	// каждую новую реплику у каждого открытого окна значит отнять ядро у
	// зеркала, которое живёт на том же хосте.
	CommentsSince(ctx context.Context, v platform.Viewer, noteID int64, after platform.FreshAfter, limit int) ([]platform.CommentView, error)
	// CommentsMoved — вторая половина живого добора: строки, которые на странице
	// уже стоят, но с тех пор переехали. Дерево перестраивается под открытой
	// страницей (зеркало ставит ребро по обращению, обход мобильной версии
	// заменяет его настоящим), а по границе id переехавшая строка не приезжает
	// никогда — id у неё прежний. Возвращает и новую границу переездов.
	CommentsMoved(ctx context.Context, v platform.Viewer, noteID int64, after platform.MovedAfter, limit int) ([]platform.CommentView, platform.MovedAfter, error)
	// ThreadFreshAfter — граница добора для только что нарисованной страницы
	// заметки. Спрашивается у ядра, а не считается по показанным репликам: в
	// линейном виде на странице окно, а не весь тред.
	ThreadFreshAfter(ctx context.Context, noteID int64) (platform.FreshAfter, error)
	NotesSince(ctx context.Context, v platform.Viewer, after time.Time, afterID int64, limit int) ([]platform.NoteView, error)
	// NoteReactions — реакции заметки и всего треда разом. Отдельным методом, а не
	// полем в CommentView: реакции меняются чаще самих реплик и читаются одним
	// запросом на страницу, а не по одному на строку.
	NoteReactions(ctx context.Context, viewerID, noteID int64) (map[int64][]platform.Reaction, error)
}

// Auth — вход, сессии и согласия. Отдельным интерфейсом от Store намеренно:
// Store читает публичные страницы и обязан оставаться списком «что морда умеет
// делать с чужими данными», а здесь — операции над данными ОДНОГО человека, и
// смешивать их в один список значило бы потерять это различие.
type Auth interface {
	StartTalksChallenge(ctx context.Context, profileID int64) (platform.Challenge, error)
	VerifyTalksCode(ctx context.Context, profileID int64, code string) error
	StartProfileChallenge(ctx context.Context, profileID int64) (platform.Challenge, error)
	VerifyProfileChallenge(ctx context.Context, profileID int64, code, aboutMe string) error
	CompleteNGSLogin(ctx context.Context, prof platform.MirroredAuthor, gender platform.Gender) (int64, error)
	AbortLogin(ctx context.Context, userID int64) error
	RedeemInvite(ctx context.Context, code, nick string) (int64, error)

	CreateSession(ctx context.Context, userID int64, ua string) (string, time.Time, error)
	SessionUser(ctx context.Context, token string) (platform.User, error)
	RevokeSession(ctx context.Context, token string) error

	MemberCard(ctx context.Context, id int64) (platform.Author, error)
	MissingConsent(ctx context.Context, userID int64, op platform.Operator) (platform.ConsentDoc, error)
	UserConsents(ctx context.Context, userID int64) (platform.Consents, error)
	GrantConsent(ctx context.Context, userID int64, kind string, version int, ua string) error
	RevokeConsent(ctx context.Context, userID int64, kind string) error
}

// SiteProfile — анкета НГС, какой она нужна входу. Свой тип, а не love.Profile:
// пакет web о существовании НГС знать не обязан, а перевод стоит десяти строк в
// сборке команды (там же, где живёт клиент сайта).
type SiteProfile struct {
	Nick string
	// PassportID — сквозной номер аккаунта НГС. Личные сообщения адресуются им,
	// а не номером анкеты, поэтому без него канал «код в личку» недоступен.
	PassportID int64
	AvatarURL  string // на hsmedia.ru — наружу не отдаём, только для сверки
	AboutMe    string
	Gender     platform.Gender
	Blocked    bool
}

// ErrNoProfile — анкеты с таким номером сайт не отдал.
var ErrNoProfile = errors.New("анкета не найдена")

// Site — чтение анкеты НГС для входа по коду. nil означает, что этот путь
// недоступен (нет RU-IP, сайт закрылся): тогда остаются приглашения, и страница
// входа говорит об этом прямо, а не показывает форму, которая не сработает.
type Site interface {
	Profile(ctx context.Context, id int64) (SiteProfile, error)
	// Avatar — байты фото по ссылке из анкеты. Рядом с Profile, а не отдельной
	// способностью: это то же самое анонимное чтение чужого сайта, и живут они
	// или умирают вместе.
	Avatar(ctx context.Context, url string) ([]byte, error)
}

// SiteMessenger — необязательная способность клиента НГС: отправить код личным
// сообщением от служебного аккаунта. Type-assertion, а не отдельный параметр
// конструктора, — тот же приём, что у dmbot.SiteProfile: способности нет,
// значит нет и канала, и страница входа сразу предлагает запасной путь вместо
// формы, которая не сработает.
//
// Отдельным интерфейсом, а не методом Site, потому что это ЗАПИСЬ: чтение
// анкеты анонимно и безобидно, а отправка сообщения идёт под живой сессией
// служебного аккаунта и видна получателю.
type SiteMessenger interface {
	SendCode(ctx context.Context, passportID int64, code string) error
}

// messenger — доступен ли канал «код в личку».
func (s *Server) messenger() (SiteMessenger, bool) {
	if s.site == nil {
		return nil, false
	}
	m, ok := s.site.(SiteMessenger)
	return m, ok
}

// Server — HTTP-морда площадки.
type Server struct {
	cfg  Config
	st   Store
	auth Auth
	wr   Writer    // nil — площадка только на чтение
	mod  Moderator // nil — модерации нет: ни /mod, ни кнопок под репликами
	site Site      // nil — вход по анкете НГС недоступен
	// events — шина событий (эпик F): nil ⇒ ни страницы событий, ни
	// колокольчика, ни живого канала. Подключается SetEvents, а не
	// конструктором: способность необязательная (см. events.go).
	events Events
	// hub — живой канал (hub.go): nil, если шина не умеет отдавать поток. Живёт
	// рядом с events, а не внутри, потому что это состояние ПРОЦЕССА (слушатели,
	// курсоры), а не способность хранилища.
	hub   *hub
	log   *slog.Logger
	http  *http.Server
	media *mediaServer // nil, если каталог не задан
	// guard — потолки наплыва (guard.go). Общий на сервер: корзины клиентов и
	// семафор одновременности имеют смысл только как одно состояние.
	guard *guard
	// notes — длина ленты, посчитанная недавно (feed.go).
	notes feedCount
	// secure — куки помечаются Secure и получают префикс __Host-. Выводится из
	// BaseURL: по http браузер такие куки просто отбросит, и разработка встала
	// бы на ровном месте.
	secure bool
}

func New(cfg Config, st Store, auth Auth, wr Writer, mod Moderator, site Site) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:    cfg,
		st:     st,
		auth:   auth,
		wr:     wr,
		mod:    mod,
		site:   site,
		log:    log,
		guard:  newGuard(log),
		secure: strings.HasPrefix(cfg.BaseURL, "https://"),
	}
	// Свой адрес — единственный, который в текстах становится ссылкой
	// (см. linkMode). Ставится здесь и только читается: сервер в процессе
	// один, а шаблоны с их FuncMap разбираются один раз на процесс.
	setOwnLinkPrefix(cfg.BaseURL)
	// Превью роликов лежат в том же каталоге медиа (video.go, preview.go).
	// Ставится тут по той же причине и с той же оговоркой: пустой каталог значит
	// «карточек не бывает», и ссылки остаются текстом.
	setVideoPreviews(cfg.MediaDir, log)
	if site == nil {
		log.Warn("клиент НГС не задан — вход по коду в анкете недоступен, остаются приглашения")
	}
	if cfg.MediaDir != "" {
		m, err := newMediaServer(cfg.MediaDir)
		if err != nil {
			// Не отказ: без медиа страницы рисуются, а в бою этот путь всё
			// равно перехватывает Caddy — Go до него не доходит вовсе.
			log.Warn("медиа не отдаются приложением", "dir", cfg.MediaDir, "err", err)
		} else {
			s.media = m
		}
	}
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	return s
}

// routes собирает роутер. Слоя «за воротами» здесь больше нет: чтение открыто
// всем, а вход — это отдельная страница, на которую ведёт кнопка в шапке.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	// Справка открыта всем, включая не вошедших: правила, которые видно только
	// изнутри, — это не правила, а сюрприз.
	mux.HandleFunc("GET /help", s.handleHelp)
	// Бумаги — по той же причине и с добавкой от закона: политику обработки
	// оператор обязан опубликовать так, чтобы её прочёл ЛЮБОЙ (ч. 2 ст. 18.1),
	// значит за вход её убирать нельзя. /consents (множественное) — чтение
	// документов, /consent ниже — шаг входа, где их подписывают.
	mux.HandleFunc("GET /consents", s.handleConsentDocs)
	mux.HandleFunc("GET /privacy", s.handlePrivacy)
	mux.HandleFunc("GET /disclaimer", s.handleDisclaimer)
	mux.HandleFunc("GET /assets/{name...}", s.handleAsset)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("POST /login", s.handleLoginStart)
	mux.HandleFunc("POST /login/check", s.handleLoginCheck)
	mux.HandleFunc("GET /login/invite", s.handleInvite)
	mux.HandleFunc("POST /login/invite", s.handleInviteSubmit)
	mux.HandleFunc("GET /consent", s.handleConsent)
	mux.HandleFunc("POST /consent", s.handleConsentGrant)
	mux.HandleFunc("POST /consent/refuse", s.handleConsentRefuse)
	mux.HandleFunc("GET /me", s.handleMe)
	mux.HandleFunc("POST /me/consent", s.handleMeConsent)
	mux.HandleFunc("POST /me/nick", s.handleNick)
	mux.HandleFunc("POST /me/avatar", s.handleAvatar)
	mux.HandleFunc("POST /logout", s.handleLogout)
	// События: свои поводы и отметка прочитанного. Маршруты заведены всегда —
	// без шины они отвечают «нет такой страницы», как /mod без модерации.
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("POST /events/read", s.handleEventsRead)
	// Живой канал. Идёт мимо семафора и срока запроса (см. withGuard и шапку
	// live.go): соединение живёт минутами, а общий потолок морды — двенадцать
	// запросов в работе разом при пуле в четыре соединения к базе.
	mux.HandleFunc("GET /live", s.handleLive)
	mux.HandleFunc("POST /theme", s.handleTheme)
	// Запись. Правки и удаления ЧУЖОГО среди этих путей нет, и это не упущение:
	// участник только пишет, остальное — у модератора (Ш7).
	mux.HandleFunc("GET /new", s.handleNewNote)
	mux.HandleFunc("POST /new", s.handleCreateNote)
	mux.HandleFunc("GET /n/{id}/edit", s.handleEditNote)
	mux.HandleFunc("POST /n/{id}/edit", s.handleUpdateNote)
	// GET и POST по одному адресу: сервер отдаёт форму ответа и он же её
	// принимает. Форма приходит ГОТОВОЙ строкой, как и живой добор, — второго
	// способа собрать разметку у площадки не заводится (replyform.go).
	mux.HandleFunc("GET /n/{id}/reply", s.handleReplyForm)
	mux.HandleFunc("POST /n/{id}/reply", s.handleCreateComment)
	mux.HandleFunc("POST /n/{id}/react", s.handleReact)
	// Модерация. Скрытие и возврат — единственные способности, которых нет у
	// участника; правки чужого текста среди них по-прежнему нет, и это решение,
	// а не недоделка: тихая правка под чужим ником хуже удаления.
	mux.HandleFunc("GET /mod", s.handleMod)
	mux.HandleFunc("GET /mod/log", s.handleModLog)
	mux.HandleFunc("POST /mod/act", s.handleModAct)
	mux.HandleFunc("GET /mod/u/{id}", s.handleModUser)
	mux.HandleFunc("POST /mod/u/{id}", s.handleModUserAct)
	mux.HandleFunc("GET /report", s.handleReport)
	mux.HandleFunc("POST /report", s.handleReportSubmit)
	mux.HandleFunc("POST /appeal", s.handleAppeal)
	if s.media != nil {
		mux.Handle("GET /media/", http.StripPrefix("/media/", s.media))
	}
	// Живой добор — вторая половина живого канала: /live говорит, ЧТО новое, а
	// сюда страница приходит за готовой строкой (fresh.go).
	mux.HandleFunc("GET /fresh", s.handleFreshFeed)
	mux.HandleFunc("GET /n/{id}/fresh", s.handleFresh)
	mux.HandleFunc("GET /{$}", s.handleFeed)
	mux.HandleFunc("GET /n/{id}", s.handleNote)
	mux.HandleFunc("/", s.handleNotFound)

	// Порядок слоёв: заголовки безопасности достаются и отказам, лог видит их
	// статус, а потолки стоят ДО withViewer — тот читает сессию из базы.
	return s.withSecurityHeaders(s.withLog(s.withGuard(s.withViewer(mux))))
}

// withSecurityHeaders ставит заголовки, которые дешевле завести сразу, чем
// вспоминать перед открытием доступа. CSP запрещает inline-скрипты и чужие
// хосты — это и есть причина, по которой у площадки нет npm и CDN.
//
// Из того же запрета следует неочевидное: inline-атрибут style тоже запрещён
// (хеши и nonce к атрибутам неприменимы), поэтому глубина ветки в треде
// выражается КЛАССОМ, а не подставленной в разметку переменной, — см.
// depthClass и .d1…d12 в style.css.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), interest-cohort=()")
		// Чтение открыто людям, но не поисковикам: зеркало чужой переписки в
		// выдаче — это распространение персональных данных без согласия
		// (ст. 10.1), и снимать запрет можно только вместе с бумагой (Ш9).
		// Заголовком, а не только robots.txt: robots.txt соблюдают не все.
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		next.ServeHTTP(w, r)
	})
}

// withLog пишет строку на запрос. Без адресов: сырых IP у нас нет нигде,
// включая логи прокси, и заводить их здесь — значит завести персональные
// данные там, где их специально нет.
func (s *Server) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		lvl := slog.LevelDebug
		if rec.status >= 500 {
			lvl = slog.LevelWarn
		}
		s.log.Log(r.Context(), lvl, "запрос",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", time.Since(start).Milliseconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap отдаёт настоящий ResponseWriter. Обязателен, а не любезен: обёртка
// ВСТРАИВАЕТ интерфейс, а встраивание интерфейса промотирует только его
// собственные методы, — значит Flush и SetWriteDeadline настоящего писателя за
// ней не видны вовсе. http.ResponseController ищет их именно через Unwrap.
//
// Пока страницы собирались в буфер и отдавались целиком, этого никто не
// замечал. Живому каналу (live.go) без обеих способностей конец: без Flush
// сигналы копятся в буфере, без снятого дедлайна поток обрывается на 45-й
// секунде. Стережёт это TestLiveStreamHeaders — он и нашёл пропажу.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// handleHealth отвечает 200, только если жива и база: иначе оркестратор будет
// считать здоровым контейнер, который не может обслужить ни одного запроса.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		s.log.Error("healthz: база не отвечает", "err", err)
		http.Error(w, "база не отвечает", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleRobots(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
}

// viewer — кто смотрит, для запросов к ядру: по нему считается «моё» и видны ли
// инструменты модерации. Гость — нулевое значение.
func (s *Server) viewer(r *http.Request) platform.Viewer {
	u, ok := s.me(r)
	if !ok {
		return platform.Viewer{}
	}
	return platform.Viewer{UserID: u.ID, Role: u.Role}
}

// sameOrigin — первая линия защиты от CSRF: запрос пришёл с нашей же страницы.
// Заголовок Sec-Fetch-Site шлют все живые браузеры и подделать его со стороны
// нельзя. Вторая линия — скрытое поле формы (csrf.go), и стоит она у всего, что
// пишет от имени вошедшего.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// postForm — общий вход для всех форм: происхождение запроса и разбор тела.
// false означает «ответ уже отправлен». Одним местом, потому что забытая
// проверка происхождения — это дыра, а не мелкий недосмотр.
func (s *Server) postForm(w http.ResponseWriter, r *http.Request) bool {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return false
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return false
	}
	return true
}

// localPath оставляет от адреса возврата только безопасную часть: свой путь и
// свой запрос. Без этого форма темы становится открытым редиректом — «//evil»
// браузер читает как чужой хост, а «\\evil» им же нормализуется.
func localPath(p string) string {
	if p == "" || p[0] != '/' {
		return "/"
	}
	if strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) {
		return "/"
	}
	if strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

// Close освобождает то, что сервер держал открытым (сейчас — дескриптор
// каталога медиа). Вызывается после Run; повторный вызов безопасен.
func (s *Server) Close() error { return s.media.Close() }

// Run поднимает сервер и гасит его по отмене контекста. Пригоден и как элемент
// errgroup демона, и как самостоятельная команда.
func (s *Server) Run(ctx context.Context) error {
	defer s.Close() //nolint:errcheck // закрытие каталога на выходе ничего не решает
	errc := make(chan error, 1)
	if s.hub != nil {
		// Один опрос базы на процесс, независимо от числа открытых страниц.
		go s.hub.run(ctx)
	}
	go func() {
		s.log.Info("веб-морда слушает", "addr", s.cfg.Listen)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		// Свой контекст на остановку: родительский уже отменён, а недоговорённые
		// ответы надо дописать, иначе выкатка рвёт страницы читателям.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := s.http.Shutdown(sctx); err != nil {
			return fmt.Errorf("остановка веб-морды: %w", err)
		}
		s.log.Info("веб-морда остановлена")
		return nil
	}
}
