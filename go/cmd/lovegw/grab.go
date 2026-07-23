package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/archive"
	"lovegw/internal/config"
	"lovegw/internal/love"
)

// grabResult — денормализованный снимок выгрузки: сама заметка и плоский список
// всех её комментариев. Пишется в -json для просмотра; для графа те же данные
// нормализуются в archive.db (users/notes/comments).
type grabResult struct {
	Note     love.Note      `json:"note"`
	Comments []love.Comment `json:"comments"`
}

// hardPageCap — страховочный предел числа страниц комментариев, даже при
// -max-pages=0. У самых обсуждаемых заметок счёт страниц идёт на десятки.
const hardPageCap = 500

// defaultArchivePath — БД архива по умолчанию, отдельно от боевой data/lovegw.db.
const defaultArchivePath = "data/archive.db"

// cmdGrab — разовый граббер: по номеру заметки выгружает её саму и все страницы
// комментариев в древовидном виде и нормализует в archive.db (типажи в отдельной
// таблице users, дерево через parent_id). Только чтение сайта, без кук.
// Пример: `lovegw grab 312750` (+ `-json` для <id>.json на просмотр).
func cmdGrab(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grab", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath, configFlagUsage)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	writeJSON := fs.Bool("json", false, "дополнительно выгрузить <id>.json (денормализованный снимок)")
	outDir := fs.String("out", ".", "каталог для <id>.json (с -json)")
	saveHTML := fs.String("save-html", "", "каталог для сохранения сырого HTML (фикстуры)")
	view := fs.String("view", love.ViewTree, "вид комментариев: tree (с деревом ответов) или linear")
	maxPages := fs.Int("max-pages", 0, "предел числа страниц комментариев (0 — все)")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"config": true, "db": true, "out": true, "save-html": true, "view": true, "max-pages": true,
	})); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("grab: не указан id заметки")
	}
	noteID := fs.Arg(0)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)

	res, err := grabNote(ctx, client, grabOptions{
		baseURL:  cfg.Site.BaseURL,
		noteID:   noteID,
		view:     *view,
		maxPages: *maxPages,
		saveHTML: *saveHTML,
	}, log)
	if err != nil {
		return err
	}

	// Нормализуем в archive.db.
	note, comments, users, err := mapGrabToArchive(cfg.Site.BaseURL, res)
	if err != nil {
		return err
	}
	ar, err := archive.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()
	st, err := ar.SaveGrab(ctx, note, comments, users, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr,
		"архив %s ← заметка %s (%q): комментариев +%d/%d; типажи: затронуто %d, новых %d, аватар обновлён %d, имя %d\n",
		*dbPath, noteID, res.Note.AuthorName, st.CommentsInserted, st.CommentsTotal,
		len(users), st.NewUsers, st.AvatarChanged, st.NameChanged)

	// Денормализованный JSON — только по запросу.
	if *writeJSON {
		if err := writeGrabJSON(*outDir, noteID, res); err != nil {
			return err
		}
	}
	return nil
}

// grabOptions — параметры обхода одной заметки (сгруппированы, чтобы не плодить
// длинную сигнатуру).
type grabOptions struct {
	baseURL  string
	noteID   string
	view     string
	maxPages int
	saveHTML string
}

// grabNote обходит страницы комментариев заметки, пока на очередной странице
// появляются новые (не виданные) комментарии, и собирает их в один плоский
// список с дедупликацией по id. Шапку заметки берёт с первой страницы.
//
// Условия остановки: закончились новые комментарии (страница пуста или всё —
// дубли), достигнут предел страниц, либо сайт вернул ошибку на странице > 1
// (считаем концом пейджера). 403 (геоблок/бан) прекращает работу сразу.
func grabNote(ctx context.Context, client *love.Client, opt grabOptions, log *slog.Logger) (grabResult, error) {
	var res grabResult
	seen := map[int64]bool{}

	limit := opt.maxPages
	if limit <= 0 || limit > hardPageCap {
		limit = hardPageCap
	}

	for page := 1; page <= limit; page++ {
		raw, err := client.RawCommentsView(ctx, opt.noteID, page, opt.view)
		if err != nil {
			if errors.Is(err, love.ErrForbidden) {
				return res, err // блок/геоблок — прекращаем и сообщаем наверх
			}
			if page == 1 {
				return res, err // даже первую страницу не получили
			}
			// Страница за пределами пейджера: сайт может ответить 404 —
			// это не ошибка, а конец треда.
			log.Warn("страница комментариев не получена, считаю концом", "note", opt.noteID, "page", page, "err", err)
			break
		}
		if opt.saveHTML != "" {
			name := fmt.Sprintf("comments_%s_%s_p%d.html", opt.view, opt.noteID, page)
			if err := saveRaw(opt.saveHTML, name, raw); err != nil {
				return res, err
			}
		}

		if page == 1 {
			note, err := love.ParseNoteFromCommentsPage(bytes.NewReader(raw), opt.baseURL)
			if err != nil {
				return res, err
			}
			note.ID = opt.noteID
			res.Note = note
		}

		comments, err := love.ParseComments(bytes.NewReader(raw), opt.baseURL)
		if err != nil {
			return res, err
		}
		added := 0
		for _, c := range comments {
			if seen[c.ID] {
				continue
			}
			seen[c.ID] = true
			res.Comments = append(res.Comments, c)
			added++
		}
		log.Info("страница комментариев разобрана", "note", opt.noteID, "page", page, "new", added, "total", len(res.Comments))
		// Пейджер сайта помечает следующую страницу <link rel="next">. Его
		// отсутствие — конец треда: для древовидного вида (весь тред на одной
		// странице) это 1 запрос на заметку, без лишнего добора пустой страницы.
		if added == 0 || !bytes.Contains(raw, []byte(`rel="next"`)) {
			break
		}
	}

	// Плоский список по возрастанию id (≈ хронология) — так дерево по parent_id
	// читается сверху вниз в текстовом редакторе.
	sort.Slice(res.Comments, func(i, j int) bool { return res.Comments[i].ID < res.Comments[j].ID })
	return res, nil
}

// mapGrabToArchive переводит денормализованную выгрузку в структуры архива:
// выделяет типажей (автор заметки + авторы комментариев) в отдельный список
// users с дедупликацией по числовому id анкеты; данные комментатора полнее
// (есть возраст и ссылка), поэтому перекрывают разреженные данные автора заметки.
func mapGrabToArchive(baseURL string, res grabResult) (archive.Note, []archive.Comment, []archive.User, error) {
	noteID, err := strconv.ParseInt(res.Note.ID, 10, 64)
	if err != nil {
		return archive.Note{}, nil, nil, fmt.Errorf("id заметки не число: %q", res.Note.ID)
	}

	note := archive.Note{
		ID:             noteID,
		AuthorID:       parseAuthorID(res.Note.AuthorID),
		Text:           res.Note.Text,
		Images:         res.Note.Images,
		CommentsClosed: res.Note.CommentsClosed,
		PublishedAt:    res.Note.PublishedAt,
	}

	users := map[int64]archive.User{}
	if note.AuthorID > 0 {
		users[note.AuthorID] = archive.User{
			ID:         note.AuthorID,
			Name:       res.Note.AuthorName,
			ProfileURL: profileURL(baseURL, note.AuthorID),
			AvatarURL:  res.Note.AuthorAvatarURL,
		}
	}

	comments := make([]archive.Comment, 0, len(res.Comments))
	for _, c := range res.Comments {
		aid := parseAuthorID(c.AuthorID)
		comments = append(comments, archive.Comment{
			ID:          c.ID,
			NoteID:      noteID,
			ParentID:    c.ParentID,
			AuthorID:    aid,
			Text:        c.Text,
			PublishedAt: c.PublishedAt,
		})
		if aid > 0 {
			users[aid] = archive.User{
				ID:         aid,
				Name:       c.AuthorName,
				Age:        c.AuthorAge,
				ProfileURL: c.AuthorLink,
				AvatarURL:  c.AvatarURL,
			}
		}
	}

	list := make([]archive.User, 0, len(users))
	for _, u := range users {
		list = append(list, u)
	}
	return note, comments, list, nil
}

// parseAuthorID: числовой id анкеты; 0 — аноним/непарсибельно.
func parseAuthorID(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// profileURL достраивает абсолютный URL анкеты по числовому id (для автора
// заметки, у которого в шапке нет готовой ссылки).
func profileURL(baseURL string, id int64) string {
	return strings.TrimSuffix(baseURL, "/") + "/profile/" + strconv.FormatInt(id, 10) + "/"
}

func writeGrabJSON(outDir, noteID string, res grabResult) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(outDir, noteID+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "JSON сохранён:", path)
	return nil
}
