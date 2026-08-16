// lovegw — шлюз между заметками love.ngs.ru и Telegram.
//
// Подкоманды:
//
//	crawl  — отладочный обход сайта: разобрать ленту или комментарии и
//	         напечатать JSON; --save-html пишет сырой HTML для фикстур
//	grab   — разовая выгрузка одной заметки со всеми комментариями в
//	         древовидном виде, нормализованная в archive.db (типажи в отдельной
//	         таблице users, дерево ответов через parent_id); -json — снимок в файл
//	export — офлайн-выгрузка заметки из archive.db во вложенное JSON-дерево
//	backfill — массовая выгрузка диапазона заметок в archive.db (перечисление
//	         живых id по ленте, пул воркеров, резюм, -proxy для сайта)
//	personas — распознавание личностей поверх archive.db: склейка альт-анкет
//	         одного человека (flag → candidates → link → cluster → set)
//	import — импорт состояния старой Python-версии (M2)
//	run    — основной демон (M3+)
//	digest — еженедельный дайджест: draft → правка LLM-рубрик → publish
//	pulpit — амвон: черновик своей реплики под заметкой и состояние службы
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // тайзоны в бинарнике: нужны в scratch/distroless-образе

	"golang.org/x/sync/errgroup"

	"lovegw/internal/asr"
	"lovegw/internal/bridge"
	"lovegw/internal/config"
	"lovegw/internal/digest"
	"lovegw/internal/dmbot"
	"lovegw/internal/legacy"
	"lovegw/internal/love"
	"lovegw/internal/maxx"
	"lovegw/internal/mirror"
	"lovegw/internal/news"
	"lovegw/internal/store"
	"lovegw/internal/talks"
	"lovegw/internal/tgx"
)

// talksSite адаптирует *love.Client под talks.SiteTalks (имена методов Talks*).
type talksSite struct{ c *love.Client }

func (t talksSite) Dialogs(ctx context.Context, ck []*http.Cookie, limit int) ([]love.TalkDialog, error) {
	return t.c.TalksDialogs(ctx, ck, limit)
}
func (t talksSite) History(ctx context.Context, ck []*http.Cookie, passportID, afterMsgID string, limit int) ([]love.TalkMessage, error) {
	return t.c.TalksHistory(ctx, ck, passportID, afterMsgID, limit)
}
func (t talksSite) Send(ctx context.Context, ck []*http.Cookie, passportID, text string) (love.TalkMessage, error) {
	return t.c.TalksSend(ctx, ck, passportID, text)
}

// SiteIdentity (talks.SiteIdentifier) — паспорт владельца сессии: по нему
// поллер связывает вход одного человека в двух мессенджерах.
func (t talksSite) SiteIdentity(ctx context.Context, ck []*http.Cookie) (string, string, string, error) {
	return t.c.SiteIdentity(ctx, ck)
}

const (
	defaultConfigPath = "config.json"
	configFlagUsage   = "путь к конфигу"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "crawl":
		err = cmdCrawl(ctx, os.Args[2:])
	case "import":
		err = cmdImport(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "repost":
		err = cmdRepost(ctx, os.Args[2:])
	case "pull":
		err = cmdPull(ctx, os.Args[2:])
	case "grab":
		err = cmdGrab(ctx, os.Args[2:])
	case "export":
		err = cmdExport(ctx, os.Args[2:])
	case "backfill":
		err = cmdBackfill(ctx, os.Args[2:])
	case "personas":
		err = cmdPersonas(ctx, os.Args[2:])
	case "talks":
		err = cmdTalks(ctx, os.Args[2:])
	case "digest":
		err = cmdDigest(ctx, os.Args[2:])
	case "modwatch":
		err = cmdModwatch(ctx, os.Args[2:])
	case "account":
		err = cmdAccount(ctx, os.Args[2:])
	case "pulpit":
		err = cmdPulpit(ctx, os.Args[2:])
	case "secrets":
		err = cmdSecrets(ctx, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `использование:
  lovegw crawl [-config config.json] [-save-html dir] notes
  lovegw crawl [-config config.json] [-save-html dir] comments <note_id>
  lovegw import [-config config.json] [-notes notes.json] [-sessions sessions_export.json] [-subscribers subscribers.json]
  lovegw run    [-config config.json] [-seed]
  lovegw doctor [-config config.json] [-post-test]
  lovegw talks  [-config config.json] -db <копия.db> [-once] [-interval 20s] [-max-dialogs N] [-history-limit N] watch
  lovegw repost [-config config.json] <note_id> [<note_id> ...]
  lovegw digest [-config config.json] [-db lovegw.db] [-week N] [-out dir] [-llm] [-force] draft   # черновик недели (+LLM-рубрики через Claude API)
  lovegw digest [-config config.json] [-db lovegw.db] [-week N] [-in draft.txt] [-to telegram|max] [-force] preview
  lovegw digest [-config config.json] [-db lovegw.db] [-week N] [-in draft.txt] [-to telegram|max] [-force] publish
  lovegw pull   [-config config.json] [-db lovegw.db] <note_id> [<note_id> ...]   # завести заметку по прямому id, минуя ленту
  lovegw grab   [-config config.json] [-db archive.db] [-json] [-out dir] [-save-html dir] [-view tree|linear] [-max-pages N] <note_id>
  lovegw export [-db archive.db] [-out dir] <note_id>
  lovegw backfill [-config config.json] [-db archive.db] [-proxy] [-workers N] [-interval-ms MS] [-from ID] [-to ID] [-start-page N] [-refresh] [-limit N]
  lovegw personas [-db archive.db] [-out dir] flag [-patterns file]
  lovegw personas [-db archive.db] [-out dir] candidates [-limit N]
  lovegw personas [-db archive.db] [-out dir] [-in links.json] link
  lovegw personas [-db archive.db] [-out dir] [-min-score F] [-max-persona N] [-min-density F] cluster
  lovegw personas [-db archive.db] set <persona_id> <confirmed|rejected|pending>
  lovegw personas [-db archive.db] [-config config.json] [-proxy] [-workers N] [-limit N] avatars fetch
  lovegw personas [-db archive.db] [-max-dist D] [-generic-max N] avatars cluster
  lovegw personas [-db archive.db] [-min-chars N] [-dims N] [-genre all|notes] stylometry build
  lovegw personas [-db archive.db] [-min-cosine F] [-top-k N] [-max-pairs N] stylometry cluster
  lovegw personas [-db archive.db] [-lex-min-tokens N] [-lex-dims N] [-genre all|notes] lexis build
  lovegw personas [-db archive.db] [-top N] [-note id] [-in text.txt] [-lex-weight F] [-active-days N] [-min-author-notes N] [-genre notes] attribute [текст …]
  lovegw personas [-db archive.db] [-author p<id>|u<id>|user_id] [-lex-weight F] [-genre notes] attribute   # пакет: все заметки личности
  lovegw personas [-db archive.db] -notes id,id,… [-author p<id>] [-active-days N] [-min-author-notes N] [-genre notes] calibrate # leave-one-out калибровка отпечатка
  lovegw personas [-db archive.db] -suspect p<id> [-in text.txt] [-note id] [-null N] [-active-days N] [-min-author-notes N] [-genre notes] verify   # «это он? да/нет» с калиброванным порогом
  lovegw personas [-db archive.db] [-out dir] [-top N] portrait <p<id>|u<id>|user_id>
  lovegw personas [-db archive.db] diag <id> <id> …
  lovegw personas [-db archive.db] [-out dir] [-ens-top-k N] [-handoff-days D] [-ens-floor F] ensemble
  lovegw personas [-db archive.db] [-topics file] [-min-hits N] [-min-notes N] [-evidence N] facts scan
  lovegw personas [-db archive.db] [-out dir] [-min-hits N] [-min-notes N] facts candidates
  lovegw personas [-db archive.db] [-out dir] [-in facts_llm.json] facts import
  lovegw personas [-db archive.db] [-rel-min-replies N] relations score
  lovegw personas [-db archive.db] [-out dir] [-cand-replies N] [-band-min N] [-band-top N] [-exchanges N] relations candidates
  lovegw personas [-db archive.db] [-out dir] [-in relations_llm.json] relations import
  lovegw personas [-db archive.db] [-out dir] [-report-top N] [-active-days N] [-html] report
  lovegw personas [-db archive.db] [-config config.json] [-tg-user id] [-active-days N] [-limit N] gender
  lovegw personas [-db archive.db] [-out dir] [-genre notes|all] [-recent N] [-samples N] [-band N] [-seed N] [-top-words N] voice card <p<id>|u<id>|user_id>   # читаемая карта манеры письма
  lovegw personas [-db archive.db] [-config config.json] [-topic "…"] [-drafts N] [-rounds N] [-control p<id>] [-accept F] [-max-copy F] voice note <p<id>|u<id>|user_id>      # заметка в манере автора + замкнутый цикл через атрибуцию
  lovegw personas [-db archive.db] [-config config.json] -reply-to <comment_id> [-drafts N] [-rounds N] [-control p<id>] voice comment <p<id>|u<id>|user_id>                   # реплика в живую ветку
  lovegw personas [-db archive.db] [-config config.json] -note <note_id> [-drafts N] [-rounds N] [-control p<id>] voice comment <p<id>|u<id>|user_id>                          # комментарий первого уровня к заметке
  lovegw modwatch [-config config.json] [-db modwatch.db] [-feed-interval 90s] [-thread-interval 5m] [-window 48h] [-depth 6h] [-max-threads N] [-pages N] [-once] watch   # ловить моменты удалений/одобрений онлайн
  lovegw modwatch [-db modwatch.db] [-since 72h] [-until …] [-kind note_gone,comment_gone,…] [-presence-window 5m] [-controls N] [-seed N] [-min-hits N] [-top N] report   # кто систематически на площадке в момент действий
  lovegw modwatch [-db modwatch.db] [-since 72h] [-kind …] [-limit N] events
  lovegw modwatch [-db modwatch.db] status
  lovegw modwatch [-db modwatch.db] [-tolerance 3h] [-max-window 1h] bans   # запреты, выведенные из ритма жертв
  lovegw modwatch [-config config.json] [-db modwatch.db] [-interval 15m] [-once] guests watch   # копить визиты в свою анкету
  lovegw modwatch [-db modwatch.db] [-near "2026-08-12 19:38"] [-window 30m] guests log          # кто заходил вокруг момента
  lovegw modwatch [-config config.json] [-db modwatch.db] [-source lovegw.db] [-interval 10m] [-batch N] [-users id,id] activity watch   # копить присутствие людей на сайте
  lovegw modwatch [-config config.json] [-db modwatch.db] [-source lovegw.db] activity scan                                              # сплошной обход круга и сразу отчёт
  lovegw modwatch [-config config.json] [-db modwatch.db] [-silence 72h] [-fresh 24h] [-min-missed 20] [-top N] activity report          # кто замолчал: «ходит молча» = запрет, «не заходит» = ушёл
  lovegw modwatch [-db modwatch.db] [-user <id>] [-near "2026-08-12 19:38"] [-window 30m] activity log                                   # кто был на сайте вокруг момента, включая молчунов
  lovegw account [-config config.json] [-accounts accounts.db] -name reserve [-purpose "…"] login   # вход сервисным аккаунтом (логин/пароль со stdin)
  lovegw account [-accounts accounts.db] list                                                    # какие аккаунты есть и живы ли их сессии
  lovegw account [-accounts accounts.db] [-name reserve] check                                   # проверить сессию на сайте (и заметить смену ника)
  lovegw account [-accounts accounts.db] -name reserve forget
  lovegw account [-accounts accounts.db] -name reserve cookie                                    # заголовок Cookie локальным скриптам; только в пайп
  lovegw account [-accounts accounts.db] -name reserve -note <id> [-reply <comment_id>] [-no-prefix] [-yes] say [текст …]   # комментарий на сайт от сервисного аккаунта
  lovegw pulpit [-config config.json] [-db lovegw.db] draft <note_id> [<note_id> ...]            # черновик реплики амвона, на сайт ничего не уходит
  lovegw pulpit [-config config.json] [-db lovegw.db] status                                     # включён ли амвон, что писал последним
  lovegw secrets keygen                                                                          # новый ключ шифрования сессий
  lovegw secrets [-config config.json] [-accounts accounts.db] status                            # что лежит открыто, что зашифровано
  lovegw secrets [-config config.json] [-accounts accounts.db] [-old-key-env NAME] encrypt       # зашифровать/перешить под текущий ключ`)
}

// cmdRun — основной демон: зеркалирование ленты и комментариев в Telegram.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	seed := fs.Bool("seed", false, "первый обход ленты: запомнить заметки без постинга")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	tgCfg, maxCfg := cfg.Messengers.Telegram, cfg.Messengers.Max
	if !tgCfg.Enabled && !maxCfg.Enabled {
		return fmt.Errorf("run: не включён ни один мессенджер (messengers.telegram/max или mirror_bot)")
	}
	if tgCfg.Enabled && (tgCfg.Token == "" || tgCfg.ChannelID == 0 || tgCfg.DiscussionChatID == 0) {
		return fmt.Errorf("run: telegram включён, но не заданы token / channel_id / discussion_chat_id")
	}
	if maxCfg.Enabled && (maxCfg.Token == "" || maxCfg.ChannelID == 0) {
		return fmt.Errorf("run: max включён, но не заданы token / channel_id")
	}
	log := newLogger(cfg.LogLevel)
	for _, w := range cfg.Warnings {
		log.Warn("конфиг: " + w)
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	if !*seed {
		if ids, err := st.KnownNoteIDs(ctx); err == nil && len(ids) == 0 {
			log.Warn("БД пуста и запуск без -seed: текущие заметки с ленты будут опубликованы; " +
				"для перехода с Python сначала выполните import, затем запуск с -seed")
		}
	}
	return runDaemon(ctx, cfg, st, *seed, log)
}

// daemon — состояние сборки демона. Сборка идёт в три фазы: build (создать
// объекты) → wire (все Set*-инжекции) → start (поднять поллеры и службы,
// метод run). ЛЮБАЯ Set*-инжекция после старта поллера — data race по модели
// памяти Go (поля ботов не под мьютексом), поэтому setup-методы только копят
// starts, а g.Go зовётся одним циклом в конце.
type daemon struct {
	cfg    *config.Config
	st     *store.Store
	client *love.Client
	log    *slog.Logger
	seed   bool

	sinks     []mirror.Sink
	alerters  []func(ctx context.Context, text string)
	subNotify map[string]mirror.SubNotify
	starts    []func(context.Context) error

	asrSvc     *asr.Service
	dm         *dmbot.Bot // Telegram: бот команд (РюмкинЪ)
	tgTalks    *dmbot.Bot // Telegram: бот переписки (talks_token), иначе = dm
	tg         *tgx.Mirror
	mx         *maxx.Mirror // MAX: бот зеркала — канал, чат обсуждения и ЛС-команды
	maxTalks   *maxx.Mirror // MAX: бот переписки (talks_token), иначе = mx
	maxDM      *dmbot.Logic // ЛС-команды бота зеркала MAX
	maxTalksDM *dmbot.Logic
	onNewNote  func(context.Context, love.Note)
}

// runDaemon собирает включённые мессенджеры (гейт messengers) и крутит все
// компоненты под общим errgroup. Telegram поднимает полный контур (зеркало,
// мост, ЛС-бот РюмкинЪ); MAX — зеркало + мост + ЛС-диалоги: при заданном
// dm_token личка живёт в отдельном боте (по аналогии с telegram), иначе всё
// через одного бота (long polling GetUpdates в каждом).
func runDaemon(ctx context.Context, cfg *config.Config, st *store.Store, seed bool, log *slog.Logger) error {
	d := &daemon{
		cfg:       cfg,
		st:        st,
		client:    newSiteClient(cfg, log),
		log:       log,
		seed:      seed,
		subNotify: map[string]mirror.SubNotify{},
	}
	if err := d.setupASR(); err != nil {
		return err
	}
	if err := d.setupTelegram(ctx); err != nil {
		return err
	}
	if err := d.setupMax(); err != nil {
		return err
	}
	// Алертеры собраны — подключаем к ним ASR: о сбое ключа или исчерпанном
	// балансе провайдера админ узнаёт один раз (в треде при этом тишина).
	if d.asrSvc != nil {
		d.asrSvc.SetAlert(fanOutAlerts(d.alerters))
	}
	d.wirePollAlerts()
	d.setupTalks()
	d.setupNews()
	d.publishCommands(ctx)
	if err := d.setupDigest(); err != nil {
		return err
	}
	if err := d.setupPulpit(); err != nil {
		return err
	}
	return d.run(ctx)
}

// setupASR — распознавание голосовых в тредах. Выключен — гейт как раньше.
func (d *daemon) setupASR() error {
	cfg, log := d.cfg, d.log
	if !cfg.ASR.Enabled {
		return nil
	}
	var err error
	if d.asrSvc, err = newASR(cfg, d.st, log); err != nil {
		return err
	}
	d.starts = append(d.starts, d.asrSvc.Run)
	log.Info("распознавание голосовых включено", "provider", cfg.ASR.Provider,
		"max_duration_sec", cfg.ASR.MaxDurationSec,
		"daily_limit_sec", cfg.ASR.UserDailyLimitSec, "concurrency", cfg.ASR.Concurrency)
	if !cfg.Messengers.Telegram.Enabled {
		log.Warn("asr включён, но telegram выключен — голосовые слушать некому")
	}
	return nil
}

// setupTelegram — зеркало, мост и ЛС-боты Telegram-стороны.
func (d *daemon) setupTelegram(ctx context.Context) error {
	cfg, st, client, log := d.cfg, d.st, d.client, d.log
	tgCfg := cfg.Messengers.Telegram
	if !tgCfg.Enabled {
		return nil
	}
	tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
	if err != nil {
		return err
	}

	// ЛС-бот РюмкинЪ (опционален): без него мост не сможет уведомлять
	// пользователей о протухшей сессии, но зеркалирование работает.
	var notify bridge.Notify
	if tgCfg.DMToken != "" {
		if d.dm, err = dmbot.New(tgCfg.DMToken, st, client, tgClient, log); err != nil {
			return err
		}
		notify = d.dm.Notify
	}
	dm := d.dm

	handler := bridge.New(st, client, notify, tgCfg.ChannelID, tgCfg.DiscussionChatID, log)
	tg, err := tgx.NewMirror(tgx.Params{
		Token:            tgCfg.Token,
		ChannelID:        tgCfg.ChannelID,
		DiscussionChatID: tgCfg.DiscussionChatID,
		Signature:        tgCfg.Signature,
		BaseURL:          cfg.Site.BaseURL,
		HTTPClient:       tgClient,
	}, log, handler.Handle)
	if err != nil {
		return err
	}
	d.tg = tg
	d.sinks = append(d.sinks, tg)

	// Бот переписки (опционален): личная переписка сайта уезжает к нему
	// целиком, у бота команд остаётся только маршрутизация старых реплаев.
	d.tgTalks = dm
	if tgCfg.TalksToken != "" {
		if d.tgTalks, err = dmbot.NewTalks(tgCfg.TalksToken, st, tgClient, log); err != nil {
			return err
		}
		talksBot := d.tgTalks
		d.starts = append(d.starts, func(ctx context.Context) error {
			talksBot.Start(ctx)
			return nil
		})
	}

	if tgCfg.AdminUserID != 0 {
		// Алерты шлём в ЛС — приоритет боту переписки, затем боту команд;
		// без ЛС-ботов остаётся постер (в личку он писать сможет не всегда).
		adminID := tgCfg.AdminUserID
		notifier := d.tgTalks
		d.alerters = append(d.alerters, func(ctx context.Context, text string) {
			if notifier != nil {
				notifier.Notify(ctx, adminID, "⚠️ lovegw: "+text)
				return
			}
			if err := tg.SendText(ctx, adminID, "⚠️ lovegw: "+text); err != nil {
				log.Warn("не удалось отправить алерт админу", "sink", "telegram", "err", err)
			}
		})
	}

	// Уведомления подписчиков шлём через РюмкинЪ (его пользователь точно
	// запускал, раз подписался) — постер-бот не смог бы написать в ЛС.
	if dm != nil {
		d.subNotify[store.MessengerTelegram] = func(ctx context.Context, userID int64, ev mirror.SubEvent) {
			link := subLinkTG(tgCfg, ev, cfg.Site.BaseURL, log)
			dm.NotifyHTML(ctx, userID,
				tgx.ComposeSubNotice(ev.Reason(), ev.Note, ev.Comment, link),
				dmbot.UnsubKeyboard(ev.Sub.ID))
		}
	}

	// Юзернейм РюмкинЪ нужен постеру: под заметкой он вешает deep-link в
	// этот ЛС. Не снялся — просто не будет кнопки.
	if dm != nil {
		if name, err := dm.Username(ctx); err != nil {
			log.Warn("юзернейм ЛС-бота не снят, кнопки подписки под заметками не будет", "err", err)
		} else {
			tg.SetSubscribeBot(name)
		}
	}

	// Хук распознавания ставим до старта поллинга — гонки нет.
	if d.asrSvc != nil {
		tg.SetVoiceHandler(tgx.NewVoiceHandler(tg, d.asrSvc, tgCfg.DiscussionChatID, log).Handle)
	}

	d.starts = append(d.starts, func(ctx context.Context) error {
		tg.Start(ctx) // блокируется до отмены контекста
		return nil
	})
	if dm != nil {
		d.starts = append(d.starts, func(ctx context.Context) error {
			dm.Start(ctx)
			return nil
		})
	}
	return nil
}

// setupMax — зеркало, мост и ЛС-диалоги MAX-стороны.
func (d *daemon) setupMax() error {
	cfg, st, client, log := d.cfg, d.st, d.client, d.log
	maxCfg := cfg.Messengers.Max
	if !maxCfg.Enabled {
		return nil
	}
	mx, err := maxx.NewMirror(maxx.Params{
		Token:            maxCfg.Token,
		ChannelID:        maxCfg.ChannelID,
		DiscussionChatID: maxCfg.DiscussionChatID,
		Signature:        maxCfg.Signature,
		BaseURL:          cfg.Site.BaseURL,
		HTTPClient:       maxx.MintsifraClient(),
	}, log)
	if err != nil {
		return err
	}
	d.mx = mx
	d.sinks = append(d.sinks, mx)
	if maxCfg.DiscussionChatID == 0 {
		log.Warn("MAX без discussion_chat_id: посты уйдут в канал, комментарии останутся в очереди")
	}

	// Бот переписки MAX (опционален): к нему уезжает только личная
	// переписка сайта. Канал, чат обсуждения и все ЛС-команды остаются у
	// бота зеркала — как было до появления переписки.
	d.maxTalks = mx
	if maxCfg.TalksToken != "" {
		if maxCfg.TalksToken == maxCfg.Token {
			log.Warn("MAX: talks_token совпадает с token — два поллера на один бот, апдейты будут теряться")
		}
		d.maxTalks, err = maxx.NewMirror(maxx.Params{
			Token:      maxCfg.TalksToken,
			BaseURL:    cfg.Site.BaseURL,
			HTTPClient: maxx.MintsifraClient(),
		}, log)
		if err != nil {
			return err
		}
	}

	// Алерты — через бота переписки (без него это тот же бот зеркала).
	if maxCfg.AdminUserID != 0 {
		adminID, pm := maxCfg.AdminUserID, d.maxTalks
		d.alerters = append(d.alerters, func(ctx context.Context, text string) {
			if err := pm.SendText(ctx, adminID, "⚠️ lovegw: "+text); err != nil {
				log.Warn("не удалось отправить алерт админу", "sink", "max", "err", err)
			}
		})
	}
	// Подписки — функция бота зеркала: он же принимает /subscribe.
	d.subNotify[store.MessengerMax] = func(ctx context.Context, userID int64, ev mirror.SubEvent) {
		link := mx.SubNoteLink(ev.Note, ev.PostMsgID)
		if ev.IsComment() {
			link = mx.SubCommentLink(ev.Note, ev.Comment.ID, ev.MsgID)
		}
		text := maxx.ComposeSubNotice(ev.Reason(), ev.Note, ev.Comment, link)
		if err := mx.NotifyHTML(ctx, userID, text, dmbot.UnsubKeyboard(ev.Sub.ID)); err != nil {
			log.Warn("уведомление подписчика не удалось", "sink", "max", "user", userID, "err", err)
		}
	}

	// Бот зеркала ведёт и мост «ответ в чате → комментарий на сайте», и
	// ЛС-команды; бот переписки — только диалоги talks.
	maxCore := bridge.NewCore(st, client, mx.Send, store.MessengerMax, log)
	d.maxDM = dmbot.NewLogic(st, client, mx, store.MessengerMax, log)
	maxDM := d.maxDM
	d.starts = append(d.starts, func(ctx context.Context) error {
		mx.Start(ctx, mx.Dispatch(maxCore, maxDM))
		return nil
	})
	if d.maxTalks != mx {
		pm := d.maxTalks
		d.maxTalksDM = dmbot.NewTalksLogic(st, pm, store.MessengerMax, log)
		talksLogic := d.maxTalksDM
		d.starts = append(d.starts, func(ctx context.Context) error {
			pm.Start(ctx, pm.Dispatch(nil, talksLogic))
			return nil
		})
	}
	return nil
}

// wirePollAlerts — супервизия поллеров: умерший getUpdates (409 от второго
// процесса, протухший токен) не должен оставлять демон молча полуживым.
// Поллеры при этом продолжают ретраить — алерт информационный. Фан-аут шлёт
// во все мессенджеры, поэтому смерть одного бота доносит живой сосед.
func (d *daemon) wirePollAlerts() {
	if len(d.alerters) == 0 {
		return
	}
	send := fanOutAlerts(d.alerters)
	if d.tg != nil {
		d.tg.SetPollAlert("Telegram (постер)", send)
	}
	if d.dm != nil {
		d.dm.SetPollAlert("Telegram (РюмкинЪ)", send)
	}
	if d.tgTalks != nil && d.tgTalks != d.dm {
		d.tgTalks.SetPollAlert("Telegram (переписка)", send)
	}
	if d.mx != nil {
		d.mx.SetPollAlert("MAX (зеркало)", send)
	}
	if d.maxTalks != nil && d.maxTalks != d.mx {
		d.maxTalks.SetPollAlert("MAX (переписка)", send)
	}
}

// setupTalks — личная переписка сайта (talks): один поллер под общим клиентом
// сайта фанит входящие ЛС в личку включённых мессенджеров, ответы
// реплаем/командой уходят на сайт. Роутер инжектится в ЛС-стороны обоих
// мессенджеров (nil-safe: выключенный talks не меняет поведения ботов).
func (d *daemon) setupTalks() {
	cfg, st, client, log := d.cfg, d.st, d.client, d.log
	tgCfg, maxCfg := cfg.Messengers.Telegram, cfg.Messengers.Max
	tgTalks, maxTalks := d.tgTalks, d.maxTalks
	maxDM, maxTalksDM := d.maxDM, d.maxTalksDM
	if cfg.Talks.Enabled {
		// Транспорт — бот переписки мессенджера (без своего токена это бот
		// команд в telegram и бот зеркала в MAX, как было раньше).
		var transports []talks.PMTransport
		adminIDs := map[string]int64{}
		if tgTalks != nil {
			transports = append(transports, tgTalks)
			adminIDs[store.MessengerTelegram] = tgCfg.AdminUserID
		}
		if maxTalks != nil {
			transports = append(transports, maxTalks)
			adminIDs[store.MessengerMax] = maxCfg.AdminUserID
		}
		if len(transports) == 0 {
			log.Warn("talks включён, но нет мессенджера с ЛС-доставкой — пропускаю")
		} else {
			// Вопрос «куда носить ЛС» задаёт диалоговое ядро того мессенджера, где
			// человек вошёл на сайт: у поллера кнопок нет. Спрашивает бот
			// переписки — тот, что эти ЛС и доставляет.
			askDelivery := func(ctx context.Context, messenger string, userID int64, current store.TalksOwner) {
				switch {
				case messenger == store.MessengerTelegram && tgTalks != nil:
					tgTalks.AskDelivery(ctx, userID, current)
				case messenger == store.MessengerMax && maxTalksDM != nil:
					maxTalksDM.AskDelivery(ctx, userID, current)
				case messenger == store.MessengerMax && maxDM != nil:
					maxDM.AskDelivery(ctx, userID, current) // переписку ведёт бот зеркала
				default:
					log.Warn("некому спросить про доставку ЛС", "messenger", messenger, "user", userID)
				}
			}
			watcher := talks.New(st, talksSite{client}, transports, talks.Config{
				BaseURL:      cfg.Site.BaseURL,
				AdminOnly:    cfg.Talks.AdminOnly,
				AdminIDs:     adminIDs,
				Interval:     time.Duration(cfg.Talks.PollIntervalS) * time.Second,
				IdleInterval: time.Duration(cfg.Talks.IdlePollIntervalS) * time.Second,
				MaxDialogs:   cfg.Talks.MaxDialogsPerTick,
				AllowSend:    cfg.Talks.AllowSend,
				StoreText:    cfg.Talks.StoreText,
				MaxReqPerMin: cfg.Talks.MaxRequestsPerMin,
				ExcludeUsers: cfg.Talks.ExcludeUsers,
				AlertSend:    fanOutAlerts(d.alerters),
				AskDelivery:  askDelivery,
			}, log)
			if tgTalks != nil {
				tgTalks.SetTalkRouter(watcher)
			}
			// Боту команд оставляем только маршрутизацию реплаев: ЛС,
			// доставленные им раньше, по-прежнему ждут ответа у него.
			if d.dm != nil && d.dm != tgTalks {
				d.dm.SetReplyRouter(watcher)
			}
			if maxTalks != nil {
				maxTalks.SetTalkRouter(watcher)
				if maxTalksDM != nil {
					maxTalksDM.SetTalkRouter(watcher)
				}
			}
			if d.mx != nil && d.mx != maxTalks {
				d.mx.SetTalkRouter(watcher) // старые реплаи в MAX; команды /talks у бота переписки
			}
			if maxTalks == d.mx && maxDM != nil {
				maxDM.SetTalkRouter(watcher) // один бот на всё — как раньше
			}
			d.starts = append(d.starts, watcher.Run)
			log.Info("talks включён", "admin_only", cfg.Talks.AdminOnly,
				"allow_send", cfg.Talks.AllowSend, "store_text", cfg.Talks.StoreText,
				"мессенджеров", len(transports))
		}
		// Retention: периодически чистим старые сообщения talks (приватность),
		// независимо от наличия транспортов.
		if cfg.Talks.RetentionDays > 0 {
			days := cfg.Talks.RetentionDays
			d.starts = append(d.starts, func(ctx context.Context) error {
				return talks.PurgeLoop(ctx, st, days, log)
			})
		}
	}
}

// setupNews — новости проекта: админ пишет /news в ЛС командному боту, и текст
// уходит постом в каналы мимо сайта (заметки на love.ngs.ru не появляется).
// Приёмники — те же, что у зеркала.
func (d *daemon) setupNews() {
	cfg, log := d.cfg, d.log
	var newsPubs []news.Publisher
	for _, s := range d.sinks {
		if p, ok := s.(news.Publisher); ok {
			newsPubs = append(newsPubs, p)
		}
	}
	if len(newsPubs) == 0 {
		return
	}
	newsSvc := news.New(d.st, newsPubs, log)
	bots := 0
	if d.dm != nil && cfg.Messengers.Telegram.AdminUserID != 0 {
		d.dm.SetNews(newsSvc, cfg.Messengers.Telegram.AdminUserID)
		bots++
	}
	if d.maxDM != nil && cfg.Messengers.Max.AdminUserID != 0 {
		d.maxDM.SetNews(newsSvc, cfg.Messengers.Max.AdminUserID)
		bots++
	}
	if bots > 0 {
		log.Info("новости проекта включены", "каналов", len(newsPubs), "ботов", bots)
	}
}

// publishCommands — меню команд в клиентах мессенджеров. Зовётся после
// подключения talks-роутера: от него зависит, попадут ли в список /talks и
// /talk. Сбой не фатален — сами команды работают и без меню, поэтому ошибку
// логирует транспорт.
func (d *daemon) publishCommands(ctx context.Context) {
	if d.dm != nil {
		d.dm.PublishCommands(ctx)
	}
	if d.tgTalks != nil && d.tgTalks != d.dm {
		d.tgTalks.PublishCommands(ctx)
	}
	if d.maxDM != nil {
		d.maxDM.PublishCommands(ctx)
	}
	if d.maxTalksDM != nil {
		d.maxTalksDM.PublishCommands(ctx)
	}
}

// setupDigest — планировщик дайджеста: в слот выпуска готовит черновик (LLM
// заполняет рубрики, если настроен) и либо публикует сам (auto_publish), либо
// зовёт админа — премодерация через lovegw digest publish.
func (d *daemon) setupDigest() error {
	cfg, log := d.cfg, d.log
	if !cfg.Digest.Enabled {
		return nil
	}
	loc, weekday, hour, err := digestSlotParams(cfg)
	if err != nil {
		return err
	}
	dcfg := digest.ScheduleConfig{
		Loc:         loc,
		Weekday:     weekday,
		Hour:        hour,
		OutDir:      digestOutDir(cfg),
		SiteBase:    cfg.Site.BaseURL,
		Notify:      fanOutAlerts(d.alerters),
		AutoPublish: cfg.Digest.AutoPublish,
	}
	// Автопубликация идёт в те же приёмники, что и зеркало.
	for _, s := range d.sinks {
		if p, ok := s.(digest.Publisher); ok {
			dcfg.Publishers = append(dcfg.Publishers, p)
		}
	}
	llmModel := ""
	if cfg.LLM.APIKey != "" {
		lc, err := llmClient(cfg)
		if err != nil {
			return err
		}
		dcfg.LLM = lc
		llmModel = lc.Model()
	}
	st := d.st
	d.starts = append(d.starts, func(ctx context.Context) error {
		return digest.RunSchedule(ctx, st, dcfg, log)
	})
	log.Info("дайджест включён", "слот",
		fmt.Sprintf("%s %02d:00 %s", weekday, hour, loc), "out", dcfg.OutDir,
		"auto_publish", cfg.Digest.AutoPublish, "llm", llmModel)
	return nil
}

// setupPulpit — амвон: своя реплика под каждой новой заметкой сайта. Служба
// собирается ДО зеркала: ей нужен колбэк OnNewNote как страховка на случай,
// если её собственный (более частый) обход ленты моргнёт. Ошибку Run наружу
// не отдаём — амвон не критичен для зеркалирования.
func (d *daemon) setupPulpit() error {
	cfg, log := d.cfg, d.log
	if !cfg.Pulpit.Enabled {
		return nil
	}
	gen, err := llmClientFor(cfg, cfg.Pulpit.Model, cfg.Pulpit.Effort,
		time.Duration(cfg.Pulpit.GenerateTimeoutS)*time.Second)
	if err != nil {
		return fmt.Errorf("амвон: %w", err)
	}
	svc, err := newPulpit(cfg, d.st, gen, fanOutAlerts(d.alerters), log)
	if err != nil {
		return err
	}
	d.onNewNote = svc.OnNewNote
	d.starts = append(d.starts, func(ctx context.Context) error {
		if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("амвон остановлен", "err", err)
		}
		return nil
	})
	// Ручка /pulpit — админам тех мессенджеров, где есть ЛС-бот команд.
	// Как и все Set*, ставится до стартов поллеров (фаза wire).
	if d.dm != nil && cfg.Messengers.Telegram.AdminUserID != 0 {
		d.dm.SetPulpit(svc, cfg.Messengers.Telegram.AdminUserID)
	}
	if d.maxDM != nil && cfg.Messengers.Max.AdminUserID != 0 {
		d.maxDM.SetPulpit(svc, cfg.Messengers.Max.AdminUserID)
	}
	return nil
}

// run — фаза start: сборка и все Set*-инжекции позади, поллеры поднимаются
// без гонок.
func (d *daemon) run(ctx context.Context) error {
	cfg, log := d.cfg, d.log
	mir := mirror.New(d.st, d.client, d.sinks, mirror.Config{
		NotesLimit:   cfg.NotesLimit,
		FeedInterval: time.Duration(cfg.FeedIntervalS) * time.Second,
		SeedFirst:    d.seed,
		AlertSend:    fanOutAlerts(d.alerters),
		SubNotify:    d.subNotify,
		OnNewNote:    d.onNewNote,
	}, log)

	log.Info("lovegw запущен", "seed", d.seed, "db", cfg.DBPath,
		"telegram", cfg.Messengers.Telegram.Enabled, "max", cfg.Messengers.Max.Enabled,
		"dm_bot", d.dm != nil,
		"tg_talks_bot", d.tgTalks != nil && d.tgTalks != d.dm,
		"max_talks_bot", d.maxTalks != nil && d.maxTalks != d.mx, "log_level", cfg.LogLevel)
	log.Debug("debug-логирование включено") // видна только на уровне debug

	g, gctx := errgroup.WithContext(ctx)
	d.starts = append(d.starts, mir.Run)
	for _, run := range d.starts {
		run := run
		g.Go(func() error { return run(gctx) })
	}
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("lovegw остановлен")
	return nil
}

// newASR собирает сервис распознавания голосовых. Провайдер российский —
// соединение прямое, мимо telegram_proxy (в отличие от Bot API и Claude).
func newASR(cfg *config.Config, st *store.Store, log *slog.Logger) (*asr.Service, error) {
	if cfg.ASR.Provider != asrProviderNexara {
		return nil, fmt.Errorf("asr: неизвестный провайдер %q (поддерживается %s)",
			cfg.ASR.Provider, asrProviderNexara)
	}
	if cfg.ASR.APIKey == "" {
		return nil, errors.New("asr включён, но не задан api_key (секция asr конфига или env LOVEGW_ASR_API_KEY)")
	}
	tr := asr.NewNexara(asr.NexaraConfig{
		APIKey:  cfg.ASR.APIKey,
		Timeout: time.Duration(cfg.ASR.TimeoutSec) * time.Second,
	}, cfg.ASR.BaseURL, log)
	return asr.New(tr, &asr.FFmpeg{Path: cfg.ASR.FFmpegPath}, st, asr.Config{
		MaxDurationSec:    cfg.ASR.MaxDurationSec,
		UserDailyLimitSec: cfg.ASR.UserDailyLimitSec,
		Concurrency:       cfg.ASR.Concurrency,
	}, log), nil
}

// asrProviderNexara — единственный поддерживаемый провайдер распознавания.
const asrProviderNexara = "nexara"

// fanOutAlerts объединяет алертеры включённых мессенджеров (nil, если ни у
// одного не задан admin id): алерт о дрейфе/блокировке уходит во все.
func fanOutAlerts(alerters []func(ctx context.Context, text string)) func(ctx context.Context, text string) {
	if len(alerters) == 0 {
		return nil
	}
	return func(ctx context.Context, text string) {
		for _, send := range alerters {
			send(ctx, text)
		}
	}
}

// subLinkTG — ссылка в уведомлении подписчику: на сам комментарий в треде или,
// у повода «новая заметка автора», на пост канала. Не разобрали id (тред ещё не
// пойман, пост не ушёл) — ведём на сайт, уведомление без ссылки бесполезно.
func subLinkTG(tgCfg config.Messenger, ev mirror.SubEvent, siteBase string, log *slog.Logger) string {
	if ev.IsComment() {
		root, err1 := tgx.ParseMessageID(ev.ThreadID)
		msgID, err2 := tgx.ParseMessageID(ev.MsgID)
		if err1 == nil && err2 == nil {
			return tgx.DeepLink(tgCfg.DiscussionChatID, int64(msgID), int64(root))
		}
		log.Warn("уведомление подписчика: не телеграмные id",
			"thread", ev.ThreadID, "msg", ev.MsgID)
		return fmt.Sprintf("%s/notes/%s/#anchor-%d", siteBase, ev.Note.ID, ev.Comment.ID)
	}
	if postID, err := tgx.ParseMessageID(ev.PostMsgID); err == nil {
		return tgx.ChannelDeepLink(tgCfg.ChannelID, int64(postID))
	}
	return fmt.Sprintf("%s/notes/%s/", siteBase, ev.Note.ID)
}

// cmdImport переносит состояние старой Python-версии в SQLite.
// Импорт идемпотентен: повторный запуск ничего не дублирует.
func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	notesPath := fs.String("notes", "", "notes.json старой версии")
	sessionsPath := fs.String("sessions", "", "JSON с экспортированными куки пользователей старой версии")
	subscribersPath := fs.String("subscribers", "", "subscribers.json старой версии")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if *notesPath == "" && *sessionsPath == "" && *subscribersPath == "" {
		return fmt.Errorf("import: не указан ни один файл для импорта")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	now := time.Now()
	var total legacy.Stats
	importOne := func(path string, do func(*os.File) (legacy.Stats, error)) error {
		if path == "" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		stats, err := do(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		total.Notes += stats.Notes
		total.Comments += stats.Comments
		total.Sessions += stats.Sessions
		total.Subscriptions += stats.Subscriptions
		total.Warnings = append(total.Warnings, stats.Warnings...)
		return nil
	}

	if err := importOne(*notesPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportNotes(ctx, st, f, now)
	}); err != nil {
		return err
	}
	if err := importOne(*sessionsPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportSessions(ctx, st, f, now)
	}); err != nil {
		return err
	}
	if err := importOne(*subscribersPath, func(f *os.File) (legacy.Stats, error) {
		return legacy.ImportSubscribers(ctx, st, f)
	}); err != nil {
		return err
	}

	fmt.Printf("импортировано: заметок %d, комментариев %d, сессий %d, подписок %d\n",
		total.Notes, total.Comments, total.Sessions, total.Subscriptions)
	for _, w := range total.Warnings {
		fmt.Fprintln(os.Stderr, "предупреждение:", w)
	}
	return nil
}

// reorderArgs переносит флаги перед позиционными аргументами: стандартный
// flag прекращает разбор на первом позиционном, а команды вида
// `crawl notes -save-html dir` естественно писать флагами после.
// valueFlags — имена флагов, ожидающих значение следующим токеном.
func reorderArgs(args []string, fs *flag.FlagSet) []string {
	valueFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		// Булев флаг значения не берёт: «-html cluster» — это флаг и действие,
		// а не флаг со значением.
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			return
		}
		valueFlags[f.Name] = true
	})
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if valueFlags[name] && !strings.Contains(a, "=") && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// splitSubcommand выделяет подкоманду из аргументов. Она может стоять и после
// флагов (`secrets -config … status`), поэтому ищем по имени, а не по позиции.
func splitSubcommand(args []string, names map[string]bool) (sub string, rest []string) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if sub == "" && names[a] {
			sub = a
			continue
		}
		rest = append(rest, a)
	}
	return sub, rest
}

func cmdCrawl(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	saveHTML := fs.String("save-html", "", "каталог для сохранения сырого HTML (фикстуры)")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("crawl: не указан объект (notes|comments)")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	client := newSiteClient(cfg, log)

	switch fs.Arg(0) {
	case "notes":
		return crawlNotes(ctx, client, *saveHTML)
	case "comments":
		if fs.NArg() < 2 {
			return fmt.Errorf("crawl comments: не указан id заметки")
		}
		return crawlComments(ctx, client, cfg.Site.BaseURL, fs.Arg(1), *saveHTML)
	default:
		return fmt.Errorf("crawl: неизвестный объект %q", fs.Arg(0))
	}
}

func crawlNotes(ctx context.Context, client *love.Client, saveDir string) error {
	raw, err := client.RawNotes(ctx)
	if err != nil {
		if saveDir != "" {
			return fmt.Errorf("%w — страница не получена, HTML НЕ сохранён", err)
		}
		return err
	}
	if err := saveRaw(saveDir, "notes_feed.html", raw); err != nil {
		return err
	}
	notes, err := love.ParseNotes(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return printJSON(notes)
}

func crawlComments(ctx context.Context, client *love.Client, baseURL, noteID, saveDir string) error {
	raw, err := client.RawComments(ctx, noteID)
	if err != nil {
		if saveDir != "" {
			return fmt.Errorf("%w — страница не получена, HTML НЕ сохранён", err)
		}
		return err
	}
	if err := saveRaw(saveDir, "comments_"+noteID+".html", raw); err != nil {
		return err
	}
	comments, err := love.ParseComments(bytes.NewReader(raw), baseURL)
	if err != nil {
		return err
	}
	return printJSON(comments)
}

func saveRaw(dir, name string, raw []byte) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "HTML сохранён:", path)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
