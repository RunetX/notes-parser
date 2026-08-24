package main

// lovegw morning — ручной инструмент утренней заметки. В бою публикует её
// демон; здесь можно посмотреть, что отдали календари, погонять промпт и
// догнать пропущенный день руками.
//
// Стенд нужен ровно потому, что заметка на сайте необратима: текст должен
// устраивать ДО того, как окажется в ленте, в канале и в чужих ЛС.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/holidays"
	"lovegw/internal/llm"
	"lovegw/internal/morning"
	"lovegw/internal/store"
)

func cmdMorning(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{
		"sources": true, "draft": true, "post": true, "status": true,
	})
	fs := flag.NewFlagSet("morning", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", "", "путь к БД (по умолчанию из конфига)")
	day := fs.String("day", "", "день в формате 2006-01-02 (по умолчанию сегодняшний слот)")
	force := fs.Bool("force", false, "post: публиковать, даже если утро уже сказал кто-то другой")
	if err := fs.Parse(reorderArgs(rest, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	log := newLogger(cfg.LogLevel)

	if sub == "sources" {
		// Единственная подкоманда, которой не нужна ни БД, ни сессия: она
		// спрашивает только чужие календари.
		return morningSources(ctx, cfg, *day, log)
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "draft":
		return morningDraft(ctx, cfg, st, *day, log)
	case "post":
		gen, err := morningGenerator(cfg)
		if err != nil {
			return err
		}
		svc, err := newMorning(cfg, st, gen, nil, log)
		if err != nil {
			return err
		}
		summary, err := svc.PublishToday(ctx, *force)
		if err != nil {
			return err
		}
		fmt.Println(summary)
		return nil
	case "status":
		svc, err := newMorning(cfg, st, nil, nil, log)
		if err != nil {
			return err
		}
		report, _, _ := svc.Status(ctx)
		fmt.Println(report)
		return nil
	default:
		usage()
		return fmt.Errorf("morning: неизвестная подкоманда %q (sources|draft|post|status)", sub)
	}
}

// morningSources печатает поводы дня так, как их видит служба: что осталось
// после слияния и фильтра и почему ушло остальное. Это стенд ПАРСЕРОВ и
// стоп-списка — правится он по такому выводу, а не на глаз.
func morningSources(ctx context.Context, cfg *config.Config, day string, log *slog.Logger) error {
	slot, err := morningDay(cfg, day)
	if err != nil {
		return err
	}
	srcs, err := holidays.Build(cfg.Morning.Sources, time.Duration(cfg.Morning.SourceTimeoutS)*time.Second)
	if err != nil {
		return err
	}
	all, err := holidays.Collect(ctx, srcs, slot, log)
	if err != nil {
		return err
	}
	fmt.Printf("=== %s: поводов всего %d\n\n", slot.Format(morning.DayLayout), len(all))
	kept := 0
	for _, o := range all {
		reason := holidays.Reject(o)
		mark := "+"
		if reason != "" {
			mark = "-"
		} else {
			kept++
		}
		fmt.Printf("%s [%v/%v] %s", mark, o.Kind, o.Scope, o.Title)
		if o.Year > 0 {
			fmt.Printf(" (%d)", o.Year)
		}
		fmt.Printf("  ← %v", o.Sources)
		if reason != "" {
			fmt.Printf("  ✗ %s", reason)
		}
		fmt.Println()
	}
	fmt.Printf("\nгодных: %d, в промпт пойдут первые %d\n", kept, cfg.Morning.MaxFacts)
	return nil
}

// morningDraft пишет заметку и печатает её. На сайт ничего не уходит.
func morningDraft(ctx context.Context, cfg *config.Config, st *store.Store, day string, log *slog.Logger) error {
	slot, err := morningDay(cfg, day)
	if err != nil {
		return err
	}
	gen, err := morningGenerator(cfg)
	if err != nil {
		return err
	}
	svc, err := newMorning(cfg, st, gen, nil, log)
	if err != nil {
		return err
	}
	started := time.Now()
	d, facts, err := svc.Draft(ctx, slot)
	if err != nil {
		return fmt.Errorf("черновик %s: %w", slot.Format(morning.DayLayout), err)
	}
	fmt.Printf("=== %s (поводов %d, %s)\n", slot.Format(morning.DayLayout), len(facts),
		time.Since(started).Round(time.Millisecond))
	if d.Skip {
		// Штатный исход, а не сбой: в поводах дня не нашлось светлого.
		fmt.Printf("--- писать нечего [%s]\n", d.Idea)
		return nil
	}
	fmt.Printf("--- %d знаков, поводы %v [%s]\n\n%s\n", len([]rune(d.Text)), d.Used, d.Idea, d.Text)
	return nil
}

// morningDay — слот дня: заданного флагом или последнего наступившего.
func morningDay(cfg *config.Config, day string) (time.Time, error) {
	loc, err := morningLoc(cfg)
	if err != nil {
		return time.Time{}, err
	}
	if day == "" {
		_, slot := morning.SlotFor(time.Now(), loc, cfg.Morning.Hour)
		return slot, nil
	}
	t, err := time.ParseInLocation(morning.DayLayout, day, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("-day %q: ожидалась дата вида 2026-08-24", day)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), cfg.Morning.Hour, 0, 0, 0, loc), nil
}

func morningLoc(cfg *config.Config) (*time.Location, error) {
	name := cfg.Morning.TZ
	if name == "" {
		name = morning.DefaultTZ
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("morning.tz %q: %w", name, err)
	}
	return loc, nil
}

func morningGenerator(cfg *config.Config) (morning.JSONGenerator, error) {
	return llmClientFor(cfg, cfg.Morning.Model, cfg.Morning.Effort,
		time.Duration(cfg.Morning.GenerateTimeoutS)*time.Second)
}

// newMorning собирает службу из конфига. Общий конструктор для демона и CLI:
// параметры должны быть одни и те же, иначе черновик врал бы.
func newMorning(cfg *config.Config, st *store.Store, gen morning.JSONGenerator,
	alert func(ctx context.Context, text string), log *slog.Logger) (*morning.Service, error) {
	author := cfg.Morning.AuthorProfileID
	if author == "" {
		// Тот же человек, что подписывает амвон и дайджест, и та же его
		// сессия: заводить вторую настройку под одну анкету незачем.
		author = cfg.Pulpit.OwnerProfileID
	}
	if author == "" {
		return nil, errors.New("morning: не задан автор заметки — morning.author_profile_id (или pulpit.owner_profile_id)")
	}
	loc, err := morningLoc(cfg)
	if err != nil {
		return nil, err
	}
	srcs, err := holidays.Build(cfg.Morning.Sources, time.Duration(cfg.Morning.SourceTimeoutS)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("morning: %w", err)
	}
	model := cfg.Morning.Model
	if model == "" {
		model = llm.DefaultModel
	}
	m := cfg.Morning
	return morning.New(st, morning.NewSite(cfg.Site.BaseURL, cfg.Site.UserAgent, log), gen, morning.Config{
		OwnerProfileID:  author,
		BaseURL:         cfg.Site.BaseURL,
		Model:           model,
		Loc:             loc,
		Hour:            m.Hour,
		Grace:           time.Duration(m.GraceH) * time.Hour,
		GenerateTimeout: time.Duration(m.GenerateTimeoutS) * time.Second,
		MinRunes:        m.MinRunes,
		MaxRunes:        m.MaxRunes,
		MaxLines:        m.MaxLines,
		HistorySize:     m.HistorySize,
		MaxFacts:        m.MaxFacts,
		Sources:         srcs,
		FuseMisses:      m.FuseMisses,
		AlertSend:       alert,
	}, log), nil
}
