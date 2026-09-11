package main

// lovegw friday — ручной инструмент пятничной рубрики (эпик J).
//
// В бою вечер ведёт демон; здесь можно посмотреть, ЧТО он соберёт, — и это не
// удобство, а необходимость: вопрос, опубликованный в тред, читают сразу, а
// отобранный автоматом материал бывает и скучным, и неудачным. Стенд тот же, что
// у утренней заметки и амвона, и по той же причине.
//
// `draft` печатает выпуск и НЕ ПУБЛИКУЕТ ничего; выпуск детерминирован неделей,
// поэтому напечатанное и есть то, что выйдет.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/platform"
	"lovegw/internal/platfriday"
	"lovegw/internal/web"
)

func cmdFriday(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{
		"draft": true, "publish": true, "status": true,
	})
	fs := flag.NewFlagSet("friday", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	day := fs.String("day", "", "день выпуска в формате 2006-01-02 (по умолчанию сегодня)")
	if err := fs.Parse(reorderArgs(rest, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.Platform.Enabled || cfg.Platform.DSN == "" {
		return errors.New("площадка не настроена: рубрике неоткуда брать архив")
	}
	when := time.Now()
	if *day != "" {
		when, err = time.ParseInLocation("2006-01-02", *day, time.Local)
		if err != nil {
			return fmt.Errorf("день выпуска: %w", err)
		}
		// Час слота, чтобы «сегодня» и «-day» вели себя одинаково.
		when = when.Add(time.Duration(cfg.Friday.Hour) * time.Hour)
	}

	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	src := platfriday.NewPG(p.Pool())
	fcfg := fridayConfig(cfg, newLogger(cfg.LogLevel))

	switch sub {
	case "publish":
		fcfg.Live = true
		res, err := platfriday.Step(ctx, p, src, fcfg, when)
		if err != nil {
			return err
		}
		printFridayResult(res)
		return nil
	case "status":
		return fridayStatus(ctx, p, when)
	default:
		return fridayDraft(ctx, src, fcfg, when)
	}
}

// fridayConfig — настройки рубрики из конфига.
func fridayConfig(cfg *config.Config, log *slog.Logger) platfriday.Config {
	return platfriday.Config{
		Weekday:   time.Weekday(cfg.Friday.Weekday),
		Hour:      cfg.Friday.Hour,
		Questions: cfg.Friday.Questions,
		Gap:       time.Duration(cfg.Friday.GapMinutes) * time.Minute,
		SiteNick:  web.SiteName,
		Live:      cfg.Friday.Mode == "live",
		Log:       log,
	}
}

// fridayDraft печатает выпуск, ничего не публикуя.
func fridayDraft(ctx context.Context, src platfriday.Source, cfg platfriday.Config, when time.Time) error {
	issue, err := platfriday.Build(ctx, src, when, cfg.Questions)
	if err != nil {
		return err
	}
	fmt.Printf("ВЫПУСК %s — %d вопрос(ов)\n\n", issue.Week, len(issue.Questions))
	fmt.Println("── ВСТУПИТЕЛЬНАЯ ЗАМЕТКА ──")
	fmt.Println(issue.Intro)
	for i, q := range issue.Questions {
		fmt.Printf("\n── ВОПРОС %d (%s) ──\n%s\n", i+1, q.Kind, q.Body)
		for j, o := range q.Quiz.Options {
			mark := "  "
			if j == q.Quiz.Right {
				mark = "▸ "
			}
			fmt.Printf("   %s%s\n", mark, o)
		}
		fmt.Printf("   разгадка: %s\n", q.Quiz.Reveal)
		fmt.Printf("   источник: /n/%d\n", q.Quiz.SourceNote)
	}
	fmt.Println("\nНичего не опубликовано: это черновик. Публикует демон либо `friday publish`.")
	return nil
}

// fridayStatus — что стоит на площадке сейчас.
func fridayStatus(ctx context.Context, p *platform.Platform, when time.Time) error {
	week := platfriday.WeekOf(when)
	noteID, err := p.FridayIssue(ctx, week)
	if err != nil {
		return err
	}
	if noteID == 0 {
		fmt.Printf("неделя %s: выпуска нет\n", week)
		return nil
	}
	stats, err := p.QuizStats(ctx, noteID)
	if err != nil {
		return err
	}
	fmt.Printf("неделя %s: заметка /n/%d, вопросов %d\n", week, noteID, len(stats))
	for i, s := range stats {
		share := "—"
		if s.Votes > 0 {
			share = fmt.Sprintf("%d%%", s.Right*100/s.Votes)
		}
		fmt.Printf("  %d. задан %s · ответили %d · угадали %s · источник /n/%d\n",
			i+1, s.Asked.Local().Format("15:04"), s.Votes, share, s.SourceNote)
	}
	return nil
}

func printFridayResult(res platfriday.Result) {
	var b strings.Builder
	fmt.Fprintf(&b, "неделя %s", res.Week)
	if res.NoteID != 0 {
		fmt.Fprintf(&b, ", заметка /n/%d", res.NoteID)
	}
	fmt.Fprintf(&b, ", опубликовано за проход: %d, вопросов всего: %d", res.Posted, res.Asked)
	if res.Skipped != "" {
		fmt.Fprintf(&b, " (%s)", res.Skipped)
	}
	fmt.Println(b.String())
}
