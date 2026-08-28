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
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/narod"
)

// defaultCardsDir — каталог жителей. Внутри data/, потому что карточка выведена
// из архивных писем живых людей, а репозиторий публичный.
var defaultCardsDir = filepath.Join("data", "narod", "cards")

func cmdNarod(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, map[string]bool{
		"card": true, "compose": true, "show": true, "world": true, "replay": true,
	})
	fs := flag.NewFlagSet("narod", flag.ExitOnError)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	cardsDir := fs.String("cards", defaultCardsDir, "каталог карточек персонажей")
	worldPath := fs.String("world", filepath.Join("data", "narod.db"), "база состояния мира")
	recent := fs.Int("recent", 2000, "card: последних реплик в замер")
	normSample := fs.Int("norm-sample", 100000, "card: комментариев в норму корпуса (с чем сравнивать ошибки)")
	seed := fs.Int64("seed", 1, "card: зерно выборки образцов; replay: зерно кубика")
	verify := fs.Bool("verify", false, "compose: только проверить близость к донорам, карточку не писать")

	actor := fs.String("actor", "", "replay: слепок, на месте которого играем (u<id>)")
	notes := fs.String("note", "", "replay: id заметок через запятую; пусто — подобрать по донору")
	threads := fs.Int("threads", 5, "replay: сколько тредов подобрать")
	minSaid := fs.Int("min-said", 5, "replay: сколько реплик донора нужно в треде")
	// Модель выключена по умолчанию: бесплатный прогон считает матрицу решений,
	// и крутить его можно сколько угодно. Деньги тратятся только по -speak.
	speak := fs.Bool("speak", false, "replay: звать модель (ПЛАТНО); без флага считается только матрица решений")
	maxSpeak := fs.Int("max-speak", 40, "replay: потолок обращений к модели за прогон (0 — без потолка)")
	drafts := fs.Int("drafts", 3, "replay: черновиков за один запрос к модели")
	// Раунд по умолчанию один: мерка голоса — это ПЕРВАЯ попытка, а раунд с
	// обратной связью меряет судью, а не пишущего, и стоит вдвое.
	rounds := fs.Int("rounds", 1, "replay: раундов с обратной связью (1 — без неё)")
	outDir := fs.String("out", filepath.Join("data", "narod", "replay"), "replay: куда класть отчёты")
	cfgPath := fs.String("config", "config.json", "replay: конфиг (нужен только с -speak)")
	model := fs.String("model", "", "replay: модель (пусто — из секции llm)")
	if err := fs.Parse(reorderArgs(rest, fs)); err != nil {
		return err
	}

	switch sub {
	case "card":
		opts := defaultSnapshotOpts()
		opts.Recent, opts.NormSample, opts.Seed = *recent, *normSample, *seed
		return narodCardBuild(ctx, *dbPath, *cardsDir, fs.Args(), opts)
	case "compose":
		if len(fs.Args()) != 1 {
			return fmt.Errorf("narod compose: нужен файл рецепта")
		}
		return narodCompose(ctx, *cardsDir, fs.Args()[0], *verify)
	case "show":
		return narodShow(*cardsDir, fs.Args())
	case "world":
		return narodWorld(ctx, *worldPath)
	case "replay":
		return narodReplay(ctx, replayOpts{
			dbPath: *dbPath, cardsDir: *cardsDir, outDir: *outDir,
			actor: *actor, notes: *notes, threads: *threads, minSaid: *minSaid,
			speak: *speak, maxSpeak: *maxSpeak, drafts: *drafts, rounds: *rounds,
			seed: *seed, cfgPath: *cfgPath, model: *model,
		})
	default:
		return fmt.Errorf("narod: нужна подкоманда (card|compose|show|world|replay)")
	}
}

// narodCardBuild снимает слепки доноров и кладёт их в каталог.
func narodCardBuild(ctx context.Context, dbPath, dir string, tokens []string, opts snapshotOpts) error {
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
	now := time.Now()
	for _, token := range tokens {
		card, err := buildSnapshotCard(ctx, ar, token, opts, now)
		if err != nil {
			return err
		}
		path := filepath.Join(dir, card.ID+narod.CardExt)
		if err := writeCardFile(path, card); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "слепок %s → %s\n", token, path)
		if err := narod.WriteCardBrief(os.Stdout, card); err != nil {
			return err
		}
	}
	return nil
}

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
func narodShow(dir string, args []string) error {
	var cards []narod.Card
	if len(args) > 0 {
		for _, path := range args {
			c, err := narod.LoadCard(path)
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
	return nil
}
