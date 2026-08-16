package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/tgx"
)

// avatarOpts — параметры действия avatars (из флагов cmdPersonas).
type avatarOpts struct {
	cfgPath             string
	proxy, refresh      bool
	workers, intervalMS int
	limit               int
	maxDist, genericMax int
}

// personasAvatars — Фаза 2: перцептивные хэши аватаров. Под-действие 2-м
// позиционным аргументом: fetch (скачать+хэшировать) или cluster (склеить
// похожие → alias_candidates(avatar_phash)). Дальше — обычный `personas cluster`.
func personasAvatars(ctx context.Context, ar *archive.Store, args []string, opt avatarOpts) error {
	if len(args) < 1 {
		return fmt.Errorf("personas avatars: нужно под-действие (fetch|cluster)")
	}
	switch sub := args[0]; sub {
	case "fetch":
		return personasAvatarsFetch(ctx, ar, opt)
	case "cluster":
		return personasAvatarsCluster(ctx, ar, opt)
	default:
		return fmt.Errorf("personas avatars: неизвестное под-действие %q (fetch|cluster)", sub)
	}
}

// personasAvatarsFetch скачивает настоящие аватары (не дефолтные заглушки) пулом
// воркеров, считает dHash и пишет в avatar_hashes. Идемпотентно (пропуск уже
// хэшированных без -refresh), стоп на 403 с резюмом. Аватары на CDN hsmedia
// гео-блокируются, поэтому -proxy через telegram_proxy (как backfill).
func personasAvatarsFetch(ctx context.Context, ar *archive.Store, opt avatarOpts) error {
	cfg, err := config.Load(opt.cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	var hc *http.Client
	if opt.proxy {
		if hc, err = tgx.ProxyClient(cfg.TelegramProxy); err != nil {
			return err
		}
		if hc == nil {
			return fmt.Errorf("avatars fetch -proxy: telegram_proxy не задан в конфиге")
		}
		log.Info("аватары качаются через прокси (telegram_proxy)")
	}
	client := love.NewWithClient(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(opt.intervalMS)*time.Millisecond, hc, log)

	marked, err := ar.MarkDefaultAvatars(ctx, time.Now())
	if err != nil {
		return err
	}
	targets, err := ar.AvatarTargets(ctx, opt.refresh, opt.limit)
	if err != nil {
		return err
	}
	log.Info("avatars fetch", "дефолтов_помечено", marked, "к_загрузке", len(targets),
		"воркеров", opt.workers, "proxy", opt.proxy)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "avatars fetch: нечего качать (всё уже хэшировано или только дефолты)")
		return nil
	}
	return runAvatarFetch(ctx, client, ar, targets, opt.workers, log)
}

// runAvatarFetch крутит пул воркеров: каждый качает аватар, считает dHash и
// сохраняет. Ошибка одного аватара не валит прогон (kind='error'); 403 —
// вероятный блок IP, останавливаемся с резюмом.
func runAvatarFetch(ctx context.Context, client *love.Client, ar *archive.Store, targets []archive.AvatarTarget, workers int, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan archive.AvatarTarget, workers*4)
	var ok, failed atomic.Int64
	var blocked atomic.Bool
	var wg sync.WaitGroup
	start := time.Now()

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for t := range jobs {
				err := fetchOneAvatar(ctx, client, ar, t)
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, love.ErrForbidden):
					blocked.Store(true)
					cancel()
					return
				case errors.Is(err, context.Canceled):
					return
				default:
					failed.Add(1)
					if err := ar.SaveAvatarHash(ctx, t.UserID, t.URL, nil, "error", time.Now()); err != nil {
						log.Warn("avatars: отметка ошибки не записана", "user", t.UserID, "err", err)
					}
				}
				if n := ok.Load() + failed.Load(); n%200 == 0 {
					el := time.Since(start).Seconds()
					log.Info("avatars", "готово", n, "ok", ok.Load(), "err", failed.Load(),
						"в_сек", fmt.Sprintf("%.1f", float64(n)/el))
				}
			}
		}()
	}

	// Раздача останавливается по отмене (403 гасит контекст из воркера):
	// иначе продюсер вхолостую докручивал бы весь остаток списка — а это
	// десятки тысяч анкет.
producer:
	for _, t := range targets {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break producer
		}
	}
	close(jobs)
	wg.Wait()

	log.Info("avatars fetch завершён", "ok", ok.Load(), "ошибок", failed.Load(),
		"время", time.Since(start).Round(time.Second))
	if blocked.Load() {
		return fmt.Errorf("остановлено на 403 (блок IP): сохранено %d; перезапустите — уже скачанные пропустятся", ok.Load())
	}
	return nil
}

// fetchOneAvatar качает один аватар, считает dHash и сохраняет kind='ok'.
func fetchOneAvatar(ctx context.Context, client *love.Client, ar *archive.Store, t archive.AvatarTarget) error {
	data, err := client.FetchMedia(ctx, t.URL)
	if err != nil {
		return err
	}
	h, err := archive.DHashFromBytes(data)
	if err != nil {
		return err // битая/неподдерживаемая картинка — наверх как обычная ошибка
	}
	return ar.SaveAvatarHash(ctx, t.UserID, t.URL, &h, "ok", time.Now())
}

// personasAvatarsCluster ищет визуально похожие аватары и пишет их в
// alias_candidates(avatar_phash). Дальше — `personas cluster` для склейки в личности.
func personasAvatarsCluster(ctx context.Context, ar *archive.Store, opt avatarOpts) error {
	cov, err := ar.AvatarCoverage(ctx)
	if err != nil {
		return err
	}
	st, err := ar.ClusterAvatars(ctx, opt.maxDist, opt.genericMax, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"avatars cluster: хэшей ok=%d (дефолтов=%d, ошибок=%d); generic-групп=%d (пропущено %d анкет); "+
			"рёбер avatar_phash=%d (max-dist=%d)\n",
		cov.OK, cov.Default, cov.Error, st.GenericHash, st.Skipped, st.Pairs, opt.maxDist)
	fmt.Fprintln(os.Stderr, "дальше: `personas cluster` — склейка всех сигналов в личности")
	return nil
}
