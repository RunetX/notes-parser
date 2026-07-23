package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/archive"
)

// defaultDisclosurePatterns — валидированный на реальных данных набор LIKE-шаблонов
// самораскрытия альт-анкет (в нижнем регистре; сравнение регистронезависимо через
// ulower). Точное совпадение аватара оказалось мёртвым сигналом (у каждой анкеты
// свой URL), а текстовые признания — золотой жилой. Переопределяется -patterns.
var defaultDisclosurePatterns = []string{
	"%втор%анкет%",   // «это моя вторая анкета», «со второй анкеты»
	"%стар%анкет%",   // «на старой анкете»
	"%фейк%анкет%",   // «фейк-анкета»
	"%это моя%анкет%", // прямое указание
	"%бывш%ник%",     // «бывший ник»
	"%под ником%",    // «пишу под ником …»
	"%смени%ник%",    // «сменил/сменила ник»
	"%мой клон%",     // «это мой клон»
}

// cmdPersonas — слой распознавания личностей (persona resolution) поверх archive.db.
// Вероятностная, ревью-гейтед склейка альт-анкет одного человека в «личности».
// Действия (2-й позиционный аргумент):
//
//	flag        — просканировать комментарии по шаблонам самораскрытия → disclosure_hits
//	candidates  — выгрузить непроработанные пометки с контекстом (hits.jsonl + users_index.json)
//	link        — импортировать извлечённые связи (links.json) в alias_candidates
//	cluster     — склеить связные компоненты в personas (порог -min-score) + отчёт
//	set <id> <confirmed|rejected|pending> — проставить статус личности после ревью
func cmdPersonas(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("personas", flag.ExitOnError)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	outDir := fs.String("out", "dump", "каталог выгрузок (candidates/cluster)")
	inPath := fs.String("in", "", "входной JSON для link (по умолчанию <out>/links.json)")
	limit := fs.Int("limit", 200, "предел выборки (candidates; avatars fetch; 0 — все)")
	minScore := fs.Float64("min-score", 0.7, "порог веса ребра для склейки (cluster)")
	patternsPath := fs.String("patterns", "", "файл LIKE-шаблонов (flag; по строке, # — коммент; иначе встроенный набор)")
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	useProxy := fs.Bool("proxy", false, "качать аватары через telegram_proxy (avatars fetch)")
	workers := fs.Int("workers", 6, "воркеров загрузки (avatars fetch)")
	intervalMS := fs.Int("interval-ms", 150, "интервал запросов, мс (avatars fetch)")
	refresh := fs.Bool("refresh", false, "пере-скачать уже хэшированные аватары (avatars fetch)")
	maxDist := fs.Int("max-dist", 4, "макс. Hamming dHash для склейки (avatars cluster)")
	genericMax := fs.Int("generic-max", 4, "exact-группа аватаров больше — generic, пропуск (avatars cluster)")
	minChars := fs.Int("min-chars", 1000, "мин. суммарного текста автора для профиля (stylometry build)")
	dims := fs.Int("dims", 512, "размерность хэш-вектора стиля (stylometry build)")
	minCosine := fs.Float64("min-cosine", 0.5, "порог центр-косинуса стиля (stylometry cluster)")
	topK := fs.Int("top-k", 2, "сколько ближайших по стилю на автора (stylometry cluster)")
	maxPairs := fs.Int("max-pairs", 500, "предел числа пар стиля (stylometry cluster)")
	minReplies := fs.Int("min-replies", 3, "мин. вес ребра для экспорта (graph)")
	dropSelf := fs.Bool("drop-self", false, "убрать само-петли — ответы между альтами (graph)")
	top := fs.Int("top", 8, "сколько собеседников в каждую сторону (portrait)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "out": true, "in": true, "limit": true, "min-score": true, "patterns": true,
		"config": true, "workers": true, "interval-ms": true, "max-dist": true, "generic-max": true,
		"min-chars": true, "dims": true, "min-cosine": true, "top-k": true, "max-pairs": true,
		"min-replies": true, "top": true,
	})); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("personas: не указано действие (flag|candidates|link|cluster|set|avatars)")
	}

	ar, err := archive.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	switch action := fs.Arg(0); action {
	case "flag":
		return personasFlag(ctx, ar, *patternsPath)
	case "candidates":
		return personasCandidates(ctx, ar, *outDir, *limit)
	case "link":
		return personasLink(ctx, ar, linkInputPath(*inPath, *outDir))
	case "cluster":
		return personasCluster(ctx, ar, *minScore, *outDir)
	case "set":
		return personasSet(ctx, ar, fs.Args()[1:])
	case "avatars":
		return personasAvatars(ctx, ar, fs.Args()[1:], avatarOpts{
			cfgPath: *cfgPath, proxy: *useProxy, workers: *workers, intervalMS: *intervalMS,
			refresh: *refresh, limit: *limit, maxDist: *maxDist, genericMax: *genericMax,
		})
	case "stylometry":
		return personasStylometry(ctx, ar, fs.Args()[1:], styloOpts{
			minChars: *minChars, dims: *dims, minCosine: *minCosine, topK: *topK, maxPairs: *maxPairs,
		})
	case "graph":
		return personasGraph(ctx, ar, *outDir, *minReplies, *dropSelf)
	case "portrait":
		return personasPortrait(ctx, ar, fs.Args()[1:], *outDir, *top)
	default:
		return fmt.Errorf("personas: неизвестное действие %q (flag|candidates|link|cluster|set|avatars|stylometry|graph|portrait)", action)
	}
}

// personasFlag сканирует комментарии по шаблонам самораскрытия.
func personasFlag(ctx context.Context, ar *archive.Store, patternsPath string) error {
	patterns := defaultDisclosurePatterns
	if patternsPath != "" {
		p, err := readPatterns(patternsPath)
		if err != nil {
			return err
		}
		patterns = p
	}
	st, err := ar.FlagDisclosures(ctx, patterns)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "flag: новых пометок %d, всего в архиве %d\n", st.Inserted, st.Total)

	type kv struct {
		p string
		n int
	}
	list := make([]kv, 0, len(st.PerPattern))
	for p, n := range st.PerPattern {
		list = append(list, kv{p, n})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
	for _, e := range list {
		fmt.Fprintf(os.Stderr, "  %-18s %d\n", e.p, e.n)
	}
	return nil
}

// personasCandidates выгружает материал для извлечения связей: непроработанные
// пометки с контекстом (hits.jsonl) + глобальный индекс пользователей.
func personasCandidates(ctx context.Context, ar *archive.Store, outDir string, limit int) error {
	hits, err := ar.DisclosureCandidates(ctx, limit)
	if err != nil {
		return err
	}
	idx, err := ar.UsersIndex(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	hitsPath := filepath.Join(outDir, "hits.jsonl")
	if err := writeJSONL(hitsPath, hits); err != nil {
		return err
	}
	idxPath := filepath.Join(outDir, "users_index.json")
	if err := writeJSONFile(idxPath, idx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "candidates: пометок %d → %s; индекс пользователей %d → %s\n",
		len(hits), hitsPath, len(idx), idxPath)
	return nil
}

// personasLink импортирует извлечённые связи в alias_candidates.
func personasLink(ctx context.Context, ar *archive.Store, inPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	links, resolved, err := parseLinkInput(data)
	if err != nil {
		return fmt.Errorf("%s: %w", inPath, err)
	}
	st, err := ar.ImportAliasLinks(ctx, links, resolved, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "link: импортировано связей %d, пропущено %d, помечено пометок %d\n",
		st.Links, st.Skipped, st.HitsResolved)
	return nil
}

// personasCluster склеивает личности и печатает отчёт-ревью.
func personasCluster(ctx context.Context, ar *archive.Store, minScore float64, outDir string) error {
	clusters, err := ar.ClusterPersonas(ctx, minScore, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	reportPath := filepath.Join(outDir, "clusters.json")
	if err := writeJSONFile(reportPath, clusters); err != nil {
		return err
	}

	members := 0
	for _, c := range clusters {
		members += len(c.Members)
	}
	fmt.Fprintf(os.Stderr, "cluster: личностей %d (участников %d, порог %.2f) → %s\n",
		len(clusters), members, minScore, reportPath)
	for i, c := range clusters {
		if i >= 10 {
			fmt.Fprintf(os.Stderr, "  … ещё %d\n", len(clusters)-10)
			break
		}
		names := make([]string, 0, len(c.Members))
		for _, m := range c.Members {
			names = append(names, fmt.Sprintf("%s(%d)", m.Name, m.ID))
		}
		fmt.Fprintf(os.Stderr, "  #%d %q conf=%.2f: %s\n",
			c.PersonaID, c.Label, c.Confidence, strings.Join(names, ", "))
	}
	return nil
}

// personasSet проставляет статус личности после ревью.
func personasSet(ctx context.Context, ar *archive.Store, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("personas set: нужно <persona_id> <confirmed|rejected|pending>")
	}
	pid, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("personas set: id не число: %q", args[0])
	}
	n, err := ar.SetPersonaStatus(ctx, pid, args[1])
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("personas set: личность %d не найдена", pid)
	}
	fmt.Fprintf(os.Stderr, "set: личность %d → %s (участников %d)\n", pid, args[1], n)
	return nil
}

// --- ввод/вывод ---

// linkInputPath — путь входного JSON для link: явный -in или <out>/links.json.
func linkInputPath(in, outDir string) string {
	if in != "" {
		return in
	}
	return filepath.Join(outDir, "links.json")
}

// parseLinkInput принимает две формы: голый массив связей [{user_a,…}] или
// объект {"links":[…],"resolved":[…]} (resolved — id пометок-тупиков без связи).
func parseLinkInput(data []byte) ([]archive.AliasLink, []int64, error) {
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '[' {
		var links []archive.AliasLink
		if err := json.Unmarshal(t, &links); err != nil {
			return nil, nil, err
		}
		return links, nil, nil
	}
	var obj struct {
		Links    []archive.AliasLink `json:"links"`
		Resolved []int64             `json:"resolved"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, nil, err
	}
	return obj.Links, obj.Resolved, nil
}

// writeJSONL пишет по одному JSON-объекту на строку (для LLM-построчной обработки).
func writeJSONL[T any](path string, items []T) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err = enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readPatterns читает шаблоны из файла: по строке, пустые и #-комментарии пропускаются.
func readPatterns(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("файл шаблонов пуст: %s", path)
	}
	return out, nil
}
