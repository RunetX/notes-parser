package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lovegw/internal/archive"
)

// personasReport — итоговый Markdown «персонажи»: личности с интересами
// (identity_facts) и отношениями (v_relations). Прямой ответ на «кто с кем
// дружит, кто любит собак»: тип отношений — только из LLM/ручной разметки,
// сырой тон подписан как тональность. activeDays>0 — отобрать только тех, кто
// публиковался в последние N суток (окно от самой свежей даты в архиве),
// иначе — все личности. topN>0 — ограничить самыми активными по объёму.
func personasReport(ctx context.Context, ar *archive.Store, outDir string, topN, activeDays int) error {
	facts, err := ar.AllFacts(ctx)
	if err != nil {
		return err
	}
	var (
		list      []archive.GraphNode
		weekly    map[string]int
		scope     = "самых активных"
		sinceNote string
		full      int
	)
	if activeDays > 0 {
		// Cohort-first: сначала дешёвый временной срез (когорта + счётчики за
		// окно), затем all-time итоги ТОЛЬКО для показываемых личностей. Полный
		// проход v_persona_activity по всем ~22 тыс. личностей (≈25с) не нужен —
		// отчёту хватает десятков.
		since, err := recentSince(ctx, ar, activeDays)
		if err != nil {
			return err
		}
		if weekly, err = ar.ActiveCountsSince(ctx, since); err != nil {
			return err
		}
		full = countPositive(weekly)
		idents := cohortTop(weekly, topN)
		nodes, err := ar.CohortNodes(ctx, idents)
		if err != nil {
			return err
		}
		for _, id := range idents {
			list = append(list, nodes[id])
		}
		scope = fmt.Sprintf("активных с %s (топ по числу сообщений за окно)", shortDate(since))
		sinceNote = fmt.Sprintf(" (активность за последние %d сут., с %s)", activeDays, shortDate(since))
	} else {
		nodes, err := ar.GraphNodes(ctx)
		if err != nil {
			return err
		}
		for _, nd := range nodes {
			list = append(list, nd)
		}
		full = len(list)
	}
	sortByActivity(list, weekly)
	if activeDays == 0 && topN > 0 && len(list) > topN {
		list = list[:topN]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Персонажи заметок%s\n\nЛичностей в выборке: %d; показаны %d %s.\n",
		sinceNote, full, len(list), scope)
	titles := topicTitles(archive.DefaultTopics())
	for i, nd := range list {
		if err := reportCharacter(ctx, ar, &b, i+1, nd, facts[nd.Identity], titles, weekly, activeDays); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(outDir, "characters.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "report: %d персонажей → %s\n", len(list), path)
	return nil
}

// recentSince вычисляет начало окна активности: самая свежая дата в архиве
// минус days суток (ISO-8601 UTC). Пустой архив → нулевая дата (окном всё).
func recentSince(ctx context.Context, ar *archive.Store, days int) (string, error) {
	mx, err := ar.MaxPublishedAt(ctx)
	if err != nil || mx == "" {
		return "", err
	}
	end, err := time.Parse(time.RFC3339, mx)
	if err != nil {
		return "", fmt.Errorf("разбор max published_at %q: %w", mx, err)
	}
	return end.AddDate(0, 0, -days).UTC().Format(time.RFC3339), nil
}

// countPositive — сколько личностей проявили активность за окно (>0 сообщений).
func countPositive(weekly map[string]int) int {
	n := 0
	for _, c := range weekly {
		if c > 0 {
			n++
		}
	}
	return n
}

// cohortTop — топ-N личностей окна по числу сообщений за окно (при равенстве —
// по identity для стабильности); только с ненулевой активностью. Возвращает
// именно тех, для кого дальше тянутся all-time итоги (CohortNodes) — за окном
// хвоста нет смысла грузить полный агрегат.
func cohortTop(weekly map[string]int, topN int) []string {
	ids := make([]string, 0, len(weekly))
	for id, c := range weekly {
		if c > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if weekly[ids[i]] != weekly[ids[j]] {
			return weekly[ids[i]] > weekly[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if topN > 0 && len(ids) > topN {
		ids = ids[:topN]
	}
	return ids
}

// sortByActivity сортирует по недавней активности (если weekly задан), иначе по
// суммарному числу комментариев; при равенстве — по identity для стабильности.
func sortByActivity(list []archive.GraphNode, weekly map[string]int) {
	sort.Slice(list, func(i, j int) bool {
		if weekly != nil {
			if wi, wj := weekly[list[i].Identity], weekly[list[j].Identity]; wi != wj {
				return wi > wj
			}
		}
		if list[i].Comments != list[j].Comments {
			return list[i].Comments > list[j].Comments
		}
		return list[i].Identity < list[j].Identity
	})
}

// reportCharacter пишет раздел одной личности. weekly/activeDays непусты в
// режиме -active-days — тогда в строке показывается активность за окно (по ней
// же строится топ).
func reportCharacter(ctx context.Context, ar *archive.Store, b *strings.Builder, n int, nd archive.GraphNode, facts []archive.IdentityFact, titles map[string]string, weekly map[string]int, activeDays int) error {
	fmt.Fprintf(b, "\n## %d. %s (%s)\n\n", n, nd.Label, nd.Identity)
	kind := "анкета"
	if nd.IsPersona {
		kind = fmt.Sprintf("личность из %d анкет", nd.Accounts)
	}
	recent := ""
	if activeDays > 0 {
		recent = fmt.Sprintf(" · за %d сут.: %d сообщений", activeDays, weekly[nd.Identity])
	}
	fmt.Fprintf(b, "%s · всего %d комментариев · %d заметок%s · активность %s … %s\n",
		kind, nd.Comments, nd.Notes, recent, shortDate(nd.FirstSeen), shortDate(nd.LastSeen))

	reportFacts(b, facts, titles)

	rels, err := ar.IdentityRelations(ctx, nd.Identity, 0)
	if err != nil {
		return err
	}
	reportRelations(b, rels)
	return nil
}

// reportFacts — блок интересов: до 6 сильнейших тем, с цитатой у размеченных.
func reportFacts(b *strings.Builder, facts []archive.IdentityFact, titles map[string]string) {
	if len(facts) == 0 {
		return
	}
	if len(facts) > 6 {
		facts = facts[:6]
	}
	fmt.Fprintf(b, "\n**Интересы:**\n\n")
	for _, f := range facts {
		title := titles[f.Topic]
		if title == "" {
			title = f.Topic
		}
		fmt.Fprintf(b, "- %s — %s (%d упоминаний в %d заметках)", title, polarityRu(f.Polarity), f.Hits, f.NotesCount)
		if quote := factQuote(f); quote != "" && f.Polarity != archive.PolarityMentions {
			fmt.Fprintf(b, " — «%s»", quote)
		}
		fmt.Fprintln(b)
	}
}

// factQuote — первая маркированная цитата факта (или просто первая).
func factQuote(f archive.IdentityFact) string {
	for _, ev := range f.Evidence {
		if ev.Marker != "" {
			return shortQuote(ev.Quote)
		}
	}
	if len(f.Evidence) > 0 {
		return shortQuote(f.Evidence[0].Quote)
	}
	return ""
}

func shortQuote(s string) string {
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// reportRelations — блоки отношений: разметка (дружба/конфликт/флирт) отдельно,
// тональность самых тёплых/колючих пар — отдельно, с оговоркой про сырой тон.
func reportRelations(b *strings.Builder, rels []archive.RelationRow) {
	byKind := map[string][]archive.RelationRow{}
	var toned []archive.RelationRow
	for _, r := range rels {
		if r.Kind != archive.KindTone {
			byKind[r.Kind] = append(byKind[r.Kind], r)
			continue
		}
		toned = append(toned, r)
	}
	kindTitles := []struct{ kind, title string }{
		{archive.KindFriendship, "Дружит с"},
		{archive.KindFlirt, "Флирт с"},
		{archive.KindConflict, "Конфликтует с"},
	}
	for _, kt := range kindTitles {
		list := byKind[kt.kind]
		if len(list) == 0 {
			continue
		}
		parts := make([]string, 0, len(list))
		for _, r := range list {
			parts = append(parts, fmt.Sprintf("%s (%s, ×%d, conf %.2f)", r.Label, r.To, r.Replies, r.Score))
		}
		fmt.Fprintf(b, "\n**%s:** %s\n", kt.title, strings.Join(parts, ", "))
	}
	reportTone(b, toned)
}

// reportTone — топ-3 тёплых и колючих пар по сырому тону (не разметка типа!).
func reportTone(b *strings.Builder, toned []archive.RelationRow) {
	warm, cold := splitByTone(toned)
	if len(warm) > 0 {
		fmt.Fprintf(b, "\n**Тёплые по тону:** %s\n", joinToneRows(warm))
	}
	if len(cold) > 0 {
		fmt.Fprintf(b, "\n**Колючие по тону** (тон ≠ вражда — возможна дружеская пикировка)**:** %s\n",
			joinToneRows(cold))
	}
}

// splitByTone — до 3 самых тёплых (тон > 0.05) и колючих (тон < -0.05) пар,
// приоритет — заметному объёму переписки.
func splitByTone(toned []archive.RelationRow) (warm, cold []archive.RelationRow) {
	sort.Slice(toned, func(i, j int) bool { return toned[i].Tone > toned[j].Tone })
	for _, r := range toned {
		if r.Tone > 0.05 && len(warm) < 3 {
			warm = append(warm, r)
		}
	}
	for i := len(toned) - 1; i >= 0; i-- {
		if r := toned[i]; r.Tone < -0.05 && len(cold) < 3 {
			cold = append(cold, r)
		}
	}
	return warm, cold
}

func joinToneRows(rows []archive.RelationRow) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s (%s, ×%d, %+.2f)", r.Label, r.To, r.Replies, r.Tone))
	}
	return strings.Join(parts, ", ")
}
