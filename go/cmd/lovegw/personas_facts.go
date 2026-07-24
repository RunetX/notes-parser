package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"lovegw/internal/archive"
)

// factOpts — настройки действий facts.
type factOpts struct {
	outDir      string
	inPath      string // вход import (по умолчанию <out>/facts_llm.json)
	topicsPath  string // JSON с темами (иначе встроенный набор)
	minHits     int
	minNotes    int
	evidencePer int
}

// personasFacts — интересы личностей из текстов (Фаза 3). Действия:
//
//	scan       — лексиконный скан комментариев и заметок → identity_facts(lexicon)
//	candidates — выгрузить факты с цитатами и контекстом на LLM-разметку (facts.jsonl)
//	import     — импортировать разметку (facts_llm.json) → identity_facts(llm)
func personasFacts(ctx context.Context, ar *archive.Store, args []string, o factOpts) error {
	if len(args) < 1 {
		return fmt.Errorf("personas facts: нужно действие (scan|candidates|import)")
	}
	topics, err := loadTopics(o.topicsPath)
	if err != nil {
		return err
	}
	switch args[0] {
	case "scan":
		return factsScan(ctx, ar, topics, o)
	case "candidates":
		return factsCandidates(ctx, ar, topics, o)
	case "import":
		return factsImport(ctx, ar, defaultPath(o.inPath, o.outDir, "facts_llm.json"))
	default:
		return fmt.Errorf("personas facts: неизвестное действие %q (scan|candidates|import)", args[0])
	}
}

// loadTopics — темы из JSON-файла ([{key,title,stems:[…]}]) или встроенные.
func loadTopics(path string) ([]archive.TopicLexicon, error) {
	if path == "" {
		return archive.DefaultTopics(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var topics []archive.TopicLexicon
	if err := json.Unmarshal(data, &topics); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(topics) == 0 {
		return nil, fmt.Errorf("файл тем пуст: %s", path)
	}
	return topics, nil
}

// topicTitles — карта key→Title для выгрузки кандидатов.
func topicTitles(topics []archive.TopicLexicon) map[string]string {
	m := make(map[string]string, len(topics))
	for _, t := range topics {
		m[t.Key] = t.Title
	}
	return m
}

// factsScan прогоняет лексиконный скан и печатает разбивку по темам.
func factsScan(ctx context.Context, ar *archive.Store, topics []archive.TopicLexicon, o factOpts) error {
	fmt.Fprintf(os.Stderr, "facts scan: %d тем, пороги hits≥%d notes≥%d — весь corpus, займёт минуты…\n",
		len(topics), o.minHits, o.minNotes)
	st, err := ar.ScanFacts(ctx, topics, archive.FactScanParams{
		MinHits: o.minHits, MinNotes: o.minNotes, EvidencePer: o.evidencePer,
	}, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "facts scan: комментариев %d, заметок %d, фактов записано %d\n",
		st.Comments, st.Notes, st.Rows)
	type kv struct {
		key string
		st  archive.TopicScanStat
	}
	list := make([]kv, 0, len(st.PerTopic))
	for k, v := range st.PerTopic {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].st.Hits > list[j].st.Hits })
	for _, e := range list {
		fmt.Fprintf(os.Stderr, "  %-10s упоминаний %6d → фактов %d\n", e.key, e.st.Hits, e.st.Rows)
	}
	return nil
}

// factsCandidates выгружает материал на LLM-разметку полярности.
func factsCandidates(ctx context.Context, ar *archive.Store, topics []archive.TopicLexicon, o factOpts) error {
	cands, err := ar.FactCandidates(ctx, o.minHits, o.minNotes, topicTitles(topics))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(o.outDir, "facts.jsonl")
	if err := writeJSONL(path, cands); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "facts candidates: %d пар личность×тема → %s\n", len(cands), path)
	return nil
}

// factsImport заносит LLM-разметку в identity_facts(llm).
func factsImport(ctx context.Context, ar *archive.Store, inPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	items, err := parseFactImport(data)
	if err != nil {
		return fmt.Errorf("%s: %w", inPath, err)
	}
	st, err := ar.ImportFacts(ctx, items, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "facts import: записано %d, отклонено %d, пропущено %d, ремапнуто %d\n",
		st.Written, st.Rejected, st.Skipped, st.Remapped)
	return nil
}

// parseFactImport принимает голый массив [{identity,…}] или объект {"facts":[…]}.
func parseFactImport(data []byte) ([]archive.FactImport, error) {
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		var items []archive.FactImport
		return items, json.Unmarshal(t, &items)
	}
	var obj struct {
		Facts []archive.FactImport `json:"facts"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj.Facts, nil
}

// defaultPath — явный путь или <outDir>/<name>.
func defaultPath(in, outDir, name string) string {
	if in != "" {
		return in
	}
	return filepath.Join(outDir, name)
}
