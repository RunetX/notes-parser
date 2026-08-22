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
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/chantext"
	"lovegw/internal/config"
	"lovegw/internal/digest"
	"lovegw/internal/maxx"
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
	is, err := digest.Build(ctx, st, w, cfg.Site.BaseURL)
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
		blocks, err := digest.ResolveLinks(ctx, st, d, p, cfg.Site.BaseURL)
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
		sent, err := digest.Publish(ctx, st, p, d, w.ID, cfg.Site.BaseURL)
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
	author, err := digestAuthorID(cfg)
	if err != nil {
		return err
	}
	p, err := platform.Open(ctx, cfg.Platform.DSN)
	if err != nil {
		return err
	}
	defer p.Close()

	site := digestSite{p: p, author: author}
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
			BaseURL:          cfg.Site.BaseURL,
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
// выходит нативной заметкой от анкеты подписанта и закрепляется наверху ленты.
//
// Прослойка нужна затем, чтобы `digest` не знал ни про pgx, ни про Viewer: у
// него всего два дела с площадкой, и список этих дел — часть ответа на вопрос
// «что выпуск вправе с ней сделать».
type digestSite struct {
	p      *platform.Platform
	author int64
}

func (s digestSite) PublishNote(ctx context.Context, body string) (int64, error) {
	return s.p.CreateNote(ctx, platform.NewNote{AuthorID: s.author, Body: body})
}

// PinNote закрепляет и открепляет выпуск. Роль подписанта читается КАЖДЫЙ раз,
// а не запоминается на старте: закреп бывает раз в неделю, а права меняются
// командой `platform role` — запомненная роль соврала бы при первой же смене.
func (s digestSite) PinNote(ctx context.Context, noteID int64, pinned bool) error {
	u, err := s.p.UserByID(ctx, s.author)
	if err != nil {
		return err
	}
	return s.p.SetNotePinned(ctx, platform.Viewer{UserID: u.ID, Role: u.Role},
		noteID, pinned, "выпуск дайджеста")
}

// digestAuthorID — анкета подписанта выпуска: своя настройка, иначе владелец
// амвона. Один и тот же человек, но настройки разные: амвон можно выключить
// целиком, а выпуску подписант нужен всегда.
func digestAuthorID(cfg *config.Config) (int64, error) {
	raw := cfg.Digest.AuthorProfileID
	if raw == "" {
		raw = cfg.Pulpit.OwnerProfileID
	}
	if raw == "" {
		return 0, errors.New("не задан подписант выпуска: digest.author_profile_id (или pulpit.owner_profile_id)")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("подписант выпуска %q: не число", raw)
	}
	return id, nil
}
