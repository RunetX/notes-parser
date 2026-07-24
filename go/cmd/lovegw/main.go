// lovegw — шлюз между заметками love.ngs.ru и Telegram.
//
// Подкоманды:
//
//	crawl  — отладочный обход сайта: разобрать ленту или комментарии и
//	         напечатать JSON; --save-html пишет сырой HTML для фикстур
//	grab   — разовая выгрузка одной заметки со всеми комментариями в
//	         древовидном виде, нормализованная в archive.db (типажи в отдельной
//	         таблице users, дерево ответов через parent_id); -json — снимок в файл
//	export — офлайн-выгрузка заметки из archive.db во вложенное JSON-дерево
//	backfill — массовая выгрузка диапазона заметок в archive.db (перечисление
//	         живых id по ленте, пул воркеров, резюм, -proxy для сайта)
//	personas — распознавание личностей поверх archive.db: склейка альт-анкет
//	         одного человека (flag → candidates → link → cluster → set)
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

const (
	defaultConfigPath = "config.json"
	configFlagUsage   = "путь к конфигу"
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
	case "repost":
		err = cmdRepost(ctx, os.Args[2:])
	case "grab":
		err = cmdGrab(ctx, os.Args[2:])
	case "export":
		err = cmdExport(ctx, os.Args[2:])
	case "backfill":
		err = cmdBackfill(ctx, os.Args[2:])
	case "personas":
		err = cmdPersonas(ctx, os.Args[2:])
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
  lovegw doctor [-config config.json] [-post-test]
  lovegw repost [-config config.json] <note_id> [<note_id> ...]
  lovegw grab   [-config config.json] [-db archive.db] [-json] [-out dir] [-save-html dir] [-view tree|linear] [-max-pages N] <note_id>
  lovegw export [-db archive.db] [-out dir] <note_id>
  lovegw backfill [-config config.json] [-db archive.db] [-proxy] [-workers N] [-interval-ms MS] [-from ID] [-to ID] [-start-page N] [-refresh] [-limit N]
  lovegw personas [-db archive.db] [-out dir] flag [-patterns file]
  lovegw personas [-db archive.db] [-out dir] candidates [-limit N]
  lovegw personas [-db archive.db] [-out dir] [-in links.json] link
  lovegw personas [-db archive.db] [-out dir] [-min-score F] [-max-persona N] [-min-density F] cluster
  lovegw personas [-db archive.db] set <persona_id> <confirmed|rejected|pending>
  lovegw personas [-db archive.db] [-config config.json] [-proxy] [-workers N] [-limit N] avatars fetch
  lovegw personas [-db archive.db] [-max-dist D] [-generic-max N] avatars cluster
  lovegw personas [-db archive.db] [-min-chars N] [-dims N] stylometry build
  lovegw personas [-db archive.db] [-min-cosine F] [-top-k N] [-max-pairs N] stylometry cluster
  lovegw personas [-db archive.db] [-lex-min-tokens N] [-lex-dims N] lexis build
  lovegw personas [-db archive.db] [-top N] [-note id] [-in text.txt] [-lex-weight F] attribute [текст …]
  lovegw personas [-db archive.db] [-author p<id>|u<id>|user_id] [-lex-weight F] attribute   # пакет: все заметки личности
  lovegw personas [-db archive.db] -notes id,id,… [-author p<id>] [-lex-weight F] calibrate  # leave-one-out калибровка отпечатка
  lovegw personas [-db archive.db] -suspect p<id> [-in text.txt] [-note id] [-null N] verify   # «это он? да/нет» с калиброванным порогом
  lovegw personas [-db archive.db] [-out dir] [-top N] portrait <p<id>|u<id>|user_id>
  lovegw personas [-db archive.db] diag <id> <id> …
  lovegw personas [-db archive.db] [-out dir] [-ens-top-k N] [-handoff-days D] [-ens-floor F] ensemble
  lovegw personas [-db archive.db] [-topics file] [-min-hits N] [-min-notes N] [-evidence N] facts scan
  lovegw personas [-db archive.db] [-out dir] [-min-hits N] [-min-notes N] facts candidates
  lovegw personas [-db archive.db] [-out dir] [-in facts_llm.json] facts import
  lovegw personas [-db archive.db] [-rel-min-replies N] relations score
  lovegw personas [-db archive.db] [-out dir] [-cand-replies N] [-band-min N] [-band-top N] [-exchanges N] relations candidates
  lovegw personas [-db archive.db] [-out dir] [-in relations_llm.json] relations import
  lovegw personas [-db archive.db] [-out dir] [-report-top N] [-active-days N] [-html] report
  lovegw personas [-db archive.db] [-config config.json] [-tg-user id] [-active-days N] [-limit N] gender`)
}

// cmdRun — основной демон: зеркалирование ленты и комментариев в Telegram.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
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

	if !*seed {
		if ids, err := st.KnownNoteIDs(ctx); err == nil && len(ids) == 0 {
			log.Warn("БД пуста и запуск без -seed: текущие заметки с ленты будут опубликованы; " +
				"для перехода с Python сначала выполните import, затем запуск с -seed")
		}
	}
	return runDaemon(ctx, cfg, st, *seed, log)
}

// runDaemon собирает компоненты и крутит их под общим errgroup.
func runDaemon(ctx context.Context, cfg *config.Config, st *store.Store, seed bool, log *slog.Logger) error {
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
	if err != nil {
		return err
	}

	// ЛС-бот РюмкинЪ (опционален): без него мост не сможет уведомлять
	// пользователей о протухшей сессии, но зеркалирование работает.
	var dm *dmbot.Bot
	var notify bridge.Notify
	if cfg.DMBot.Token != "" {
		if dm, err = dmbot.New(cfg.DMBot.Token, st, client, tgClient, log); err != nil {
			return err
		}
		notify = dm.Notify
	}

	handler := bridge.New(st, client, notify,
		cfg.MirrorBot.ChannelID, cfg.MirrorBot.DiscussionChatID, log)
	tg, err := tgx.NewMirror(tgx.Params{
		Token:            cfg.MirrorBot.Token,
		ChannelID:        cfg.MirrorBot.ChannelID,
		DiscussionChatID: cfg.MirrorBot.DiscussionChatID,
		Signature:        cfg.Signature,
		BaseURL:          cfg.Site.BaseURL,
		HTTPClient:       tgClient,
	}, log, handler.Handle)
	if err != nil {
		return err
	}

	// Уведомления подписчиков шлём через РюмкинЪ (его пользователь точно
	// запускал, раз подписался) — постер-бот не смог бы написать в ЛС.
	var subNotify func(ctx context.Context, userID int64, n store.Note, c store.Comment)
	if dm != nil {
		subNotify = func(ctx context.Context, userID int64, n store.Note, c store.Comment) {
			link := tgx.DeepLink(cfg.MirrorBot.DiscussionChatID, c.TGMessageID, n.TGThreadID)
			dm.Notify(ctx, userID, "🔔 Новый комментарий по вашему ключевому слову:\n"+link)
		}
	}

	mir := mirror.New(st, client, tg, mirror.Config{
		NotesLimit:   cfg.NotesLimit,
		FeedInterval: time.Duration(cfg.FeedIntervalS) * time.Second,
		SeedFirst:    seed,
		AlertSend:    adminAlerter(tg, cfg.AdminTGUserID, log),
		SubNotify:    subNotify,
	}, log)

	log.Info("lovegw запущен", "seed", seed, "db", cfg.DBPath, "dm_bot", dm != nil)
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

// adminAlerter возвращает функцию алертов админу (nil, если admin id не задан).
func adminAlerter(tg *tgx.Mirror, adminID int64, log *slog.Logger) func(ctx context.Context, text string) {
	if adminID == 0 {
		return nil
	}
	return func(ctx context.Context, text string) {
		if err := tg.SendText(ctx, adminID, "⚠️ lovegw: "+text); err != nil {
			log.Warn("не удалось отправить алерт админу", "err", err)
		}
	}
}

// cmdImport переносит состояние старой Python-версии в SQLite.
// Импорт идемпотентен: повторный запуск ничего не дублирует.
func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	notesPath := fs.String("notes", "", "notes.json старой версии")
	sessionsPath := fs.String("sessions", "", "JSON с экспортированными куки пользователей старой версии")
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
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
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
