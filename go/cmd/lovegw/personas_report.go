package main

import (
	"context"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"lovegw/internal/archive"
)

// personasReport — итоговый отчёт «персонажи»: личности с интересами
// (identity_facts) и отношениями (v_relations). Прямой ответ на «кто с кем
// дружит, кто любит собак»: тип отношений — только из LLM/ручной разметки,
// сырой тон подписан как тональность. activeDays>0 — отобрать только тех, кто
// публиковался в последние N суток (окно от самой свежей даты в архиве), иначе
// — все личности. topN>0 — ограничить самыми активными. htmlOut — дополнительно
// собрать красивый characters.html (тот же контент, что characters.md).
func personasReport(ctx context.Context, ar *archive.Store, outDir string, topN, activeDays int, htmlOut bool) error {
	facts, err := ar.AllFacts(ctx)
	if err != nil {
		return err
	}
	var (
		list        []archive.GraphNode
		weekly      map[string]int // сообщений за окно — для отбора/сортировки когорты
		weeklyNotes map[string]int // заметок за окно — для показа
		scope       = "самых активных"
		sinceNote   string
		full        int
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
		if weeklyNotes, err = ar.ActiveNotesSince(ctx, since); err != nil {
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

	titles := topicTitles(archive.DefaultTopics())
	m := reportModel{TitleSuffix: sinceNote, Total: full, Scope: scope, ActiveDays: activeDays}
	m.Window = strings.TrimSuffix(strings.TrimPrefix(sinceNote, " ("), ")")
	for i, nd := range list {
		cv, err := gatherChar(ctx, ar, i+1, nd, facts[nd.Identity], titles, weekly, weeklyNotes)
		if err != nil {
			return err
		}
		m.Chars = append(m.Chars, cv)
	}
	m.Shown = len(m.Chars)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	mdPath := filepath.Join(outDir, "characters.md")
	if err := os.WriteFile(mdPath, []byte(renderMarkdown(m)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "report: %d персонажей → %s\n", m.Shown, mdPath)
	if htmlOut {
		html, err := renderHTML(m)
		if err != nil {
			return err
		}
		htmlPath := filepath.Join(outDir, "characters.html")
		if err := os.WriteFile(htmlPath, []byte(html), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "report: HTML → %s\n", htmlPath)
	}
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

// --- модель отчёта (общая для markdown и HTML рендеров) ---

// reportModel — вся выборка отчёта: шапка + список персонажей. Строится один
// раз, затем рендерится и в Markdown, и в HTML (одни числа, разное оформление).
type reportModel struct {
	TitleSuffix string      // суффикс заголовка md: " (активность за последние N сут., с DATE)"
	Window      string      // то же для HTML без скобок, "" если окно не задано
	Total       int         // личностей в выборке (всего активных за окно)
	Shown       int         // показано (после -report-top)
	Scope       string      // человекочитаемая приписка про отбор
	ActiveDays  int         // окно активности (0 — все)
	Chars       []charView
}

// charView — один персонаж: идентификация, профиль, интересы, отношения.
type charView struct {
	N           int
	Label       string
	Identity    string
	Kind        string // "анкета" | "личность из N анкет"
	GenderRu    string // "муж" | "жен" | "" — для markdown
	GenderSym   string // "♂" | "♀" | "" — для HTML-бейджа
	GenderKey   string // "male" | "female" | "" — ключ CSS-класса
	Age            string // "52 года" или ""
	AvatarURL      string
	Initial        string // первая буква имени — фолбэк-аватар
	AvaColor       string // цвет фолбэк-кружка (hsl из хэша identity)
	WeeklyComments int    // комментариев за окно
	WeeklyNotes    int    // заметок за окно
	Interests      []interestView
	Friends     []relView
	Flirts      []relView
	Conflicts   []relView
	Warm        []toneView // тёплые по тону
	Cold        []toneView // колючие по тону
}

// interestView — одна тема интереса с полярностью и (для размеченных) цитатой.
type interestView struct {
	Title    string
	Polarity string // русская подпись
	PolKey   string // likes|dislikes|owns|mentions — ключ CSS-класса
	Hits     int
	Notes    int
	Quote    string
}

// relView — размеченная связь (дружба/флирт/конфликт) с уверенностью.
type relView struct {
	Label   string
	To      string
	Replies int
	Conf    float64
}

// toneView — пара по сырому тону (тёплая/колючая).
type toneView struct {
	Label   string
	To      string
	Replies int
	Tone    float64
}

// gatherChar собирает view-модель одного персонажа (без вывода): профиль
// (пол/возраст/аватар), интересы (до 6 сильнейших) и отношения по типам и тону.
func gatherChar(ctx context.Context, ar *archive.Store, n int, nd archive.GraphNode, facts []archive.IdentityFact, titles map[string]string, weekly, weeklyNotes map[string]int) (charView, error) {
	notes := weeklyNotes[nd.Identity]
	comments := weekly[nd.Identity] - notes // weekly = комментарии + заметки
	if comments < 0 {
		comments = 0
	}
	cv := charView{
		N: n, Label: nd.Label, Identity: nd.Identity,
		GenderRu: genderRu(nd.Gender), GenderSym: genderSym(nd.Gender), GenderKey: nd.Gender,
		Age: nd.Age, AvatarURL: nd.AvatarURL,
		Initial: avatarInitial(nd.Label), AvaColor: avatarColor(nd.Identity),
		WeeklyComments: comments, WeeklyNotes: notes,
		Kind: "анкета",
	}
	if nd.IsPersona {
		cv.Kind = fmt.Sprintf("личность из %d анкет", nd.Accounts)
	}
	if len(facts) > 6 {
		facts = facts[:6]
	}
	for _, f := range facts {
		title := titles[f.Topic]
		if title == "" {
			title = f.Topic
		}
		iv := interestView{
			Title: title, Polarity: polarityRu(f.Polarity), PolKey: polKey(f.Polarity),
			Hits: f.Hits, Notes: f.NotesCount,
		}
		if q := factQuote(f); q != "" && f.Polarity != archive.PolarityMentions {
			iv.Quote = q
		}
		cv.Interests = append(cv.Interests, iv)
	}

	rels, err := ar.IdentityRelations(ctx, nd.Identity, 0)
	if err != nil {
		return cv, err
	}
	var toned []archive.RelationRow
	for _, r := range rels {
		switch r.Kind {
		case archive.KindTone:
			toned = append(toned, r)
		case archive.KindFriendship:
			cv.Friends = append(cv.Friends, relView{r.Label, r.To, r.Replies, r.Score})
		case archive.KindFlirt:
			cv.Flirts = append(cv.Flirts, relView{r.Label, r.To, r.Replies, r.Score})
		case archive.KindConflict:
			cv.Conflicts = append(cv.Conflicts, relView{r.Label, r.To, r.Replies, r.Score})
		}
	}
	warm, cold := splitByTone(toned)
	for _, r := range warm {
		cv.Warm = append(cv.Warm, toneView{r.Label, r.To, r.Replies, r.Tone})
	}
	for _, r := range cold {
		cv.Cold = append(cv.Cold, toneView{r.Label, r.To, r.Replies, r.Tone})
	}
	return cv, nil
}

// polKey нормализует полярность к безопасному ключу CSS-класса.
func polKey(p string) string {
	switch p {
	case archive.PolarityLikes, archive.PolarityDislikes, archive.PolarityOwns:
		return p
	default:
		return archive.PolarityMentions
	}
}

// genderRu — короткая подпись пола для markdown ("" — пол не размечен).
func genderRu(g string) string {
	switch g {
	case "male":
		return "муж"
	case "female":
		return "жен"
	default:
		return ""
	}
}

// genderSym — символ пола для HTML-бейджа ("" — пол не размечен).
func genderSym(g string) string {
	switch g {
	case "male":
		return "♂"
	case "female":
		return "♀"
	default:
		return ""
	}
}

// avatarInitial — первая буква имени (заглавная) для фолбэк-кружка аватара.
func avatarInitial(name string) string {
	for _, r := range strings.TrimSpace(name) {
		return string(unicode.ToUpper(r))
	}
	return "?"
}

// avatarColor — детерминированный цвет фолбэк-кружка из identity (FNV-1a → hue).
func avatarColor(seed string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	return fmt.Sprintf("hsl(%d 48%% 42%%)", h%360)
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

// --- Markdown-рендер (совместим с прежним форматом characters.md) ---

func renderMarkdown(m reportModel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Персонажи заметок%s\n\nЛичностей в выборке: %d; показаны %d %s.\n",
		m.TitleSuffix, m.Total, m.Shown, m.Scope)
	for _, c := range m.Chars {
		fmt.Fprintf(&b, "\n## %d. %s (%s)\n\n", c.N, c.Label, c.Identity)
		meta := []string{c.Kind}
		if c.GenderRu != "" {
			meta = append(meta, c.GenderRu)
		}
		if c.Age != "" {
			meta = append(meta, c.Age)
		}
		if m.ActiveDays > 0 {
			meta = append(meta, fmt.Sprintf("за %d сут.: %d комм., %d зам.", m.ActiveDays, c.WeeklyComments, c.WeeklyNotes))
		}
		fmt.Fprintf(&b, "%s\n", strings.Join(meta, " · "))

		if len(c.Interests) > 0 {
			fmt.Fprintf(&b, "\n**Интересы:**\n\n")
			for _, iv := range c.Interests {
				fmt.Fprintf(&b, "- %s — %s (%d упоминаний в %d заметках)", iv.Title, iv.Polarity, iv.Hits, iv.Notes)
				if iv.Quote != "" {
					fmt.Fprintf(&b, " — «%s»", iv.Quote)
				}
				fmt.Fprintln(&b)
			}
		}
		writeRelMd(&b, "Дружит с", c.Friends)
		writeRelMd(&b, "Флирт с", c.Flirts)
		writeRelMd(&b, "Конфликтует с", c.Conflicts)
		if len(c.Warm) > 0 {
			fmt.Fprintf(&b, "\n**Тёплые по тону:** %s\n", joinToneMd(c.Warm))
		}
		if len(c.Cold) > 0 {
			fmt.Fprintf(&b, "\n**Колючие по тону** (тон ≠ вражда — возможна дружеская пикировка)**:** %s\n",
				joinToneMd(c.Cold))
		}
	}
	return b.String()
}

func writeRelMd(b *strings.Builder, title string, list []relView) {
	if len(list) == 0 {
		return
	}
	parts := make([]string, 0, len(list))
	for _, r := range list {
		parts = append(parts, fmt.Sprintf("%s (%s, ×%d, conf %.2f)", r.Label, r.To, r.Replies, r.Conf))
	}
	fmt.Fprintf(b, "\n**%s:** %s\n", title, strings.Join(parts, ", "))
}

func joinToneMd(list []toneView) string {
	parts := make([]string, 0, len(list))
	for _, r := range list {
		parts = append(parts, fmt.Sprintf("%s (%s, ×%d, %+.2f)", r.Label, r.To, r.Replies, r.Tone))
	}
	return strings.Join(parts, ", ")
}

// --- HTML-рендер: самодостаточная страница (инлайн-CSS, без внешних ресурсов) ---

func renderHTML(m reportModel) (string, error) {
	t, err := template.New("report").Parse(reportHTMLTemplate)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, m); err != nil {
		return "", err
	}
	return b.String(), nil
}

const reportHTMLTemplate = `<!doctype html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Персонажи заметок</title>
<style>
:root{
  --bg:#eef0f4; --card:#fff; --ink:#161a1e; --muted:#66707c; --line:#e4e7ec;
  --accent:#5b60e0; --shadow:0 1px 2px rgba(16,24,40,.05),0 6px 20px rgba(16,24,40,.07);
  --likes:#128a3f; --likes-bg:#dcfce7; --dislikes:#c81e1e; --dislikes-bg:#fde4e4;
  --owns:#6d28d9; --owns-bg:#ece7fd; --mentions:#5b6470; --mentions-bg:#eef1f4;
  --friend:#0b8c80; --friend-bg:#cbf6ef; --flirt:#c81e78; --flirt-bg:#fce0ee;
  --conflict:#c81e1e; --conflict-bg:#fde4e4; --warm:#d1580f; --warm-bg:#ffe8d4;
  --cold:#0a72c0; --cold-bg:#dcf0fc;
}
@media (prefers-color-scheme:dark){
:root{
  --bg:#0c1016; --card:#151b23; --ink:#e6edf3; --muted:#8b96a3; --line:#28303b;
  --accent:#8b8ff5; --shadow:0 1px 2px rgba(0,0,0,.4),0 10px 28px rgba(0,0,0,.35);
  --likes:#54d98c; --likes-bg:#0a2f1a; --dislikes:#f98a8a; --dislikes-bg:#3d1114;
  --owns:#b49bfb; --owns-bg:#241748; --mentions:#9aa5b1; --mentions-bg:#1d242d;
  --friend:#3fd9c8; --friend-bg:#082e2b; --flirt:#f579b3; --flirt-bg:#3d0f28;
  --conflict:#f98a8a; --conflict-bg:#3d1114; --warm:#fb9a52; --warm-bg:#3a1c0a;
  --cold:#54b6f5; --cold-bg:#0a2a42;
}
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);
  font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;
  line-height:1.5;-webkit-font-smoothing:antialiased}
.wrap{max-width:960px;margin:0 auto;padding:32px 20px 64px}
.head{margin-bottom:28px}
.head h1{margin:0 0 6px;font-size:30px;font-weight:800;letter-spacing:-.02em}
.head .sub{margin:0 0 12px;color:var(--muted);font-size:15px}
.head .meta{margin:0;font-size:14px;color:var(--muted)}
.head .meta b{color:var(--ink)}
.card{background:var(--card);border:1px solid var(--line);border-radius:16px;
  box-shadow:var(--shadow);padding:20px 22px;margin-bottom:18px}
.card-head{display:flex;align-items:center;gap:13px}
.rank{flex:0 0 auto;width:24px;text-align:center;font-weight:700;font-size:14px;
  color:var(--muted);font-variant-numeric:tabular-nums}
.ava{flex:0 0 auto;position:relative;width:54px;height:54px;border-radius:50%;
  display:flex;align-items:center;justify-content:center;color:#fff;font-weight:700;
  font-size:21px;overflow:hidden;user-select:none;box-shadow:0 0 0 1px rgba(0,0,0,.06) inset}
.ava img{position:absolute;inset:0;width:100%;height:100%;object-fit:cover}
.who{flex:1 1 auto;min-width:0}
.name-row{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.who h2{margin:0;font-size:20px;font-weight:700;letter-spacing:-.01em;
  overflow-wrap:anywhere}
.gender{flex:0 0 auto;width:19px;height:19px;border-radius:50%;display:inline-flex;
  align-items:center;justify-content:center;font-size:12px;line-height:1;color:#fff}
.gender--male{background:#3b82f6}
.gender--female{background:#ec4899}
.age{font-size:13px;color:var(--muted);font-weight:500}
.tags{margin-top:4px;display:flex;flex-wrap:wrap;gap:6px;align-items:center}
.tags .id{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  font-size:12px;color:var(--muted);background:var(--mentions-bg);
  padding:2px 7px;border-radius:6px}
.tags .kind{font-size:12.5px;color:var(--muted)}
.stats{display:flex;flex-wrap:wrap;gap:10px;margin:16px 0 4px}
.stat{flex:1 1 auto;min-width:96px;background:var(--mentions-bg);border-radius:10px;
  padding:9px 12px;display:flex;flex-direction:column;gap:1px}
.stat .v{font-weight:700;font-size:16px;font-variant-numeric:tabular-nums}
.stat .l{font-size:11.5px;color:var(--muted);text-transform:uppercase;letter-spacing:.03em}
.stat.hot .v{color:var(--warm)}
.stat.span .v{font-size:13px;font-weight:600;color:var(--muted)}
.block{margin-top:16px}
.block-title{font-size:12px;font-weight:700;text-transform:uppercase;letter-spacing:.05em;
  color:var(--muted);margin-bottom:9px}
.chips{display:flex;flex-wrap:wrap;gap:7px}
.chip{font-size:13px;padding:4px 10px;border-radius:999px;display:inline-flex;
  align-items:center;gap:6px;border:1px solid transparent}
.chip b{font-weight:600}
.chip .c{font-size:11.5px;opacity:.7;font-variant-numeric:tabular-nums}
.chip--likes{color:var(--likes);background:var(--likes-bg)}
.chip--dislikes{color:var(--dislikes);background:var(--dislikes-bg)}
.chip--owns{color:var(--owns);background:var(--owns-bg)}
.chip--mentions{color:var(--mentions);background:var(--mentions-bg)}
.rel{display:flex;align-items:baseline;gap:8px;margin:8px 0;flex-wrap:wrap}
.rel-ic{flex:0 0 auto;font-size:15px;line-height:1}
.rel-lb{flex:0 0 auto;font-size:13px;font-weight:600;color:var(--muted);min-width:82px}
.pills{display:flex;flex-wrap:wrap;gap:6px;flex:1 1 auto}
.pill{font-size:12.5px;padding:3px 9px;border-radius:8px;display:inline-flex;
  align-items:center;gap:5px;background:var(--mentions-bg);color:var(--ink)}
.pill .m{font-size:11px;opacity:.65;font-variant-numeric:tabular-nums}
.pill--friend{background:var(--friend-bg);color:var(--friend)}
.pill--flirt{background:var(--flirt-bg);color:var(--flirt)}
.pill--conflict{background:var(--conflict-bg);color:var(--conflict)}
.pill--warm{background:var(--warm-bg);color:var(--warm)}
.pill--cold{background:var(--cold-bg);color:var(--cold)}
.note{margin:6px 0 0;font-size:12px;color:var(--muted);font-style:italic}
.foot{margin-top:32px;text-align:center;color:var(--muted);font-size:12.5px}
</style>
</head>
<body>
<div class="wrap">
  <header class="head">
    <h1>Персонажи заметок</h1>
    {{if .Window}}<p class="sub">{{.Window}}</p>{{end}}
    <p class="meta">Личностей в выборке: <b>{{.Total}}</b> · показаны <b>{{.Shown}}</b> {{.Scope}}</p>
  </header>
  {{range .Chars}}
  <section class="card">
    <div class="card-head">
      <div class="rank">{{.N}}</div>
      <div class="ava" style="background:{{.AvaColor}}">{{.Initial}}{{if .AvatarURL}}<img src="{{.AvatarURL}}" alt="" loading="lazy" referrerpolicy="no-referrer" onerror="this.remove()">{{end}}</div>
      <div class="who">
        <div class="name-row">
          <h2>{{.Label}}</h2>
          {{if .GenderSym}}<span class="gender gender--{{.GenderKey}}" title="{{.GenderRu}}">{{.GenderSym}}</span>{{end}}
          {{if .Age}}<span class="age">{{.Age}}</span>{{end}}
        </div>
        <div class="tags"><span class="id">{{.Identity}}</span><span class="kind">{{.Kind}}</span></div>
      </div>
    </div>
    {{if $.ActiveDays}}
    <div class="stats">
      <div class="stat hot"><span class="v">{{.WeeklyComments}}</span><span class="l">комментариев за {{$.ActiveDays}} сут.</span></div>
      <div class="stat"><span class="v">{{.WeeklyNotes}}</span><span class="l">заметок за {{$.ActiveDays}} сут.</span></div>
    </div>
    {{end}}
    {{if .Interests}}
    <div class="block">
      <div class="block-title">Интересы</div>
      <div class="chips">
        {{range .Interests}}<span class="chip chip--{{.PolKey}}"{{if .Quote}} title="{{.Quote}}"{{end}}><b>{{.Title}}</b> — {{.Polarity}} <span class="c">{{.Hits}}/{{.Notes}}</span></span>{{end}}
      </div>
    </div>
    {{end}}
    {{if or .Friends .Flirts .Conflicts}}
    <div class="block">
      {{if .Friends}}<div class="rel"><span class="rel-ic">🤝</span><span class="rel-lb">Дружит с</span><span class="pills">{{range .Friends}}<span class="pill pill--friend">{{.Label}} <span class="m">×{{.Replies}} · {{printf "%.2f" .Conf}}</span></span>{{end}}</span></div>{{end}}
      {{if .Flirts}}<div class="rel"><span class="rel-ic">💘</span><span class="rel-lb">Флирт с</span><span class="pills">{{range .Flirts}}<span class="pill pill--flirt">{{.Label}} <span class="m">×{{.Replies}} · {{printf "%.2f" .Conf}}</span></span>{{end}}</span></div>{{end}}
      {{if .Conflicts}}<div class="rel"><span class="rel-ic">⚔️</span><span class="rel-lb">Конфликт с</span><span class="pills">{{range .Conflicts}}<span class="pill pill--conflict">{{.Label}} <span class="m">×{{.Replies}} · {{printf "%.2f" .Conf}}</span></span>{{end}}</span></div>{{end}}
    </div>
    {{end}}
    {{if or .Warm .Cold}}
    <div class="block">
      {{if .Warm}}<div class="rel"><span class="rel-ic">🔥</span><span class="rel-lb">Тёплые</span><span class="pills">{{range .Warm}}<span class="pill pill--warm">{{.Label}} <span class="m">×{{.Replies}} · {{printf "%+.2f" .Tone}}</span></span>{{end}}</span></div>{{end}}
      {{if .Cold}}<div class="rel"><span class="rel-ic">❄️</span><span class="rel-lb">Колючие</span><span class="pills">{{range .Cold}}<span class="pill pill--cold">{{.Label}} <span class="m">×{{.Replies}} · {{printf "%+.2f" .Tone}}</span></span>{{end}}</span></div>{{end}}
      <p class="note">Тон ≠ вражда — возможна дружеская пикировка.</p>
    </div>
    {{end}}
  </section>
  {{end}}
  <footer class="foot">Сгенерировано lovegw · personas report</footer>
</div>
</body>
</html>
`
