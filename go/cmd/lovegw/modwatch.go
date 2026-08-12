package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/modwatch"
	"lovegw/internal/store"
)

// defaultModwatchPath — БД наблюдателя, отдельно и от боевой, и от архива.
const (
	defaultModwatchPath = "data/modwatch.db"
	modwatchDBUsage     = "путь к modwatch.db"
	flagAgeMin          = "age-min"
	flagAgeMax          = "age-max"
	momentUsage         = "момент: 2026-08-05, «2026-08-05 14:30» или длительность назад (72h)"
)

// cmdModwatch — наблюдение за действиями модерации: сборщик пишет моменты
// удалений/одобрений, отчёт сверяет их с присутствием людей на площадке.
func cmdModwatch(ctx context.Context, args []string) error {
	// Подкоманда может стоять и после флагов (`modwatch -db … watch`), поэтому
	// ищем её по имени, а не по позиции.
	sub, rest := "", make([]string, 0, len(args))
	for _, a := range args {
		if sub == "" && modwatchSubcommands[a] {
			sub = a
			continue
		}
		rest = append(rest, a)
	}
	switch sub {
	case "watch":
		return modwatchWatch(ctx, rest)
	case "report":
		return modwatchReport(ctx, rest)
	case "events":
		return modwatchEvents(ctx, rest)
	case "status":
		return modwatchStatus(ctx, rest)
	case "bans":
		return modwatchBans(ctx, rest)
	case "guests":
		return modwatchGuests(ctx, rest)
	default:
		usage()
		return fmt.Errorf("modwatch: нужна подкоманда (watch|report|events|status|bans|guests)")
	}
}

var modwatchSubcommands = map[string]bool{
	"watch": true, "report": true, "events": true, "status": true,
	"bans": true, "guests": true,
}

// modwatchBans — запреты, выведенные из ритма жертв. Наблюдать бан нечем: он
// ничего не убирает с площадки, поэтому единственный его след — молчание ровно
// на срок с возвратом сразу после снятия.
func modwatchBans(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch bans", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	tolerance := fs.Duration("tolerance", modwatch.DefaultBanTolerance, "допустимое опоздание возврата после снятия")
	minAround := fs.Int("min-around", modwatch.DefaultBanMinAround, "сколько реплик должно быть по обе стороны паузы")
	maxWindow := fs.Duration("max-window", 0, "показывать только запреты с окном не шире (0 — все)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "tolerance": true, "min-around": true, "max-window": true,
	})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	bans, err := store.Bans(ctx, modwatch.BanOptions{Tolerance: *tolerance, MinAround: *minAround})
	if err != nil {
		return err
	}
	names, err := store.Names(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "окно запрета\tширина\tсрок\tопоздание\tанкета\tник")
	shown := 0
	for _, b := range bans {
		width := b.To.Sub(b.From)
		if *maxWindow > 0 && width > *maxWindow {
			continue
		}
		shown++
		fmt.Fprintf(tw, "%s … %s\t%s\t%s\t%s\tu%d\t%s\n",
			fmtTime(b.From), fmtTime(b.To), fmtDur(width), fmtDur(b.Tier), fmtDur(b.Delay()),
			b.UserID, names[b.UserID])
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nнайдено кандидатов: %d (показано %d)\n", len(bans), shown)
	fmt.Println(`Окно — [последняя реплика перед молчанием; возврат минус срок]: раньше первого
запретить не могли, позже второго человек не смог бы вернуться вовремя.

ЭТО КАНДИДАТЫ, А НЕ НАХОДКИ. Естественных суточных пауз столько, что подпись в
них тонет: на замере 12.08.2026 подставной срок в 20 ч дал столько же строк,
сколько настоящие сутки, а в архиве за 2025–2026 горба сразу за отметкой нет
вовсе. Список полезен, только когда запрет известен со стороны: тогда он
восстанавливает окно наложения с точностью до десятков минут.`)
	return nil
}

// guestSource — адаптер *love.Client под modwatch.GuestSource: список гостей
// читается только под своей сессией, поэтому куки живут прямо в замыкании.
type guestSource struct {
	c       *love.Client
	cookies []*http.Cookie
}

func (g guestSource) Guests(ctx context.Context, page int) ([]love.Guest, error) {
	return g.c.Guests(ctx, g.cookies, page)
}

// modwatchGuests — визиты в свою анкету: единственный след, который оставляет
// модератор. Наказывает он молча, но перед этим открывает анкету (бан
// 12.08.2026 в 19:38 и визит Гадёныша в 19:38 — минута в минуту). Снимать
// приходится регулярно: сайт держит одну строку на человека и при повторном
// визите затирает прежнюю.
func modwatchGuests(ctx context.Context, args []string) error {
	sub := "watch"
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "watch" || a == "log" {
			sub = a
			continue
		}
		rest = append(rest, a)
	}
	fs := flag.NewFlagSet("modwatch guests", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	messenger := fs.String("messenger", "telegram", "чья сессия: telegram|max")
	userID := fs.Int64("user", 0, "id пользователя в мессенджере (0 — админ из конфига)")
	interval := fs.Duration("interval", modwatch.DefaultGuestInterval, "период снятия списка")
	pages := fs.Int("pages", modwatch.DefaultGuestPages, "сколько страниц списка обходить")
	once := fs.Bool("once", false, "снять один раз и выйти")
	near := fs.String("near", "", "показать визиты вокруг момента — "+momentUsage)
	window := fs.Duration("window", 30*time.Minute, "полуширина окна для -near")
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	if err := fs.Parse(reorderArgs(rest, map[string]bool{
		"config": true, "db": true, "messenger": true, "user": true,
		"interval": true, "pages": true, "near": true, "window": true, "since": true,
	})); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	db, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if sub == "log" {
		return modwatchGuestsLog(ctx, db, *since, *near, *window)
	}

	cookies, err := adminCookies(ctx, cfg, *messenger, *userID)
	if err != nil {
		return err
	}
	w := &modwatch.GuestWatcher{
		Source: guestSource{c: love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
			time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log), cookies: cookies},
		Store: db, Log: log, Interval: *interval, Pages: *pages,
	}
	if *once {
		n, err := w.Poll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("снято, новых визитов: %d\n", n)
		return nil
	}
	log.Info("наблюдение за гостями запущено", "db", *dbPath, "период", *interval)
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// adminCookies достаёт сессию сайта владельца анкеты из боевой БД.
func adminCookies(ctx context.Context, cfg *config.Config, messenger string, userID int64) ([]*http.Cookie, error) {
	if userID == 0 {
		if cfg.Messengers != nil {
			switch messenger {
			case "max":
				userID = cfg.Messengers.Max.AdminUserID
			default:
				userID = cfg.Messengers.Telegram.AdminUserID
			}
		}
		if userID == 0 {
			userID = cfg.AdminTGUserID
		}
	}
	if userID == 0 {
		return nil, fmt.Errorf("не задан пользователь: ни -user, ни admin_user_id в конфиге")
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("боевая БД %s: %w", cfg.DBPath, err)
	}
	defer st.Close()
	raw, valid, err := st.SessionCookies(ctx, messenger, userID)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("сессия %s/%d помечена недействительной — нужен /login", messenger, userID)
	}
	return love.CookiesFromJSON([]byte(raw), time.Now())
}

// modwatchGuestsLog печатает накопленные визиты; с -near — только вокруг
// указанной минуты, то есть готовый список кандидатов на исполнителя.
func modwatchGuestsLog(ctx context.Context, db *modwatch.Store, since, near string, window time.Duration) error {
	from, err := parseMoment(since)
	if err != nil {
		return err
	}
	var to time.Time
	if near != "" {
		at, err := parseMoment(near)
		if err != nil {
			return err
		}
		if at.IsZero() {
			return fmt.Errorf("-near: не разобран момент %q", near)
		}
		from, to = at.Add(-window), at.Add(window)
		fmt.Printf("визиты в окне %s … %s\n\n", fmtTime(from), fmtTime(to))
	}
	visits, err := db.VisitsIn(ctx, from, to)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "визит (Нск)\tанкета\tник\tснято")
	for _, v := range visits {
		fmt.Fprintf(tw, "%s\tu%d\t%s\t%s\n", fmtTime(v.VisitedAt), v.UserID, v.Nick, fmtTime(v.FirstSeen))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nвсего визитов: %d\n", len(visits))
	return nil
}

// modwatchSite — адаптер *love.Client под modwatch.Site.
type modwatchSite struct {
	c       *love.Client
	baseURL string
}

func (m modwatchSite) Feed(ctx context.Context) ([]love.Note, error) {
	raw, err := m.c.RawNotesPage(ctx, 1)
	if err != nil {
		return nil, err
	}
	return love.ParseNotes(bytes.NewReader(raw))
}

func (m modwatchSite) Thread(ctx context.Context, noteID string, page int) ([]love.Comment, *love.Note, error) {
	raw, err := m.c.RawCommentsView(ctx, noteID, page, love.ViewLinear)
	if err != nil {
		return nil, nil, err
	}
	comments, err := love.ParseComments(bytes.NewReader(raw), m.baseURL)
	if err != nil {
		return nil, nil, err
	}
	var header *love.Note
	if n, err := love.ParseNoteFromCommentsPage(bytes.NewReader(raw), m.baseURL); err == nil {
		header = &n
	}
	return comments, header, nil
}

func modwatchWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch watch", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	feedInterval := fs.Duration("feed-interval", modwatch.DefaultFeedInterval, "период опроса ленты")
	threadInterval := fs.Duration("thread-interval", modwatch.DefaultThreadInterval, "минимальный период опроса одного треда")
	window := fs.Duration("window", modwatch.DefaultWindow, "сколько заметка считается активной после первой встречи")
	depth := fs.Duration("depth", modwatch.DefaultDepth, "насколько вглубь треда листать по времени комментариев")
	maxThreads := fs.Int("max-threads", modwatch.DefaultMaxThreads, "сколько тредов опрашивать за тик")
	maxPages := fs.Int("pages", modwatch.DefaultMaxPages, "предел страниц комментариев на тред")
	once := fs.Bool("once", false, "один проход и выход (проверка настройки)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "feed-interval": true, "thread-interval": true,
		"window": true, "depth": true, "max-threads": true, "pages": true,
	})); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	w := &modwatch.Watcher{
		Site:           modwatchSite{c: client, baseURL: cfg.Site.BaseURL},
		Store:          store,
		Log:            log,
		FeedInterval:   *feedInterval,
		ThreadInterval: *threadInterval,
		Window:         *window,
		Depth:          *depth,
		MaxThreads:     *maxThreads,
		MaxPages:       *maxPages,
	}
	if *once {
		if err := w.Poll(ctx); err != nil {
			return err
		}
		return printCounts(ctx, store)
	}
	log.Info("наблюдение запущено", "db", *dbPath, "лента", feedInterval.String(), "тред", threadInterval.String())
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return printCounts(ctx, store)
}

func modwatchStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch status", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	if err := fs.Parse(reorderArgs(args, map[string]bool{"db": true})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	return printCounts(ctx, store)
}

func printCounts(ctx context.Context, store *modwatch.Store) error {
	c, err := store.Counts(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("наблюдение: %s — %s\n", fmtTime(c.FirstSeen), fmtTime(c.LastSeen))
	fmt.Printf("заметок %d (исчезло %d), комментариев %d (исчезло %d), событий %d, анкет %d\n",
		c.Notes, c.NotesGone, c.Comments, c.CommentsGone, c.Events, c.Users)
	return nil
}

func modwatchEvents(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch events", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	until := fs.String("until", "", "по какой момент — "+momentUsage)
	kinds := fs.String("kind", "", "виды событий через запятую (пусто — все)")
	minAge := fs.Duration(flagAgeMin, 0, "только события над объектом старше указанного возраста")
	maxAge := fs.Duration(flagAgeMax, 0, "только события над объектом моложе указанного возраста (проверка таймером)")
	limit := fs.Int("limit", 200, "сколько строк показать")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "since": true, "until": true, "kind": true,
		flagAgeMin: true, flagAgeMax: true, "limit": true,
	})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	from, err := parseMoment(*since)
	if err != nil {
		return err
	}
	to, err := parseMoment(*until)
	if err != nil {
		return err
	}
	events, err := store.Events(ctx, modwatch.EventFilter{
		Since: from, Until: to, Kinds: splitKinds(*kinds),
		MinAge: *minAge, MaxAge: *maxAge, Limit: *limit,
	})
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "когда (±)\tвид\tвозраст\tтишина\tреплик\tзаметка\tобъект\tчто")
	for _, e := range events {
		fmt.Fprintf(tw, "%s (±%s)\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			fmtTime(e.DetectedAt), e.DetectedAt.Sub(e.PrevSeen).Round(time.Second),
			e.Kind, fmtDur(e.Age), fmtDur(e.Idle), e.Size, e.NoteID, e.RefID, e.Details)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nвсего событий: %d\n", len(events))
	fmt.Println(`«возраст» — сколько прожил объект до действия, «тишина» — сколько не писали
в треде перед ним, «реплик» — сколько их там к тому моменту знал наблюдатель
(оценка снизу: глубже охвата он не смотрит). Автоматика повторяется по одному и
тому же признаку — по возрасту или по числу реплик; выпавшее из этой кучи и есть
рука. Отсечь таймер: -age-max <срок минус запас>.`)
	return nil
}

func modwatchReport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("modwatch report", flag.ExitOnError)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	until := fs.String("until", "", "по какой момент — "+momentUsage)
	kinds := fs.String("kind", strings.Join(moderationKinds, ","), "виды событий через запятую")
	minAge := fs.Duration(flagAgeMin, 0, "только события над объектом старше указанного возраста")
	maxAge := fs.Duration(flagAgeMax, 0, "только события над объектом моложе указанного возраста (отсечь автоматику по таймеру)")
	window := fs.Duration("presence-window", modwatch.DefaultPresenceWindow, "расширение окна события в обе стороны")
	controls := fs.Int("controls", modwatch.DefaultControls, "контрольных окон на событие")
	seed := fs.Int64("seed", 1, "зерно выбора контрольных окон")
	minHits := fs.Int("min-hits", 3, "не показывать тех, кто совпал реже")
	top := fs.Int("top", 25, "сколько строк показать")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"db": true, "since": true, "until": true, "kind": true, "presence-window": true,
		"controls": true, "seed": true, "min-hits": true, "top": true,
	})); err != nil {
		return err
	}
	store, err := modwatch.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	from, err := parseMoment(*since)
	if err != nil {
		return err
	}
	to, err := parseMoment(*until)
	if err != nil {
		return err
	}
	rep, err := store.Analyze(ctx, modwatch.ReportOptions{
		Since: from, Until: to, Kinds: splitKinds(*kinds),
		MinAge: *minAge, MaxAge: *maxAge,
		Window: *window, Controls: *controls, Seed: *seed,
		MinHits: *minHits, Top: *top,
	})
	if err != nil {
		return err
	}
	if rep.Occasions == 0 {
		fmt.Println("событий для расчёта нет: либо наблюдение слишком короткое, либо не нашлось контрольных окон (нужно хотя бы двое суток наблюдения)")
		if rep.EventsSkipped > 0 {
			fmt.Printf("окказий отброшено без контроля: %d\n", rep.EventsSkipped)
		}
		return nil
	}
	// Наблюдений столько, сколько окказий: чистка треда пачкой — один момент, а
	// не N независимых совпадений (иначе z раздувается примерно в √N раз).
	fmt.Printf("наблюдение %s — %s, событий %d в %d окказиях (без контроля %d), контрольных окон на окказию %d\n",
		fmtTime(rep.From), fmtTime(rep.To), rep.Events, rep.Occasions, rep.EventsSkipped, rep.Controls)
	if rep.EventsReturned > 0 {
		fmt.Printf("отброшено как перемодерация (заметка вернулась в ленту): %d\n", rep.EventsReturned)
	}
	fmt.Printf("людей в окне события в среднем %.1f, в контрольном %.1f\n\n", rep.AvgPresent, rep.AvgPresentCtrl)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "z\tсовпал\tожидалось\tво сколько раз\tреплик всего\tанкета\tник")
	for _, r := range rep.Rows {
		fmt.Fprintf(tw, "%+.2f\t%d\t%.1f\t%.2f\t%d\tu%d\t%s\n",
			r.Z, r.Hits, r.Expected, r.Lift, r.Comments, r.UserID, r.Name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println(`
Как читать: «совпал» — в скольких окнах действий человек писал, «ожидалось» —
столько же окон, но сдвинутых на целые сутки (тот же час, другой день). z выше
3 при десятках событий — систематическое присутствие, а не совпадение; одиночные
всплески при малом числе событий не значат ничего.`)
	return nil
}

// moderationKinds — виды событий, которые по умолчанию считаются действием
// модерации. Закрытие комментариев оставлено в наборе намеренно: сайт метит
// «не актуальна» и сам, но закрывают и руками, а отделяется одно от другого не
// исключением вида, а проверкой возраста (-age-max). note_published и
// nick_changed не входят: первое случается и без человека, второе редкое и
// опосредованное; оба доступны явным -kind.
//
// image_added тоже НЕ входит, хотя раньше входило: картинку ставит АВТОР,
// правя свою заметку, а модерация тут лишь пропускает правку. Признак хорош как
// метка «заметка уехала на премодерацию», но как действие модератора он ложный.
var moderationKinds = []string{
	modwatch.KindNoteGone, modwatch.KindCommentGone, modwatch.KindCommentsClosed,
}

func splitKinds(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// parseMoment принимает либо дату/время, либо длительность «назад».
// siteTZ — пояс площадки. Весь modwatch говорит о событиях сайта, а сайт
// показывает время по Новосибирску; хост при этом живёт по UTC (в контейнере)
// или по своему поясу, и вывод расходился с тем, что человек видит на странице.
// Поэтому и печать, и разбор моментов идут в поясе сайта, а не хоста.
func siteTZ() *time.Location {
	if loc, err := time.LoadLocation("Asia/Novosibirsk"); err == nil {
		return loc
	}
	return time.FixedZone("NOVT", 7*3600)
}

func parseMoment(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, siteTZ()); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("не понял момент %q: нужен 2026-08-05, 2026-08-05 14:30 или длительность назад (72h)", s)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(siteTZ()).Format("2006-01-02 15:04")
}

// fmtDur печатает длительность коротко: 4ч15м, 9м, «—» для неизвестной.
func fmtDur(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dч%02dм", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dм", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dс", int(d.Seconds()))
	}
}
