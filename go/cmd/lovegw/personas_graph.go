package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"lovegw/internal/archive"
)

// personasGraph экспортирует persona-aware соцграф в CSV для Gephi: nodes.csv
// (личности/анкеты с активностью) + edges.csv (ответы с весом). Узлы —
// только те, что участвуют в отфильтрованных рёбрах (без изолятов). Граф уже
// «по людям»: альты подтверждённых/предложенных личностей слиты в один узел.
func personasGraph(ctx context.Context, ar *archive.Store, outDir string, minReplies int, dropSelf bool) error {
	fmt.Fprintf(os.Stderr, "graph: строю рёбра (min-replies=%d, drop-self=%v)…\n", minReplies, dropSelf)
	edges, err := ar.GraphEdges(ctx, minReplies, dropSelf)
	if err != nil {
		return err
	}
	nodes, err := ar.GraphNodes(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Оставляем только узлы, участвующие в рёбрах.
	used := map[string]bool{}
	for _, e := range edges {
		used[e.From] = true
		used[e.To] = true
	}
	nodesPath := filepath.Join(outDir, "nodes.csv")
	nUsed, err := writeNodesCSV(nodesPath, nodes, used)
	if err != nil {
		return err
	}
	edgesPath := filepath.Join(outDir, "edges.csv")
	if err := writeEdgesCSV(edgesPath, edges); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "graph: узлов %d, рёбер %d → %s, %s\n", nUsed, len(edges), nodesPath, edgesPath)
	return nil
}

func writeNodesCSV(path string, nodes map[string]archive.GraphNode, used map[string]bool) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	// Gephi: столбец Id обязателен; Label — подпись.
	if err := w.Write([]string{"Id", "Label", "is_persona", "accounts", "comments", "notes", "first_seen", "last_seen"}); err != nil {
		return 0, err
	}
	n := 0
	for id := range used {
		nd, ok := nodes[id]
		if !ok {
			continue
		}
		if err := w.Write([]string{
			nd.Identity, nd.Label, boolStr(nd.IsPersona),
			strconv.Itoa(nd.Accounts), strconv.Itoa(nd.Comments), strconv.Itoa(nd.Notes),
			nd.FirstSeen, nd.LastSeen,
		}); err != nil {
			return n, err
		}
		n++
	}
	w.Flush()
	return n, w.Error()
}

func writeEdgesCSV(path string, edges []archive.GraphEdge) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	// Gephi: Source/Target/Weight, Type=Directed.
	if err := w.Write([]string{"Source", "Target", "Weight", "Type"}); err != nil {
		return err
	}
	for _, e := range edges {
		if err := w.Write([]string{e.From, e.To, strconv.Itoa(e.Replies), "Directed"}); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// personasPortrait печатает досье личности (и пишет <identity>.json).
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

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// shortDate — YYYY-MM-DD из RFC3339 (или как есть, если короче).
func shortDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
