package main

import (
	"context"
	"errors"
	"fmt"
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

// personasGender размечает пол анкет обходом их профилей love.ngs.ru
// (/profile/<id>/, класс заголовка _male/_female) ПОД СЕССИЕЙ пользователя:
// куки берутся из основной БД бота (таблица sessions). Идемпотентно —
// обходятся только анкеты без пола; запросы троттлятся клиентом. -active-days
// N обходит когорту отчёта (самых активных), иначе — все без пола до -limit.
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

	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
	log.Info("обход профилей под сессией", "tg", tgUser, "анкет", len(ids))

	var done, set int
	blocked := false
	for _, id := range ids {
		gender, err := client.FetchGender(ctx, cookies, strconv.FormatInt(id, 10))
		if err != nil {
			if errors.Is(err, love.ErrForbidden) {
				// 403 — это про IP/троттлинг DDoS-Guard, НЕ про сессию: сессию не
				// трогаем (иначе сломаем постинг бота). Останавливаемся мягко —
				// обход идемпотентен, остаток доберётся повторным прогоном позже.
				log.Warn("403 — троттлинг/геоблок, стоп; остаток доберётся позже", "id", id,
					"обойдено", done, "из", len(ids))
				blocked = true
				break
			}
			log.Warn("профиль пропущен", "id", id, "err", err)
			done++
			continue
		}
		if gender != "" {
			if _, err := ar.SetUserGender(ctx, id, gender); err != nil {
				return err
			}
			set++
		}
		done++
		if done%25 == 0 {
			log.Info("прогресс обхода", "готово", done, "из", len(ids), "размечено", set)
		}
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
