package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// cmdPull заводит заметку по прямому id, минуя ленту: читает страницу заметки
// (шапка + комментарии) и кладёт её в БД со статусом pending. Дальше работает
// обычная логика демона — retryPending постит заметку в каналы всех
// приёмников, а воркер комментариев дозабирает и зеркалит все комментарии.
// Нужна, когда заметка не попала в обход ленты (уехала из окна за время
// простоя) и вернуть её туда уже нельзя.
//
// Безопасна при работающем демоне: пишется одна строка, всё остальное делает
// сам демон. Уже запощенную заметку не трогает — для перепоста есть repost.
func cmdPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", "", "путь к БД (перебивает db_path из конфига)")
	full := fs.Bool("full", false,
		"дотянуть весь тред (view=tree, все страницы) и снять заметку с архива — все комментарии уйдут в мессенджеры")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	ids := fs.Args()
	if len(ids) == 0 {
		return fmt.Errorf("pull: не указан id заметки")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dbPath != "" {
		cfg.DBPath = *dbPath
	}
	log := newLogger(cfg.LogLevel)
	client := newSiteClient(cfg, log)

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	for _, id := range ids {
		if err := pullOne(ctx, st, client, cfg.Site.BaseURL, id, *full, log); err != nil {
			return fmt.Errorf("pull %s: %w", id, err)
		}
	}
	return nil
}

func pullOne(ctx context.Context, st *store.Store, client *love.Client,
	baseURL, id string, full bool, log *slog.Logger) error {
	existing, err := st.NoteByID(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// заметки нет — заводим ниже
	case err != nil:
		return err
	case existing.Status == store.StatusSeeded:
		// Зафиксирована seed-обходом без постинга: снимаем, чтобы завести заново.
		if err := st.DeleteNote(ctx, id); err != nil {
			return err
		}
		fmt.Printf("заметка %s была seeded — заводим заново\n", id)
	case full:
		// Заметка уже в канале (возможно, снята с отслеживания) — возвращаем в
		// работу и дотягиваем тред: недостающие комментарии уйдут в те же треды.
		if err := st.SetNoteStatusPosted(ctx, id); err != nil {
			return err
		}
		fmt.Printf("заметка %s (была %q) возвращена в отслеживание\n", id, existing.Status)
		return backfillComments(ctx, st, client, baseURL, id, log)
	default:
		return fmt.Errorf("заметка уже в БД со статусом %q (для перепоста — lovegw repost %s, "+
			"для дотяжки комментариев — lovegw pull -full %s)", existing.Status, id, id)
	}

	page, err := client.FetchCommentsPage(ctx, id)
	if err != nil {
		return err
	}
	if page.Note == nil {
		return fmt.Errorf("шапка заметки не разобралась — вёрстка сайта изменилась")
	}
	n := *page.Note

	added, err := st.InsertNote(ctx, store.Note{
		ID:              id,
		AuthorID:        n.AuthorID,
		AuthorName:      n.AuthorName,
		Text:            n.Text,
		AuthorAvatarURL: n.AuthorAvatarURL,
		Status:          store.StatusPending,
		FirstSeenAt:     time.Now(),
	})
	if err != nil {
		return err
	}
	if !added {
		return fmt.Errorf("заметка появилась в БД параллельно — повторите позже")
	}
	for i, url := range n.Images {
		if err := st.InsertNoteImage(ctx, id, i, url); err != nil {
			return err
		}
	}
	if n.CommentsClosed {
		if _, err := st.MarkNoteCommentsClosed(ctx, id); err != nil {
			return err
		}
	}
	fmt.Printf("заметка %s заведена: автор %s, иллюстраций %d, комментариев на сайте %d — "+
		"демон запостит её и зеркалит комментарии в ближайшем цикле\n",
		id, n.AuthorName, len(n.Images), len(page.Comments))
	if full {
		return backfillComments(ctx, st, client, baseURL, id, log)
	}
	return nil
}

// backfillComments дотягивает весь тред заметки (древовидный вид отдаёт его
// целиком, обычный опрос видит только первую страницу) и кладёт недостающие
// комментарии в БД. Постит их уже демон — по одному в тред каждого приёмника,
// в порядке возрастания id.
func backfillComments(ctx context.Context, st *store.Store, client *love.Client,
	baseURL, id string, log *slog.Logger) error {
	res, err := grabNote(ctx, client, grabOptions{
		baseURL: baseURL,
		noteID:  id,
		view:    love.ViewTree,
	}, log)
	if err != nil {
		return err
	}
	known, err := st.CommentIDs(ctx, id)
	if err != nil {
		return err
	}
	added := 0
	for _, c := range res.Comments {
		if known[c.ID] {
			continue
		}
		if _, err := st.InsertComment(ctx, store.Comment{
			ID:          c.ID,
			NoteID:      id,
			AuthorName:  c.AuthorName,
			AuthorAge:   c.AuthorAge,
			AuthorLink:  c.AuthorLink,
			AvatarURL:   c.AvatarURL,
			PublishedAt: c.PublishedAt,
			Text:        c.Text,
			CreatedAt:   time.Now(),
		}); err != nil {
			return err
		}
		added++
	}
	fmt.Printf("тред %s: в БД добавлено %d комментариев из %d — демон отправит их в мессенджеры\n",
		id, added, len(res.Comments))
	return nil
}
