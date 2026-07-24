package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"lovegw/internal/archive"
)

// calibOpts — параметры действия calibrate.
type calibOpts struct {
	notes     string // id заметок через запятую
	author    string // личность для оценки разрыва (опционально)
	lexWeight float64
	top       int
}

// personasCalibrate — калибровка отпечатка автора по набору заметок: честная
// leave-one-out проверка обобщения (предсказывает ли схема НОВЫЙ текст, а не
// подгоняется под обучающий) + объединённый голос + разрыв с именными анкетами.
func personasCalibrate(ctx context.Context, ar *archive.Store, opt calibOpts) error {
	ids, err := parseIDList(opt.notes)
	if err != nil {
		return err
	}
	if len(ids) < 2 {
		return fmt.Errorf("calibrate: нужно ≥2 заметок в -notes (leave-one-out); дано %d", len(ids))
	}
	cal, err := ar.CalibrateNotes(ctx, ids, opt.author, opt.lexWeight, opt.top)
	if err != nil {
		return err
	}

	lex := strconv.Itoa(cal.LexProfiles)
	if cal.LexProfiles == 0 {
		lex = "не построена"
	}
	fmt.Fprintf(os.Stderr, "calibrate: заметок %d (знаков %d); стиль-профилей %d, лексика %s; вес лексики %.2f\n",
		cal.Notes, cal.Chars, cal.StyleProfiles, lex, cal.LexWeight)
	if len(cal.Loo) == 0 {
		fmt.Fprintln(os.Stderr, "  нет пригодных заметок (пустой текст или их нет в архиве)")
		return nil
	}

	hasID := cal.Identity != ""
	fmt.Fprintln(os.Stderr, "\nLEAVE-ONE-OUT (эталон и ранг считаются на ОТЛОЖЕННОЙ заметке — честная проверка на невиданном тексте):")
	if hasID {
		fmt.Fprintf(os.Stderr, "  %-8s %-6s %-6s  %-12s  %s\n", "заметка", "эт.ранг", "score", "твоя анкета", "обошёл эталон (если не #1)")
	} else {
		fmt.Fprintf(os.Stderr, "  %-8s %-6s %-6s  %s\n", "заметка", "эт.ранг", "score", "обошёл эталон (если не #1)")
	}
	loo := append([]archive.LooResult(nil), cal.Loo...)
	sort.Slice(loo, func(i, j int) bool { return loo[i].Rank < loo[j].Rank })
	for _, l := range loo {
		beat := "— (эталон #1)"
		if l.BeatByID != 0 {
			beat = fmt.Sprintf("%s(%d)", nameOr(l.BeatByName), l.BeatByID)
		}
		if hasID {
			idcol := fmt.Sprintf("#%d %s", l.IdRank, nameOr(l.IdBestName))
			fmt.Fprintf(os.Stderr, "  %-8d %-6d %-6.1f  %-12s  %s\n", l.NoteID, l.Rank, l.RefScore, idcol, beat)
		} else {
			fmt.Fprintf(os.Stderr, "  %-8d %-6d %-6.1f  %s\n", l.NoteID, l.Rank, l.RefScore, beat)
		}
	}
	fmt.Fprintf(os.Stderr, "  эталон-из-остальных: #1 в %d/%d, топ-5 в %d/%d, медиана ранга %d\n",
		cal.LooTop1, len(cal.Loo), cal.LooTop5, len(cal.Loo), cal.LooMedianRank)
	if hasID {
		fmt.Fprintf(os.Stderr, "  твой существующий профиль %s: топ-10 в %d/%d, медиана ранга %d (out-of-sample — заметки не в профиле)\n",
			cal.Identity, cal.IdTop10, len(cal.Loo), cal.IdMedianRank)
	}
	fmt.Fprintf(os.Stderr, "  → %s\n", looVerdict(cal))

	if len(cal.Pooled) > 0 {
		fmt.Fprintln(os.Stderr, "\nОБЪЕДИНЁННЫЙ ГОЛОС (все заметки в один отпечаток) — ближайшие авторы:")
		for _, c := range cal.Pooled {
			fmt.Fprintf(os.Stderr, "  %2d. %s  %s\n", c.Rank, attrScore(c, cal.LexProfiles > 0), attrLabel(c))
		}
	}

	if cal.Identity != "" {
		fmt.Fprintf(os.Stderr, "\nРАЗРЫВ С ЛИЧНОСТЬЮ %s: лучшая именная анкета — место %d из %d",
			cal.Identity, cal.IdentityRank, cal.StyleProfiles)
		if cal.IdentityBestID != 0 {
			fmt.Fprintf(os.Stderr, " (%s(%d), score %.1f)", nameOr(cal.IdentityBestName), cal.IdentityBestID, cal.IdentityBestScore)
		}
		fmt.Fprintf(os.Stderr, "\n  %s\n", identityGapVerdict(cal))
	}
	return nil
}

// looVerdict — словесный вывод. Приоритет у out-of-sample ранга существующего
// профиля (он и отвечает на вопрос «предсказывает ли новый текст»); ранг
// свежего эталона-из-остальных вторичен (мало образцов → он слаб сам по себе).
func looVerdict(cal archive.Calibration) string {
	n := len(cal.Loo)
	if cal.Identity != "" {
		switch {
		case cal.IdTop10*2 >= n:
			return "ПРЕДСКАЗЫВАЕТ новый текст: на отложенных заметках твой существующий профиль стабильно в топе (out-of-sample) — метод работает, не подгонка; ограничитель — объём текста в запросе, а не схема"
		case cal.IdMedianRank <= 200:
			return "сигнал есть, но шумный: твой профиль на невиданных заметках держится в верхних процентах, но не в самом топе — нужно больше текста в запросе (пул нескольких абзацев) для уверенности"
		default:
			return "на коротких одиночных заметках профиль теряется среди 8900 авторов; при этом ПУЛ всех заметок обычно узнаётся (см. ниже) — вывод: решает объём текста, а одиночный абзац слишком шумный"
		}
	}
	switch {
	case cal.LooTop1*2 >= n:
		return "почерк ПРЕДСКАЗУЕМ: эталон-из-остальных стабильно первый на отложенном тексте"
	case cal.LooTop5*2 >= n:
		return "почерк умеренно устойчив: эталон обычно в топе, но не всегда первый"
	default:
		return "эталон из немногих коротких образцов слаб; добавь текста/образцов или сравни с существующим профилем (-author)"
	}
}

func identityGapVerdict(cal archive.Calibration) string {
	switch {
	case cal.IdentityRank == 0:
		return "у личности нет стиль-профилей для сравнения"
	case cal.IdentityRank <= 20:
		return "именное письмо БЛИЗКО к анонимному — разрыв невелик, дело в размытии/пороге, а не в разной манере"
	case cal.IdentityRank <= 200:
		return "именное письмо заметно отличается от анонимного — есть смысл вести анонимный эталон отдельно"
	default:
		return "анонимная манера СИЛЬНО отличается от именной — писать анонимно = другой регистр; отдельный эталон обязателен"
	}
}

// parseIDList разбирает "1,2,3" в список id (пробелы и пустые игнорируются).
func parseIDList(s string) ([]int64, error) {
	var out []int64
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		id, err := strconv.ParseInt(tok, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("calibrate: id не число: %q", tok)
		}
		out = append(out, id)
	}
	return out, nil
}
