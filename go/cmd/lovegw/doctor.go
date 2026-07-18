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
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
)

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	postTest := fs.Bool("post-test", false, "тестовый пост в канал с проверкой автофорварда (сообщение удаляется)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"config": true})); err != nil {
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

	st, err := store.Open(ctx, cfg.DBPath)
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

	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, nil)
	if _, err := client.RawNotes(ctx); err != nil {
		if errors.Is(err, love.ErrForbidden) {
			warn("сайт", "403: геоблок — нужен разрешённый (российский) IP")
		} else {
			fail("сайт", err)
		}
	} else {
		ok("сайт", cfg.Site.BaseURL)
	}

	mirrorOK := false
	if cfg.MirrorBot.Token == "" {
		warn("постер-бот", "токен не задан")
	} else if me, err := tgx.CheckToken(ctx, cfg.MirrorBot.Token); err != nil {
		fail("постер-бот", err)
	} else {
		ok("постер-бот", "@"+me.Username)
		mirrorOK = true
	}

	if cfg.DMBot.Token == "" {
		warn("ЛС-бот", "токен не задан")
	} else if me, err := tgx.CheckToken(ctx, cfg.DMBot.Token); err != nil {
		fail("ЛС-бот", err)
	} else {
		ok("ЛС-бот", "@"+me.Username)
	}

	queueEmpty := false
	if mirrorOK {
		switch n, err := tgx.ProbePendingUpdates(ctx, cfg.MirrorBot.Token); {
		case errors.Is(err, tgx.ErrPollingConflict):
			warn("очередь обновлений", "409: бота слушает другой процесс (старый poster.py ещё работает?)")
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
	return runPostTest(ctx, cfg, ok, fail)
}

// runPostTest: тихий пост в канал → ожидание автофорварда → удаление
// форварда и поста. Проверяет права бота и механику тредов end-to-end.
func runPostTest(ctx context.Context, cfg *config.Config,
	ok func(string, string), fail func(string, error)) error {

	var postedID atomic.Int64
	forwardCh := make(chan int, 1)
	handler := func(_ context.Context, u *models.Update) {
		msg := u.Message
		if msg == nil || !msg.IsAutomaticForward ||
			msg.ForwardOrigin == nil || msg.ForwardOrigin.MessageOriginChannel == nil {
			return
		}
		origin := msg.ForwardOrigin.MessageOriginChannel
		if origin.Chat.ID == cfg.MirrorBot.ChannelID && int64(origin.MessageID) == postedID.Load() {
			select {
			case forwardCh <- msg.ID:
			default:
			}
		}
	}

	tg, err := tgx.NewMirror(cfg.MirrorBot.Token, cfg.MirrorBot.ChannelID,
		cfg.MirrorBot.DiscussionChatID, cfg.Signature, cfg.Site.BaseURL, nil, handler)
	if err != nil {
		fail("post-test", err)
		return err
	}

	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go tg.Start(pollCtx)

	msgID, err := tg.SendSilent(ctx, cfg.MirrorBot.ChannelID,
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
		if err := tg.DeleteMessage(ctx, cfg.MirrorBot.DiscussionChatID, fwdID); err != nil {
			fail("post-test: удаление форварда", err)
		}
	case <-time.After(90 * time.Second):
		fail("post-test: автофорвард", errors.New(
			"не пойман за 90с — канал не связан с группой или нет прав"))
	case <-ctx.Done():
		return ctx.Err()
	}

	if err := tg.DeleteMessage(ctx, cfg.MirrorBot.ChannelID, msgID); err != nil {
		fail("post-test: удаление поста", err)
	} else {
		ok("post-test: удаление поста", "тестовое сообщение удалено")
	}
	return nil
}
