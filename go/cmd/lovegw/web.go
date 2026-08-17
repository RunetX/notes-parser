package main

import (
	"context"
	"flag"
	"fmt"

	"lovegw/internal/config"
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

	srv := web.New(web.Config{
		Listen:     cfg.Platform.Listen,
		BaseURL:    cfg.Platform.BaseURL,
		MediaDir:   cfg.Platform.MediaDir,
		PreviewKey: cfg.Platform.PreviewKey,
		Log:        log,
	}, pf)
	return srv.Run(ctx)
}
