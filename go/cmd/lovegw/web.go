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

	pf, err := platform.Open(ctx, cfg.Platform.DSN)
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
	site, err := loginSite(cfg, log)
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
	}, pf, pf, site)
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
func loginSite(cfg *config.Config, log *slog.Logger) (web.Site, error) {
	base, err := love.MobileBaseURL(cfg.Site.BaseURL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	// Свой jar — только под куки DDoS-Guard, которые он выдаёт сам; сессий
	// пользователей здесь нет и быть не может.
	hc := &http.Client{Timeout: loginProfileTimeout, Jar: jar}
	c := love.NewWithClient(base, replyScanMobileUA,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, hc, log)
	c.StrictPacing()
	return loginProfiles{c: c}, nil
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
		Nick:      prof.Nick,
		AvatarURL: prof.AvatarURL,
		AboutMe:   prof.AboutMe,
		Gender:    platformGender(prof.Gender),
		Blocked:   prof.Blocked,
	}, nil
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
