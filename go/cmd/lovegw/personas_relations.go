package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"lovegw/internal/archive"
)

// relOpts — настройки действий relations.
type relOpts struct {
	outDir      string
	inPath      string // вход import (по умолчанию <out>/relations_llm.json)
	minReplies  int    // score: писать тон для направленных пар ≥
	candReplies int    // candidates: ядро — сумма реплик пары ≥
	bandMin     int    // candidates: нижняя граница полосы добора
	bandTop     int    // candidates: сколько поляризованных пар из полосы
	exchanges   int    // candidates: обменов на пару
}

// personasRelations — отношения личностей (Фаза 3). Действия:
//
//	score      — тональный скоринг рёбер по смайлам/словарю → relation_edges(tone)
//	candidates — выгрузить пары с сэмплом диалогов на LLM-разметку (pairs.jsonl)
//	import     — импортировать разметку (relations_llm.json) → relation_edges(llm)
func personasRelations(ctx context.Context, ar *archive.Store, args []string, o relOpts) error {
	if len(args) < 1 {
		return fmt.Errorf("personas relations: нужно действие (score|candidates|import)")
	}
	switch args[0] {
	case "score":
		return relationsScore(ctx, ar, o)
	case "candidates":
		return relationsCandidates(ctx, ar, o)
	case "import":
		return relationsImport(ctx, ar, defaultPath(o.inPath, o.outDir, "relations_llm.json"))
	default:
		return fmt.Errorf("personas relations: неизвестное действие %q (score|candidates|import)", args[0])
	}
}

// relationsScore — тональный проход по всем ответам.
func relationsScore(ctx context.Context, ar *archive.Store, o relOpts) error {
	fmt.Fprintf(os.Stderr, "relations score: тон всех ответов (min-replies=%d) — весь corpus, займёт минуты…\n",
		o.minReplies)
	st, err := ar.ScoreTone(ctx, archive.ToneParams{MinReplies: o.minReplies}, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "relations score: ответов %d, направленных пар %d, записано строк %d\n",
		st.Replies, st.Pairs, st.Written)
	return nil
}

// relationsCandidates выгружает пары с диалогами на LLM-разметку.
func relationsCandidates(ctx context.Context, ar *archive.Store, o relOpts) error {
	fmt.Fprintf(os.Stderr, "relations candidates: ядро ≥%d, полоса [%d,%d) топ-%d, обменов %d…\n",
		o.candReplies, o.bandMin, o.candReplies, o.bandTop, o.exchanges)
	cands, err := ar.RelationCandidates(ctx, archive.RelationCandidateParams{
		MinReplies: o.candReplies, BandMin: o.bandMin, BandTop: o.bandTop, Exchanges: o.exchanges,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.outDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(o.outDir, "pairs.jsonl")
	if err := writeJSONL(path, cands); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "relations candidates: %d пар → %s\n", len(cands), path)
	return nil
}

// relationsImport заносит LLM-разметку в relation_edges(llm).
func relationsImport(ctx context.Context, ar *archive.Store, inPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	items, err := parseRelationImport(data)
	if err != nil {
		return fmt.Errorf("%s: %w", inPath, err)
	}
	st, err := ar.ImportRelations(ctx, items, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "relations import: записано пар %d, пропущено %d, ремапнуто %d\n",
		st.Written, st.Skipped, st.Remapped)
	return nil
}

// parseRelationImport принимает голый массив [{a,b,kind,…}] или объект
// {"relations":[…]}.
func parseRelationImport(data []byte) ([]archive.RelationImport, error) {
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		var items []archive.RelationImport
		return items, json.Unmarshal(t, &items)
	}
	var obj struct {
		Relations []archive.RelationImport `json:"relations"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj.Relations, nil
}
