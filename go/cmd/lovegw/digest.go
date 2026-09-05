package main

// cmdDigest — еженедельный дайджест: draft строит черновик и материалы для
// LLM-редактуры, preview рендерит выпуск per-messenger в stdout, publish
// постит его в каналы. Публикация идемпотентна (message_targets), поэтому
// команду безопасно запускать при работающем демоне: поллинг здесь не
// поднимается, боты только шлют.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/chantext"
	"lovegw/internal/config"
	"lovegw/internal/digest"
	"lovegw/internal/maxx"
	"lovegw/internal/platdigest"
	"lovegw/internal/platform"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
)

type digestOpts struct {
	week  int
	out   string
	in    string
	to    string
	force bool
	llm   bool
}

func cmdDigest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", "", "путь к БД (перебивает db_path из конфига)")
	var o digestOpts
	fs.IntVar(&o.week, "week", 0, "смещение слота в неделях (0 — последний прошедший, -1 — предыдущий)")
	fs.StringVar(&o.out, "out", "", "каталог черновиков (по умолчанию digest рядом с БД)")
	fs.StringVar(&o.in, "in", "", "файл черновика для preview/publish (по умолчанию из -out по неделе)")
	fs.StringVar(&o.to, "to", "", "только один мессенджер: telegram или max")
	fs.BoolVar(&o.force, "force", false, "draft: перезаписать черновик; preview/publish: выбросить секции с незаполненными LLM-плейсхолдерами")
	fs.BoolVar(&o.llm, "llm", false, "draft: заполнить LLM-рубрики через Claude API (нужен llm.api_key)")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if o.week > 0 {
		return errors.New("digest: -week задаёт смещение назад и должен быть <= 0")
	}
	switch o.to {
	case "", store.MessengerTelegram, store.MessengerMax:
	default:
		return fmt.Errorf("digest: неизвестный мессенджер %q (-to telegram|max)", o.to)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	if o.out == "" {
		o.out = digestOutDir(cfg)
	}

	loc, weekday, hour, err := digestSlotParams(cfg)
	if err != nil {
		return err
	}
	w := digest.SlotFor(time.Now(), loc, weekday, hour, o.week)

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	switch fs.Arg(0) {
	case "draft":
		return digestDraft(ctx, cfg, st, w, o)
	case "preview":
		return digestPreview(ctx, cfg, st, w, o)
	case "publish":
		return digestPublish(ctx, cfg, st, w, o)
	default:
		return fmt.Errorf("digest: неизвестное действие %q (draft|preview|publish)", fs.Arg(0))
	}
}

// digestOutDir — каталог черновиков: из конфига, иначе digest рядом с БД.
func digestOutDir(cfg *config.Config) string {
	if cfg.Digest.OutDir != "" {
		return cfg.Digest.OutDir
	}
	return filepath.Join(filepath.Dir(cfg.DBPath), "digest")
}

// digestSlotParams — параметры слота выпуска из конфига (дефолты — пакета
// digest). Общие для CLI и планировщика демона: их окна должны совпадать.
func digestSlotParams(cfg *config.Config) (*time.Location, time.Weekday, int, error) {
	d := cfg.Digest
	tz := d.TZ
	if tz == "" {
		tz = digest.DefaultTZ
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("digest.tz: %w", err)
	}
	if d.Weekday < 0 || d.Weekday > 6 {
		return nil, 0, 0, fmt.Errorf("digest.weekday %d вне 0–6 (0=воскресенье)", d.Weekday)
	}
	if d.Hour < 0 || d.Hour > 23 {
		return nil, 0, 0, fmt.Errorf("digest.hour %d вне 0–23", d.Hour)
	}
	return loc, time.Weekday(d.Weekday), d.Hour, nil
}

func digestDraftPath(o digestOpts, w digest.Window) string {
	if o.in != "" {
		return o.in
	}
	return digest.DraftPath(o.out, w.ID)
}

// digestDraft считает выпуск и пишет черновик + материалы.
func digestDraft(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	if !o.force {
		if _, err := os.Stat(digest.DraftPath(o.out, w.ID)); err == nil {
			return fmt.Errorf("черновик %s уже существует (правки админа — ценность); перезапись только с -force",
				digest.DraftPath(o.out, w.ID))
		}
	}
	src, closeSrc, err := digestSource(ctx, cfg, st)
	if err != nil {
		return err
	}
	defer closeSrc()

	is, err := digest.Build(ctx, src, w)
	if err != nil {
		return err
	}
	if o.llm {
		client, err := llmClient(cfg)
		if err != nil {
			return err
		}
		fmt.Printf("заполняю LLM-рубрики (%s, может занять минуты)…\n", client.Model())
		ed, err := digest.GenerateEditorial(ctx, client, is)
		if err != nil {
			return fmt.Errorf("LLM-редактура: %w", err)
		}
		is.Editorial = ed
	}
	draftPath, matPath, err := digest.WriteIssueFiles(is, o.out)
	if err != nil {
		return err
	}
	fmt.Printf("выпуск %s: заметок %d, комментариев %d, участников %d\n",
		w.ID, is.Stats.Notes, is.Stats.Comments, is.Stats.Commenters)
	fmt.Printf("черновик:  %s\nматериалы: %s\n", draftPath, matPath)
	if is.Editorial != nil {
		fmt.Println("дальше: проверьте preview, затем publish (рубрики уже заполнены)")
	} else {
		fmt.Println("дальше: заполните LLM-рубрики из материалов, проверьте preview, затем publish")
	}
	return nil
}

func loadDraft(o digestOpts, w digest.Window) (digest.Draft, error) {
	path := digestDraftPath(o, w)
	f, err := os.Open(path)
	if err != nil {
		return digest.Draft{}, fmt.Errorf("черновик не открыт (нужен digest draft?): %w", err)
	}
	defer f.Close()
	d, err := digest.ParseDraft(f, !o.force)
	if err != nil {
		return digest.Draft{}, fmt.Errorf("%s: %w", path, err)
	}
	if d.Dropped > 0 {
		fmt.Printf("внимание: %d секций с незаполненными LLM-плейсхолдерами выброшено (-force)\n", d.Dropped)
	}
	return d, nil
}

// digestPreview рендерит выпуск в stdout без отправки и без создания ботов.
// С настроенной площадкой первым идёт ТЕЛО ЗАМЕТКИ — то, что увидят люди, — а
// за ним разбиение по мессенджерам: оно остаётся способом посмотреть длины, но
// в бою выпуск в каналы несёт исходящий обход.
func digestPreview(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	d, err := loadDraft(o, w)
	if err != nil {
		return err
	}
	if cfg.Platform.Enabled && o.to == "" {
		body := digest.RenderPlatform(d, cfg.Platform.BaseURL)
		fmt.Printf("=== площадка: выпуск %s, %d знаков (потолок тела %d) ===\n%s\n\n",
			w.ID, len([]rune(body)), platform.MaxBodyRunes, body)
	}
	pubs := previewPublishers(cfg, o.to)
	if len(pubs) == 0 {
		return errors.New("digest: ни один мессенджер не включён (messengers.telegram/max)")
	}
	for _, p := range pubs {
		blocks, err := digest.ResolveLinks(ctx, st, d, p, cfg.Platform.BaseURL)
		if err != nil {
			return err
		}
		b := digest.BudgetFor(p)
		msgs := digest.SplitMessages(blocks, b)
		fmt.Printf("=== %s: выпуск %s, %d %s (предел %d) ===\n", p.Name(), w.ID,
			len(msgs), pluralParts(len(msgs)), b.Limit)
		for i, m := range msgs {
			fmt.Printf("--- часть %d/%d · %d видимых, %d по мере мессенджера ---\n%s\n",
				i+1, len(msgs), chantext.VisibleLen(m), b.Length(m), m)
		}
	}
	return nil
}

func pluralParts(n int) string {
	if n == 1 {
		return "часть"
	}
	if n >= 2 && n <= 4 {
		return "части"
	}
	return "частей"
}

// linkPub — офлайн-«приёмник» для preview: имя, формат ссылок и мера длины
// сообщения. Мера нужна затем же, зачем и сам предпросмотр: у MAX она своя, и
// без неё preview показывал бы разбиение, которого в бою не будет.
type linkPub struct {
	name   string
	link   func(threadID string) string
	budget digest.Budget
}

func (p linkPub) Name() string { return p.name }
func (p linkPub) PostChannelHTML(context.Context, string) (string, error) {
	return "", errors.New("preview не публикует")
}
func (p linkPub) ThreadLink(threadID string) string { return p.link(threadID) }

// MessageBudget реализует digest.SplitBudget.
func (p linkPub) MessageBudget() (int, func(string) int) { return p.budget.Limit, p.budget.Length }

func previewPublishers(cfg *config.Config, only string) []digest.Publisher {
	var pubs []digest.Publisher
	tgCfg, maxCfg := cfg.Messengers.Telegram, cfg.Messengers.Max
	if tgCfg.Enabled && (only == "" || only == store.MessengerTelegram) {
		chat := tgCfg.DiscussionChatID
		pubs = append(pubs, linkPub{
			name:   store.MessengerTelegram,
			budget: digest.RuneBudget(digest.MessageBudget),
			link:   func(t string) string { return tgx.ThreadDeepLink(chat, t) },
		})
	}
	if maxCfg.Enabled && (only == "" || only == store.MessengerMax) {
		chat := maxCfg.DiscussionChatID
		limit, length := maxx.MessageBudget()
		pubs = append(pubs, linkPub{
			name:   store.MessengerMax,
			budget: digest.Budget{Limit: limit, Length: length},
			link: func(t string) string {
				if chat == 0 {
					return ""
				}
				return maxx.MessageLink(chat, t)
			},
		})
	}
	return pubs
}

// digestPublish публикует выпуск. С настроенной площадкой — нативной заметкой
// ЗДЕСЬ (в каналы её отнесёт исходящий обход демона), без площадки — прямо в
// каналы. И то и другое идемпотентно по message_targets: повторный запуск
// докатывает недостающее и не дублирует уже опубликованное.
func digestPublish(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	d, err := loadDraft(o, w)
	if err != nil {
		return err
	}
	if cfg.Platform.Enabled && o.to == "" {
		return digestPublishSite(ctx, cfg, st, w, d)
	}
	pubs, err := publishPublishers(cfg, o.to)
	if err != nil {
		return err
	}
	if len(pubs) == 0 {
		return errors.New("digest: ни один мессенджер не включён (messengers.telegram/max)")
	}
	for _, p := range pubs {
		sent, err := digest.Publish(ctx, st, p, d, w.ID, cfg.Platform.BaseURL)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w (повторный publish докатит недостающие части)", p.Name(), err)
		case sent == 0:
			fmt.Printf("%s: выпуск %s уже опубликован\n", p.Name(), w.ID)
		default:
			fmt.Printf("%s: выпуск %s опубликован, отправлено %d %s\n", p.Name(), w.ID, sent, pluralParts(sent))
		}
	}
	return nil
}

// digestPublishSite — выпуск заметкой на площадке. Открывает свой пул: команду
// запускают руками при работающем демоне, и делить с ним нечего.
func digestPublishSite(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, d digest.Draft) error {
	if cfg.Platform.BaseURL == "" {
		return errors.New("digest: platform.base_url пуст — ссылки в выпуске вели бы в никуда")
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	site := digestSite{p: p}
	noteID, created, err := digest.PublishPlatform(ctx, st, site, d, w.ID, cfg.Platform.BaseURL)
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("площадка: выпуск %s уже опубликован — %s/n/%d\n", w.ID,
			strings.TrimSuffix(cfg.Platform.BaseURL, "/"), noteID)
		return nil
	}
	fmt.Printf("площадка: выпуск %s опубликован — %s/n/%d (в каналы его отнесёт демон)\n",
		w.ID, strings.TrimSuffix(cfg.Platform.BaseURL, "/"), noteID)
	if err := digest.PinIssue(ctx, st, site, noteID); err != nil {
		fmt.Printf("выпуск не закреплён: %v\n", err)
	}
	return nil
}

func publishPublishers(cfg *config.Config, only string) ([]digest.Publisher, error) {
	var pubs []digest.Publisher
	tgCfg, maxCfg := cfg.Messengers.Telegram, cfg.Messengers.Max
	if tgCfg.Enabled && (only == "" || only == store.MessengerTelegram) {
		if tgCfg.Token == "" || tgCfg.ChannelID == 0 {
			return nil, errors.New("digest: telegram включён, но не заданы token/channel_id")
		}
		tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
		if err != nil {
			return nil, err
		}
		tg, err := tgx.NewMirror(tgx.Params{
			Token:            tgCfg.Token,
			ChannelID:        tgCfg.ChannelID,
			DiscussionChatID: tgCfg.DiscussionChatID,
			Signature:        tgCfg.Signature,
			HTTPClient:       tgClient,
		}, slog.Default(), func(context.Context, *models.Update) {})
		if err != nil {
			return nil, err
		}
		pubs = append(pubs, tg)
	}
	if maxCfg.Enabled && (only == "" || only == store.MessengerMax) {
		if maxCfg.Token == "" || maxCfg.ChannelID == 0 {
			return nil, errors.New("digest: max включён, но не заданы token/channel_id")
		}
		mx, err := maxx.NewMirror(maxx.Params{
			Token:            maxCfg.Token,
			ChannelID:        maxCfg.ChannelID,
			DiscussionChatID: maxCfg.DiscussionChatID,
			Signature:        maxCfg.Signature,
			BaseURL:          cfg.Site.BaseURL,
			HTTPClient:       maxx.MintsifraClient(),
		}, slog.Default())
		if err != nil {
			return nil, err
		}
		pubs = append(pubs, mx)
	}
	return pubs, nil
}

// digestSite — площадка глазами дайджеста (реализация digest.Site): выпуск
// выходит нативной заметкой ОТ САМОЙ ПЛОЩАДКИ и закрепляется наверху ленты.
//
// Прослойка нужна затем, чтобы `digest` не знал ни про pgx, ни про Viewer: у
// него всего два дела с площадкой, и список этих дел — часть ответа на вопрос
// «что выпуск вправе с ней сделать».
//
// ПОДПИСАНТА БОЛЬШЕ НЕ НАСТРАИВАЮТ (05.09.2026). До этого дня им был живой
// человек — своя настройка `digest.author_profile_id`, иначе владелец амвона, —
// и 05.09 сводка про Зазеркалье уехала заметкой на love.ngs.ru под его именем:
// галочка «отправлять мои записи на НГС» стои́т у АВТОРА, а не у текста. Чинить
// это исключением для дайджеста значило бы завести второе место, где написано,
// что выпуск особенный; вместо этого у площадки появилась своя анкета, и
// правило стало одним: чьё это слово, тот его и уносит.
type digestSite struct {
	p *platform.Platform
}

// author — служебная анкета площадки. Спрашивается КАЖДЫЙ раз, а не на старте
// демона: анкету заводит `platform migrate`, то есть рука администратора в
// известный момент, и запомненный на старте отказ пережил бы саму починку —
// демон, поднятый до миграции, молчал бы и после неё до перезапуска. Стоит
// вопрос одного range-scan по частичному индексу, а задают его раз в неделю.
func (s digestSite) author(ctx context.Context) (platform.User, error) {
	id, err := s.p.SystemUserID(ctx)
	if err != nil {
		return platform.User{}, err
	}
	return s.p.UserByID(ctx, id)
}

func (s digestSite) PublishNote(ctx context.Context, body string) (int64, error) {
	u, err := s.author(ctx)
	if err != nil {
		return 0, err
	}
	return s.p.CreateNote(ctx, platform.NewNote{AuthorID: u.ID, Body: body})
}

// PinNote закрепляет и открепляет выпуск.
//
// Роль подписанта читается КАЖДЫЙ раз, а не запоминается на старте: закреп
// бывает раз в неделю, а права меняются командой `platform role` — запомненная
// роль соврала бы при первой же смене.
//
// «Состояние уже такое» — УСПЕХ, а не отказ: у метода спрашивают «сделай, чтобы
// было так», а не «переключи». Довод не теоретический — 05.09.2026 прошлый
// выпуск оказался откреплён руками, ErrNothingToDo с его открепления уехал
// наверх, и свежий выпуск не закрепился вовсе; PinIssue при этом ровно про этот
// случай и написала в комментарии, что он не повод.
func (s digestSite) PinNote(ctx context.Context, noteID int64, pinned bool) error {
	u, err := s.author(ctx)
	if err != nil {
		return err
	}
	err = s.p.SetNotePinned(ctx, platform.Viewer{UserID: u.ID, Role: u.Role},
		noteID, pinned, "выпуск дайджеста")
	if errors.Is(err, platform.ErrNothingToDo) {
		return nil
	}
	return err
}

// digestSource — по чему считать выпуск. С площадкой — по ней: сводку
// публикуют там, значит и считать её надо по тому, что человек видит вокруг
// выпуска, а написанное на площадке в SQLite не попадает вовсе. Без площадки
// остаётся зеркало НГС, по которому дайджест жил с самого начала.
//
// Возвращает закрывалку пула: команда разовая, и держать соединения после неё
// незачем.
func digestSource(ctx context.Context, cfg *config.Config, st *store.Store) (digest.Source, func(), error) {
	if !cfg.Platform.Enabled {
		return digest.NewStoreSource(st, cfg.Site.BaseURL), func() {}, nil
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return nil, nil, err
	}
	return platdigest.New(p), p.Close, nil
}
