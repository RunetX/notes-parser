package main

import (
	"context"
	"fmt"
	"os"

	"lovegw/internal/archive"
)

// anonOpts — параметры `personas anon`.
type anonOpts struct {
	suspect        string
	genre          string
	lexWeight      float64
	activeDays     int
	minAuthorNotes int
	from, to       string
	minText        int
	top            int
	nullN          int
}

// personasAnon ищет среди анонимных заметок похожие на почерк подозреваемого.
func personasAnon(ctx context.Context, ar *archive.Store, opt anonOpts) error {
	if opt.suspect == "" {
		return fmt.Errorf("anon: нужен -suspect p<id>|u<id>|user_id")
	}
	res, err := ar.ScanAnonymous(ctx, archive.AnonScanParams{
		Suspect: opt.suspect, Genre: opt.genre, LexWeight: opt.lexWeight,
		ActiveDays: opt.activeDays, MinAuthorNotes: opt.minAuthorNotes,
		From: opt.from, To: opt.to, MinChars: opt.minText, Top: opt.top, NullN: opt.nullN,
	})
	if err != nil {
		return err
	}

	window := "весь архив"
	if opt.from != "" || opt.to != "" {
		window = fmt.Sprintf("%s … %s", orDash(opt.from), orDash(opt.to))
	}
	fmt.Fprintf(os.Stderr, "anon: подозреваемый %s (анкет %d); эталон %s (%s); окно %s; порог длины %d знаков\n",
		res.Identity, res.Accounts, res.Genre, genreLabel(res.Genre), window, opt.minText)
	fmt.Fprintf(os.Stderr, "  отскорено анонимок %d (коротких пропущено %d); фон %d профилей из %d\n",
		res.Scanned, res.SkippedShort, res.BgProfiles, res.StyleProfiles)
	if res.NullN < 10 {
		fmt.Fprintf(os.Stderr, "  фон мал (%d текстов) — пороги недостоверны, увеличь -null\n", res.NullN)
	}
	fmt.Fprintf(os.Stderr, "  нулевое распределение (%d чужих заметок): z ~ %.2f ± %.2f, FPR5%%=%.2f, FPR1%%=%.2f, макс=%.2f\n",
		res.NullN, res.NullMean, res.NullStd, res.NullP95, res.NullP99, res.NullMax)
	fmt.Fprintf(os.Stderr, "  выше FPR5%%: %d (ложных ожидается ~%.0f) · выше FPR1%%: %d (~%.0f) · выше максимума фона: %d\n",
		res.AboveP95, res.ExpectedFP95, res.AboveP99, res.ExpectedFP99, res.AboveMax)
	fmt.Fprintln(os.Stderr, "  ⚠ на таком объёме верхушка списка — не «его заметки», а кандидаты: ожидаемые ложные считаны выше")

	if len(res.Hits) == 0 {
		fmt.Fprintln(os.Stderr, "  подходящих анонимок нет")
		return nil
	}
	fmt.Fprintf(os.Stderr, "\n  %-8s %-10s %-6s %-6s %-6s %-5s %-16s %s\n",
		"note", "дата", "z", "стиль", "лекс", "знак", "анкета", "текст")
	for _, h := range res.Hits {
		lex := "—"
		if h.HasLex {
			lex = fmt.Sprintf("%.1f", h.LexZ)
		}
		fmt.Fprintf(os.Stderr, "  %-8d %-10s %-6.2f %-6.1f %-6s %-5d %-16s %s\n",
			h.NoteID, dateOnly(h.PublishedAt), h.Z, h.StyleZ, lex, h.Chars,
			trunc(fmt.Sprintf("%s(%d)", nameOr(h.BestName), h.BestID), 16), h.Snippet)
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func dateOnly(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}
