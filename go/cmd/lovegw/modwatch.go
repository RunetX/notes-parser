package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/modwatch"
)

// defaultModwatchPath — БД наблюдателя, отдельно и от боевой, и от архива.
const (
	defaultModwatchPath = "data/modwatch.db"
	modwatchDBUsage     = "путь к modwatch.db"
	momentUsage         = "момент: 2026-08-05, «2026-08-05 14:30» или длительность назад (72h)"
)

// cmdModwatch — наблюдение за действиями модерации: сборщик пишет моменты
// удалений/одобрений, отчёт сверяет их с присутствием людей на площадке.
func cmdModwatch(ctx context.Context, args []string) error {
	// Подкоманда может стоять и после флагов (`modwatch -db … watch`), поэтому
	// ищем её по имени, а не по позиции.
	sub, rest := "", make([]string, 0, len(args))
	for _, a := range args {
		if sub == "" && modwatchSubcommands[a] {
			sub = a
			continue
		}
		rest = append(rest, a)
	}
	switch sub {
	case "watch":
		return modwatchWatch(ctx, rest)
	case "report":
		return modwatchReport(ctx, rest)
	case "events":
		return modwatchEvents(ctx, rest)
	case "status":
		return modwatchStatus(ctx, rest)
	default:
		usage()
		return fmt.Errorf("modwatch: нужна подкоманда (watch|report|events|status)")
	}
}

var modwatchSubcommands = map[string]bool{"watch": true, "report": true, "events": true, "status": true}

// modwatchSite — адаптер *love.Client под modwatch.Site.
type modwatchSite struct {
	c       *love.Client
	baseURL string
}

func (m modwatchSite) Feed(ctx context.Context) ([]love.Note, error) {
	raw, err := m.c.RawNotesPage(ctx, 1)
	if err != nil {
		return nil, err
	}
	return love.ParseNotes(bytes.NewReader(raw))
}

func (m modwatchSite) Thread(ctx context.Context, noteID string, page int) ([]love.Comment, *love.Note, error) {
	raw, err := m.c.RawCommentsView(ctx, noteID, page, love.ViewLinear)
	if err != nil {
		return nil, nil, err
	}
	comments, err := love.ParseComments(bytes.NewReader(raw), m.baseURL)
	if err != nil {
		return nil, nil, err
	}
	var header *love.Note
	if n, err := love.ParseNoteFromCommentsPage(bytes.NewReader(raw), m.baseURL); err == nil {
		header = &n
	}
	return comments, header, nil
}

func modwatchWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch watch", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	feedInterval := fs.Duration("feed-interval", modwatch.DefaultFeedInterval, "период опроса ленты")
	threadInterval := fs.Duration("thread-interval", modwatch.DefaultThreadInterval, "минимальный период опроса одного треда")
	window := fs.Duration("window", modwatch.DefaultWindow, "сколько заметка считается активной после первой встречи")
	depth := fs.Duration("depth", modwatch.DefaultDepth, "насколько вглубь треда листать по времени комментариев")
	maxThreads := fs.Int("max-threads", modwatch.DefaultMaxThreads, "сколько тредов опрашивать за тик")
	maxPages := fs.Int("pages", modwatch.DefaultMaxPages, "предел страниц комментариев на тред")
	once := fs.Bool("once", false, "один проход и выход (проверка настройки)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "feed-interval": true, "thread-interval": true,
		"window": true, "depth": true, "max-threads": true, "pages": true,
	})); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	w := &modwatch.Watcher{
		Site:           modwatchSite{c: client, baseURL: cfg.Site.BaseURL},
		Store:          store,
		Log:            log,
		FeedInterval:   *feedInterval,
		ThreadInterval: *threadInterval,
		Window:         *window,
		Depth:          *depth,
		MaxThreads:     *maxThreads,
		MaxPages:       *maxPages,
	}
	if *once {
		if err := w.Poll(ctx); err != nil {
			return err
		}
		return printCounts(ctx, store)
	}
	log.Info("наблюдение запущено", "db", *dbPath, "лента", feedInterval.String(), "тред", threadInterval.String())
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return printCounts(ctx, store)
}

func modwatchStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch status", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	if err := fs.Parse(reorderArgs(args, map[string]bool{"db": true})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return printCounts(ctx, store)
}

func printCounts(ctx context.Context, store *modwatch.Store) error {
	c, err := store.Counts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("наблюдение: %s — %s\n", fmtTime(c.FirstSeen), fmtTime(c.LastSeen))
	fmt.Printf("заметок %d (исчезло %d), комментариев %d (исчезло %d), событий %d, анкет %d\n",
		c.Notes, c.NotesGone, c.Comments, c.CommentsGone, c.Events, c.Users)
	return nil
}

func modwatchEvents(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch events", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	until := fs.String("until", "", "по какой момент — "+momentUsage)
	kinds := fs.String("kind", "", "виды событий через запятую (по умолчанию — модераторские)")
	limit := fs.Int("limit", 200, "сколько строк показать")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "since": true, "until": true, "kind": true, "limit": true,
	})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	from, err := parseMoment(*since)
	if err != nil {
		return err
	}
	to, err := parseMoment(*until)
	if err != nil {
		return err
	}
	events, err := store.Events(ctx, from, to, splitKinds(*kinds), *limit)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "когда (±)\tвид\tзаметка\tобъект\tчто")
	for _, e := range events {
		fmt.Fprintf(tw, "%s (±%s)\t%s\t%d\t%d\t%s\n",
			fmtTime(e.DetectedAt), e.DetectedAt.Sub(e.PrevSeen).Round(time.Second),
			e.Kind, e.NoteID, e.RefID, e.Details)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nвсего событий: %d\n", len(events))
	return nil
}

func modwatchReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch report", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	until := fs.String("until", "", "по какой момент — "+momentUsage)
	kinds := fs.String("kind", strings.Join(moderationKinds, ","), "виды событий через запятую")
	window := fs.Duration("presence-window", modwatch.DefaultPresenceWindow, "расширение окна события в обе стороны")
	controls := fs.Int("controls", modwatch.DefaultControls, "контрольных окон на событие")
	seed := fs.Int64("seed", 1, "зерно выбора контрольных окон")
	minHits := fs.Int("min-hits", 3, "не показывать тех, кто совпал реже")
	top := fs.Int("top", 25, "сколько строк показать")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "since": true, "until": true, "kind": true, "presence-window": true,
		"controls": true, "seed": true, "min-hits": true, "top": true,
	})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	from, err := parseMoment(*since)
	if err != nil {
		return err
	}
	to, err := parseMoment(*until)
	if err != nil {
		return err
	}
	rep, err := store.Analyze(ctx, modwatch.ReportOptions{
		Since: from, Until: to, Kinds: splitKinds(*kinds),
		Window: *window, Controls: *controls, Seed: *seed,
		MinHits: *minHits, Top: *top,
	})
	if err != nil {
		return err
	}
	if rep.Events == 0 {
		fmt.Println("событий для расчёта нет: либо наблюдение слишком короткое, либо не нашлось контрольных окон (нужно хотя бы двое суток наблюдения)")
		if rep.EventsSkipped > 0 {
			fmt.Printf("событий отброшено без контроля: %d\n", rep.EventsSkipped)
		}
		return nil
	}
	fmt.Printf("наблюдение %s — %s, событий %d (без контроля %d), контрольных окон на событие %d\n",
		fmtTime(rep.From), fmtTime(rep.To), rep.Events, rep.EventsSkipped, rep.Controls)
	fmt.Printf("людей в окне события в среднем %.1f, в контрольном %.1f\n\n", rep.AvgPresent, rep.AvgPresentCtrl)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "z\tсовпал\tожидалось\tво сколько раз\tреплик всего\tанкета\tник")
	for _, r := range rep.Rows {
		fmt.Fprintf(tw, "%+.2f\t%d\t%.1f\t%.2f\t%d\tu%d\t%s\n",
			r.Z, r.Hits, r.Expected, r.Lift, r.Comments, r.UserID, r.Name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println(`
Как читать: «совпал» — в скольких окнах действий человек писал, «ожидалось» —
столько же окон, но сдвинутых на целые сутки (тот же час, другой день). z выше
3 при десятках событий — систематическое присутствие, а не совпадение; одиночные
всплески при малом числе событий не значат ничего.`)
	return nil
}

// moderationKinds — виды событий, которые по умолчанию считаются действием
// модерации. note_published и nick_changed сюда не входят: первое случается и
// без человека (заметка без картинки публикуется сама), второе — редкое.
var moderationKinds = []string{
	modwatch.KindNoteGone, modwatch.KindCommentGone,
	modwatch.KindImageAdded, modwatch.KindCommentsClosed,
}

func splitKinds(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// parseMoment принимает либо дату/время, либо длительность «назад».
func parseMoment(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("не понял момент %q: нужен 2026-08-05, 2026-08-05 14:30 или длительность назад (72h)", s)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}
