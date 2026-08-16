package main

// lovegw pulpit — ручной инструмент амвона. Публикует он только из демона;
// здесь можно посмотреть состояние и, главное, погонять промпт по вчерашним
// заметкам: реплика на сайте необратима, и текст должен устраивать ДО того, как
// уйдёт под живую заметку.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/llm"
	"lovegw/internal/love"
	"lovegw/internal/pulpit"
	"lovegw/internal/store"
	"lovegw/internal/textutil"
)

func cmdPulpit(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{"draft": true, "status": true})
	fs := flag.NewFlagSet("pulpit", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", "", "путь к БД (по умолчанию из конфига)")
	if err := fs.Parse(reorderArgs(rest, map[string]bool{"config": true, "db": true})); err != nil {
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
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "draft":
		if fs.NArg() < 1 {
			return fmt.Errorf("pulpit draft: не указан id заметки")
		}
		return pulpitDraft(ctx, cfg, st, fs.Args(), log)
	case "status":
		svc, err := newPulpit(cfg, st, nil, nil, log)
		if err != nil {
			return err
		}
		report, _, _ := svc.PulpitStatus(ctx)
		fmt.Println(report)
		return nil
	default:
		usage()
		return fmt.Errorf("pulpit: неизвестная подкоманда %q (draft|status)", sub)
	}
}

// pulpitDraft генерирует реплики по заметкам и печатает их. На сайт ничего не
// уходит: это стенд для правки промпта.
func pulpitDraft(ctx context.Context, cfg *config.Config, st *store.Store, noteIDs []string, log *slog.Logger) error {
	gen, err := llmClientFor(cfg, cfg.Pulpit.Model, cfg.Pulpit.Effort,
		time.Duration(cfg.Pulpit.GenerateTimeoutS)*time.Second)
	if err != nil {
		return err
	}
	svc, err := newPulpit(cfg, st, gen, nil, log)
	if err != nil {
		return err
	}
	for _, id := range noteIDs {
		n, err := svc.NoteByID(ctx, id)
		if err != nil {
			return fmt.Errorf("заметка %s: %w", id, err)
		}
		started := time.Now()
		sm, err := svc.Draft(ctx, n)
		if err != nil {
			fmt.Printf("=== заметка %s: реплика не получена: %v\n\n", id, err)
			continue
		}
		fmt.Printf("=== заметка %s (%s)\n%s\n\n", id, noteAuthor(n), textutil.Truncate(n.Text, 400))
		if sm.Skip {
			// Штатный исход, а не сбой: под настоящей бедой шутить нечем.
			fmt.Printf("--- шутить нельзя [%s, %s]\n\n",
				sm.Idea, time.Since(started).Round(time.Millisecond))
			continue
		}
		fmt.Printf("--- реплика [%s, деталь: %s, %s, %d знаков, %s]\n%s\n\n",
			sm.Form, sm.Hook, sm.Idea, len([]rune(sm.Text)),
			time.Since(started).Round(time.Millisecond), sm.Text)
	}
	return nil
}

func noteAuthor(n love.Note) string {
	if n.AuthorID == "" || n.AuthorID == "0" {
		return "анонимно"
	}
	return n.AuthorName
}

// newPulpit собирает службу амвона из конфига. Общий конструктор для демона и
// CLI: параметры службы должны быть одни и те же, иначе черновик врал бы.
func newPulpit(cfg *config.Config, st *store.Store, gen pulpit.JSONGenerator,
	alert func(ctx context.Context, text string), log *slog.Logger) (*pulpit.Service, error) {
	if cfg.Pulpit.OwnerProfileID == "" {
		return nil, errors.New("pulpit: не задан owner_profile_id — id анкеты владельца на сайте")
	}
	model := cfg.Pulpit.Model
	if model == "" {
		model = llm.DefaultModel
	}
	p := cfg.Pulpit
	return pulpit.New(st, pulpit.NewSite(cfg.Site.BaseURL, cfg.Site.UserAgent, log), gen, pulpit.Config{
		OwnerProfileID:   p.OwnerProfileID,
		BaseURL:          cfg.Site.BaseURL,
		Model:            model,
		FeedInterval:     time.Duration(p.FeedIntervalS) * time.Second,
		Freshness:        time.Duration(p.FreshnessMin) * time.Minute,
		MaxLatency:       time.Duration(p.MaxLatencyS) * time.Second,
		GenerateTimeout:  time.Duration(p.GenerateTimeoutS) * time.Second,
		MaxPerDay:        p.MaxPerDay,
		MinRunes:         p.MinRunes,
		MaxRunes:         p.MaxRunes,
		MaxLines:         p.MaxLines,
		AllowEmoji:       p.AllowEmoji,
		HistorySize:      p.HistorySize,
		FormCooldown:     p.FormCooldown,
		ReplyProbability: p.ReplyProbability,
		RepliesPerNote:   p.RepliesPerNote,
		RepliesPerDay:    p.RepliesPerDay,
		ReplyWindow:      time.Duration(p.ReplyWindowH) * time.Hour,
		FuseMisses:       p.FuseMisses,
		AlertSend:        alert,
	}, log), nil
}
