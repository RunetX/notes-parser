package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/tgx"
)

// defaultMinNoteID — первая заметка, у которой ещё сохранились комментарии
// (у более древних их удалили). Нижняя граница массовой выгрузки по умолчанию.
const defaultMinNoteID = 240866

// enumMaxPages — страховочный предел числа страниц ленты при перечислении.
const enumMaxPages = 5000

// backfillParams — параметры прогона (сгруппированы, чтобы не плодить сигнатуру).
type backfillParams struct {
	fromID, toID   int64
	workers, limit int
	startPage      int // с какой страницы ленты начинать обход (1 — с самой свежей)
}

// cmdBackfill — массовая выгрузка заметок диапазона в archive.db. Перечисляет
// живые id обходом ленты (пропуская удалённые) и сразу же скармливает их пулу
// воркеров, которые тянут каждую заметку в древовидном виде (1 запрос) и
// нормализуют — перечисление и загрузка идут внахлёст. Идемпотентно: уже
// загруженные пропускаются, так что после остановки/блока (403) достаточно
// перезапустить. С -proxy сайт идёт через telegram_proxy (бережём основной IP).
func cmdBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	useProxy := fs.Bool("proxy", false, "ходить к сайту через telegram_proxy (беречь основной IP)")
	workers := fs.Int("workers", 6, "число параллельных воркеров")
	intervalMS := fs.Int("interval-ms", 150, "минимальный интервал между запросами (потолок темпа)")
	fromID := fs.Int64("from", defaultMinNoteID, "нижняя граница id")
	toID := fs.Int64("to", 0, "верхняя граница id (0 — от самой свежей)")
	refresh := fs.Bool("refresh", false, "пере-обходить уже загруженные заметки")
	limit := fs.Int("limit", 0, "ограничить число заметок в прогоне (0 — все; для теста)")
	startPage := fs.Int("start-page", 1, "начать обход ленты с этой страницы (id≈240866 ≈ стр. 1500; экономит тысячи страниц на старом диапазоне)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "workers": true, "interval-ms": true,
		"from": true, "to": true, "limit": true, "start-page": true,
	})); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	var hc *http.Client
	if *useProxy {
		hc, err = tgx.ProxyClient(cfg.TelegramProxy)
		if err != nil {
			return err
		}
		if hc == nil {
			return fmt.Errorf("backfill -proxy: telegram_proxy не задан в конфиге")
		}
		log.Info("сайт маршрутизируется через прокси (telegram_proxy)")
	}
	client := love.NewWithClient(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(*intervalMS)*time.Millisecond, hc, log)

	ar, err := archive.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	known := map[int64]bool{}
	if !*refresh {
		if known, err = ar.KnownNoteIDs(ctx); err != nil {
			return err
		}
	}
	log.Info("старт backfill", "from", *fromID, "to", *toID, "воркеров", *workers,
		"стартовая_страница", *startPage, "уже_в_архиве", len(known), "proxy", *useProxy)

	return runBackfill(ctx, client, ar, cfg.Site.BaseURL,
		backfillParams{fromID: *fromID, toID: *toID, workers: *workers, limit: *limit,
			startPage: *startPage}, known, log)
}

// runBackfill запускает пул воркеров и в этой же горутине идёт продюсером:
// перечисляет id по ленте и отправляет их в канал заданий. Общий лимитер
// клиента держит темп (сайт видит ≤1 запрос/интервал независимо от числа
// воркеров). Каждая заметка сохраняется своей транзакцией, поэтому остановка
// на 403 не теряет прогресс — перезапуск продолжит с пропуском загруженного.
func runBackfill(ctx context.Context, client *love.Client, ar *archive.Store, baseURL string, p backfillParams, known map[int64]bool, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int64, p.workers*4)
	var (
		done, savedNotes, savedComments, discovered atomic.Int64
		blocked                                     atomic.Bool
		wg                                          sync.WaitGroup
	)
	start := time.Now()

	wg.Add(p.workers)
	for i := 0; i < p.workers; i++ {
		go func() {
			defer wg.Done()
			for id := range jobs {
				n, err := backfillOne(ctx, client, ar, baseURL, id, log)
				if err != nil {
					if errors.Is(err, love.ErrForbidden) {
						blocked.Store(true)
						cancel()
						return
					}
					if errors.Is(err, context.Canceled) {
						return
					}
					log.Warn("заметка не выгружена", "id", id, "err", err)
					done.Add(1)
					continue
				}
				savedNotes.Add(1)
				savedComments.Add(int64(n))
				if d := done.Add(1); d%200 == 0 {
					el := time.Since(start).Seconds()
					log.Info("прогресс", "готово", d, "поставлено", discovered.Load(),
						"req_в_сек", fmt.Sprintf("%.2f", float64(d)/el))
				}
			}
		}()
	}

	// Продюсер: перечисляем id по ленте и сразу отдаём воркерам.
	enumErr := feedFromListing(ctx, client, jobs, p, known, &discovered, log)
	close(jobs)
	wg.Wait()

	log.Info("бэкофилл завершён", "заметок", savedNotes.Load(), "комментариев", savedComments.Load(),
		"поставлено", discovered.Load(), "время", time.Since(start).Round(time.Second))

	switch {
	case blocked.Load():
		return fmt.Errorf("остановлено на 403 (блок IP): сохранено %d заметок; перезапустите backfill — уже загруженные пропустятся", savedNotes.Load())
	case enumErr != nil && !errors.Is(enumErr, context.Canceled):
		return fmt.Errorf("перечисление ленты: %w", enumErr)
	default:
		return nil
	}
}

// backfillOne выгружает одну заметку (tree, 1 запрос) и нормализует в архив.
func backfillOne(ctx context.Context, client *love.Client, ar *archive.Store, baseURL string, id int64, log *slog.Logger) (int, error) {
	res, err := grabNote(ctx, client, grabOptions{
		baseURL: baseURL, noteID: strconv.FormatInt(id, 10), view: love.ViewTree,
	}, log)
	if err != nil {
		return 0, err
	}
	note, comments, users, err := mapGrabToArchive(baseURL, res)
	if err != nil {
		return 0, err
	}
	if _, err := ar.SaveGrab(ctx, note, comments, users, time.Now()); err != nil {
		return 0, err
	}
	return len(comments), nil
}

// feedFromListing обходит ленту (новые → старые) и отправляет в jobs id живых
// заметок из [from, to], которых ещё нет в known. Останавливается, когда лента
// опускается ниже from, достигнут limit, контекст отменён или сайт вернул 403.
func feedFromListing(ctx context.Context, client *love.Client, jobs chan<- int64, p backfillParams, known map[int64]bool, discovered *atomic.Int64, log *slog.Logger) error {
	seen := map[int64]bool{}
	start := p.startPage
	if start < 1 {
		start = 1
	}
	// Лента идёт новые→старые, а -from/-to лишь фильтруют найденное, поэтому без
	// -start-page обход старого диапазона тратит тысячи страниц на уже собранное
	// (id ≈ 240866 лежит около страницы 1500).
	for page := start; page <= enumMaxPages; page++ {
		raw, err := client.RawNotesPage(ctx, page)
		if err != nil {
			if errors.Is(err, love.ErrForbidden) || errors.Is(err, context.Canceled) {
				return err
			}
			if page == 1 {
				return err
			}
			log.Warn("лента: страница не получена, останавливаю перечисление", "page", page, "err", err)
			return nil
		}
		notes, err := love.ParseNotes(bytes.NewReader(raw))
		if err != nil {
			var me *love.MarkupError
			if errors.As(err, &me) {
				return nil // лента кончилась/пуста
			}
			return err
		}
		if len(notes) == 0 {
			return nil
		}
		var pageMin int64 = 1 << 62
		for _, n := range notes {
			id, e := strconv.ParseInt(n.ID, 10, 64)
			if e != nil || id == 0 {
				continue
			}
			if id < pageMin {
				pageMin = id
			}
			if id < p.fromID || (p.toID > 0 && id > p.toID) || seen[id] || known[id] {
				continue
			}
			seen[id] = true
			select {
			case jobs <- id:
				if n := discovered.Add(1); p.limit > 0 && n >= int64(p.limit) {
					return nil
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if page%20 == 0 {
			log.Info("лента", "page", page, "поставлено", discovered.Load(), "нижний_id", pageMin)
		}
		if pageMin <= p.fromID {
			return nil // прошли нижнюю границу
		}
	}
	return nil
}
