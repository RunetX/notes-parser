// Пакет web — SSR-морда площадки: роутер, middleware, шаблоны. Ходит в
// internal/platform напрямую, а НЕ в собственный API по петле: лишний хоп на
// одном ядре стоит дороже, чем экономит, и разводит два пути чтения данных.
package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"lovegw/internal/platform"
)

const (
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 90 * time.Second
	shutdownGrace     = 10 * time.Second
)

// Config — что серверу нужно знать о себе.
type Config struct {
	Listen  string
	BaseURL string
	Log     *slog.Logger
}

// Server — HTTP-морда площадки.
type Server struct {
	cfg  Config
	pf   *platform.Platform
	log  *slog.Logger
	http *http.Server
}

func New(cfg Config, pf *platform.Platform) *Server {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, pf: pf, log: log}
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.routes(),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return s.withSecurityHeaders(mux)
}

// withSecurityHeaders ставит заголовки, которые дешевле завести сразу, чем
// вспоминать перед открытием доступа. CSP запрещает inline-скрипты и чужие
// хосты — это и есть причина, по которой у площадки нет npm и CDN.
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

// handleHealth отвечает 200, только если жива и база: иначе оркестратор будет
// считать здоровым контейнер, который не может обслужить ни одного запроса.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.pf.Pool().Ping(ctx); err != nil {
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

// handleIndex — временная заглушка. Настоящая лента появится вместе с шаблонами.
func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><html lang="ru"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<meta name="robots" content="noindex, nofollow"><title>Заметки</title></head>`+
		`<body><h1>Заметки</h1><p>Площадка поднимается.</p></body></html>`)
}

// Run поднимает сервер и гасит его по отмене контекста. Пригоден и как элемент
// errgroup демона, и как самостоятельная команда.
func (s *Server) Run(ctx context.Context) error {
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
