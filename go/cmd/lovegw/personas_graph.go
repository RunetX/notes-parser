package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"lovegw/internal/archive"
)

// personasPortrait печатает досье личности (и пишет <identity>.json): слитые
// анкеты, активность, ключевые собеседники, интересы и отношения.
func personasPortrait(ctx context.Context, ar *archive.Store, args []string, outDir string, top int) error {
	if len(args) < 1 {
		return fmt.Errorf("personas portrait: нужен идентификатор (p<id> | u<id> | <user_id>)")
	}
	p, err := ar.Portrait(ctx, args[0], top)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "=== Портрет %s: %q ===\n", p.Identity, p.Label)
	kind := "анкета"
	if p.IsPersona {
		kind = fmt.Sprintf("личность из %d анкет", len(p.Accounts))
	}
	fmt.Fprintf(os.Stderr, "%s | комментариев %d | заметок %d | активность %s … %s\n",
		kind, p.Comments, p.Notes, shortDate(p.FirstSeen), shortDate(p.LastSeen))
	if len(p.Accounts) > 1 {
		fmt.Fprintln(os.Stderr, "Анкеты:")
		for _, a := range p.Accounts {
			fmt.Fprintf(os.Stderr, "  • %s (id %d, %s) — %d комм.\n", a.Name, a.ID, a.Age, a.Comments)
		}
	}
	printEdges("Чаще всего отвечает:", p.RepliesTo)
	printEdges("Чаще всего отвечают ему:", p.RepliedBy)
	printFacts(p.Facts)
	printRelations(p.Relations)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(outDir, p.Identity+".json")
	if err := writeJSONFile(path, p); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "JSON:", path)
	return nil
}

func printEdges(title string, edges []archive.PortraitEdge) {
	if len(edges) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, title)
	for _, e := range edges {
		fmt.Fprintf(os.Stderr, "  → %s (%s) ×%d\n", e.Label, e.Identity, e.Replies)
	}
}

// polarityRu — русская подпись полярности факта.
func polarityRu(p string) string {
	switch p {
	case archive.PolarityLikes:
		return "любит"
	case archive.PolarityDislikes:
		return "не любит"
	case archive.PolarityOwns:
		return "у него/неё это есть"
	default:
		return "упоминает"
	}
}

// kindRu — русская подпись типа отношений.
func kindRu(k string) string {
	switch k {
	case archive.KindFriendship:
		return "дружба"
	case archive.KindConflict:
		return "конфликт"
	case archive.KindFlirt:
		return "флирт"
	case archive.KindNeutral:
		return "нейтрально"
	default:
		return k
	}
}

func printFacts(facts []archive.IdentityFact) {
	if len(facts) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "Интересы:")
	for _, f := range facts {
		fmt.Fprintf(os.Stderr, "  • %s — %s (%d упоминаний в %d заметках, %s)\n",
			f.Topic, polarityRu(f.Polarity), f.Hits, f.NotesCount, f.Source)
	}
}

func printRelations(rels []archive.RelationRow) {
	if len(rels) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr, "Отношения:")
	for _, r := range rels {
		mark := fmt.Sprintf("тон %+.2f (+%d/−%d)", r.Tone, r.Pos, r.Neg)
		if r.Kind != archive.KindTone {
			mark = fmt.Sprintf("%s (conf %.2f), тон %+.2f", kindRu(r.Kind), r.Score, r.Tone)
		}
		fmt.Fprintf(os.Stderr, "  ↔ %s (%s) ×%d, взаимность %.2f — %s\n",
			r.Label, r.To, r.Replies, r.Reciprocity, mark)
	}
}

// shortDate — YYYY-MM-DD из RFC3339 (или как есть, если короче).
func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
