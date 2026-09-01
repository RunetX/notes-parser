package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/imgconv"
	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/web"
)

// Команда web — HTTP-морда площадки. Отдельная подкоманда, а не второй
// бинарник: изоляция, которая реально нужна (упала морда — боты живы), даётся
// отдельным контейнером с своим command, ровно как у modwatch/guests/activity.
// Второй бинарник стоил бы ещё одного прохода компиляции того же дерева.
func cmdWeb(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "путь к config.json")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if !cfg.Platform.Enabled {
		return fmt.Errorf("platform.enabled = false: площадка выключена в конфиге")
	}
	if cfg.Platform.DSN == "" {
		return fmt.Errorf("platform.dsn не задан (или env LOVEGW_PLATFORM_DSN)")
	}

	log := newLogger(cfg.LogLevel)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	// Пул под роль морды: она единственная, куда стучится посторонний, поэтому
	// и соединений у неё меньше, и запрос на стороне Postgres обязан умирать по
	// сроку (см. platform.WebOpts).
	pf, err := platform.OpenWith(ctx, cfg.Platform.DSN, platform.WebOpts())
	if err != nil {
		return err
	}
	defer pf.Close()

	// Схему НЕ накатываем: это дело `platform migrate`, запускаемого руками.
	// Зато проверяем и отказываемся стартовать на чужой версии — работать по
	// схеме, которой не знаешь, хуже, чем не подняться.
	inDB, wanted, err := pf.Version(ctx)
	if err != nil {
		return err
	}
	if inDB != wanted {
		return fmt.Errorf("схема базы v%d, а бинарник рассчитан на v%d — выполните `lovegw platform migrate`", inDB, wanted)
	}

	// Клиент НГС нужен для двух вещей — прочитать анкету при входе и забрать из
	// неё фото по кнопке «Обновить аватар». Не поднялся (нет RU-IP, сайт
	// закрылся) — морда живёт дальше: вход показывает приглашения вместо формы,
	// кнопки аватара просто нет. Это её штатное состояние после смерти НГС, а не
	// авария.
	site, err := loginSite(ctx, cfg, log)
	if err != nil {
		log.Warn("вход по анкете НГС недоступен", "err", err)
		site = nil
	}

	// Писателю нужно хранилище медиа: фото из анкеты ложится файлом в CAS, а в
	// строку человека уходит его sha. Каталог обязателен при platform.enabled,
	// поэтому отказ здесь — отказ старта, а не тихая потеря кнопки.
	media, err := platform.NewMediaStore(pf, cfg.Platform.MediaDir)
	if err != nil {
		return err
	}

	// Один и тот же webWriter уходит и как Writer, и как Moderator: списки
	// остаются разными (в этом их смысл), а хранилище медиа нужно обоим — файл
	// кладут и публикация участника, и картинка, поставленная администратором.
	wr := webWriter{Platform: pf, media: media}
	srv := web.New(web.Config{
		Listen:   cfg.Platform.Listen,
		BaseURL:  cfg.Platform.BaseURL,
		MediaDir: cfg.Platform.MediaDir,
		Operator: operatorOf(cfg),
		Log:      log,
	}, pf, pf, wr, wr, site)
	// Шина событий: страница «События», колокольчик и живой канал. Морда только
	// ЧИТАЕТ поводы и отмечает их прочитанными — раздаёт их демон (platbus),
	// потому что горутина, поднятая и здесь, и там, делала бы одну работу вдвоём.
	srv.SetEvents(pf)
	// Приём картинок к заметкам. Не поднялся перекодировщик — морда живёт
	// дальше, просто без поля файла на форме: чужой бинарник, который не
	// отвечает, это не повод не открыть площадку.
	if conv := noteShots(ctx, cfg, log); conv != nil {
		srv.SetShots(conv)
	}
	return srv.Run(ctx)
}

// noteShots поднимает перекодировщик картинок. nil — картинок площадка не
// принимает, и это рабочее состояние, а не авария.
func noteShots(ctx context.Context, cfg *config.Config, log *slog.Logger) *imgconv.FFmpeg {
	if !cfg.Platform.Shots.Enabled {
		return nil
	}
	conv := &imgconv.FFmpeg{Path: ffmpegPath(cfg)}
	if err := conv.Probe(ctx); err != nil {
		log.Warn("картинки к заметкам выключены: ffmpeg не отвечает", "err", err)
		return nil
	}
	if conv.Codec() == "jpeg" {
		// Не отказ: JPEG это тот же вид на странице, только файл на треть
		// толще. Но знать об этом владельцу надо — иначе расход диска окажется
		// сюрпризом.
		log.Warn("в сборке ffmpeg нет libwebp — картинки будут в JPEG")
	}
	log.Info("картинки к заметкам приняты", "codec", conv.Codec(), "ffmpeg", ffmpegPath(cfg))
	return conv
}

// ffmpegPath — бинарник один на весь образ, поэтому и путь один: своё значение у
// картинок только затем, чтобы его можно было подменить, не трогая расшифровку
// голосовых.
func ffmpegPath(cfg *config.Config) string {
	return cmp.Or(cfg.Platform.Shots.FFmpegPath, cfg.ASR.FFmpegPath)
}

// webWriter — запись площадки плюс хранилище медиа.
//
// Отдельный тип нужен ровно из-за аватара: это единственная запись морды,
// которой мало базы — байты ложатся файлом, и класть их обязан тот же код, что
// у зеркала и у команды `platform avatar` (putNGSAvatar), иначе три места будут
// по-разному решать, что считать картинкой.
type webWriter struct {
	*platform.Platform
	media *platform.MediaStore
}

func (w webWriter) SetOwnAvatar(ctx context.Context, userID int64, url string, data []byte) error {
	_, err := putNGSAvatar(ctx, w.Platform, w.media, userID, url, data)
	return err
}

// ClearOwnAvatar — снять фото. Общей дороги с putNGSAvatar тут нет и не нужно:
// хранилища эта запись не касается вовсе — файл остаётся лежать (имя файла есть
// его содержимое, и на ту же картинку ссылаются чужие строки), снимается только
// привязка.
func (w webWriter) ClearOwnAvatar(ctx context.Context, userID int64) error {
	return w.Platform.ClearAvatar(ctx, userID)
}

// CreateNote — заметка вместе с приложенной картинкой.
//
// Байты ложатся на диск ДО транзакции, и иначе нельзя: note_images ссылается на
// media, а строка без файла — это битая картинка на странице, то есть поломка
// ВИДИМАЯ. Файл без строки не виден никому.
//
// Цена этого порядка названа прямо: откатилась транзакция (бан, отзыв согласия,
// частота) — файл остался, а уборки каталога у площадки нет вовсе. Ради этого
// морда и спрашивает MayPublishNote ДО перекодирования: гонка в сто
// миллисекунд оставит один осиротевший файл, отсутствие проверки — тысячи.
//
// sourceURL пуст намеренно: качать было неоткуда, картинку принесли. По этой
// пустоте своё и отличается от привезённого с НГС (см. шапку platform/media.go).
func (w webWriter) CreateNote(ctx context.Context, in platform.NewNote, shot *web.Shot) (int64, error) {
	if shot != nil {
		m, err := w.media.PutSized(ctx, shot.Data, "", shot.Width, shot.Height)
		if err != nil {
			return 0, err
		}
		in.Image = &m
	}
	return w.Platform.CreateNote(ctx, in)
}

// SetNoteImageAsAdmin — картинка ЛЮБОЙ заметки решением администратора.
//
// Порядок тот же, что у CreateNote, и по той же причине: байты ложатся на диск
// ДО транзакции, потому что строка note_images без файла — это битая картинка на
// странице, то есть поломка ВИДИМАЯ, а файл без строки не виден никому. Цена та
// же и названа так же: откатилась транзакция — файл остался, уборки каталога у
// площадки нет вовсе. Здесь она дешевле, чем у публикации: жмёт кнопку
// администратор, а не кто угодно.
//
// sourceURL пуст: качать было неоткуда, картинку принесли (см. platform/media.go).
func (w webWriter) SetNoteImageAsAdmin(ctx context.Context, actor platform.Viewer, noteID int64, shot *web.Shot, reason string) error {
	var img *platform.Media
	if shot != nil {
		m, err := w.media.PutSized(ctx, shot.Data, "", shot.Width, shot.Height)
		if err != nil {
			return err
		}
		img = &m
	}
	return w.Platform.SetNoteImageAsAdmin(ctx, actor, noteID, img, reason)
}

// SetPersonaAvatar — фото ЖИТЕЛЯ решением администратора.
//
// Порядок тот же, что у картинки к заметке, и по той же причине: байты ложатся
// на диск ДО транзакции, потому что строка без файла — это битая картинка на
// каждой реплике этого жителя, то есть поломка ВИДИМАЯ, а файл без строки не
// виден никому. Цена та же и названа так же: откатилась транзакция — файл
// остался, уборки каталога у площадки нет вовсе.
//
// sourceURL пуст: качать было неоткуда, картинку принесли. У жителя её и не
// откуда взять — анкеты НГС у персонажа нет, и это единственная причина, по
// которой площадка вообще принимает фото файлом.
func (w webWriter) SetPersonaAvatar(ctx context.Context, actor platform.Viewer, id int64, shot *web.Shot, reason string) error {
	var img *platform.Media
	if shot != nil {
		m, err := w.media.PutSized(ctx, shot.Data, "", shot.Width, shot.Height)
		if err != nil {
			return err
		}
		img = &m
	}
	return w.Platform.SetPersonaAvatarAsAdmin(ctx, actor, id, img, reason)
}

// operatorOf — реквизиты оператора персональных данных для текстов согласий.
func operatorOf(cfg *config.Config) platform.Operator {
	return platform.Operator{Name: cfg.Platform.Operator, Contact: cfg.Platform.Contact}
}

// loginSite — чтение анкеты для входа. Мобильный vhost и мобильный UA: с
// десктопным сайт уводит редиректом, а десктопный vhost банит серию почти сразу.
// Читаем АНОНИМНО — без jar сессий: визит под чужой кукой двигал бы
// last_activity человека и светил бы его в чужих «гостях».
func loginSite(ctx context.Context, cfg *config.Config, log *slog.Logger) (web.Site, error) {
	c, err := mobileProfileClient(cfg, log)
	if err != nil {
		return nil, err
	}
	// Отправки кода в личку НГС здесь больше нет: служебный аккаунт, от имени
	// которого уходили письма, удалён (владелец, 01.09.2026). Осталось одно
	// анонимное чтение анкеты — код человек вставляет в «о себе» сам.
	return loginProfiles{c: c}, nil
}

// mobileProfileClient — анонимное чтение анкеты. Мобильный vhost и мобильный
// UA: с десктопным сайт уводит редиректом, а десктопный vhost банит серию почти
// сразу. Без сессий: визит под чужой кукой двигал бы last_activity человека и
// светил бы его в чужих «гостях».
//
// Общий у входа на площадку и у команды `platform avatar`: задача у них одна —
// прочитать чужую анкету, ничего в ней не потревожив.
func mobileProfileClient(cfg *config.Config, log *slog.Logger) (*love.Client, error) {
	base, err := love.MobileBaseURL(cfg.Site.BaseURL)
	if err != nil {
		return nil, err
	}
	// Свой jar — только под куки DDoS-Guard, которые он выдаёт сам; сессий
	// пользователей здесь нет и быть не может.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: loginProfileTimeout, Jar: jar}
	c := love.NewWithClient(base, replyScanMobileUA,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, hc, log)
	c.StrictPacing()
	return c, nil
}

// loginProfileTimeout — анкета отдаётся быстро; человек ждёт этот запрос,
// глядя на форму, поэтому потолок короткий.
const loginProfileTimeout = 15 * time.Second

type loginProfiles struct{ c *love.Client }

func (p loginProfiles) Profile(ctx context.Context, id int64) (web.SiteProfile, error) {
	prof, err := p.c.FetchProfile(ctx, strconv.FormatInt(id, 10))
	if errors.Is(err, love.ErrProfileMissing) {
		return web.SiteProfile{}, web.ErrNoProfile
	}
	if err != nil {
		return web.SiteProfile{}, err
	}
	return web.SiteProfile{
		Nick:       prof.Nick,
		AvatarURL:  realAvatar(prof.AvatarURL),
		AboutMe:    prof.AboutMe,
		Gender:     platformGender(prof.Gender),
		Blocked:    prof.Blocked,
	}, nil
}

// Avatar — байты фото анкеты. Тот же клиент и тот же лимитер, что у чтения
// анкеты: ходит он анонимно, а ссылка ведёт на CDN сайта.
func (p loginProfiles) Avatar(ctx context.Context, url string) ([]byte, error) {
	return p.c.FetchMedia(ctx, url)
}

// realAvatar — фото или ничего. Силуэт по умолчанию площадка не хранит («аватар
// есть у всех» — это фон, а не аватар), и в ngs_avatar_url ему тоже не место:
// разовый добор медиа однажды принёс бы по такой ссылке всем одну картинку.
func realAvatar(url string) string {
	if !love.IsRealAvatar(url) {
		return ""
	}
	return url
}

// platformGender — перевод значений сайта в наши. Живёт здесь, а не в ядре:
// platform о существовании НГС не знает и знать не должен (тот же приём, что в
// platsink.convertGenders).
func platformGender(g string) platform.Gender {
	switch g {
	case love.GenderMale:
		return platform.GenderMale
	case love.GenderFemale:
		return platform.GenderFemale
	default:
		return platform.GenderUnknown
	}
}
