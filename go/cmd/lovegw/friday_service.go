package main

// Пятничная рубрика в демоне (эпик J).
//
// Служба ведёт вечер сама: в слот публикует вступительную заметку, дальше раз в
// час задаёт очередной вопрос. Своего состояния не держит вовсе — заметка
// ключуется неделей, число заданных вопросов считается по самим вопросам, а
// какой следующий, выводится из недели (см. platfriday). Отсюда и рестарт
// посреди вечера ничего не теряет, и ручной `friday publish` рядом с работающим
// демоном ничего не удваивает.

import (
	"context"
	"errors"

	"lovegw/internal/platfriday"
)

func (d *daemon) setupFriday() error {
	cfg, log := d.cfg, d.log
	if !cfg.Friday.Enabled {
		return nil
	}
	if d.plat == nil {
		// Не отказ демона: площадка могла не подняться из-за разошедшейся схемы,
		// и это уже сказано алертом — второй раз про то же кричать незачем.
		// Тот же довод, что у народа.
		log.Warn("пятничная рубрика не подключена: площадка выключена или не поднялась")
		return nil
	}
	svc := platfriday.NewService(d.plat, platfriday.NewPG(d.plat.Pool()), fridayConfig(cfg, log))
	d.starts = append(d.starts, func(ctx context.Context) error {
		if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("пятничная рубрика остановлена", "err", err)
		}
		return nil
	})
	log.Info("пятничная рубрика подключена",
		"режим", cfg.Friday.Mode, "день", cfg.Friday.Weekday, "час", cfg.Friday.Hour,
		"вопросов", cfg.Friday.Questions)
	return nil
}
