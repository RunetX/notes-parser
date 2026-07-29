package main

// lovegw talks watch — безопасный прогон ТОЛЬКО поллера личной переписки
// (talks), без зеркала и без long polling. Читает диалоги под сессией админа и
// доставляет входящие ЛС в его личку через ЛС-бота методом SendMessage — а не
// getUpdates, — поэтому не конфликтует с боевым демоном на тех же токенах
// (получение апдейтов у Telegram single-consumer, отправка — нет). Жёстко
// read-only: allow_send=false, mark-read не трогаем, на сайт ничего не пишется.
// Рассчитан на КОПИЮ БД (флаг -db): открытие мигрирует схему до v5.

import (
	"context"
	"flag"
	"fmt"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/dmbot"
	"lovegw/internal/love"
	"lovegw/internal/store"
	"lovegw/internal/talks"
	"lovegw/internal/tgx"
)

func cmdTalks(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("talks", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", "", "путь к БД — КОПИЯ для read-only прогона (прогон мигрирует её до v5)")
	interval := fs.Duration("interval", 20*time.Second, "интервал опроса диалогов")
	maxDialogs := fs.Int("max-dialogs", 2, "диалогов дозабирать историей за тик (бюджет запросов)")
	historyLimit := fs.Int("history-limit", 8, "предел сообщений за один запрос истории")
	once := fs.Bool("once", false, "один проход и выход (иначе цикл до Ctrl+C)")
	backfill := fs.Int("backfill", 0, "разово доставить последние N входящих из каждого диалога (обкатка на существующей переписке); 0 — обычный поллинг")
	testSend := fs.Bool("test-send", false, "диагностика доставки: показать @ЛС-бота и отправить одно тестовое ЛС")
	to := fs.Int64("to", 0, "Telegram chat_id получателя (0 — сам админ); для показа в другой аккаунт")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "interval": true, "max-dialogs": true, "history-limit": true, "backfill": true, "to": true,
	})); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.Arg(0) != "watch" {
		return fmt.Errorf("использование: lovegw talks watch -db <копия.db> [-once] [-interval 20s] [-max-dialogs 2] [-history-limit 8]")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// Обязательная копия: прогон открывает БД на запись и мигрирует схему v3→v5.
	// Боевую/dev-БД трогать нельзя, поэтому -db без дефолта на cfg.DBPath.
	if *dbPath == "" {
		return fmt.Errorf("talks: укажите -db <копия БД>; прогон мигрирует её до v5 — рабочую БД не трогаем")
	}
	cfg.DBPath = *dbPath

	tgCfg := cfg.Messengers.Telegram
	if !tgCfg.Enabled || tgCfg.DMToken == "" {
		return fmt.Errorf("talks: нужен telegram ЛС-бот (dm_token) для доставки входящих")
	}
	if tgCfg.AdminUserID == 0 {
		return fmt.Errorf("talks: не задан admin id (admin_tg_user_id) — admin-only не выберет сессию")
	}

	log := newLogger(cfg.LogLevel)
	st, err := store.Open(ctx, cfg.DBPath) // мигрирует копию v3→v5
	if err != nil {
		return err
	}
	defer st.Close()

	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
	if err != nil {
		return err
	}
	// ЛС-бот создаётся, но НЕ стартуется: Start() — это long polling getUpdates,
	// а нам нужны только SendPM/Confirm (SendMessage). Так боевой РюмкинЪ на том
	// же токене продолжает получать апдейты без конфликта.
	dm, err := dmbot.New(tgCfg.DMToken, st, client, tgClient, log)
	if err != nil {
		return err
	}

	dest := *to
	if dest == 0 {
		dest = tgCfg.AdminUserID
	}

	if *testSend {
		if me, err := tgx.CheckToken(ctx, tgCfg.DMToken, tgClient); err != nil {
			fmt.Printf("getMe ЛС-бота: ОШИБКА %v\n", err)
		} else {
			fmt.Printf("ЛС-бот: @%s — тестовое ЛС ищи в диалоге ИМЕННО с ним\n", me.Username)
		}
		id, err := dm.SendPM(ctx, dest, "🔧 talks watch: тест доставки. Видишь это — мост в Telegram работает.")
		if err != nil {
			fmt.Printf("SendPM в чат %d: ОШИБКА %v\n", dest, err)
			return err
		}
		fmt.Printf("SendPM в чат %d: ok, message_id=%s\n", dest, id)
		return nil
	}

	w := talks.New(st, talksSite{client}, []talks.PMTransport{dm}, talks.Config{
		BaseURL:      cfg.Site.BaseURL,
		AdminOnly:    true,
		AdminIDs:     map[string]int64{store.MessengerTelegram: tgCfg.AdminUserID},
		Interval:     *interval,
		IdleInterval: *interval,
		MaxDialogs:   *maxDialogs,
		HistoryLimit: *historyLimit,
		AllowSend:    false, // read-only: на сайт не пишем
		StoreText:    false, // приватность: текст в БД не кладём
		MaxReqPerMin: cfg.Talks.MaxRequestsPerMin,
	}, log)

	fmt.Printf("talks watch (read-only): admin=%d, db=%s, interval=%s, max-dialogs=%d, history-limit=%d\n",
		tgCfg.AdminUserID, cfg.DBPath, *interval, *maxDialogs, *historyLimit)
	fmt.Println("на сайт ничего не пишется; непрочитанные входящие ЛС уедут в личку админа. Бот НЕ поллит getUpdates.")

	if *backfill > 0 {
		n, err := w.DeliverExisting(ctx, *maxDialogs, *backfill, *to)
		if err != nil {
			return err
		}
		fmt.Printf("бэкфилл: доставлено %d сообщений в чат %d (последние %d входящих из ≤%d диалогов)\n",
			n, dest, *backfill, *maxDialogs)
		return nil
	}
	if *once {
		active := w.PollOnce(ctx)
		fmt.Printf("проход завершён; была новая активность: %v\n", active)
		return nil
	}
	return w.Run(ctx)
}
