package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/platimport"
	"lovegw/internal/platsink"
	"lovegw/internal/store"
)

// Команда platform — обслуживание собственной площадки «Зазеркалье»: схема и
// диагностика. Сам HTTP-сервер поднимает подкоманда web.
//
// Миграции вынесены в явную команду, а не в старт сервера, намеренно: схему
// меняет администратор в известный момент, а не любой поднявшийся контейнер.
// Иначе рестарт после выкатки тихо перекраивает боевую базу.

var platformSubcommands = map[string]bool{
	"migrate":         true,
	"doctor":          true,
	"reconcile":       true,
	"media":           true,
	"avatar":          true,
	"reply-scan":      true,
	"import-restored": true,
	"invite":          true,
	"post":            true,
	"role":            true,
	"import-archive":  true,
	"moderation":      true,
	"triage":          true,
	"events":          true,
	"anonymize":       true,
	"export":          true,
	"ban":             true,
	"unban":           true,
}

func cmdPlatform(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("platform", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к config.json")
	dbPath := fs.String("db", "", "путь к боевой lovegw.db (по умолчанию из конфига)")
	limit := fs.Int("limit", 500, "media: сколько файлов забрать за проход; reply-scan: сколько заметок обойти")
	note := fs.Int64("note", 0, "reply-scan / triage: взять только эту заметку")
	bind := fs.Int64("bind", 0, "invite: привязать приглашение к участнику (его прежний след станет своим)")
	label := fs.String("note-text", "", "invite: пометка для себя, кому выдано")
	days := fs.Int("days", 30, "invite: сколько дней действует")
	archivePath := fs.String("archive", "", "import-archive / import-restored: путь к archive.db")
	batch := fs.Int("batch", 0, "import-archive: комментариев в одной транзакции (0 — 50 000)")
	onlyNotes := fs.Int("notes", 0, "import-archive / import-restored: взять только столько заметок (0 — все)")
	keepIdx := fs.Bool("keep-indexes", false, "import-archive: не снимать индексы comments")
	dry := fs.Bool("dry-run", false, "import-archive / import-restored: посчитать, ничего не записывая")
	out := fs.String("out", "", "export: файл выгрузки (пусто — stdout)")
	model := fs.String("model", "", "triage: подменить модель классификатора, не трогая конфиг")
	reason := fs.String("reason", "", "ban / unban: причина, её увидит сам человек")
	yes := fs.Bool("yes", false, "anonymize: подтвердить необратимую операцию")
	body := fs.String("body", "", "post: файл с текстом заметки («-» — читать stdin)")
	author := fs.Int64("author", 0, "post: от чьего имени (пусто — администратор площадки)")
	pin := fs.Bool("pin", false, "post: закрепить заметку наверху ленты")
	sub, rest := splitSubcommand(reorderArgs(args, fs), platformSubcommands)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	tail := fs.Args()
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
	case "avatar":
		ids, err := parseUserIDs(tail)
		if err != nil {
			return err
		}
		return platformAvatar(ctx, cfg, ids)
	case "reply-scan":
		return platformReplyScan(ctx, cfg, *limit, *note)
	case "invite":
		return platformInvite(ctx, cfg, *bind, *label, *days)
	case "post":
		return platformPost(ctx, cfg, *author, *pin, *body)
	case "import-archive":
		if *archivePath == "" {
			return fmt.Errorf("platform import-archive -archive <путь к archive.db>")
		}
		return platformImportArchive(ctx, cfg, platimport.Options{
			Archive: *archivePath, Batch: *batch, Notes: *onlyNotes, OnlyNote: *note,
			KeepIndexes: *keepIdx, DryRun: *dry,
		})
	case "import-restored":
		if *archivePath == "" {
			return fmt.Errorf("platform import-restored -archive <путь к archive.db>")
		}
		return platformImportRestored(ctx, cfg, platimport.RestoredOptions{
			Archive: *archivePath, Notes: *onlyNotes, OnlyNote: *note, DryRun: *dry,
		})
	case "role":
		if len(tail) != 2 {
			return fmt.Errorf("platform role <id участника> <user|moderator|admin>")
		}
		id, err := strconv.ParseInt(tail[0], 10, 64)
		if err != nil {
			return fmt.Errorf("id участника %q: %w", tail[0], err)
		}
		return platformRole(ctx, cfg, id, tail[1])
	case "moderation":
		return platformModeration(ctx, cfg, *limit)
	case "triage":
		return platformTriage(ctx, cfg, *limit, *model, *note)
	case "events":
		return platformEvents(ctx, cfg, *limit)
	case "anonymize":
		id, err := oneUserID(tail, "platform anonymize <id участника> -yes")
		if err != nil {
			return err
		}
		return platformAnonymize(ctx, cfg, id, *yes)
	case "export":
		id, err := oneUserID(tail, "platform export <id участника> [-out файл]")
		if err != nil {
			return err
		}
		return platformExport(ctx, cfg, id, *out)
	case "ban", "unban":
		id, err := oneUserID(tail, "platform "+sub+" <id участника> [-days N] [-reason «…»]")
		if err != nil {
			return err
		}
		return platformBan(ctx, cfg, id, sub == "ban", *days, *reason)
	default:
		return fmt.Errorf("platform: укажите подкоманду " +
			"(migrate, doctor, reconcile, media, avatar, reply-scan, invite, post, role, " +
			"moderation, events, ban, unban, anonymize, export, import-archive, import-restored)")
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

// platformAvatar переносит фото из анкеты НГС людям, названным по номеру.
//
// Зачем руками: аватар на площадку приносит ЗЕРКАЛО, вместе с комментарием, — а
// комментариев на НГС нет с 17.08.2026, значит фото здесь больше не обновится
// само никогда. Сменившая фото в анкете остаётся у нас с прошлым, и починить это
// сегодня можно только отсюда. Своей кнопки у человека пока нет (Ш5д в бэклоге),
// а у тени её и не будет: за неё никто не входил, но фото под её репликами — её.
//
// Загрузку своего файла площадка по-прежнему не принимает: здесь ровно перенос
// того, что человек и так показывает на НГС.
func platformAvatar(ctx context.Context, cfg *config.Config, ids []int64) error {
	if cfg.Platform.MediaDir == "" {
		return fmt.Errorf("platform.media_dir не задан (или env LOVEGW_PLATFORM_MEDIA_DIR)")
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	media, err := platform.NewMediaStore(p, cfg.Platform.MediaDir)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	site, err := mobileProfileClient(cfg, log)
	if err != nil {
		return err
	}

	var bad int
	for _, id := range ids {
		if err := platformAvatarOne(ctx, p, media, site, id); err != nil {
			// Один отказ не отменяет остальных: команда идёт по списку людей, а
			// не по одной операции.
			bad++
			fmt.Printf("%d: %v\n", id, err)
		}
	}
	if bad > 0 {
		return fmt.Errorf("фото не перенесено у %d из %d", bad, len(ids))
	}
	return nil
}

// platformAvatarOne — один человек: прочитать анкету, забрать файл, привязать.
//
// Строка на площадке спрашивается ПЕРВОЙ: если такого номера у нас нет, ходить
// на чужой сайт незачем — и ошибка выйдет про нас, а не про НГС.
func platformAvatarOne(ctx context.Context, p *platform.Platform, media *platform.MediaStore,
	site *love.Client, id int64) error {
	u, err := p.UserByID(ctx, id)
	if err != nil {
		return err
	}
	prof, err := site.FetchProfile(ctx, strconv.FormatInt(id, 10))
	if errors.Is(err, love.ErrProfileMissing) {
		return fmt.Errorf("анкеты нет на НГС: снесена или скрыта целиком")
	}
	if err != nil {
		return err
	}
	if !love.IsRealAvatar(prof.AvatarURL) {
		// Силуэт по умолчанию — не фото. Своё при этом НЕ снимаем: файлов
		// площадка не принимает, вернуть его будет неоткуда, а «обновил и
		// остался без фото» — потеря по чужой руке.
		fmt.Printf("%d %q: в анкете нет фото — оставляю как было\n", id, u.Nick)
		return nil
	}
	data, err := site.FetchMedia(ctx, prof.AvatarURL)
	if err != nil {
		return err
	}
	m, err := putNGSAvatar(ctx, p, media, id, prof.AvatarURL, data)
	if err != nil {
		return err
	}
	if bytes.Equal(m.SHA256, u.AvatarSHA) {
		fmt.Printf("%d %q (в анкете %q): фото то же, что было — %s\n",
			id, u.Nick, prof.Nick, shortSHA(m.SHA256))
		return nil
	}
	// Прежний sha печатается НЕ для красоты: файл из хранилища никуда не делся
	// (имя файла есть его содержимое), поэтому строка лога — единственное, по
	// чему правку можно откатить, если человек попросит вернуть прошлое фото.
	fmt.Printf("%d %q (в анкете %q): фото %s → %s — %s %d×%d, %d КБ\n",
		id, u.Nick, prof.Nick, shortSHA(u.AvatarSHA), shortSHA(m.SHA256),
		m.MIME, m.Width, m.Height, len(data)/1024)
	return nil
}

// shortSHA — как называть файл хранилища в отчёте человеку: восьми знаков хватает,
// чтобы найти его на диске и в media.
func shortSHA(sha []byte) string {
	if len(sha) == 0 {
		return "нет"
	}
	return hex.EncodeToString(sha)[:8]
}

// putNGSAvatar кладёт байты фото в хранилище и привязывает их к человеку.
//
// Общая дорога двух входов — кнопки «Обновить аватар» на «моей странице»
// (webWriter.SetOwnAvatar) и этой команды. Правило у них одно, и разъехаться эти
// два места не должны: хранилище само откажется принять не-картинку — геоблок
// DDoS-Guard отдаёт на запрос файла HTML с кодом 200, и такой «аватар» осел бы у
// нас молча, а на странице оказался битым.
func putNGSAvatar(ctx context.Context, p *platform.Platform, media *platform.MediaStore,
	userID int64, url string, data []byte) (platform.Media, error) {
	m, err := media.Put(ctx, data, url)
	if err != nil {
		return platform.Media{}, err
	}
	return m, p.SetNGSAvatar(ctx, userID, m.SHA256, url)
}

// parseUserIDs разбирает список номеров анкет из хвоста командной строки.
func parseUserIDs(args []string) ([]int64, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("укажите номера анкет: platform avatar <id> [<id>…]")
	}
	ids := make([]int64, 0, len(args))
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("номер анкеты %q: %w", a, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
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
	if before != wanted {
		fmt.Printf("накатываю схему: v%d → v%d\n", before, wanted)
		if err := p.Migrate(ctx); err != nil {
			return err
		}
		after, _, err := p.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("готово, схема на версии %d\n", after)
	} else {
		fmt.Printf("схема уже на версии %d\n", wanted)
	}

	// Тексты согласий публикуются здесь же: это такая же смена содержимого базы
	// в известный момент, и делает её тот же человек. Правка выпущенной
	// редакции без смены номера версии — ОТКАЗ: молча переписанный текст
	// превращает все прежние согласия в бумажку без содержания.
	if err := p.EnsureConsentDocs(ctx, operatorOf(cfg)); err != nil {
		return err
	}
	docs, err := platform.CurrentConsentDocs(operatorOf(cfg))
	if err != nil {
		return err
	}
	for _, d := range docs {
		fmt.Printf("согласие %s: редакция v%d\n", d.Kind, d.Version)
	}
	return nil
}

// platformInvite выдаёт приглашение — третий путь входа и единственный,
// переживающий смерть НГС.
//
// Он же путь для ПЕРВОГО администратора: пока на площадке нет ни одного
// участника, выдать приглашение из веб-морды некому.
func platformInvite(ctx context.Context, cfg *config.Config, bind int64, note string, days int) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	// Выдающий — сам приглашённый, если приглашение к кому-то привязано, иначе
	// первый администратор. Колонка issued_by обязательная: приглашение без
	// автора не проследить, а история выдач — часть модерации.
	issuer := bind
	if issuer == 0 {
		if issuer, err = p.AnyAdmin(ctx); err != nil {
			return fmt.Errorf("%w: у непривязанного приглашения нужен администратор — "+
				"назначьте его командой `platform role <id> admin`", err)
		}
	}
	code, err := p.CreateInvite(ctx, issuer, bind, note, time.Duration(days)*24*time.Hour)
	if err != nil {
		return err
	}
	if bind != 0 {
		fmt.Printf("приглашение для участника %d (действует %d дн.): %s\n", bind, days, code)
	} else {
		fmt.Printf("приглашение без привязки (действует %d дн.): %s\n", days, code)
	}
	fmt.Println("код показывается один раз — в базе лежит только его хеш")
	return nil
}

// platformPost публикует нативную заметку и, если попросили, закрепляет её
// наверху ленты.
//
// Форма на странице для этого есть, и команда её не заменяет: нужна она ровно
// для объявлений площадки — их пишет тот, кто её выкатывает, с рабочей машины,
// где сессии участника нет и заводить её ради одной заметки незачем. Тем же
// путём объявление можно положить в скрипт выкатки.
//
// Текст берётся из файла или со stdin (`-body -`), а не из аргумента: в
// объявлении есть переносы строк и кавычки, и командная строка их калечит.
//
// Идёт всё через ЯДРО, а не в базу напрямую: та же проверка права писать, тот
// же потолок частоты, та же строка в очередь модерации и то же событие. Заметка
// после этого ничем не отличается от написанной в форме — включая то, что
// исходящий обход отнесёт её в каналы мессенджеров.
func platformPost(ctx context.Context, cfg *config.Config, author int64, pin bool, bodyPath string) error {
	body, err := readTextArg(bodyPath)
	if err != nil {
		return err
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	// Актор нужен и для закрепления (ядро спрашивает права модератора), и как
	// подпись по умолчанию: объявление площадки подписывает её администратор.
	actor, err := adminViewer(ctx, p)
	if err != nil {
		return err
	}
	if author == 0 {
		author = actor.UserID
	}
	id, err := p.CreateNote(ctx, platform.NewNote{AuthorID: author, Body: body})
	if err != nil {
		return err
	}
	fmt.Printf("заметка %d опубликована: %s/n/%d\n", id, cfg.Platform.BaseURL, id)
	if !pin {
		return nil
	}
	// Закрепление — отдельным шагом и после публикации, как у выпуска дайджеста:
	// сорвавшийся закреп не должен ни отменять заметку, ни звать повторить её.
	if err := p.SetNotePinned(ctx, actor, id, true, "объявление площадки"); err != nil {
		return fmt.Errorf("заметка опубликована, но не закреплена (мест всего %d): %w",
			platform.MaxPinned, err)
	}
	fmt.Println("закреплена наверху ленты")
	return nil
}

// readTextArg читает текст из файла, а «-» означает stdin.
func readTextArg(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("нужен текст: -body <файл> или -body - (со stdin)")
	}
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("текст заметки: %w", err)
	}
	return string(raw), nil
}

// platformRole меняет права участника. Руками, потому что первого
// администратора назначить неоткуда, а раздача прав из веб-морды — это Ш7.
func platformRole(ctx context.Context, cfg *config.Config, id int64, role string) error {
	var r platform.Role
	switch role {
	case "user", "участник":
		r = platform.RoleUser
	case "moderator", "модератор":
		r = platform.RoleModerator
	case "admin", "админ":
		r = platform.RoleAdmin
	default:
		return fmt.Errorf("роль %q: ожидалось user, moderator или admin", role)
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	// Актор нулевой намеренно: команду даёт администратор из консоли, и
	// приписывать действие какому-нибудь живому админу было бы враньём в
	// журнале — там честнее пустая строка «без автора».
	if err := p.SetRole(ctx, platform.Viewer{}, id, r); err != nil {
		return err
	}
	u, err := p.UserByID(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("%s (%d): роль %s\n", u.Nick, u.ID, role)
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

	// Живой канал. Спрашиваем не «настроен ли», а «доходит ли звонок»: подписка
	// и уведомление — это две разные способности Postgres, и отвечает эта строка
	// на вопрос, в каком режиме поднимется морда, ДО того как её поднимут. Что у
	// неё в этом режиме прямо сейчас, doctor не видит вовсе — она чужой процесс,
	// и про сейчас отвечает её собственный healthz.
	if err := liveRingCheck(ctx, p); err != nil {
		fmt.Fprintf(w, "живой канал\tПО ТАКТУ\tзвонок не доходит: %v\n", err)
	} else {
		fmt.Fprintf(w, "живой канал\tok\tзвонок доходит, такт — страховка\n")
	}
	return nil
}

// liveRingCheck — звонок самому себе. Подписываемся, звоним и ждём: это ровно
// тот путь, которым живой канал узнаёт о новом, и проверять его половинками
// («LISTEN не упал») значило бы не проверять вовсе.
func liveRingCheck(ctx context.Context, p *platform.Platform) error {
	ctx, cancel := context.WithTimeout(ctx, liveRingWait)
	defer cancel()

	rang := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- p.ListenLive(ctx, func() {
			// Звоним только ПОСЛЕ того, как подписка встала: уведомление, ушедшее
			// раньше, честно потерялось бы, и проба соврала бы про бой.
			_, _ = p.Pool().Exec(ctx, `SELECT pg_notify($1, '')`, platform.LiveChannel)
		}, func() {
			select {
			case rang <- struct{}{}:
			default:
			}
		})
	}()
	select {
	case <-rang:
		cancel()
		<-done
		return nil
	case err := <-done:
		if err == nil {
			err = errors.New("подписка кончилась молча")
		}
		return err
	case <-ctx.Done():
		<-done
		return fmt.Errorf("звонок не пришёл за %s", liveRingWait)
	}
}

// liveRingWait — сколько ждём собственный звонок. Пять секунд: локальный
// Postgres отвечает за миллисекунды, а через ssh-туннель к бою — за десятки.
const liveRingWait = 5 * time.Second

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

// platformImportArchive раскатывает архив грабера (archive.db) в Postgres
// площадки: 117 588 заметок и 10,8 млн комментариев с 2004 года против трёх
// недель живого зеркала.
//
// Разовая администраторская операция, а не рабочий путь, поэтому и команда
// отдельная. Гонять её ТОЛЬКО на хосте: это миллионы строк, и через ssh-туннель
// решает RTT, а сам archive.db (4,7 ГБ) быстрее скачать на хост напрямую, чем
// поднимать по 10 КБ/с с рабочей машины.
//
// При работающем демоне безопасна: приём идемпотентен, а уже зеркалённые
// заметки раскатка не трогает вовсе. На время работы у comments сняты индексы —
// страницы треда идут перебором, то есть площадка жива, но медленна.
// platformImportRestored доносит эпоху, которой нет на самом сайте.
//
// Отдельная команда, а не флаг раскатки: правила у неё обратные. Раскатка
// архива пропускает заметку, уже лежащую на площадке, — здесь именно в такую
// заметку и дописывается тред, потому что он пуст и другим уже не станет
// (комментарии до конца 2013 года НГС стёр). Запускать можно в любой момент и
// сколько угодно раз: ключи третьей полосы считаются из ключей архива, поэтому
// повтор ничего не удваивает.
func platformImportRestored(ctx context.Context, cfg *config.Config, opt platimport.RestoredOptions) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	log := newLogger(cfg.LogLevel)
	start := time.Now()
	st, err := platimport.RunRestored(ctx, p, opt, log)
	fmt.Printf("добор восстановленного за %s:\n", time.Since(start).Truncate(time.Second))
	fmt.Printf("  анкет %d, заметок %d, комментариев %d, подписей возвращено %d\n",
		st.Users, st.Notes, st.Comments, st.Signed)
	fmt.Printf("  пропущено заметок: нет на площадке %d, тред уже непуст %d\n",
		st.SkipAbsent, st.SkipFilled)
	fmt.Printf("  рёбра ответов: обращение %d, нет %d; снято обращений %d\n",
		st.EdgeAddr, st.EdgeNone, st.Trimmed)
	if err != nil {
		return err
	}
	if opt.DryRun {
		return nil
	}
	fmt.Print("обновление статистики планировщика… ")
	if err := platimport.Analyze(ctx, p.Pool()); err != nil {
		return err
	}
	fmt.Println("готово")
	return nil
}

func platformImportArchive(ctx context.Context, cfg *config.Config, opt platimport.Options) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	log := newLogger(cfg.LogLevel)
	start := time.Now()
	st, err := platimport.Run(ctx, p, opt, log)
	fmt.Printf("раскатка архива за %s:\n", time.Since(start).Truncate(time.Second))
	fmt.Printf("  анкет %d, заметок %d, комментариев %d, иллюстраций %d\n",
		st.Users, st.Notes, st.Comments, st.Images)
	fmt.Printf("  пропущено: заметок уже в базе %d, комментариев без id сайта %d\n",
		st.SkipNotes, st.SkipComments)
	fmt.Printf("  рёбра ответов: дерево %d, обращение %d, ветка %d, нет %d; снято обращений %d\n",
		st.EdgeTree, st.EdgeAddr, st.EdgeParent, st.EdgeNone, st.Trimmed)
	if err != nil {
		return err
	}
	if opt.DryRun {
		return nil
	}
	// Статистика планировщика — не косметика: на 61 тыс. комментариев планы
	// считались другие, а лента и тред обязаны идти по индексам.
	fmt.Print("обновление статистики планировщика… ")
	if err := platimport.Analyze(ctx, p.Pool()); err != nil {
		return err
	}
	fmt.Println("готово")
	return nil
}
