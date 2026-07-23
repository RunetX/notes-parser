package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"lovegw/internal/archive"
)

// personasDiag — ground-truth диагностика: по набору заведомо связанных анкет
// печатает активность, стилевую близость (ранг сиблингов среди всех профилей),
// пересечение собеседников и временной паттерн. Только чтение, ничего не пишет.
func personasDiag(ctx context.Context, ar *archive.Store, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("personas diag: нужно ≥2 id анкет, напр.: personas diag 1472546 1111405 441015")
	}
	ids := make([]int64, 0, len(args))
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return fmt.Errorf("personas diag: id не число: %q", a)
		}
		ids = append(ids, id)
	}
	d, err := ar.DiagPersonas(ctx, ids)
	if err != nil {
		return err
	}
	printDiag(os.Stdout, d, ids)
	return nil
}

func printDiag(w io.Writer, d archive.PersonaDiag, ids []int64) {
	name := map[int64]string{}
	for _, a := range d.Accounts {
		name[a.ID] = a.Name
	}
	fmt.Fprintf(w, "Диагностика %d анкет (ground truth)\n\n", len(ids))

	fmt.Fprintln(w, "АНКЕТЫ:")
	for _, a := range d.Accounts {
		prof := "нет"
		if a.HasProfile {
			prof = fmt.Sprintf("да (%d 3-грамм)", a.Ngrams)
		}
		fmt.Fprintf(w, "  %d «%s» %s — %d комм. / %d заметок · %s · профиль: %s\n",
			a.ID, a.Name, a.Age, a.Comments, a.Notes, spanStr(a.ActiveFrom, a.ActiveTo), prof)
	}

	fmt.Fprintln(w, "\nСТИЛЬ (центр-косинус; чем выше ранг сиблинга, тем лучше сигнал):")
	for _, st := range d.Style {
		printDiagStyle(w, st, name, ids)
	}

	fmt.Fprintln(w, "\nПАРЫ:")
	for _, p := range d.Pairs {
		printDiagPair(w, p)
	}
}

func printDiagStyle(w io.Writer, st archive.DiagStyle, name map[int64]string, ids []int64) {
	fmt.Fprintf(w, "  %d «%s» (из %d профилей):\n", st.ID, name[st.ID], st.Total)
	if st.Total == 0 || len(st.Neighbors) == 0 {
		fmt.Fprintln(w, "     нет профиля стиля (мало текста)")
		return
	}
	var sib []string
	for _, other := range ids {
		if other == st.ID {
			continue
		}
		if r, ok := st.Siblings[other]; ok && r.Rank > 0 {
			sib = append(sib, fmt.Sprintf("%d ранг #%d cos %.3f", other, r.Rank, r.Cosine))
		} else {
			sib = append(sib, fmt.Sprintf("%d нет профиля", other))
		}
	}
	fmt.Fprintf(w, "     свои: %s\n", strings.Join(sib, " ; "))

	var nb []string
	for _, n := range st.Neighbors {
		mark := ""
		if n.Known {
			mark = "◄СВОЙ "
		}
		nb = append(nb, fmt.Sprintf("%s%s(%d) %.3f", mark, n.Name, n.ID, n.Cosine))
	}
	fmt.Fprintf(w, "     топ-соседи: %s\n", strings.Join(nb, " ; "))
}

func printDiagPair(w io.Writer, p archive.DiagPair) {
	cos := "нет профиля"
	if !math.IsNaN(p.StyleCosine) {
		cos = fmt.Sprintf("%.3f", p.StyleCosine)
	}
	t := p.TemporalRelation
	if p.GapDays > 0 {
		t = fmt.Sprintf("%s (разрыв %dд)", t, p.GapDays)
	}
	fmt.Fprintf(w, "  %d ↔ %d: стиль cos %s | кросс-ответы %d→/%d← | собеседники %d/%d общих %d (J=%.3f) | время %s\n",
		p.A, p.B, cos, p.CrossRepliesAB, p.CrossRepliesBA, p.PartnersA, p.PartnersB, p.SharedPartners, p.JaccardPartners, t)
}

// spanStr — «2015-06-01 … 2018-03-04» из ISO-дат (берём префикс-дату); пусто → прочерк.
func spanStr(from, to string) string {
	d := func(s string) string {
		if len(s) >= 10 {
			return s[:10]
		}
		return "?"
	}
	if from == "" && to == "" {
		return "нет активности"
	}
	return d(from) + " … " + d(to)
}
