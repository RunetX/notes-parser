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
	"lovegw/internal/store"
)

// genderOpts — параметры действия gender (из флагов cmdPersonas).
type genderOpts struct {
	cfgPath    string
	tgUser     int64 // чья сессия сайта (0 — admin_tg_user_id из конфига)
	activeDays int   // >0 — обойти только анкеты активной за окно когорты
	reportTop  int   // размер когорты (как в report)
	limit      int   // сплошной режим: максимум анкет за прогон (0 — все)
}

// personasGender размечает пол анкет обходом их профилей через мобильную
// версию (m.love.ngs.ru/profile/<id>/, sex из JSON dataFromBlade.layout) ПОД
// СЕССИЕЙ пользователя: куки берутся из основной БД бота (таблица sessions).
// Идемпотентно — обходятся только анкеты без пола; запросы троттлятся
// клиентом, волны 403 пережидаются. -active-days N обходит когорту отчёта
// (самых активных), иначе — все без пола до -limit.
func personasGender(ctx context.Context, ar *archive.Store, opt genderOpts) error {
	cfg, err := config.Load(opt.cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	// Куки сессии Рантье — из основной БД бота.
	tgUser := opt.tgUser
	if tgUser == 0 {
		tgUser = cfg.AdminTGUserID
	}
	if tgUser == 0 {
		return fmt.Errorf("gender: не задан admin_tg_user_id в конфиге (или -tg-user)")
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("gender: открытие БД бота %s: %w", cfg.DBPath, err)
	}
	defer st.Close()
	cookiesJSON, valid, err := st.SessionCookies(ctx, tgUser)
	if err != nil {
		return fmt.Errorf("gender: сессия tg=%d: %w", tgUser, err)
	}
	if cookiesJSON == "" {
		return fmt.Errorf("gender: у tg=%d нет сохранённой сессии сайта — сначала /login в РюмкинЪ", tgUser)
	}
	if !valid {
		log.Warn("сессия помечена невалидной — пробуем всё равно", "tg", tgUser)
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		return fmt.Errorf("gender: разбор кук сессии: %w", err)
	}

	ids, err := genderTargets(ctx, ar, opt)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "gender: нечего обходить (все нужные анкеты уже с полом)")
		return nil
	}

	client, err := genderClient(cfg, cookies, log)
	if err != nil {
		return err
	}
	log.Info("обход профилей под сессией", "tg", tgUser, "анкет", len(ids))

	done, set, blocked, err := genderCrawl(ctx, ar, client, ids, log)
	if err != nil {
		return err
	}

	male, female, unknown, err := ar.GenderStats(ctx)
	if err != nil {
		return err
	}
	tail := ""
	if blocked {
		tail = fmt.Sprintf(" (остановлено на 403 после %d/%d — прогоните ещё раз позже)", done, len(ids))
	}
	fmt.Fprintf(os.Stderr, "gender: обойдено %d анкет, размечено %d%s; в архиве муж %d / жен %d / без пола %d\n",
		done, set, tail, male, female, unknown)
	return nil
}

// genderMobileUA — User-Agent мобильного браузера для m.love.ngs.ru: десктопный
// UA мобильная версия может уводить редиректом на полную.
const genderMobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"

// genderClient — клиент для массового обхода профилей ЧЕРЕЗ МОБИЛЬНУЮ ВЕРСИЮ
// (m.love.ngs.ru): её vhost переносит серию запросов, на которой десктопный
// DDoS-Guard банит IP почти сразу. Отличия от обычного клиента: cookie jar
// (куки DDoS-Guard из ответов живут между запросами — без них каждый запрос
// выглядит новым клиентом; туда же сажаются куки сессии, на m. они подходят)
// и строгий темп без стартового «залпа» лимитера.
func genderClient(cfg *config.Config, cookies []*http.Cookie, log *slog.Logger) (*love.Client, error) {
	mobileBase, err := love.MobileBaseURL(cfg.Site.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("gender: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(mobileBase)
	if err != nil {
		return nil, fmt.Errorf("gender: base_url: %w", err)
	}
	jar.SetCookies(base, cookies)
	client := love.NewWithClient(mobileBase, genderMobileUA,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond,
		&http.Client{Timeout: 20 * time.Second, Jar: jar}, log)
	client.StrictPacing()
	return client, nil
}

// Терпение обхода к 403: блок DDoS-Guard обычно временный, поэтому вместо
// стопа ждём с нарастающей паузой и пробуем ту же анкету снова; сдаёмся после
// genderMaxWaves безуспешных волн подряд.
const (
	genderMaxWaves = 6
	genderCoolMin  = time.Minute
	genderCoolMax  = 15 * time.Minute
)

// genderJitterMax — случайная добавка к интервалу лимитера перед каждым
// запросом: ровный машинный темп сам по себе подозрителен для DDoS-Guard.
const genderJitterMax = 1500 * time.Millisecond

// wave403 — терпение обхода к 403: счётчик волн подряд и нарастающая пауза.
type wave403 struct {
	n    int
	cool time.Duration
}

// wait пережидает очередную волну 403; stop=true — волны исчерпаны, пора
// останавливаться (остаток доберётся повторным прогоном, обход идемпотентен).
func (w *wave403) wait(ctx context.Context, done, total int, log *slog.Logger) (stop bool, err error) {
	w.n++
	if w.n > genderMaxWaves {
		log.Warn("403 не отпускает — стоп; остаток доберётся позже", "обойдено", done, "из", total)
		return true, nil
	}
	log.Warn("403 — пауза перед повтором", "пауза", w.cool, "волна", w.n, "обойдено", done, "из", total)
	if err := sleepCtx(ctx, w.cool); err != nil {
		return false, err
	}
	w.cool = min(w.cool*2, genderCoolMax)
	return false, nil
}

// reset — успешный запрос обнуляет волны.
func (w *wave403) reset() { w.n, w.cool = 0, genderCoolMin }

// genderCrawl обходит профили по списку id и проставляет пол в архив. Куки
// (сессия + DDoS-Guard) несёт jar клиента. blocked=true — волны 403 исчерпаны.
// 403 — это про IP/троттлинг, НЕ про сессию: сессию не трогаем, иначе сломаем
// постинг бота.
func genderCrawl(ctx context.Context, ar *archive.Store, client *love.Client, ids []int64, log *slog.Logger) (done, set int, blocked bool, err error) {
	w := wave403{cool: genderCoolMin}
	for i := 0; i < len(ids); {
		if err := sleepCtx(ctx, time.Duration(rand.Int64N(int64(genderJitterMax)))); err != nil {
			return done, set, false, err
		}
		gender, ferr := client.FetchGenderMobile(ctx, nil, strconv.FormatInt(ids[i], 10))
		switch {
		case errors.Is(ferr, love.ErrForbidden):
			stop, werr := w.wait(ctx, done, len(ids), log)
			if stop || werr != nil {
				return done, set, stop, werr
			}
			continue
		case ferr != nil:
			log.Warn("профиль пропущен", "id", ids[i], "err", ferr)
		case gender != "":
			if _, err := ar.SetUserGender(ctx, ids[i], gender); err != nil {
				return done, set, false, err
			}
			set++
		}
		w.reset()
		done++
		i++
		if done%25 == 0 {
			log.Info("прогресс обхода", "готово", done, "из", len(ids), "размечено", set)
		}
	}
	return done, set, false, nil
}

// sleepCtx ждёт d либо отмену контекста.
func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// genderTargets — какие анкеты обходить: когорту отчёта (-active-days) или все
// без пола до -limit.
func genderTargets(ctx context.Context, ar *archive.Store, opt genderOpts) ([]int64, error) {
	if opt.activeDays <= 0 {
		return ar.UsersMissingGender(ctx, opt.limit)
	}
	since, err := recentSince(ctx, ar, opt.activeDays)
	if err != nil {
		return nil, err
	}
	weekly, err := ar.ActiveCountsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	return ar.AccountsMissingGender(ctx, cohortTop(weekly, opt.reportTop))
}
