package main

// Шина событий из командной строки (эпик F).
//
// Команда одна и отвечает на один вопрос: жива ли шина. У службы, работающей
// молча, других признаков жизни нет — колокольчик в морде показывает результат,
// но не отличает «поводов не было» от «раздача встала неделю назад».
//
// Поэтому она и раздаёт, и показывает: сначала делает проход (это безопасно при
// работающем демоне — раздача идемпотентна по ключу (user_id, event_id)), потом
// печатает наполнение. Расхождение видно сразу: если «ждёт раздачи» не ноль
// ПОСЛЕ прохода, значит фактов больше, чем пачка, и такт не поспевает.

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/platbus"
	"lovegw/internal/platform"
)

// platformEvents гонит один проход шины и печатает её наполнение.
func platformEvents(ctx context.Context, cfg *config.Config, limit int) error {
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	log := newLogger(cfg.LogLevel)
	pass, err := platbus.New(platbus.Config{Batch: limit}, p, log).Once(ctx)
	if err != nil {
		return err
	}
	stats, err := p.BusStats(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "проход\tраздано фактов %d, разобрано реакций %d\n", pass.Fanned, pass.Reactions)
	if pass.Pruned.Any() {
		fmt.Fprintf(w, "убрано\tпрочитанных %d, непрочитанных %d, фактов %d\n",
			pass.Pruned.Read, pass.Pruned.Unread, pass.Pruned.Events)
	}
	fmt.Fprintf(w, "фактов\t%d\n", stats.Events)
	fmt.Fprintf(w, "ждёт раздачи\t%d\n", stats.Pending)
	if stats.Pending > 0 {
		// Возраст самого старого нерозданного — единственное число, по которому
		// видно, что служба стоит: наполнение растёт и когда она работает.
		fmt.Fprintf(w, "  самый старый\t%s назад\n", stats.OldestAge.Truncate(time.Second))
	}
	fmt.Fprintf(w, "поводов\t%d (непрочитанных %d)\n", stats.Notices, stats.Unread)
	fmt.Fprintf(w, "реакций ждёт разбора\t%d\n", stats.Reactions)
	return w.Flush()
}
