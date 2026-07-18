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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // тайзоны в бинарнике: нужны в scratch/distroless-образе

	"golang.org/x/sync/errgroup"

	"lovegw/internal/bridge"
	"lovegw/internal/config"
	"lovegw/internal/dmbot"
	"lovegw/internal/legacy"
	"lovegw/internal/love"
	"lovegw/internal/mirror"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
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
		err = cmdRun(ctx, os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
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
  lovegw run    [-config config.json] [-seed]
  lovegw doctor [-config config.json] [-post-test]`)
}

// cmdRun — основной демон: зеркалирование ленты и комментариев в Telegram.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к конфигу")
	seed := fs.Bool("seed", false, "первый обход ленты: запомнить заметки без постинга")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"config": true})); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.MirrorBot.Token == "" || cfg.MirrorBot.ChannelID == 0 || cfg.MirrorBot.DiscussionChatID == 0 {
		return fmt.Errorf("run: не заданы mirror_bot.token / channel_id / discussion_chat_id")
	}
	log := newLogger(cfg.LogLevel)

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	// ЛС-бот РюмкинЪ (опционален): без него мост не сможет уведомлять
	// пользователей о протухшей сессии, но зеркалирование работает.
	var dm *dmbot.Bot
	var notify bridge.Notify
	if cfg.DMBot.Token != "" {
		dm, err = dmbot.New(cfg.DMBot.Token, st, client, log)
		if err != nil {
			return err
		}
		notify = dm.Notify
	}

	handler := bridge.New(st, client, notify,
		cfg.MirrorBot.ChannelID, cfg.MirrorBot.DiscussionChatID, log)
	tg, err := tgx.NewMirror(cfg.MirrorBot.Token, cfg.MirrorBot.ChannelID,
		cfg.MirrorBot.DiscussionChatID, cfg.Signature, cfg.Site.BaseURL, log, handler.Handle)
	if err != nil {
		return err
	}
	mir := mirror.New(st, client, tg, cfg.NotesLimit,
		time.Duration(cfg.FeedIntervalS)*time.Second, *seed, log)

	log.Info("lovegw запущен", "seed", *seed, "db", cfg.DBPath, "dm_bot", dm != nil)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		tg.Start(gctx) // блокируется до отмены контекста
		return nil
	})
	g.Go(func() error { return mir.Run(gctx) })
	if dm != nil {
		g.Go(func() error {
			dm.Start(gctx)
			return nil
		})
	}
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("lovegw остановлен")
	return nil
}

// cmdImport переносит состояние старой Python-версии в SQLite.
// Импорт идемпотентен: повторный запуск ничего не дублирует.
func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к конфигу")
	notesPath := fs.String("notes", "", "notes.json старой версии")
	sessionsPath := fs.String("sessions", "", "sessions_export.json (из tools/export_sessions.py)")
	subscribersPath := fs.String("subscribers", "", "subscribers.json старой версии")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "notes": true, "sessions": true, "subscribers": true,
	})); err != nil {
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

// reorderArgs переносит флаги перед позиционными аргументами: стандартный
// flag прекращает разбор на первом позиционном, а команды вида
// `crawl notes -save-html dir` естественно писать флагами после.
// valueFlags — имена флагов, ожидающих значение следующим токеном.
func reorderArgs(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if valueFlags[name] && !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func cmdCrawl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к конфигу")
	saveHTML := fs.String("save-html", "", "каталог для сохранения сырого HTML (фикстуры)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"config": true, "save-html": true})); err != nil {
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
		if saveDir != "" {
			return fmt.Errorf("%w — страница не получена, HTML НЕ сохранён", err)
		}
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
		if saveDir != "" {
			return fmt.Errorf("%w — страница не получена, HTML НЕ сохранён", err)
		}
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
