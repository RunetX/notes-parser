package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/love"
)

// repliesOpts — параметры действия replies (из флагов cmdPersonas).
type repliesOpts struct {
	cfgPath     string
	since       string // ISO-дата: обойти только заметки не старше (пусто — весь архив)
	limit       int    // максимум заметок за прогон (0 — все)
	maxComments int    // пропускать треды длиннее (0 — не пропускать)
	retry       bool   // повторить заметки, на которых страница уже падала
}

// personasReplies обогащает уже выкачанный архив настоящими целями ответа:
// обходит страницы заметок МОБИЛЬНОЙ версии (m.love.ngs.ru/notes/<id>/) и
// записывает пары «реплика → та, которой отвечают».
//
// Зачем. Десктопная страница, по которой шёл грабинг, отдаёт в parent_id корень
// ветки, а не адресата; слой адресатов (personas addressees) восстанавливает по
// обращению «Ник, …» только человека и только там, где обращение есть. У
// мобильной дерево настоящее — точная реплика, в том числе у ответов без
// обращения.
//
// Механизм выгрузки не трогается: тексты, авторы и даты остаются десктопными
// (на мобильной даты относительные), отсюда берутся только пары id.
//
// Идемпотентно и резюмируемо: обойдённые заметки помечаются в reply_scan и
// повторно не берутся. Длинные треды сайт отдавать отказывается (500) — это
// штатный исход, он тоже помечается; -retry берёт такие снова, -max-comments
// заранее их не трогает.
func personasReplies(ctx context.Context, ar *archive.Store, opt repliesOpts) error {
	cfg, err := config.Load(opt.cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	ids, err := ar.ReplyScanTargets(ctx, opt.limit, opt.maxComments, opt.since, opt.retry)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "replies: нечего обходить (все заметки уже размечены)")
		return printReplyCoverage(ctx, ar)
	}
	// -limit общий для всех действий personas и по умолчанию не нулевой, так
	// что выборку легко обрезать, не заметив: хвост остаётся необойдённым, а
	// покрытие выглядит полным. Молчать об этом нельзя.
	if opt.limit > 0 && len(ids) == opt.limit {
		fmt.Fprintf(os.Stderr,
			"replies: выборка обрезана до -limit %d — хвост останется на следующий прогон\n", opt.limit)
	}

	client, err := repliesClient(cfg, log)
	if err != nil {
		return err
	}
	log.Info("обход дерева ответов через мобильную версию", "заметок", len(ids))

	res, err := repliesCrawl(ctx, ar, client, ids, log)
	if err != nil {
		return err
	}
	tail := ""
	if res.blocked {
		tail = fmt.Sprintf(" (остановлено на 403 после %d/%d — прогоните ещё раз позже)", res.done, len(ids))
	}
	fmt.Fprintf(os.Stderr, "replies: обойдено %d заметок, записано %d пар, не отдалось %d%s\n",
		res.done, res.pairs, res.failed+res.noNote, tail)
	if res.noNote > 0 {
		// Заметки без текста на странице — обычно просто удалённые с сайта.
		// Но так же выглядит и дрейф вёрстки, поэтому число видно отдельно:
		// если оно вдруг сравнимо с числом обойдённых, дело не в удалениях.
		fmt.Fprintf(os.Stderr, "replies: из них %d страниц без заметки (удалена с сайта — или уехала вёрстка)\n",
			res.noNote)
	}
	fmt.Fprintln(os.Stderr, "replies: чтобы граф увидел новые пары, пересчитайте `personas addressees`")
	return printReplyCoverage(ctx, ar)
}

// repliesClient — клиент мобильной версии. Как в gender: свой cookie jar (куки
// DDoS-Guard живут между запросами, иначе каждый запрос выглядит новым
// клиентом) и строгий темп без стартового «залпа». Сессия не нужна —
// страницы заметок отдаются гостю.
func repliesClient(cfg *config.Config, log *slog.Logger) (*love.Client, error) {
	mobileBase, err := love.MobileBaseURL(cfg.Site.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("replies: %w", err)
	}
	if _, err := url.Parse(mobileBase); err != nil {
		return nil, fmt.Errorf("replies: base_url: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	// Тред целиком сайт рендерит долго — таймаут щедрее обычного, но конечный:
	// на неподъёмных заметках ответ всё равно не придёт.
	client := love.NewWithClient(mobileBase, genderMobileUA,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond,
		&http.Client{Timeout: repliesTimeout, Jar: jar}, log)
	client.StrictPacing()
	return client, nil
}

// repliesTimeout — потолок ожидания страницы. Мерено: тред на 248
// комментариев отдаётся за ~2 c, на 848 сайт думает минуту и отвечает 500.
const repliesTimeout = 45 * time.Second

// repliesResult — итог обхода.
type repliesResult struct {
	done    int  // заметок обработано
	pairs   int  // записано пар «реплика → адресат»
	failed  int  // страница не отдалась (500/таймаут — свойство длинных тредов)
	noNote  int  // страница отдалась, но заметки на ней нет (удалена — или дрейф)
	blocked bool // волны 403 исчерпаны
}

// repliesCrawl обходит заметки и пишет деревья в архив. Волны 403 пережидаются
// тем же терпением, что и обход анкет; отказ на конкретной заметке — не повод
// останавливаться: помечаем её и идём дальше.
func repliesCrawl(ctx context.Context, ar *archive.Store, client *love.Client,
	ids []int64, log *slog.Logger) (repliesResult, error) {
	var res repliesResult
	w := wave403{cool: genderCoolMin}
	for i := 0; i < len(ids); {
		if err := sleepCtx(ctx, time.Duration(rand.Int64N(int64(genderJitterMax)))); err != nil {
			return res, err
		}
		tree, ferr := client.FetchNoteReplyTree(ctx, strconv.FormatInt(ids[i], 10))
		if errors.Is(ferr, love.ErrForbidden) {
			stop, werr := w.wait(ctx, res.done, len(ids), log)
			if stop || werr != nil {
				res.blocked = stop
				return res, werr
			}
			continue
		}
		if err := repliesStore(ctx, ar, ids[i], tree, ferr, &res, log); err != nil {
			return res, err
		}
		w.reset()
		res.done++
		i++
		if res.done%25 == 0 {
			log.Info("прогресс обхода", "готово", res.done, "из", len(ids), "пар", res.pairs)
		}
	}
	return res, nil
}

// repliesStore разбирает исход одной заметки: записывает дерево либо помечает
// заметку необойдённой. Возвращает ошибку только на сбое самого архива —
// отказы сайта штатны и считаются в res.
func repliesStore(ctx context.Context, ar *archive.Store, noteID int64,
	tree map[int64]int64, ferr error, res *repliesResult, log *slog.Logger) error {
	if ferr != nil {
		var me *love.MarkupError
		if errors.As(ferr, &me) {
			log.Warn("страница без заметки", "note", noteID, "err", ferr)
			res.noNote++
		} else {
			log.Warn("дерево не снято", "note", noteID, "err", ferr)
			res.failed++
		}
		return ar.MarkReplyScanFailed(ctx, noteID)
	}
	saved, err := ar.SaveReplyTree(ctx, noteID, tree)
	if err != nil {
		return err
	}
	res.pairs += saved.Stored
	if saved.Unknown > 0 {
		log.Info("на странице есть комментарии, которых нет в архиве",
			"note", noteID, "новых", saved.Unknown)
	}
	return nil
}

// printReplyCoverage печатает покрытие архива слоем настоящих целей ответа.
func printReplyCoverage(ctx context.Context, ar *archive.Store) error {
	st, err := ar.ReplyScanStats(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n  заметок в архиве:       %9d\n", st.Notes)
	fmt.Printf("  из них размечено:       %9d  (%.1f%%)\n", st.ScannedOK, pct(st.ScannedOK, st.Notes))
	fmt.Printf("  страница не отдалась:   %9d\n", st.Failed)
	fmt.Printf("  точных пар «кому»:      %9d  из %d ответов (%.1f%%)\n",
		st.Pairs, st.Replies, pct(st.Pairs, st.Replies))
	return nil
}
