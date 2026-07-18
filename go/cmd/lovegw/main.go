// lovegw — шлюз между заметками love.ngs.ru и Telegram.
//
// Подкоманды:
//
//	crawl  — отладочный обход сайта: разобрать ленту или комментарии и
//	         напечатать JSON; --save-html пишет сырой HTML для фикстур
//	import — импорт состояния старой Python-версии (M2)
//	run    — основной демон (M3+)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	_ "time/tzdata" // тайзоны в бинарнике: нужны в scratch/distroless-образе

	"lovegw/internal/config"
	"lovegw/internal/legacy"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "crawl":
		err = cmdCrawl(ctx, os.Args[2:])
	case "import":
		err = cmdImport(ctx, os.Args[2:])
	case "run":
		err = fmt.Errorf("run: ещё не реализовано (этап M3)")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `использование:
  lovegw crawl [-config config.json] [-save-html dir] notes
  lovegw crawl [-config config.json] [-save-html dir] comments <note_id>
  lovegw import [-config config.json] [-notes notes.json] [-sessions sessions_export.json] [-subscribers subscribers.json]
  lovegw run          (M3)`)
}

// cmdImport переносит состояние старой Python-версии в SQLite.
// Импорт идемпотентен: повторный запуск ничего не дублирует.
func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к конфигу")
	notesPath := fs.String("notes", "", "notes.json старой версии")
	sessionsPath := fs.String("sessions", "", "sessions_export.json (из tools/export_sessions.py)")
	subscribersPath := fs.String("subscribers", "", "subscribers.json старой версии")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *notesPath == "" && *sessionsPath == "" && *subscribersPath == "" {
		return fmt.Errorf("import: не указан ни один файл для импорта")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	var total legacy.Stats
	importOne := func(path string, do func(*os.File) (legacy.Stats, error)) error {
		if path == "" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		stats, err := do(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		total.Notes += stats.Notes
		total.Comments += stats.Comments
		total.Sessions += stats.Sessions
		total.Subscriptions += stats.Subscriptions
		total.Warnings = append(total.Warnings, stats.Warnings...)
		return nil
	}

	if err := importOne(*notesPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportNotes(ctx, st, f, now)
	}); err != nil {
		return err
	}
	if err := importOne(*sessionsPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportSessions(ctx, st, f, now)
	}); err != nil {
		return err
	}
	if err := importOne(*subscribersPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportSubscribers(ctx, st, f)
	}); err != nil {
		return err
	}

	fmt.Printf("импортировано: заметок %d, комментариев %d, сессий %d, подписок %d\n",
		total.Notes, total.Comments, total.Sessions, total.Subscriptions)
	for _, w := range total.Warnings {
		fmt.Fprintln(os.Stderr, "предупреждение:", w)
	}
	return nil
}

func cmdCrawl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к конфигу")
	saveHTML := fs.String("save-html", "", "каталог для сохранения сырого HTML (фикстуры)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("crawl: не указан объект (notes|comments)")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	switch fs.Arg(0) {
	case "notes":
		return crawlNotes(ctx, client, *saveHTML)
	case "comments":
		if fs.NArg() < 2 {
			return fmt.Errorf("crawl comments: не указан id заметки")
		}
		return crawlComments(ctx, client, cfg.Site.BaseURL, fs.Arg(1), *saveHTML)
	default:
		return fmt.Errorf("crawl: неизвестный объект %q", fs.Arg(0))
	}
}

func crawlNotes(ctx context.Context, client *love.Client, saveDir string) error {
	raw, err := client.RawNotes(ctx)
	if err != nil {
		return err
	}
	if err := saveRaw(saveDir, "notes_feed.html", raw); err != nil {
		return err
	}
	notes, err := love.ParseNotes(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return printJSON(notes)
}

func crawlComments(ctx context.Context, client *love.Client, baseURL, noteID, saveDir string) error {
	raw, err := client.RawComments(ctx, noteID)
	if err != nil {
		return err
	}
	if err := saveRaw(saveDir, "comments_"+noteID+".html", raw); err != nil {
		return err
	}
	comments, err := love.ParseComments(bytes.NewReader(raw), baseURL)
	if err != nil {
		return err
	}
	return printJSON(comments)
}

func saveRaw(dir, name string, raw []byte) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "HTML сохранён:", path)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
