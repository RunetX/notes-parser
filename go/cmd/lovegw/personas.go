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
	"%втор%анкет%",    // «это моя вторая анкета», «со второй анкеты»
	"%стар%анкет%",    // «на старой анкете»
	"%фейк%анкет%",    // «фейк-анкета»
	"%это моя%анкет%", // прямое указание
	"%бывш%ник%",      // «бывший ник»
	"%под ником%",     // «пишу под ником …»
	"%смени%ник%",     // «сменил/сменила ник»
	"%мой клон%",      // «это мой клон»
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
//	diag <id> <id> …                      — ground-truth диагностика набора анкет (стиль/собеседники/время)
//	ensemble    — направленный стиль + handoff + пересечение круга → alias_candidates(ensemble)
//	anon        — прогнать анонимные заметки через скоринг -suspect (поиск анонимок личности)
func cmdPersonas(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("personas", flag.ExitOnError)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	outDir := fs.String("out", "dump", "каталог выгрузок (candidates/cluster)")
	inPath := fs.String("in", "", "входной файл: link — <out>/links.json, facts import — <out>/facts_llm.json, relations import — <out>/relations_llm.json, attribute — текст запроса")
	limit := fs.Int("limit", 200, "предел выборки (candidates; avatars fetch; 0 — все)")
	minScore := fs.Float64("min-score", 0.7, "порог веса ребра для склейки (cluster)")
	patternsPath := fs.String("patterns", "", "файл LIKE-шаблонов (flag; по строке, # — коммент; иначе встроенный набор)")
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	useProxy := fs.Bool("proxy", false, "качать аватары через telegram_proxy (avatars fetch)")
	workers := fs.Int("workers", 6, "воркеров загрузки (avatars fetch)")
	intervalMS := fs.Int("interval-ms", 150, "интервал запросов, мс (avatars fetch)")
	refresh := fs.Bool("refresh", false, "пере-скачать уже хэшированные аватары (avatars fetch); переобойти заметки, набравшие комментарии после обхода (replies)")
	maxDist := fs.Int("max-dist", 4, "макс. Hamming dHash для склейки (avatars cluster)")
	genericMax := fs.Int("generic-max", 4, "exact-группа аватаров больше — generic, пропуск (avatars cluster)")
	minChars := fs.Int("min-chars", 1000, "мин. суммарного текста автора для профиля (stylometry build)")
	dims := fs.Int("dims", 512, "размерность хэш-вектора стиля (stylometry build)")
	minCosine := fs.Float64("min-cosine", 0.5, "порог центр-косинуса стиля (stylometry cluster)")
	topK := fs.Int("top-k", 2, "сколько ближайших по стилю на автора (stylometry cluster)")
	maxPairs := fs.Int("max-pairs", 500, "предел числа пар стиля (stylometry cluster)")
	top := fs.Int("top", 8, "сколько собеседников в каждую сторону (portrait) / кандидатов (attribute)")
	noteID := fs.Int64("note", 0, "id заметки архива: текст запроса (attribute) / заметка, к которой пишем комментарий первого уровня (voice comment)")
	lexWeight := fs.Float64("lex-weight", 0.5, "вес лексики в комбинированном скоре [0..1] (attribute)")
	authorIdent := fs.String("author", "", "пакетный режим: прогнать все заметки личности p<id>|u<id>|user_id (attribute)")
	notesList := fs.String("notes", "", "id заметок через запятую — калибровка отпечатка автора (calibrate)")
	suspect := fs.String("suspect", "", "подозреваемый p<id>|u<id>|user_id для проверки авторства (verify)")
	nullN := fs.Int("null", 200, "размер выборки чужих текстов для калибровки порога (verify)")
	minAuthorNotes := fs.Int("min-author-notes", 0, "жанровый фильтр: кандидат должен написать ≥N заметок (attribute/calibrate/verify; 0 — все)")
	genre := fs.String("genre", "", "жанр эталона: all (коммент+заметки) | notes (только заметки, register-matched); дефолт all — для note-эталона добавь -genre notes (build/attribute/calibrate/verify)")
	lexMinTokens := fs.Int("lex-min-tokens", 200, "мин. слов автора для лексического профиля (lexis build)")
	lexDims := fs.Int("lex-dims", 4096, "размерность хэш-вектора слов (lexis build)")
	ensTopK := fs.Int("ens-top-k", 10, "ближайших по стилю с каждой стороны (ensemble)")
	handoffDays := fs.Int("handoff-days", 120, "макс. разрыв спанов для полного веса handoff (ensemble)")
	ensFloor := fs.Float64("ens-floor", 0.5, "мин. композитный вес для записи кандидата (ensemble)")
	maxPersona := fs.Int("max-persona", 20, "гард: компонент крупнее — переклейка, не склеивать (cluster; 0 — без лимита)")
	minDensity := fs.Float64("min-density", 0.30, "гард: для компонент >4 анкет мин. плотность рёбер (cluster; 0 — без проверки)")
	topicsPath := fs.String("topics", "", "JSON с темами лексикона (facts; иначе встроенный набор)")
	minHits := fs.Int("min-hits", 3, "мин. упоминаний темы для факта (facts)")
	minNotes := fs.Int("min-notes", 2, "мин. разных заметок с темой (facts)")
	evidencePer := fs.Int("evidence", 5, "сколько цитат хранить на факт (facts scan)")
	relMinReplies := fs.Int("rel-min-replies", 20, "мин. реплик направленной пары для тона (relations score)")
	candReplies := fs.Int("cand-replies", 500, "ядро кандидатов: сумма реплик пары ≥ (relations candidates)")
	bandMin := fs.Int("band-min", 100, "нижняя граница полосы добора пар (relations candidates)")
	bandTop := fs.Int("band-top", 2000, "поляризованных пар из полосы (relations candidates; 0 — не добирать)")
	exchanges := fs.Int("exchanges", 30, "обменов реплик на пару (relations candidates)")
	reportTop := fs.Int("report-top", 50, "личностей в отчёте «персонажи» (report)")
	activeDays := fs.Int("active-days", 0, "рецент-фильтр по активности за N суток (report/attribute/calibrate/verify; 0 — все)")
	reportHTML := fs.Bool("html", false, "дополнительно собрать красивый characters.html (report)")
	tgUser := fs.Int64("tg-user", 0, "чья сессия сайта для обхода профилей (gender; 0 — admin_tg_user_id)")
	account := fs.String("account", "", "сервисный аккаунт(ы) через запятую вместо сессии админа (gender; первый живой)")
	fromDate := fs.String("from", "", "нижняя граница окна публикации, ISO-дата (anon; replies — обойти только заметки не старше)")
	toDate := fs.String("to", "", "верхняя граница окна публикации, ISO-дата, не включая (anon)")
	minText := fs.Int("min-text", 400, "мин. длина анонимки в знаках — короче не ранжируем (anon)")
	maxComments := fs.Int("max-comments", 0, "пропускать треды длиннее N комментариев — на них страница со всем деревом падает в 500 (replies; 0 — не пропускать)")
	retryFailed := fs.Bool("retry", false, "повторить заметки, на которых страница уже не отдалась (replies)")
	recent := fs.Int("recent", -1, "сколько последних текстов на жанр в замер манеры (voice; 0 — все)")
	samples := fs.Int("samples", -1, "образцов текста в промпт (voice; 0 — только измерения, дословные тексты никуда не уходят)")
	band := fs.Int("band", -1, "held-out текстов под эталонную полосу атрибуции (voice)")
	seed := fs.Int64("seed", 1, "seed детерминированного разреза корпуса на образцы и полосу (voice)")
	topWords := fs.Int("top-words", 0, "характерных слов в карте (voice; 0 — дефолт)")
	topic := fs.String("topic", "", "тема заметки (voice note)")
	replyTo := fs.Int64("reply-to", 0, "id комментария, которому отвечаем (voice comment)")
	solo := fs.Bool("solo", false, "voice: имитировать ОДНУ анкету u<id>, без склейки личности (иначе карта усредняет все склеенные анкеты, в т.ч. чужих эпох)")
	drafts := fs.Int("drafts", 3, "черновиков за один запрос к модели (voice note/comment)")
	rounds := fs.Int("rounds", 2, "запросов максимум: 1 — без обратной связи, потолок 3 (voice note/comment)")
	accept := fs.Float64("accept", 0, "порог квантиля эталонной полосы для приёмки (voice; 0 — дефолт 0.25)")
	maxCopy := fs.Float64("max-copy", 0, "потолок пересечения черновика с образцами (voice; 0 — дефолт 0.30)")
	control := fs.String("control", "", "вторая личность для контроля различающей силы (voice note/comment)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "out": true, "in": true, "limit": true, "min-score": true, "patterns": true,
		"config": true, "workers": true, "interval-ms": true, "max-dist": true, "generic-max": true,
		"min-chars": true, "dims": true, "min-cosine": true, "top-k": true, "max-pairs": true,
		"top": true, "ens-top-k": true, "handoff-days": true, "ens-floor": true,
		"max-persona": true, "min-density": true,
		"topics": true, "min-hits": true, "min-notes": true, "evidence": true,
		"rel-min-replies": true, "cand-replies": true, "band-min": true, "band-top": true,
		"exchanges": true, "report-top": true, "active-days": true, "tg-user": true, "note": true,
		"account": true,
		"lex-weight": true, "lex-min-tokens": true, "lex-dims": true, "author": true, "notes": true,
		"suspect": true, "null": true, "min-author-notes": true, "genre": true,
		"from": true, "to": true, "min-text": true, "max-comments": true,
		"recent": true, "samples": true, "band": true, "seed": true, "top-words": true,
		"topic": true, "reply-to": true, "drafts": true, "rounds": true,
		"accept": true, "max-copy": true, "control": true,
	})); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("personas: не указано действие (flag|candidates|link|cluster|set|avatars)")
	}

	// Жанр эталона: дефолт all (комментарии+заметки, обратная совместимость);
	// -genre notes — register-matched note-only слой.
	gen := *genre
	if gen == "" {
		gen = archive.GenreAll
	}
	if !archive.ValidGenre(gen) {
		return fmt.Errorf("personas: неизвестный -genre %q (all|notes)", *genre)
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
		return personasCluster(ctx, ar, archive.ClusterParams{
			MinScore: *minScore, MaxSize: *maxPersona, MinDensity: *minDensity,
		}, *outDir)
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
			genre: gen,
		})
	case "attribute":
		return personasAttribute(ctx, ar, fs.Args()[1:], attrOpts{
			top: *top, inPath: *inPath, noteID: *noteID, lexWeight: *lexWeight, author: *authorIdent,
			activeDays: *activeDays, minAuthorNotes: *minAuthorNotes, genre: gen,
		})
	case "lexis":
		return personasLexis(ctx, ar, fs.Args()[1:], lexisOpts{
			minTokens: *lexMinTokens, dims: *lexDims, genre: gen,
		})
	case "calibrate":
		return personasCalibrate(ctx, ar, calibOpts{
			notes: *notesList, author: *authorIdent, lexWeight: *lexWeight, top: *top,
			activeDays: *activeDays, minAuthorNotes: *minAuthorNotes, genre: gen,
		})
	case "verify":
		return personasVerify(ctx, ar, fs.Args()[1:], verifyOpts{
			suspect: *suspect, inPath: *inPath, noteID: *noteID, notes: *notesList,
			lexWeight: *lexWeight, nullN: *nullN,
			activeDays: *activeDays, minAuthorNotes: *minAuthorNotes, genre: gen,
		})
	case "anon":
		return personasAnon(ctx, ar, anonOpts{
			suspect: *suspect, genre: gen, lexWeight: *lexWeight,
			activeDays: *activeDays, minAuthorNotes: *minAuthorNotes,
			from: *fromDate, to: *toDate, minText: *minText, top: *top, nullN: *nullN,
		})
	case "portrait":
		return personasPortrait(ctx, ar, fs.Args()[1:], *outDir, *top)
	case "diag":
		return personasDiag(ctx, ar, fs.Args()[1:])
	case "ensemble":
		return personasEnsemble(ctx, ar, archive.EnsembleParams{
			MinCosine: *minCosine, TopK: *ensTopK, HandoffDays: *handoffDays,
			Floor: *ensFloor, MaxPairs: *maxPairs,
		}, *outDir)
	case "facts":
		return personasFacts(ctx, ar, fs.Args()[1:], factOpts{
			outDir: *outDir, inPath: *inPath, topicsPath: *topicsPath,
			minHits: *minHits, minNotes: *minNotes, evidencePer: *evidencePer,
		})
	case "relations":
		return personasRelations(ctx, ar, fs.Args()[1:], relOpts{
			outDir: *outDir, inPath: *inPath, minReplies: *relMinReplies,
			candReplies: *candReplies, bandMin: *bandMin, bandTop: *bandTop, exchanges: *exchanges,
		})
	case "replies":
		return personasReplies(ctx, ar, repliesOpts{
			cfgPath: *cfgPath, since: *fromDate, limit: *limit,
			maxComments: *maxComments, retry: *retryFailed, refresh: *refresh,
		})
	case "voice":
		return personasVoice(ctx, ar, fs.Args()[1:], voiceOpts{
			cfgPath: *cfgPath, outDir: *outDir, genre: gen, genreSet: *genre != "",
			recent: *recent, samples: *samples, band: *band, seed: *seed, topWords: *topWords,
			topic: *topic, replyTo: *replyTo, noteID: *noteID, solo: *solo,
			drafts: *drafts, rounds: *rounds,
			accept: *accept, maxCopy: *maxCopy, control: *control,
			lexWeight: *lexWeight, activeDays: *activeDays, minAuthorNotes: *minAuthorNotes,
		})
	case "addressees":
		return personasAddressees(ctx, ar)
	case "report":
		return personasReport(ctx, ar, *outDir, *reportTop, *activeDays, *reportHTML)
	case "gender":
		return personasGender(ctx, ar, genderOpts{
			cfgPath: *cfgPath, account: *account, tgUser: *tgUser, activeDays: *activeDays,
			reportTop: *reportTop, limit: *limit,
		})
	default:
		return fmt.Errorf("personas: неизвестное действие %q (flag|candidates|link|cluster|set|avatars|stylometry|lexis|attribute|calibrate|verify|portrait|diag|ensemble|facts|relations|replies|addressees|report|gender|voice)", action)
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
func personasCluster(ctx context.Context, ar *archive.Store, p archive.ClusterParams, outDir string) error {
	clusters, dropped, err := ar.ClusterPersonas(ctx, p, time.Now())
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
		len(clusters), members, p.MinScore, reportPath)
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "гард: отклонено компонент %d (переклейка через хаб; анкеты остаются раздельными):\n", len(dropped))
		for i, d := range dropped {
			if i >= 5 {
				fmt.Fprintf(os.Stderr, "  … ещё %d\n", len(dropped)-5)
				break
			}
			fmt.Fprintf(os.Stderr, "  %d анкет, плотность %.2f (рёбер %d)\n", d.Size, d.Density, d.Edges)
		}
	}
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

// genreLabel — человекочитаемое описание жанра эталона для логов.
func genreLabel(genre string) string {
	if genre == archive.GenreNotes {
		return "только заметки (register-matched)"
	}
	return "комментарии+заметки"
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
