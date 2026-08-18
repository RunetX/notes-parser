package main

import (
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

	// Клиент НГС нужен ровно для одного — прочитать «о себе» при входе. Не
	// поднялся (нет RU-IP, сайт закрылся) — морда живёт дальше, а вход
	// показывает приглашения вместо формы: это её штатное состояние после
	// смерти НГС, а не авария.
	site, err := loginSite(ctx, cfg, log)
	if err != nil {
		log.Warn("вход по анкете НГС недоступен", "err", err)
		site = nil
	}

	srv := web.New(web.Config{
		Listen:   cfg.Platform.Listen,
		BaseURL:  cfg.Platform.BaseURL,
		MediaDir: cfg.Platform.MediaDir,
		Operator: operatorOf(cfg),
		Log:      log,
	}, pf, pf, pf, site)
	return srv.Run(ctx)
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
	p := loginProfiles{c: c}

	// Отправка кода в личку — вторая, необязательная половина. Идёт она через
	// ДЕСКТОПНЫЙ vhost и под живой сессией служебного аккаунта: JSON-RPC
	// /ajax/ живёт там, а мобильный vhost нужен ровно для анонимного чтения
	// анкеты. Не нашлось аккаунта — возвращаем чтение без отправки, и вход
	// уходит на запасной путь.
	sender, why, err := loginSender(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if sender == nil {
		log.Warn("код входа в личку слать нечем — остаётся поле «о себе»", "причина", why)
		return p, nil
	}
	log.Info("код входа уходит личным сообщением", "аккаунт", sender.title)
	return loginProfilesWithTalks{loginProfiles: p, send: sender}, nil
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

// loginSender — служебный аккаунт для отправки кода. Пустой результат без
// ошибки означает «канал недоступен»: это штатное состояние (аккаунта не
// заводили, сессия истекла), а не авария, и морда переживает его сама.
func loginSender(ctx context.Context, cfg *config.Config, log *slog.Logger) (*talksSender, string, error) {
	db, err := openAccounts(ctx, cfg, "")
	if err != nil {
		// Базы аккаунтов может не быть вовсе — на площадке это норма.
		return nil, err.Error(), nil
	}
	defer db.Close()

	names := []string{cfg.Platform.SiteAccount}
	if cfg.Platform.SiteAccount == "" {
		list, err := db.List(ctx)
		if err != nil {
			return nil, "", err
		}
		names = names[:0]
		for _, a := range list {
			names = append(names, a.Name)
		}
	}
	for _, name := range names {
		cookies, title, why, err := accountCookies(ctx, db, name)
		if err != nil {
			return nil, "", err
		}
		if why != "" {
			log.Debug("служебный аккаунт не годится", "имя", name, "причина", why)
			continue
		}
		c := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
			time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
		return &talksSender{c: c, cookies: cookies, title: title}, "", nil
	}
	return nil, "живых служебных аккаунтов нет", nil
}

// talksSender — отправка кода личным сообщением от служебного аккаунта.
type talksSender struct {
	c       *love.Client
	cookies []*http.Cookie
	title   string
}

// codeMessage — текст письма. Коротко и по делу: человек его ждёт, а лишние
// слова в незнакомом сообщении читаются как спам. Ссылку не даём намеренно —
// сообщение со ссылкой и просьбой что-то ввести выглядит ровно как то, от чего
// людей учат защищаться; человек уже стоит на нашей странице.
func codeMessage(code string) string {
	return "Код для входа на площадку «Заметки» (t3h.ru): " + code +
		". Он живёт час и нужен один раз. Если вы его не запрашивали — просто не вводите его нигде."
}

func (t *talksSender) SendCode(ctx context.Context, passportID int64, code string) error {
	_, err := t.c.TalksSend(ctx, t.cookies, strconv.FormatInt(passportID, 10), codeMessage(code))
	return err
}

// loginProfilesWithTalks — чтение анкеты ПЛЮС отправка кода. Отдельный тип,
// потому что способность необязательная и опознаётся type-assertion'ом
// (web.SiteMessenger): нет служебного аккаунта — нет и этого типа.
type loginProfilesWithTalks struct {
	loginProfiles
	send *talksSender
}

func (p loginProfilesWithTalks) SendCode(ctx context.Context, passportID int64, code string) error {
	return p.send.SendCode(ctx, passportID, code)
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
		PassportID: prof.PassportID,
		AvatarURL:  realAvatar(prof.AvatarURL),
		AboutMe:    prof.AboutMe,
		Gender:     platformGender(prof.Gender),
		Blocked:    prof.Blocked,
	}, nil
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
