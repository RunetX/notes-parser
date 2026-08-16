package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/modwatch"
	"lovegw/internal/store"
)

// modwatchActivity — присутствие людей на сайте: детектор запретов, которых
// иначе не видно. Запрет в «Заметки» ничего не убирает с площадки, поэтому
// наблюдатель за модерацией его не ловит; зато анкета показывает время
// последнего действия человека, и «замолчал, но ходит» — это и есть запрет,
// а «замолчал и не заходит» — уход.
//
// Обход анонимный: сессия сюда не носится, следа в чужих гостях он не
// оставляет, и боевые куки не рискуют попасть под 403 массового обхода.
// Поэтому `-account` здесь намеренно НЕ поддержан (в отличие от `personas
// gender`, которому авторизация нужна по существу): под сессией обход стал бы
// ходить по чужим анкетам от чьего-то имени, и каждый такой заход был бы виден
// владельцу в его списке гостей. Анонимно поле `last_activity` отдаётся всё
// равно — авторизация тут ничего не добавляет, только светит.
func modwatchActivity(ctx context.Context, args []string) error {
	sub := "watch"
	rest := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "watch", "scan", "report", "log":
			sub = a
		default:
			rest = append(rest, a)
		}
	}
	fs := flag.NewFlagSet("modwatch activity", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultModwatchPath, modwatchDBUsage)
	source := fs.String("source", "", "откуда круг наблюдения: путь к боевой БД (пусто — db_path из конфига) или modwatch — по своим комментариям")
	interval := fs.Duration("interval", modwatch.DefaultActivityInterval, "период такта обхода")
	batch := fs.Int("batch", modwatch.DefaultActivityBatch, "сколько анкет опрашивать за такт (0 — все)")
	active := fs.Duration("active", modwatch.DefaultActivityWindow, "окно активности для набора круга")
	minComments := fs.Int("min-comments", modwatch.DefaultActivityMin, "порог реплик за окно, чтобы попасть в круг")
	users := fs.String("users", "", "анкеты через запятую, которые опрашивать каждый такт (подозреваемые)")
	silence := fs.Duration("silence", modwatch.DefaultSilence, "с какого молчания показывать человека в отчёте")
	fresh := fs.Duration("fresh", modwatch.DefaultFresh, "насколько свежим должен быть последний заход, чтобы считать, что человек ещё на площадке")
	margin := fs.Duration("margin", modwatch.DefaultMargin, "насколько заход должен пережить последнюю реплику, чтобы считаться «ходит молча»")
	minMissed := fs.Float64("min-missed", modwatch.DefaultMinMissed, "порог «недописанных» реплик против своего темпа")
	top := fs.Int("top", 40, "сколько строк показать")
	user := fs.Int64("user", 0, "чей след присутствия показать (log)")
	since := fs.String("since", "", "с какого момента — "+momentUsage)
	near := fs.String("near", "", "показать присутствие вокруг момента — "+momentUsage)
	window := fs.Duration("window", 30*time.Minute, "полуширина окна для -near")
	if err := fs.Parse(reorderArgs(rest, fs)); err != nil {
		return err
	}
	silenceOpts := modwatch.SilenceOptions{
		Now: time.Now(), Silence: *silence, Fresh: *fresh, Margin: *margin,
		Window: *active, MinMissed: *minMissed,
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
		return modwatchActivityLog(ctx, db, *user, *since, *near, *window)
	}

	roster, closeRoster, err := activityRoster(ctx, cfg, db, *source)
	if err != nil {
		return err
	}
	defer closeRoster()

	if sub == "report" {
		return modwatchActivityReport(ctx, db, roster, *active, *minComments, silenceOpts, *top)
	}

	client, err := genderClient(cfg, nil, log) // мобильный vhost, строгий темп, свой jar
	if err != nil {
		return err
	}
	w := &modwatch.ActivityWatcher{
		Source: activitySource{c: client}, Roster: roster, Store: db, Log: log,
		Interval: *interval, Batch: *batch, Window: *active, MinComments: *minComments,
		Always: parseIDs(*users),
		// Ровный машинный темп сам по себе подозрителен для DDoS-Guard —
		// разбавляем случайной паузой, как в обходе профилей за полом.
		Pace: func(ctx context.Context) error {
			return sleepCtx(ctx, time.Duration(rand.Int64N(int64(genderJitterMax))))
		},
	}
	if sub == "scan" {
		w.Batch = 0 // один сплошной обход круга, дальше отчёт
		log.Info("сплошной обход круга наблюдения")
		n, err := w.Poll(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("обход завершён, новых отметок присутствия: %d\n\n", n)
		return modwatchActivityReport(ctx, db, roster, *active, *minComments, silenceOpts, *top)
	}
	log.Info("наблюдение за присутствием запущено", "db", *dbPath, "такт", *interval, "порция", *batch)
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// activitySource — адаптер *love.Client под modwatch.ActivitySource.
type activitySource struct{ c *love.Client }

func (a activitySource) Activity(ctx context.Context, id int64) (love.Activity, error) {
	return a.c.FetchActivity(ctx, nil, id)
}

// mirrorRoster — круг наблюдения по зеркалу (боевая БД). Сессии оттуда не
// читаются, поэтому ключ шифрования этому обходу не нужен.
type mirrorRoster struct{ st *store.Store }

func (r mirrorRoster) Commenters(ctx context.Context, since time.Time, min int) ([]modwatch.Commenter, error) {
	people, err := r.st.Commenters(ctx, since, min)
	if err != nil {
		return nil, err
	}
	out := make([]modwatch.Commenter, 0, len(people))
	for _, p := range people {
		out = append(out, modwatch.Commenter{
			UserID: p.UserID, Nick: p.Nick, Comments: p.Comments, LastComment: p.LastComment,
		})
	}
	return out, nil
}

// activityRoster выбирает источник круга: зеркало (по умолчанию) или свои
// комментарии наблюдателя.
func activityRoster(ctx context.Context, cfg *config.Config, db *modwatch.Store, source string) (modwatch.RosterSource, func(), error) {
	if source == "modwatch" {
		return db, func() {}, nil
	}
	path := source
	if path == "" {
		path = cfg.DBPath
	}
	st, err := store.Open(ctx, path)
	if err != nil {
		return nil, nil, fmt.Errorf("боевая БД %s: %w", path, err)
	}
	return mirrorRoster{st: st}, func() { st.Close() }, nil
}

// modwatchActivityReport печатает разбор молчания: кто замолчал и продолжает
// ходить (кандидат на запрет), а кто перестал появляться вовсе.
func modwatchActivityReport(ctx context.Context, db *modwatch.Store, roster modwatch.RosterSource,
	active time.Duration, minComments int, opt modwatch.SilenceOptions, top int) error {
	people, err := roster.Commenters(ctx, time.Now().Add(-active), minComments)
	if err != nil {
		return err
	}
	profiles, err := db.Profiles(ctx)
	if err != nil {
		return err
	}
	// След берём за то же окно, что и реплики: молчание не может быть длиннее.
	trail, err := db.ActivityTrail(ctx, time.Now().Add(-active), time.Time{})
	if err != nil {
		return err
	}
	rows := modwatch.ClassifySilence(people, profiles, trail, opt)

	fmt.Printf("круг наблюдения: %d человек (от %d реплик за %s), опрошено анкет %d\n",
		len(people), minComments, fmtSpan(active), len(profiles))
	fmt.Printf("молчат дольше %s: %d\n\n", fmtSpan(opt.Silence), len(rows))

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "вердикт\tмолчит\tне заходит\tсуток на сайте\tнедописал\tреплик\tпоследняя реплика\tпоследний заход\tанкета\tник")
	for i, r := range rows {
		if top > 0 && i >= top {
			break
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%.0f\t%d\t%s\t%s\tu%d\t%s%s\n",
			r.Verdict, fmtSpan(r.Silence), fmtSpan(r.Away), r.SeenDays, r.Missed, r.Comments,
			fmtTime(r.LastComment), fmtTime(r.LastActivity), r.UserID, r.Nick, vipMark(r))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println(`
Как читать. Решает «не заходит»: запрет в «Заметки» ходить по сайту не мешает,
поэтому молчащий, который был на сайте только что, — на площадке, но писать не
может. Перестал заходить — ушёл, и неважно, сколько ещё ходил, замолчав;
«ушёл позже» значит, что он сделал это в два приёма (так выглядит и запрет, из
которого человек не вернулся). «Суток на сайте» — на скольких РАЗНЫХ сутках
молчания мы его там застали; пока их меньше двух, вердикт «мало данных», а не
«запрет»: у вернувшегося после отъезда последняя отметка такая же свежая, и
одна точка эти случаи не различает (на этом и вышла осечка с Актрисой
13.08.2026 — она вернулась в тот же вечер). «Недописал» — сколько реплик потеряно против
СВОЕГО же темпа: четверо суток молчания у пишущего раз в неделю не значат
ничего. Темп при этом средний за всё окно, поэтому у затухавшего постепенно он
завышен — наказание выглядит как ОБРЫВ на полном ходу, и разница видна по
дневным счётчикам. Отличить запрет от «читаю, но не пишу» отчёт не может — это доказывает
возвращение, наказание кончается ровно на сроке (сутки, неделя, месяц). Анкеты
с ✱ прячут присутствие «Приватностью»; на отметку это не влияет, сайт отдаёт
её всё равно.`)
	return nil
}

func vipMark(r modwatch.SilenceRow) string {
	if r.HideMe {
		return " ✱"
	}
	return ""
}

// modwatchActivityLog печатает след присутствия: по одной анкете (-user) или
// всех, кто был на сайте вокруг момента (-near) — это ответ на вопрос «кто был
// рядом с действием», когда действовавший ничего не писал.
func modwatchActivityLog(ctx context.Context, db *modwatch.Store, user int64, since, near string, window time.Duration) error {
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
		fmt.Printf("присутствие в окне %s … %s\n\n", fmtTime(from), fmtTime(to))
	}
	stamps, err := db.ActivityIn(ctx, user, from, to)
	if err != nil {
		return err
	}
	profiles, err := db.Profiles(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "был на сайте (Нск)\tанкета\tник\tснято")
	for _, s := range stamps {
		fmt.Fprintf(tw, "%s\tu%d\t%s\t%s\n", fmtTime(s.At), s.UserID, profiles[s.UserID].Nick, fmtTime(s.SeenAt))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\nвсего отметок: %d\n", len(stamps))
	fmt.Println(`Отметка — минута последнего действия человека на сайте на момент опроса.
Между опросами сайт хранит только последнюю, поэтому промежуточные заходы
теряются: пустота в следе не значит «его не было».`)
	return nil
}

// parseIDs разбирает список анкет через запятую.
func parseIDs(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(part, "u"), 10, 64); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// fmtSpan печатает длительность так, как её читают: сутками, если их больше
// двух. fmtDur для этого не годится — «313ч02м» глазами не читается.
func fmtSpan(d time.Duration) string {
	if d < 0 {
		return "—"
	}
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dс%dч", int(d.Hours())/24, int(d.Hours())%24)
	}
	return fmtDur(d)
}
