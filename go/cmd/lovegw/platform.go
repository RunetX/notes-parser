package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/platsink"
	"lovegw/internal/store"
)

// Команда platform — обслуживание собственной площадки «Заметки»: схема и
// диагностика. Сам HTTP-сервер поднимает подкоманда web.
//
// Миграции вынесены в явную команду, а не в старт сервера, намеренно: схему
// меняет администратор в известный момент, а не любой поднявшийся контейнер.
// Иначе рестарт после выкатки тихо перекраивает боевую базу.

var platformSubcommands = map[string]bool{
	"migrate":    true,
	"doctor":     true,
	"reconcile":  true,
	"media":      true,
	"reply-scan": true,
}

func cmdPlatform(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("platform", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к config.json")
	dbPath := fs.String("db", "", "путь к боевой lovegw.db (по умолчанию из конфига)")
	limit := fs.Int("limit", 500, "media: сколько файлов забрать за проход; reply-scan: сколько заметок обойти")
	note := fs.Int64("note", 0, "reply-scan: обойти только эту заметку")
	sub, rest := splitSubcommand(reorderArgs(args, fs), platformSubcommands)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Platform.DSN == "" {
		return fmt.Errorf("platform.dsn не задан (или env LOVEGW_PLATFORM_DSN)")
	}

	switch sub {
	case "migrate":
		return platformMigrate(ctx, cfg)
	case "doctor":
		return platformDoctor(ctx, cfg)
	case "reconcile":
		return platformReconcile(ctx, cfg, cmp.Or(*dbPath, cfg.DBPath))
	case "media":
		return platformMedia(ctx, cfg, *limit)
	case "reply-scan":
		return platformReplyScan(ctx, cfg, *limit, *note)
	default:
		return fmt.Errorf("platform: укажите подкоманду (migrate, doctor, reconcile, media, reply-scan)")
	}
}

// platformMedia добирает байты медиа по уже известным ссылкам.
//
// Живой поток зеркала наполняет хранилище сам, и обычно этой команды не нужно.
// Нужна она ровно тогда, когда потока нет: строки, легшие бэкфиллом, знают
// только ссылку, а 17.08.2026 НГС восстановился, но комментировать не разрешил
// — новых реплик нет, значит и аватаров не будет. Окно закрывается вместе с
// сайтом: пока hsmedia.ru отдаёт файлы, их надо забрать.
func platformMedia(ctx context.Context, cfg *config.Config, limit int) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	if cfg.Platform.MediaDir == "" {
		return fmt.Errorf("platform.media_dir не задан (или env LOVEGW_PLATFORM_MEDIA_DIR)")
	}
	media, err := platform.NewMediaStore(p, cfg.Platform.MediaDir)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	start := time.Now()
	// Клиент сайта тот же, что у зеркала: со своим лимитером и с RU-IP.
	// Хранилище проверит, что приехала картинка, а не заглушка геоблока.
	stats, err := platsink.NewMediaSweep(p, media, newSiteClient(cfg, log), log).Once(ctx, limit)
	fmt.Printf("медиа за %s: аватаров %d, иллюстраций %d, не вышло %d\n",
		time.Since(start).Truncate(time.Second), stats.Avatars, stats.Images, stats.Failed)
	return err
}

// platformReconcile — разовый проход сверки lovegw.db → Postgres. Он же бэкфилл:
// на пустой площадке первый проход переносит всё зеркало целиком, отдельной
// команды под это нет и не нужно.
//
// Безопасно при работающем демоне: приём идемпотентен по id, а направление
// одностороннее — сверка только читает SQLite.
func platformReconcile(ctx context.Context, cfg *config.Config, dbPath string) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	// Схема обязана совпадать с бинарником: приём в чужую схему — это порча
	// данных, а не «наверное, обойдётся». Тем же правилом живёт и web.
	inDB, wanted, err := p.Version(ctx)
	if err != nil {
		return err
	}
	if inDB != wanted {
		return fmt.Errorf("схема площадки v%d, бинарник рассчитан на v%d — сначала `platform migrate`", inDB, wanted)
	}

	// Ключ шифрования не нужен: сессий сверка не касается вовсе, она читает
	// заметки и комментарии (то же правило, что у modwatch activity).
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("боевая БД %s: %w", dbPath, err)
	}
	defer st.Close()

	log := newLogger(cfg.LogLevel)
	start := time.Now()
	stats, err := platsink.NewReconciler(st, p, log).Once(ctx)
	// Итог печатаем и при ошибке: на бэкфилле важно, сколько успело пройти —
	// прерванный проход продолжается с того же места, повторять его не страшно.
	fmt.Printf("сверка за %s: заметок %d, комментариев %d, иллюстраций %d, закрыто %d (сверено заметок %d)\n",
		time.Since(start).Truncate(time.Second),
		stats.Notes, stats.Comments, stats.Images, stats.Closed, stats.Scanned)
	return err
}

func platformMigrate(ctx context.Context, cfg *config.Config) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	before, wanted, err := p.Version(ctx)
	if err != nil {
		return err
	}
	if before == wanted {
		fmt.Printf("схема уже на версии %d, делать нечего\n", wanted)
		return nil
	}
	fmt.Printf("накатываю схему: v%d → v%d\n", before, wanted)
	if err := p.Migrate(ctx); err != nil {
		return err
	}
	after, _, err := p.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("готово, схема на версии %d\n", after)
	return nil
}

// platformDoctor печатает состояние площадки: до чего дотянулись, что настроено
// не так. Как и общий doctor, он ничего не чинит — только называет.
func platformDoctor(ctx context.Context, cfg *config.Config) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		fmt.Fprintf(w, "Postgres\tНЕДОСТУПЕН\t%v\n", err)
		return err
	}
	defer p.Close()
	fmt.Fprintf(w, "Postgres\tok\t\n")

	inDB, wanted, err := p.Version(ctx)
	if err != nil {
		return err
	}
	switch {
	case inDB == 0:
		fmt.Fprintf(w, "схема\tПУСТО\tнужен `platform migrate` (до v%d)\n", wanted)
	case inDB < wanted:
		fmt.Fprintf(w, "схема\tОТСТАЁТ\tv%d в базе, v%d в бинарнике — нужен `platform migrate`\n", inDB, wanted)
	case inDB > wanted:
		fmt.Fprintf(w, "схема\tИЗ БУДУЩЕГО\tv%d в базе, v%d в бинарнике — на хосте старый образ\n", inDB, wanted)
	default:
		fmt.Fprintf(w, "схема\tok\tv%d\n", inDB)
	}

	// Настройки Postgres, которые на одном ядре решают: JIT и параллелизм
	// сжигают процессор на запросах, живущих две миллисекунды.
	for _, s := range []struct{ name, want string }{
		{"shared_buffers", ""},
		{"effective_cache_size", ""},
		{"work_mem", ""},
		{"max_connections", ""},
		{"jit", "off"},
		{"max_parallel_workers_per_gather", "0"},
		{"synchronous_commit", "on"},
		{"default_text_search_config", "pg_catalog.russian"},
	} {
		var got string
		if err := p.Pool().QueryRow(ctx, "SHOW "+s.name).Scan(&got); err != nil {
			fmt.Fprintf(w, "%s\t?\t%v\n", s.name, err)
			continue
		}
		mark := "ok"
		if s.want != "" && got != s.want {
			mark = "ХОТЕЛОСЬ БЫ " + s.want
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.name, got, mark)
	}

	if inDB > 0 {
		var notes, comments, users int64
		if err := p.Pool().QueryRow(ctx,
			`SELECT (SELECT count(*) FROM notes), (SELECT count(*) FROM comments), (SELECT count(*) FROM users)`).
			Scan(&notes, &comments, &users); err != nil {
			return err
		}
		fmt.Fprintf(w, "наполнение\t%d заметок, %d комментариев, %d личностей\t\n", notes, comments, users)
	}

	if cfg.Platform.MediaDir != "" {
		if st, err := os.Stat(cfg.Platform.MediaDir); err != nil {
			fmt.Fprintf(w, "медиа\tНЕТ КАТАЛОГА\t%s (%v)\n", cfg.Platform.MediaDir, err)
		} else if !st.IsDir() {
			fmt.Fprintf(w, "медиа\tНЕ КАТАЛОГ\t%s\n", cfg.Platform.MediaDir)
		} else {
			fmt.Fprintf(w, "медиа\tok\t%s\n", cfg.Platform.MediaDir)
		}
	}
	if cfg.Platform.BaseURL == "" {
		fmt.Fprintf(w, "base_url\tНЕ ЗАДАН\tабсолютные ссылки и куки будут неверными\n")
	} else {
		fmt.Fprintf(w, "base_url\tok\t%s\n", cfg.Platform.BaseURL)
	}
	return nil
}

// platformReplyScan уточняет дерево ответов по мобильной версии сайта и попутно
// снимает пол участников с десктопной страницы комментариев.
//
// Зачем: живое зеркало знает адресата только по обращению «Ник, …» и разрешает
// его в последнюю реплику этого человека — угадывание с точностью около
// половины, и на странице это видно как ветка, выросшая не там. Настоящее
// ребро есть только в мобильной версии.
//
// Окно закрывается вместе с сайтом, а не по нашему решению.
func platformReplyScan(ctx context.Context, cfg *config.Config, limit int, note int64) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	log := newLogger(cfg.LogLevel)
	site, err := replyScanClients(cfg, log)
	if err != nil {
		return err
	}
	scanner := platsink.NewReplyScanner(p, site, log)

	start := time.Now()
	var st platsink.ReplyScanStats
	if note != 0 {
		// Одна заметка — стенд для проверки: видно, что именно поменялось.
		st, err = scanner.Note(ctx, note)
	} else {
		st, err = scanner.Once(ctx, limit)
	}
	fmt.Printf("дерево за %s: заметок %d (отказов %d), комментариев %d, рёбер %d, обращений снято %d, полов %d\n",
		time.Since(start).Truncate(time.Second), st.Notes, st.Failed, st.Comments, st.Edges, st.Trimmed, st.Genders)
	return err
}

// replyScanClients — две страницы одного сайта. Мобильная отдаёт дерево, но не
// знает пола; десктопная — наоборот. Клиента поэтому два, у каждого свои база
// и User-Agent: с десктопным UA мобильная версия уводит редиректом, а с
// мобильным десктопная отдаёт свою вёрстку.
func replyScanClients(cfg *config.Config, log *slog.Logger) (platsink.TreeSource, error) {
	mobileBase, err := love.MobileBaseURL(cfg.Site.BaseURL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	// Общий jar на оба клиента: куки DDoS-Guard живут между запросами, и без
	// них каждый заход выглядит новым гостем.
	hc := &http.Client{Timeout: replyScanTimeout, Jar: jar}
	interval := time.Duration(cfg.Site.RequestIntervalMS) * time.Millisecond

	mobile := love.NewWithClient(mobileBase, replyScanMobileUA, interval, hc, log)
	mobile.StrictPacing()
	desktop := love.NewWithClient(cfg.Site.BaseURL, replyScanDesktopUA, interval, hc, log)
	desktop.StrictPacing()
	return replyScanSite{tree: mobile, genders: desktop}, nil
}

type replyScanSite struct {
	tree    *love.Client
	genders *love.Client
}

func (s replyScanSite) FetchNoteReplyTree(ctx context.Context, noteID string) (map[int64]int64, error) {
	return s.tree.FetchNoteReplyTree(ctx, noteID)
}

func (s replyScanSite) FetchGenders(ctx context.Context, noteID string) (map[int64]string, error) {
	return s.genders.FetchGenders(ctx, noteID)
}

const (
	// replyScanTimeout — тред целиком сайт рендерит долго: на 248 комментариях
	// ~2 c, на 848 думал минуту и отвечал 500. Потолок щедрый, но конечный.
	replyScanTimeout  = 45 * time.Second
	replyScanMobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	replyScanDesktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)
