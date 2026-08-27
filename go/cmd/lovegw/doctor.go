package main

// lovegw doctor — диагностика окружения: конфиг, БД, доступ к сайту,
// токены ботов, очередь обновлений. С флагом -post-test дополнительно
// проверяется сквозная механика «пост в канал → автофорвард в группу»
// тихим тестовым сообщением, которое удаляется после проверки.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/asr"
	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/maxx"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
)

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	postTest := fs.Bool("post-test", false, "тестовый пост в канал с проверкой автофорварда (сообщение удаляется)")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}

	ok := func(name, detail string) { fmt.Printf("  ✔ %-22s %s\n", name, detail) }
	warn := func(name, detail string) { fmt.Printf("  ⚠ %-22s %s\n", name, detail) }
	fail := func(name string, err error) { fmt.Printf("  ✘ %-22s %v\n", name, err) }

	fmt.Println("lovegw doctor:")

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail("конфиг", err)
		return errors.New("диагностика прервана")
	}
	ok("конфиг", *cfgPath)
	for _, w := range cfg.Warnings {
		warn("конфиг", w)
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		fail("база данных", err)
		return errors.New("диагностика прервана")
	}
	defer st.Close()
	ids, err := st.KnownNoteIDs(ctx)
	if err != nil {
		fail("база данных", err)
		return errors.New("диагностика прервана")
	}
	ok("база данных", fmt.Sprintf("%s, заметок: %d", cfg.DBPath, len(ids)))
	doctorSecrets(ctx, cfg, st, ok, warn, fail)

	client := newSiteClient(cfg, nil)
	if _, err := client.RawNotes(ctx); err != nil {
		if errors.Is(err, love.ErrForbidden) {
			warn("сайт", "403: геоблок — нужен разрешённый (российский) IP")
		} else {
			fail("сайт", err)
		}
	} else {
		ok("сайт", cfg.Site.BaseURL)
	}

	tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
	if err != nil {
		fail("прокси telegram", err)
		return errors.New("диагностика прервана")
	}
	if cfg.TelegramProxy != "" {
		ok("прокси telegram", tgx.MaskProxy(cfg.TelegramProxy))
	}

	tgCfg := cfg.Messengers.Telegram
	mirrorOK := false
	if !tgCfg.Enabled {
		warn("telegram", "выключен (messengers.telegram.enabled=false) — проверки пропущены")
	} else {
		if tgCfg.Token == "" {
			warn("постер-бот", "токен не задан")
		} else if me, err := tgx.CheckToken(ctx, tgCfg.Token, tgClient); err != nil {
			fail("постер-бот", err)
		} else {
			ok("постер-бот", "@"+me.Username)
			mirrorOK = true
		}

		if tgCfg.DMToken == "" {
			warn("ЛС-бот", "токен не задан")
		} else if me, err := tgx.CheckToken(ctx, tgCfg.DMToken, tgClient); err != nil {
			fail("ЛС-бот", err)
		} else {
			ok("ЛС-бот", "@"+me.Username)
		}

		// Бот переписки (talks_token) опционален: без него переписку ведёт
		// ЛС-бот команд, как раньше.
		if tgCfg.TalksToken != "" {
			if me, err := tgx.CheckToken(ctx, tgCfg.TalksToken, tgClient); err != nil {
				fail("бот переписки", err)
			} else {
				ok("бот переписки", "@"+me.Username)
			}
		}
	}

	// MAX: проверка токена заодно проверяет TLS-доверие к platform-api2
	// (цепочка Минцифры) — при не вшитом/не установленном CA здесь будет
	// явная ошибка верификации сертификата.
	if maxCfg := cfg.Messengers.Max; maxCfg.Enabled {
		if maxCfg.Token == "" {
			warn("MAX-бот", "токен не задан (messengers.max.token или LOVEGW_MAX_TOKEN)")
		} else if mx, err := maxx.NewMirror(maxx.Params{
			Token:      maxCfg.Token,
			ChannelID:  maxCfg.ChannelID,
			BaseURL:    cfg.Site.BaseURL,
			HTTPClient: maxx.MintsifraClient(),
		}, nil); err != nil {
			fail("MAX-бот", err)
		} else if info, err := mx.Me(ctx); err != nil {
			fail("MAX-бот", err)
		} else {
			ok("MAX-бот", "@"+info.Username)
			if maxCfg.ChannelID == 0 {
				warn("MAX-канал", "chat_id не задан: снимите его из апдейта bot_added (GET /updates)")
			}
		}

		// Бот переписки MAX (talks_token; легаси-dm_token приводится к нему при
		// загрузке конфига) — опционален: без него личку ведёт бот зеркала.
		if maxCfg.TalksToken != "" {
			if pm, err := maxx.NewMirror(maxx.Params{
				Token:      maxCfg.TalksToken,
				BaseURL:    cfg.Site.BaseURL,
				HTTPClient: maxx.MintsifraClient(),
			}, nil); err != nil {
				fail("MAX бот переписки", err)
			} else if info, err := pm.Me(ctx); err != nil {
				fail("MAX бот переписки", err)
			} else {
				ok("MAX бот переписки", "@"+info.Username)
			}
		}
	}

	if cfg.Talks.Enabled {
		checked := false
		for _, m := range []struct {
			name    string
			adminID int64
			enabled bool
		}{
			{store.MessengerTelegram, tgCfg.AdminUserID,
				tgCfg.Enabled && (tgCfg.DMToken != "" || tgCfg.TalksToken != "")},
			{store.MessengerMax, cfg.Messengers.Max.AdminUserID, cfg.Messengers.Max.Enabled},
		} {
			if !m.enabled {
				continue
			}
			checked = true
			label := "talks/" + m.name
			// Мультисессия (admin_only=false): в обход идут валидные сессии, чьи
			// владельцы согласились на чтение переписки (сколько их — строкой
			// talks/чтение ниже). Admin-only — проверяем сессию админа поимённо.
			if !cfg.Talks.AdminOnly {
				owners, err := st.SessionOwners(ctx, m.name)
				switch {
				case err != nil:
					fail(label, err)
				case len(owners) == 0:
					warn(label, "мультисессия: нет валидных сессий сайта — некого обходить")
				default:
					ok(label, fmt.Sprintf("мультисессия: %d валидных сессий (обходим согласившихся)", len(owners)))
				}
				continue
			}
			if m.adminID == 0 {
				warn(label, "admin_user_id не задан — admin-only не выберет владельца сессии")
				continue
			}
			_, valid, err := st.SessionCookies(ctx, m.name, m.adminID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				warn(label, fmt.Sprintf("у admin %d нет сессии сайта — /login в РюмкинЪ", m.adminID))
			case err != nil:
				fail(label, err)
			case !valid:
				warn(label, "сессия админа невалидна — нужен /login")
			default:
				detail := "сессия ок"
				if _, pass, _, _ := st.SessionIdentity(ctx, m.name, m.adminID); pass == "" {
					detail += "; site-идентичность пуста (поллер снимет её сам на первом обходе)"
				}
				ok(label, detail)
			}
		}
		if !checked {
			warn("talks", "включён, но нет мессенджера с ЛС (telegram dm_token / max)")
		}
		if !cfg.Talks.AllowSend {
			warn("talks", "allow_send=false — только чтение, ответы на сайт не уходят")
		}
		checkDeliveryConsent(ctx, st, ok, warn, fail)
	}

	if cfg.ASR.Enabled {
		switch {
		case cfg.ASR.Provider != asrProviderNexara:
			fail("asr", fmt.Errorf("неизвестный провайдер %q", cfg.ASR.Provider))
		case cfg.ASR.APIKey == "":
			warn("asr", "api_key не задан (секция asr конфига или env LOVEGW_ASR_API_KEY)")
		default:
			ok("asr", fmt.Sprintf("%s, потолок %d сек, квота %d сек/сутки, воркеров %d",
				cfg.ASR.Provider, cfg.ASR.MaxDurationSec, cfg.ASR.UserDailyLimitSec, cfg.ASR.Concurrency))
		}
		// Без ffmpeg распознавание молча не работает: проверяем бинарник.
		if version, err := (&asr.FFmpeg{Path: cfg.ASR.FFmpegPath}).Check(ctx); err != nil {
			fail("asr/ffmpeg", err)
		} else {
			ok("asr/ffmpeg", version)
		}
	}

	queueEmpty := false
	if mirrorOK {
		switch n, err := tgx.ProbePendingUpdates(ctx, tgCfg.Token, tgClient); {
		case errors.Is(err, tgx.ErrPollingConflict):
			warn("очередь обновлений", "409: апдейты бота слушает другой процесс (второй экземпляр lovegw?)")
		case err != nil:
			fail("очередь обновлений", err)
		case n > 0:
			warn("очередь обновлений", fmt.Sprintf("%d необработанных обновлений (не трогаю — их ждёт другой потребитель?)", n))
		default:
			ok("очередь обновлений", "пусто")
			queueEmpty = true
		}
	}

	if !*postTest {
		return nil
	}
	if !mirrorOK {
		fail("post-test", errors.New("постер-бот недоступен"))
		return errors.New("post-test не выполнен")
	}
	if !queueEmpty {
		warn("post-test", "пропущен: очередь обновлений не пуста или занята другим процессом")
		return nil
	}
	return runPostTest(ctx, cfg, tgClient, ok, fail)
}

// runPostTest: тихий пост в канал → ожидание автофорварда → удаление
// форварда и поста. Проверяет права бота и механику тредов end-to-end.
func runPostTest(ctx context.Context, cfg *config.Config, tgClient *http.Client,
	ok func(string, string), fail func(string, error)) error {

	var postedID atomic.Int64
	forwardCh := make(chan int, 1)
	tgCfg := cfg.Messengers.Telegram
	handler := func(_ context.Context, u *models.Update) {
		msg := u.Message
		if msg == nil || !msg.IsAutomaticForward ||
			msg.ForwardOrigin == nil || msg.ForwardOrigin.MessageOriginChannel == nil {
			return
		}
		origin := msg.ForwardOrigin.MessageOriginChannel
		if origin.Chat.ID == tgCfg.ChannelID && int64(origin.MessageID) == postedID.Load() {
			select {
			case forwardCh <- msg.ID:
			default:
			}
		}
	}

	tg, err := tgx.NewMirror(tgx.Params{
		Token:            tgCfg.Token,
		ChannelID:        tgCfg.ChannelID,
		DiscussionChatID: tgCfg.DiscussionChatID,
		Signature:        tgCfg.Signature,
		HTTPClient:       tgClient,
	}, nil, handler)
	if err != nil {
		fail("post-test", err)
		return err
	}

	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go tg.Start(pollCtx)

	msgID, err := tg.SendSilent(ctx, tgCfg.ChannelID,
		"⚙️ lovegw doctor: проверка связи, сообщение сейчас будет удалено")
	if err != nil {
		fail("post-test: пост в канал", err)
		return err
	}
	postedID.Store(int64(msgID))
	ok("post-test: пост в канал", fmt.Sprintf("message_id=%d", msgID))

	started := time.Now()
	select {
	case fwdID := <-forwardCh:
		ok("post-test: автофорвард", fmt.Sprintf("пойман за %.1fс, thread=%d",
			time.Since(started).Seconds(), fwdID))
		if err := tg.DeleteMessage(ctx, tgCfg.DiscussionChatID, fwdID); err != nil {
			fail("post-test: удаление форварда", err)
		}
	case <-time.After(90 * time.Second):
		fail("post-test: автофорвард", errors.New(
			"не пойман за 90с — канал не связан с группой или нет прав"))
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := tg.DeleteMessage(ctx, tgCfg.ChannelID, msgID); err != nil {
		fail("post-test: удаление поста", err)
	} else {
		ok("post-test: удаление поста", "тестовое сообщение удалено")
	}
	return nil
}

// checkDeliveryConsent показывает, чью переписку обход вообще читает: без
// согласия человека (кнопка «читать и присылать сюда») сайт-аккаунт не трогают
// вовсе. Молчание согласием не считается, поэтому «ждут ответа» — рабочее
// состояние, а не поломка.
func checkDeliveryConsent(ctx context.Context, st *store.Store,
	ok func(name, detail string), warn func(name, detail string), fail func(name string, err error)) {
	owners, err := st.TalksOwners(ctx)
	if err != nil {
		fail("talks/чтение", err)
		return
	}
	groups := store.GroupByAccount(owners)
	var reading, waiting, refused int
	for _, group := range groups {
		switch {
		case store.ScanAllowed(group):
			reading++
		case group[0].Scan == store.ScanOff:
			refused++
		default:
			waiting++
		}
	}
	ok("talks/чтение", fmt.Sprintf("читаем переписку %d из %d сайт-аккаунтов (ждут ответа %d, отказались %d)",
		reading, len(groups), waiting, refused))
	checkDeliveryDuplicates(groups, ok, warn)
}

// checkDeliveryDuplicates ищет сайт-аккаунты, залогиненные больше чем в одном
// мессенджере: ЛС достанутся ровно одному из них (сайт помечает сообщение
// прочитанным на первом же дозаборе), а остальные будут молча пустовать. Кому
// носим — видно здесь же; поменять это человек может сам, кнопкой /delivery.
func checkDeliveryDuplicates(groups [][]store.TalksOwner,
	ok func(name, detail string), warn func(name, detail string)) {
	dupes := 0
	for _, group := range groups {
		if len(group) < 2 || !store.ScanAllowed(group) {
			continue // у нечитаемого аккаунта спор о получателе беспредметен
		}
		dupes++
		win, delivered := store.PickDelivery(group)
		where := "никуда (человек отказался)"
		if delivered {
			where = win.Messenger
		}
		warn("talks/доставка", fmt.Sprintf(
			"паспорт %s залогинен в %d мессенджерах, ЛС носим в %s (человек меняет это через /delivery)",
			group[0].PassportID, len(group), where))
	}
	if dupes == 0 {
		ok("talks/доставка", "у каждого сайт-аккаунта один мессенджер")
	}
}

// doctorSecrets — шифрование сессий и сервисные аккаунты сайта. Сетевых
// запросов не делает: показывает, что лежит в базах и подходит ли ключ.
func doctorSecrets(ctx context.Context, cfg *config.Config, st *store.Store,
	ok func(name, detail string), warn func(name, detail string), fail func(name string, err error)) {
	key, err := secretKey(cfg)
	if err != nil {
		fail("шифрование сессий", err)
		return
	}
	stats, err := st.SessionSecretStats(ctx)
	if err != nil {
		fail("шифрование сессий", err)
		return
	}
	switch {
	case !key.Enabled() && stats.Total == 0:
		warn("шифрование сессий", "ключ не задан (lovegw secrets keygen)")
	case !key.Enabled():
		warn("шифрование сессий", fmt.Sprintf(
			"ключ не задан, %d сессий лежат открыто (lovegw secrets keygen → encrypt)", stats.Plain))
	case stats.Plain > 0:
		warn("шифрование сессий", fmt.Sprintf(
			"ключ задан, но %d из %d сессий ещё открыты (lovegw secrets encrypt)", stats.Plain, stats.Total))
	default:
		ok("шифрование сессий", fmt.Sprintf("зашифровано: %d", stats.Encrypted))
	}

	accounts, err := openAccounts(ctx, cfg, "")
	if err != nil {
		fail("сервисные аккаунты", err)
		return
	}
	defer accounts.Close()
	list, err := accounts.List(ctx)
	if err != nil {
		fail("сервисные аккаунты", err)
		return
	}
	if len(list) == 0 {
		ok("сервисные аккаунты", "нет")
		return
	}
	for _, a := range list {
		detail := fmt.Sprintf("%s, вход %s", a.Title(), fmtTime(a.UpdatedAt))
		if a.Valid {
			ok("сервисный аккаунт", detail)
			continue
		}
		warn("сервисный аккаунт", detail+" — сессия невалидна (lovegw account login)")
	}
}
