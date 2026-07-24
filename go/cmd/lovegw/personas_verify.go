package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"lovegw/internal/archive"
)

// verifyOpts — параметры действия verify.
type verifyOpts struct {
	suspect        string
	inPath         string
	noteID         int64
	notes          string // пул нескольких известных заметок как один образец
	lexWeight      float64
	nullN          int
	activeDays     int // рецент-фильтр фона: только анкеты активные за N сут (0 — все)
	minAuthorNotes int // жанровый фильтр фона: только писавшие ≥N заметок (0 — все)
}

// personasVerify — калиброванная проверка авторства: «этот текст написал
// подозреваемый? да/нет» с контролируемой долей ложных срабатываний. Порог —
// перцентиль нулевого распределения (z подозреваемого на чужих текстах).
func personasVerify(ctx context.Context, ar *archive.Store, args []string, opt verifyOpts) error {
	if opt.suspect == "" {
		return fmt.Errorf("verify: нужен -suspect p<id>|u<id>|user_id")
	}
	text, err := verifyInput(ctx, ar, args, opt)
	if err != nil {
		return err
	}
	res, err := ar.VerifyText(ctx, text, opt.suspect, opt.lexWeight, opt.nullN, opt.activeDays, opt.minAuthorNotes)
	if err != nil {
		return err
	}

	suspect := res.Suspect
	if res.SuspectBestName != "" {
		suspect = fmt.Sprintf("%s (%s(%d))", res.Suspect, res.SuspectBestName, res.SuspectBestID)
	}
	fmt.Fprintf(os.Stderr, "verify: подозреваемый %s, анкет %d; запрос: 3-грамм %d, слов %d; вес лексики %.2f\n",
		suspect, res.SuspectAccounts, res.QueryNgrams, res.QueryTokens, res.LexWeight)
	if res.ActiveDays > 0 || res.MinAuthorNotes > 0 {
		var parts []string
		if res.ActiveDays > 0 {
			parts = append(parts, fmt.Sprintf("активны за %d сут", res.ActiveDays))
		}
		if res.MinAuthorNotes > 0 {
			parts = append(parts, fmt.Sprintf("заметок ≥%d", res.MinAuthorNotes))
		}
		fmt.Fprintf(os.Stderr, "  фон сужен до правдоподобных кандидатов (%s): %d из %d анкет\n",
			strings.Join(parts, " + "), res.BgProfiles, res.StyleProfiles)
	}
	if res.QueryNgrams < shortQueryNgrams {
		fmt.Fprintf(os.Stderr, "  ⚠ короткий текст (<%d 3-грамм): вердикт ненадёжен\n", shortQueryNgrams)
	}
	lexPart := "лексики нет"
	if res.HasLex {
		lexPart = fmt.Sprintf("лексика z=%.1f", res.LexZ)
	}
	fmt.Fprintf(os.Stderr, "  z подозреваемого на тексте: %.2f (стиль z=%.1f, %s)\n", res.Z, res.StyleZ, lexPart)

	if res.NullN < 10 {
		fmt.Fprintf(os.Stderr, "  фон мал (%d текстов) — вердикт недостоверен, увеличь -null\n", res.NullN)
		return nil
	}
	fmt.Fprintf(os.Stderr, "  фон (%d чужих текстов): z ~ %.2f ± %.2f, порог FPR5%%=%.2f, FPR1%%=%.2f, макс=%.2f\n",
		res.NullN, res.NullMean, res.NullStd, res.NullP95, res.NullP99, res.NullMax)
	fmt.Fprintf(os.Stderr, "  на подозреваемого текст похож сильнее, чем %.1f%% случайных текстов\n", res.Percentile*100)

	fmt.Fprintf(os.Stderr, "\n  ВЕРДИКТ при FPR 5%%: %s\n", verdict(res.Z, res.NullP95))
	fmt.Fprintf(os.Stderr, "  ВЕРДИКТ при FPR 1%%: %s\n", verdict(res.Z, res.NullP99))
	fmt.Fprintln(os.Stderr, "  (FPR — доля чужих текстов, ошибочно принятых за «его»; порог — перцентиль фона)")
	return nil
}

// verifyInput — текст для проверки: пул заметок (-notes), иначе -in/-note/аргумент/stdin.
func verifyInput(ctx context.Context, ar *archive.Store, args []string, opt verifyOpts) (string, error) {
	if opt.notes != "" {
		ids, err := parseIDList(opt.notes)
		if err != nil {
			return "", err
		}
		texts, err := ar.NoteTexts(ctx, ids)
		if err != nil {
			return "", err
		}
		if len(texts) == 0 {
			return "", fmt.Errorf("verify: заметки не найдены: %s", opt.notes)
		}
		return strings.Join(texts, "\n"), nil
	}
	text, _, err := attributeInput(ctx, ar, args, attrOpts{inPath: opt.inPath, noteID: opt.noteID})
	return text, err
}

// verdict — да/нет по порогу с запасом над ним.
func verdict(z, threshold float64) string {
	if z >= threshold {
		return fmt.Sprintf("ДА — это он (z=%.2f ≥ порога %.2f, запас +%.2f)", z, threshold, z-threshold)
	}
	return fmt.Sprintf("НЕТ — не подтверждается (z=%.2f < порога %.2f, не хватает %.2f)", z, threshold, threshold-z)
}
