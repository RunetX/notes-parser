package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"lovegw/internal/archive"
)

// attrOpts — параметры действия attribute (из флагов cmdPersonas).
type attrOpts struct {
	top       int
	inPath    string
	noteID    int64
	lexWeight float64
	author    string // непусто — пакетный режим по заметкам личности
}

// shortQueryNgrams — ниже этого объёма запроса ранжирование заметно шумит.
const shortQueryNgrams = 300

// personasAttribute — скоринг авторства: текст анонимной заметки → топ анкет,
// чьё письмо ближе всего. Два сигнала: стилометрия (символьные 3-граммы) и
// лексика (TF-IDF по словам, если построена `personas lexis build`); скор —
// комбинированный Z с весом -lex-weight. Источник текста по приоритету: -note
// <id> (заметка из архива), -in <file>, позиционные аргументы, stdin. Заметка с
// известным автором — режим валидации: печатается, на каком месте настоящий автор.
func personasAttribute(ctx context.Context, ar *archive.Store, args []string, opt attrOpts) error {
	if opt.author != "" {
		return attributeIdentityNotes(ctx, ar, opt)
	}
	text, wantID, err := attributeInput(ctx, ar, args, opt)
	if err != nil {
		return err
	}
	at, err := ar.AttributeText(ctx, text, opt.top, wantID, opt.lexWeight)
	if err != nil {
		return err
	}

	if at.LexProfiles > 0 {
		fmt.Fprintf(os.Stderr,
			"attribute: стиль-профилей %d, лексических %d; запрос: 3-грамм %d, слов %d; вес лексики %.2f\n"+
				"  фон: стиль cos %.4f±%.4f, лексика cos %.4f±%.4f\n",
			at.StyleProfiles, at.LexProfiles, at.QueryNgrams, at.QueryTokens, at.LexWeight,
			at.StyleCosMean, at.StyleCosStd, at.LexCosMean, at.LexCosStd)
	} else {
		fmt.Fprintf(os.Stderr,
			"attribute: стиль-профилей %d, в запросе 3-грамм %d (фон cos %.4f±%.4f); лексика не построена (`personas lexis build`)\n",
			at.StyleProfiles, at.QueryNgrams, at.StyleCosMean, at.StyleCosStd)
	}
	if at.QueryNgrams < shortQueryNgrams {
		fmt.Fprintf(os.Stderr, "  ⚠ короткий текст (<%d 3-грамм): ранжирование ненадёжно, топ — только подсказка\n",
			shortQueryNgrams)
	}
	lexActive := at.LexProfiles > 0
	for _, c := range at.Candidates {
		fmt.Fprintf(os.Stderr, "  %3d. %s  %s\n", c.Rank, attrScore(c, lexActive), attrLabel(c))
	}
	switch {
	case wantID != 0 && at.Want == nil:
		fmt.Fprintf(os.Stderr, "валидация: у автора %d нет стиль-профиля (мало текста)\n", wantID)
	case at.Want != nil:
		fmt.Fprintf(os.Stderr, "валидация: настоящий автор на месте %d из %d — %s  %s\n",
			at.Want.Rank, at.StyleProfiles, attrScore(*at.Want, lexActive), attrLabel(*at.Want))
		fmt.Fprintln(os.Stderr, "  (текст авторской заметки входит в её профиль — совпадение завышено)")
	}
	return nil
}

// attributeIdentityNotes — пакетный режим: атрибуция всех заметок личности
// (валидация «узнаёт ли система автора по его же заметке»).
func attributeIdentityNotes(ctx context.Context, ar *archive.Store, opt attrOpts) error {
	rep, err := ar.AttributeIdentityNotes(ctx, opt.author, opt.lexWeight)
	if err != nil {
		return err
	}
	lex := fmt.Sprintf("%d", rep.LexProfiles)
	if rep.LexProfiles == 0 {
		lex = "не построена"
	}
	fmt.Fprintf(os.Stderr, "attribute %s: заметок %d (анкет %d); стиль-профилей %d, лексика %s; вес лексики %.2f\n",
		rep.Identity, len(rep.Notes), rep.Accounts, rep.StyleProfiles, lex, rep.LexWeight)
	if len(rep.Notes) == 0 {
		fmt.Fprintln(os.Stderr, "  у личности нет заметок в архиве")
		return nil
	}

	// Сортируем по рангу (лучшие первыми); заметки без профиля (rank 0) — в конец.
	rows := append([]archive.NoteAttribution(nil), rep.Notes...)
	sort.Slice(rows, func(i, j int) bool {
		ri, rj := rankKey(rows[i].Rank), rankKey(rows[j].Rank)
		if ri != rj {
			return ri < rj
		}
		return rows[i].NoteID < rows[j].NoteID
	})

	fmt.Fprintf(os.Stderr, "  %-5s %-6s %-4s  %-24s  %-24s  %s\n", "ранг", "score", "своя", "автор", "топ-1 (система)", "текст")
	for _, n := range rows {
		rank := fmt.Sprintf("%d", n.Rank)
		self := ""
		if n.Rank == 0 {
			rank = "—"
		} else if n.Self {
			self = "✓"
		}
		fmt.Fprintf(os.Stderr, "  %-5s %-6s %-4s  %-24s  %-24s  %s\n",
			rank, fmt.Sprintf("%.1f", n.Score), self,
			trunc(fmt.Sprintf("%s(%d)", nameOr(n.AuthorName), n.AuthorID), 24),
			trunc(fmt.Sprintf("%s(%d)", nameOr(n.TopName), n.TopID), 24),
			n.Snippet)
	}

	fmt.Fprintf(os.Stderr,
		"итог: с рангом %d из %d — автор в топ-1 %d, топ-5 %d, топ-10 %d; узнан как своя личность (топ-1) %d; медиана ранга %d\n",
		rep.Scored, len(rep.Notes), rep.Top1, rep.Top5, rep.Top10, rep.SelfTop1, rep.MedianRank)
	fmt.Fprintln(os.Stderr, "  (leave-one-out нарушен: текст заметки входит в профиль автора — ранги оптимистичны)")
	return nil
}

// rankKey — ключ сортировки: ранг 0 (нет профиля) уводится в конец.
func rankKey(rank int) int {
	if rank == 0 {
		return 1 << 30
	}
	return rank
}

func nameOr(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// attrScore — колонка скора кандидата: комбинированный Z + разбивка по сигналам.
// lexActive — построен ли лексический слой (иначе скор чисто стилевой).
func attrScore(c archive.AttributionCandidate, lexActive bool) string {
	switch {
	case c.HasLex:
		return fmt.Sprintf("score=%5.1f | стиль z=%4.1f cos=%.3f | лексика z=%4.1f cos=%.3f",
			c.Score, c.StyleZ, c.StyleCos, c.LexZ, c.LexCos)
	case lexActive:
		return fmt.Sprintf("score=%5.1f | стиль z=%4.1f cos=%.3f | лексика нейтр. (нет профиля)",
			c.Score, c.StyleZ, c.StyleCos)
	default:
		return fmt.Sprintf("score=%5.1f | стиль z=%4.1f cos=%.3f | лексики нет", c.Score, c.StyleZ, c.StyleCos)
	}
}

// attributeInput — текст запроса и, для авторской заметки из архива, id автора
// (режим валидации).
func attributeInput(ctx context.Context, ar *archive.Store, args []string, opt attrOpts) (string, int64, error) {
	switch {
	case opt.noteID != 0:
		n, ok, err := ar.LoadNote(ctx, opt.noteID)
		if err != nil {
			return "", 0, err
		}
		if !ok {
			return "", 0, fmt.Errorf("attribute: заметки %d нет в архиве", opt.noteID)
		}
		var want int64
		if n.Author != nil {
			want = n.Author.ID
		}
		return n.Text, want, nil
	case opt.inPath != "":
		data, err := os.ReadFile(opt.inPath)
		if err != nil {
			return "", 0, err
		}
		return string(data), 0, nil
	case len(args) > 0:
		return strings.Join(args, " "), 0, nil
	default:
		fmt.Fprintln(os.Stderr, "attribute: читаю текст из stdin (Ctrl+Z/Ctrl+D — конец ввода)…")
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", 0, err
		}
		return string(data), 0, nil
	}
}

// attrLabel — строка кандидата: имя(id), пол, личность, объём профиля.
func attrLabel(c archive.AttributionCandidate) string {
	var b strings.Builder
	name := c.Name
	if name == "" {
		name = "?"
	}
	fmt.Fprintf(&b, "%s(%d)", name, c.UserID)
	switch c.Gender {
	case "male":
		b.WriteString(" ♂")
	case "female":
		b.WriteString(" ♀")
	}
	if c.Persona {
		fmt.Fprintf(&b, " [%s]", c.Identity)
	}
	fmt.Fprintf(&b, " — профиль %d 3-грамм", c.Ngrams)
	return b.String()
}
