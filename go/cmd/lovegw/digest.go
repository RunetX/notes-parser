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
	"time"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/config"
	"lovegw/internal/digest"
	"lovegw/internal/maxx"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
)

type digestOpts struct {
	week  int
	out   string
	in    string
	to    string
	force bool
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
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "week": true, "out": true, "in": true, "to": true,
	})); err != nil {
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
		o.out = filepath.Join(filepath.Dir(cfg.DBPath), "digest")
	}

	loc, err := time.LoadLocation(digest.DefaultTZ)
	if err != nil {
		return err
	}
	w := digest.SlotFor(time.Now(), loc, digest.DefaultWeekday, digest.DefaultHour, o.week)

	st, err := store.Open(ctx, cfg.DBPath)
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

func digestDraftPath(o digestOpts, w digest.Window) string {
	if o.in != "" {
		return o.in
	}
	return filepath.Join(o.out, "digest-"+w.ID+".draft.txt")
}

// digestDraft считает выпуск и пишет черновик + материалы.
func digestDraft(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	draftPath := filepath.Join(o.out, "digest-"+w.ID+".draft.txt")
	matPath := filepath.Join(o.out, "digest-"+w.ID+".materials.md")
	if !o.force {
		if _, err := os.Stat(draftPath); err == nil {
			return fmt.Errorf("черновик %s уже существует (правки админа — ценность); перезапись только с -force", draftPath)
		}
	}
	is, err := digest.Build(ctx, st, w, cfg.Site.BaseURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return err
	}
	if err := writeFileWith(draftPath, func(f *os.File) error { return digest.WriteDraft(f, is) }); err != nil {
		return err
	}
	if err := writeFileWith(matPath, func(f *os.File) error { return digest.WriteMaterials(f, is) }); err != nil {
		return err
	}
	fmt.Printf("выпуск %s: заметок %d, комментариев %d, участников %d\n",
		w.ID, is.Stats.Notes, is.Stats.Comments, is.Stats.Commenters)
	fmt.Printf("черновик:  %s\nматериалы: %s\n", draftPath, matPath)
	fmt.Println("дальше: заполните LLM-рубрики из материалов, проверьте preview, затем publish")
	return nil
}

func writeFileWith(path string, write func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
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

// digestPreview рендерит выпуск по каждому мессенджеру в stdout: ссылки,
// разбиение на части и видимые длины — без создания ботов и без отправки.
func digestPreview(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	d, err := loadDraft(o, w)
	if err != nil {
		return err
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
		msgs := digest.SplitMessages(blocks, digest.MessageBudget)
		fmt.Printf("=== %s: выпуск %s, %d %s ===\n", p.Name(), w.ID,
			len(msgs), pluralParts(len(msgs)))
		for i, m := range msgs {
			fmt.Printf("--- часть %d/%d · %d видимых символов ---\n%s\n", i+1, len(msgs),
				digest.VisibleLen(m), m)
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

// linkPub — офлайн-«приёмник» для preview: только имя и формат ссылок.
type linkPub struct {
	name string
	link func(threadID string) string
}

func (p linkPub) Name() string { return p.name }
func (p linkPub) PostChannelHTML(context.Context, string) (string, error) {
	return "", errors.New("preview не публикует")
}
func (p linkPub) ThreadLink(threadID string) string { return p.link(threadID) }

func previewPublishers(cfg *config.Config, only string) []digest.Publisher {
	var pubs []digest.Publisher
	tgCfg, maxCfg := cfg.Messengers.Telegram, cfg.Messengers.Max
	if tgCfg.Enabled && (only == "" || only == store.MessengerTelegram) {
		chat := tgCfg.DiscussionChatID
		pubs = append(pubs, linkPub{name: store.MessengerTelegram, link: func(t string) string {
			return tgx.ThreadDeepLink(chat, t)
		}})
	}
	if maxCfg.Enabled && (only == "" || only == store.MessengerMax) {
		chat := maxCfg.DiscussionChatID
		pubs = append(pubs, linkPub{name: store.MessengerMax, link: func(t string) string {
			if chat == 0 {
				return ""
			}
			return maxx.MessageLink(chat, t)
		}})
	}
	return pubs
}

// digestPublish постит выпуск во включённые мессенджеры. Части и головная
// запись фиксируются в message_targets — повторный запуск докатывает
// недостающее и не дублирует уже отправленное.
func digestPublish(ctx context.Context, cfg *config.Config, st *store.Store, w digest.Window, o digestOpts) error {
	d, err := loadDraft(o, w)
	if err != nil {
		return err
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
