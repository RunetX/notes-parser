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
	writeTimeout      = 30 * time.Second
	idleTimeout       = 90 * time.Second
	shutdownGrace     = 10 * time.Second
)

// SiteName — как площадка зовётся на своих страницах. Имя рабочее: настоящее
// выбирает владелец, и оно же попадёт в тексты согласий и в уведомление РКН,
// поэтому меняется оно один раз и здесь.
const SiteName = "Заметки"

// Config — что серверу нужно знать о себе.
type Config struct {
	Listen  string
	BaseURL string
	// MediaDir — каталог CAS. В бою файлы отдаёт Caddy, минуя Go; наш обработчик
	// нужен разработке и на случай запроса мимо прокси.
	MediaDir string
	// PreviewKey — общий ключ доступа до настоящего входа (Ш4). Пустой ключ
	// означает «не пускать никого», а не «пускать всех»: см. gate.go.
	PreviewKey string
	Log        *slog.Logger
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
	Feed(ctx context.Context, v platform.Viewer, cur platform.FeedCursor, limit int) ([]platform.NoteView, platform.FeedCursor, error)
	NoteViewByID(ctx context.Context, v platform.Viewer, id int64) (platform.NoteView, error)
	NoteImages(ctx context.Context, noteID int64) ([]platform.Media, error)
	Thread(ctx context.Context, v platform.Viewer, noteID int64, after string, limit int) ([]platform.CommentView, string, error)
	Flat(ctx context.Context, v platform.Viewer, noteID, afterID int64, limit int) ([]platform.CommentView, int64, error)
}

// Server — HTTP-морда площадки.
type Server struct {
	cfg   Config
	st    Store
	log   *slog.Logger
	http  *http.Server
	gate  []byte       // sha256 ключа доступа; пусто — площадка закрыта
	media *mediaServer // nil, если каталог не задан
	// secure — куки помечаются Secure и получают префикс __Host-. Выводится из
	// BaseURL: по http браузер такие куки просто отбросит, и разработка встала
	// бы на ровном месте.
	secure bool
}

func New(cfg Config, st Store) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:    cfg,
		st:     st,
		log:    log,
		gate:   gateDigest(cfg.PreviewKey),
		secure: strings.HasPrefix(cfg.BaseURL, "https://"),
	}
	if len(s.gate) == 0 {
		log.Warn("platform.preview_key не задан — страницы площадки закрыты (403), живы только /healthz и /robots.txt")
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
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	return s
}

// routes собирает роутер. Открытая часть — то, что обязано работать до входа:
// проверка здоровья, robots, статика (иначе и сама страница входа будет голой)
// и медиа. Всё остальное живёт за воротами.
func (s *Server) routes() http.Handler {
	open := http.NewServeMux()
	open.HandleFunc("GET /healthz", s.handleHealth)
	open.HandleFunc("GET /robots.txt", s.handleRobots)
	open.HandleFunc("GET /assets/{name}", s.handleAsset)
	open.HandleFunc("GET /gate", s.handleGate)
	open.HandleFunc("POST /gate", s.handleGateSubmit)
	// Тема живёт снаружи ворот намеренно: страница входа тоже должна
	// перекрашиваться, иначе тёмная тема начинается только после пароля.
	open.HandleFunc("POST /theme", s.handleTheme)
	if s.media != nil {
		open.Handle("GET /media/", http.StripPrefix("/media/", s.media))
	}

	inner := http.NewServeMux()
	inner.HandleFunc("GET /{$}", s.handleFeed)
	inner.HandleFunc("GET /n/{id}", s.handleNote)
	inner.HandleFunc("/", s.handleNotFound)
	open.Handle("/", s.withGate(inner))

	return s.withSecurityHeaders(s.withLog(open))
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
		// Площадка видна только вошедшим, поэтому индексация запрещена
		// заголовком, а не только robots.txt: robots.txt соблюдают не все.
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

// viewer — кто смотрит. До Ш4 это всегда «никто»: ворота пускают всех с общим
// ключом, и своей строки в users у гостя нет. Здесь же появится сессия.
func (s *Server) viewer(_ *http.Request) platform.Viewer { return platform.Viewer{} }

// sameOrigin — первая линия защиты от CSRF: запрос пришёл с нашей же страницы.
// Заголовок Sec-Fetch-Site шлют все живые браузеры и подделать его со стороны
// нельзя. Вторая линия (скрытое поле формы) появится вместе с записью (Ш5) —
// именно поле, а не заголовок, потому что формы обязаны работать без JS.
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
