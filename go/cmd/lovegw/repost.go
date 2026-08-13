package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-telegram/bot/models"

	"lovegw/internal/config"
	"lovegw/internal/store"
	"lovegw/internal/tgx"
)

// cmdRepost удаляет указанные заметки из Telegram и БД, чтобы демон при
// следующем обходе ленты перепостил их заново по текущей логике. Отладочная
// команда: удаляет реальные сообщения в канале и группе обсуждения —
// запускать при остановленном демоне.
func cmdRepost(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repost", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ids := fs.Args()
	if len(ids) == 0 {
		return fmt.Errorf("repost: не указан id заметки")
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

	tgClient, err := tgx.ProxyClient(cfg.TelegramProxy)
	if err != nil {
		return err
	}
	tgCfg := cfg.Messengers.Telegram
	tg, err := tgx.NewMirror(tgx.Params{
		Token:            tgCfg.Token,
		ChannelID:        tgCfg.ChannelID,
		DiscussionChatID: tgCfg.DiscussionChatID,
		Signature:        tgCfg.Signature,
		BaseURL:          cfg.Site.BaseURL,
		HTTPClient:       tgClient,
	}, slog.Default(), func(context.Context, *models.Update) {})
	if err != nil {
		return err
	}

	for _, id := range ids {
		if err := repostOne(ctx, st, tg, cfg, id); err != nil {
			fmt.Fprintf(os.Stderr, "repost %s: %v\n", id, err)
		}
	}
	return nil
}

func repostOne(ctx context.Context, st *store.Store, tg *tgx.Mirror, cfg *config.Config, id string) error {
	n, err := st.NoteByID(ctx, id)
	if err != nil {
		return err
	}

	del := func(chatID, msgID int64, what string) {
		if msgID == 0 {
			return
		}
		if err := tg.DeleteMessage(ctx, chatID, int(msgID)); err != nil {
			fmt.Fprintf(os.Stderr, "  %s %d не удалён: %v\n", what, msgID, err)
		}
	}

	comIDs, _ := st.SentCommentTGMessageIDs(ctx, id)
	imgIDs, _ := st.SentNoteImageTGMessageIDs(ctx, id)
	tgCfg := cfg.Messengers.Telegram
	for _, m := range comIDs {
		del(tgCfg.DiscussionChatID, m, "комментарий")
	}
	for _, m := range imgIDs {
		del(tgCfg.DiscussionChatID, m, "иллюстрация")
	}
	del(tgCfg.DiscussionChatID, n.TGThreadID, "корень треда")
	del(tgCfg.ChannelID, n.TGMessageID, "пост в канале")

	if err := st.DeleteNote(ctx, id); err != nil {
		return err
	}
	fmt.Printf("заметка %s удалена (комментариев %d, иллюстраций %d) — будет перепощена при обходе ленты\n",
		id, len(comIDs), len(imgIDs))
	return nil
}
