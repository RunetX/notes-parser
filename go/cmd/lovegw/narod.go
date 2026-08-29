package main

// lovegw narod — жители площадки (эпик «народ», briefs/brief-ensemble.md).
//
// Конвейер производства персонажа проходит здесь целиком: слепок донора
// снимается из архива (card), житель собирается из слепков рецептом (compose),
// и только собранное показывается глазами (show). Порядок не случаен —
// карточку должно быть видно ДО того, как она заговорит на площадке.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/narod"
)

// defaultCardsDir — каталог жителей. Внутри data/, потому что карточка выведена
// из архивных писем живых людей, а репозиторий публичный.
var defaultCardsDir = filepath.Join("data", "narod", "cards")

func cmdNarod(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{
		"card": true, "compose": true, "show": true, "world": true, "replay": true,
		"scout": true, "enroll": true, "seed": true, "stage": true, "wake": true,
	})
	fs := flag.NewFlagSet("narod", flag.ExitOnError)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	cardsDir := fs.String("cards", defaultCardsDir, "каталог карточек персонажей")
	worldPath := fs.String("world", filepath.Join("data", "narod.db"), "база состояния мира")
	recent := fs.Int("recent", 2000, "card: последних реплик в замер")
	normSample := fs.Int("norm-sample", 100000, "card: комментариев в норму корпуса (с чем сравнивать ошибки)")
	seed := fs.Int64("seed", 1, "card: зерно выборки образцов; replay: зерно кубика")
	verify := fs.Bool("verify", false, "compose: только проверить близость к донорам, карточку не писать")

	jobs := fs.Int("j", 0, "card: сколько слепков снимать разом (0 — по ядрам, до восьми)")
	seeds := fs.Int("seeds", 5, "replay: сколько зёрен прогнать (одно зерно — бросок, а не замер; модель зовут только на первом)")
	with := fs.String("with", "", "scout: состав, вокруг которого искать соседей (u<id> через запятую)")
	minComments := fs.Int("min-comments", scoutMinComments, "scout: корпус кандидата, ниже которого слепок не снять")
	scoutLimit := fs.Int("top", 40, "scout: сколько кандидатов показать")
	mode := fs.String("mode", modeSolo, "replay: solo (слепок среди настоящих) | vacuum (разговор с нуля)")
	actor := fs.String("actor", "", "replay: слепок, на месте которого играем (u<id>); в вакууме — состав через запятую")
	maxReply := fs.Int("max-replies", 400, "replay -mode vacuum: потолок реплик в треде")
	notes := fs.String("note", "", "replay: id заметок через запятую; пусто — подобрать по донору")
	threads := fs.Int("threads", 5, "replay: сколько тредов подобрать")
	minSaid := fs.Int("min-said", 5, "replay: сколько реплик донора нужно в треде")
	// Модель выключена по умолчанию: бесплатный прогон считает матрицу решений,
	// и крутить его можно сколько угодно. Деньги тратятся только по -speak.
	speak := fs.Bool("speak", false, "replay: звать модель (ПЛАТНО); без флага считается только матрица решений")
	hot := fs.Bool("hot", false, "replay: брать треды с перепалками — там, где спорили, а не соглашались")
	maxSpeak := fs.Int("max-speak", 40, "replay: потолок обращений к модели за прогон (0 — без потолка)")
	drafts := fs.Int("drafts", 3, "replay: черновиков за один запрос к модели")
	// Раунд по умолчанию один: мерка голоса — это ПЕРВАЯ попытка, а раунд с
	// обратной связью меряет судью, а не пишущего, и стоит вдвое.
	rounds := fs.Int("rounds", 1, "replay: раундов с обратной связью (1 — без неё)")
	outDir := fs.String("out", filepath.Join("data", "narod", "replay"), "replay: куда класть отчёты")
	cfgPath := fs.String("config", "config.json", "replay: конфиг (нужен только с -speak)")
	model := fs.String("model", "", "replay: модель (пусто — из секции llm)")
	body := fs.String("body", "", "seed: файл с текстом заметки-песочницы (- — stdin)")
	from := fs.Int64("from", 0, "seed: поднять архивную заметку с этим номером")
	stageNote := fs.Int64("id", 0, "stage: номер уже стоящей в ленте заметки")
	stageOff := fs.Bool("off", false, "stage: вернуть заметку людям")
	reason := fs.String("reason", "", "stage: пометка в журнал")
	if err := fs.Parse(reorderArgs(rest, fs)); err != nil {
		return err
	}

	switch sub {
	case "card":
		opts := defaultSnapshotOpts()
		opts.Recent, opts.NormSample, opts.Seed = *recent, *normSample, *seed
		return narodCardBuild(ctx, *dbPath, *cardsDir, fs.Args(), opts, *jobs)
	case "compose":
		if len(fs.Args()) != 1 {
			return fmt.Errorf("narod compose: нужен файл рецепта")
		}
		return narodCompose(ctx, *cardsDir, fs.Args()[0], *verify)
	case "show":
		return narodShow(*cardsDir, fs.Args())
	case "scout":
		return narodScout(ctx, scoutOpts{
			dbPath: *dbPath, with: *with, threads: *threads,
			minComments: *minComments, limit: *scoutLimit,
		})
	case "world":
		return narodWorld(ctx, *worldPath)
	case "enroll":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		return narodEnroll(ctx, cfg, *cardsDir, *worldPath, fs.Args())
	case "seed":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		return narodSeed(ctx, cfg, *body, *from)
	case "stage":
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		return narodStage(ctx, cfg, *stageNote, *stageOff, *reason)
	case "wake":
		return narodWake(ctx, *worldPath, *stageNote)
	case "replay":
		opts := replayOpts{
			dbPath: *dbPath, cardsDir: *cardsDir, outDir: *outDir, mode: *mode,
			actor: *actor, notes: *notes, threads: *threads, minSaid: *minSaid,
			speak: *speak, maxSpeak: *maxSpeak, maxReply: *maxReply,
			hot:    *hot,
			drafts: *drafts, rounds: *rounds,
			seed: *seed, seeds: *seeds, cfgPath: *cfgPath, model: *model,
		}
		switch *mode {
		case modeSolo:
			return narodReplay(ctx, opts)
		case modeVacuum:
			return narodVacuum(ctx, opts)
		default:
			return fmt.Errorf("narod replay: -mode бывает %s или %s, а не %q", modeSolo, modeVacuum, *mode)
		}
	default:
		return fmt.Errorf("narod: нужна подкоманда (scout|card|compose|show|world|replay|enroll|seed)")
	}
}

// narodCardBuild снимает слепки доноров и кладёт их в каталог.
// narodCardBuild снимает слепки доноров.
//
// Доноров бывает много: под один состав их девяносто, и после каждого нового
// замера все они снимаются заново — четверть часа, при двенадцати простаивающих
// ядрах.
//
// ПАРАЛЛЕЛЬНОСТЬ ЭТОГО НЕ ЛЕЧИТ, и это замерено, а не предположено (29.08.2026):
// восемь слепков в один поток идут 75 с, в восемь потоков — 76. Упирается замер
// не в ядра, а в одну базу SQLite, и добавление потоков только делит её же.
// Ключ -j оставлен не ради скорости: он даёт счётчик «[k/N]» на долгом
// пересъёме, а поднимать его имеет смысл только если однажды переедет узкое
// место. Прежде чем снова ускорять ЭТО — сперва замерить, где время.
//
// Бриф печатается только у ОДНОГО донора: `narod card <один>` — это стенд, на
// котором человек смотрит слепок глазами, а `narod card <много>` — пересъём, и
// девяносто брифов подряд читать некому.
func narodCardBuild(ctx context.Context, dbPath, dir string, tokens []string, opts snapshotOpts, jobs int) error {
	if len(tokens) == 0 {
		return fmt.Errorf("narod card: нужен донор (p<id>|u<id>|user_id)")
	}
	ar, err := archive.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	// Каталог заводится под 0700: карточка выведена из писем живого человека.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if jobs <= 0 {
		jobs = defaultCardJobs()
	}
	jobs = min(jobs, len(tokens))
	now := time.Now()

	var (
		mu   sync.Mutex
		done int
		one  narod.Card // бриф печатается только у единственного донора
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for _, token := range tokens {
		g.Go(func() error {
			card, err := buildSnapshotCard(gctx, ar, token, opts, now)
			if err != nil {
				return fmt.Errorf("слепок %s: %w", token, err)
			}
			path := filepath.Join(dir, card.ID+narod.CardExt)
			if err := writeCardFile(path, card); err != nil {
				return err
			}
			mu.Lock()
			done++
			one = card
			fmt.Fprintf(os.Stderr, "[%d/%d] слепок %s → %s\n", done, len(tokens), token, path)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if len(tokens) == 1 {
		return narod.WriteCardBrief(os.Stdout, one)
	}
	return nil
}

// defaultCardJobs — сколько слепков снимать разом по умолчанию.
//
// По ядрам, но не больше восьми. Числа эти на скорость почти не влияют (см.
// выше: замер упирается в саму базу), и восемь тут просто разумный потолок.
func defaultCardJobs() int { return min(runtime.NumCPU(), 8) }

// writeCardFile пишет карточку через временный файл: оборванная запись оставила
// бы каталог с полукарточкой, а загрузчик читает его целиком при каждом старте.
func writeCardFile(path string, card narod.Card) error {
	data, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// narodShow печатает карточку так, как её увидит модель. Тот же самый текст
// уходит в промпт — если он нечитаем человеком, он не помогает и модели.
//
// Без аргумента показывается весь каталог: список жителей нигде в коде не
// записан, и единственный способ узнать, кто сегодня живёт на площадке, — это
// прочитать каталог.
// otherGender — пол «собеседника» для показа настроения: противоположный, чтобы
// строка про разнополый разговор попала в пример. Неизвестный пол так и остаётся
// неизвестным — тогда её в примере не будет, и это правда о такой карточке.
func otherGender(g string) string {
	switch g {
	case "male":
		return "female"
	case "female":
		return "male"
	}
	return ""
}

func narodShow(dir string, args []string) error {
	var cards []narod.Card
	if len(args) > 0 {
		for _, arg := range args {
			c, err := narod.LoadCard(cardPath(dir, arg))
			if err != nil {
				return err
			}
			cards = append(cards, c)
		}
	} else {
		var err error
		if cards, err = narod.LoadCards(dir); err != nil {
			return err
		}
	}

	for i, c := range cards {
		if i > 0 {
			fmt.Println()
		}
		// Сорт карточки печатается ПЕРВЫМ: слепок реального участника и житель
		// площадки выглядят одинаково, а обходятся с ними по-разному.
		switch c.Kind {
		case narod.KindSnapshot:
			fmt.Printf("[%s] СЛЕПОК донора — только калибровка, наружу не идёт\n", c.ID)
		default:
			fmt.Printf("[%s] житель площадки\n", c.ID)
		}
		if err := narod.WriteCardBrief(os.Stdout, c); err != nil {
			return err
		}
		// Настроение — второй блок промпта, и печатать его обязательно: команда
		// обещает показать карточку РОВНО так, как её увидит модель, а больное
		// место, регистр и запрет мата живут не в брифе, а здесь. Пример берётся
		// самый горячий из возможных (разнополая пара, неприязнь, разговор уже на
		// третьей ступени) — иначе половина блока не показалась бы вовсе.
		fmt.Print(narod.WriteMood(narod.MoodPoint{
			Card: c, Peer: "собеседник", PeerGender: otherGender(c.Persona.Gender),
			Tone: -0.5, Heat: 2,
		}))
	}
	if len(args) == 0 {
		if err := narod.CheckLive(cards); err != nil {
			// Не ошибка команды: каталог со слепками — обычное состояние
			// калибровки. Но знать об этом человек обязан ДО того, как включит
			// живой режим.
			fmt.Fprintf(os.Stderr, "\nв живом режиме этот каталог играть не будет: %v\n", err)
		}
	}
	return nil
}

func narodWorld(ctx context.Context, path string) error {
	w, err := narod.OpenWorld(ctx, path)
	if err != nil {
		return err
	}
	defer w.Close()

	version, err := w.WorldVersion(ctx)
	if err != nil {
		return err
	}
	actors, err := w.Actors(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("мир %s: схема v%d, акторов %d\n", path, version, len(actors))
	for _, a := range actors {
		anketa := "—"
		if a.PlatformUserID != 0 {
			anketa = fmt.Sprint(a.PlatformUserID)
		}
		fmt.Printf("  %-16s %-8s анкета %-14s %s\n", a.ID, a.Kind, anketa, a.Nick)
	}
	return worldGraph(ctx, w, actors)
}

// worldGraph печатает то, ради чего мир и заводился: отношения и поводы.
//
// Список акторов сам по себе не отвечает ни на один вопрос эмуляции — он
// показывает, кого завели, а спрашивают всегда «ушёл ли мир от старта». Поэтому
// рядом с рёбрами стоят ЭПИЗОДЫ: шкала помнит итог, но не повод, а сообщество
// состоит как раз из поводов.
func worldGraph(ctx context.Context, w *narod.World, actors []narod.Actor) error {
	nick := make(map[string]string, len(actors))
	for _, a := range actors {
		nick[a.ID] = a.Nick
	}
	edges, err := w.Edges(ctx)
	if err != nil {
		return err
	}
	var moved int
	for _, e := range edges {
		if e.Sympathy != 0 || e.Irritation != 0 {
			moved++
		}
	}
	fmt.Printf("\nрёбер %d, из них сдвинуто отношением %d "+
		"(знакомство копится и без модели — оно считается)\n", len(edges), moved)
	for _, e := range edges {
		if e.Sympathy == 0 && e.Irritation == 0 {
			continue
		}
		fmt.Printf("  %-22s → %-22s симпатия %+5.1f  раздражение %+5.1f  виделись %.0f\n",
			nick[e.Src], nick[e.Dst], e.Sympathy, e.Irritation, e.Familiarity)
		eps, err := w.EpisodesOf(ctx, e.Src, e.Dst, 3)
		if err != nil {
			return err
		}
		for _, ep := range eps {
			fmt.Printf("      %s %s: %s\n", ep.At.Format("02.01.2006"), ep.Kind, ep.Summary)
		}
	}
	fmt.Printf("\nчто случилось с жителями вне площадки (последнее):\n")
	for _, a := range actors {
		inner, err := w.RecallKind(ctx, a.ID, narod.JournalInner, 1)
		if err != nil {
			return err
		}
		if len(inner) == 0 {
			continue
		}
		fmt.Printf("  %-22s %s  %s\n", a.Nick,
			inner[0].At.Format("02.01.2006"), inner[0].Text)
	}
	return nil
}
