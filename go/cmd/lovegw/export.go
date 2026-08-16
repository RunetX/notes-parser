package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"lovegw/internal/archive"
)

// Экспортные структуры: заметка + дерево комментариев (вложенное), с
// денормализованными авторами — самодостаточный JSON для внешних инструментов
// (визуализация графа и т.п.). Строится из archive.db без обращения к сайту.
type exportUser struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Age        string `json:"age,omitempty"`
	ProfileURL string `json:"profile_url,omitempty"`
	AvatarURL  string `json:"avatar_url,omitempty"`
}

type exportComment struct {
	ID          int64            `json:"id"`
	Author      exportUser       `json:"author"`
	PublishedAt *time.Time       `json:"published_at,omitempty"`
	Text        string           `json:"text"`
	Replies     []*exportComment `json:"replies,omitempty"`
}

type exportNote struct {
	ID             int64       `json:"id"`
	Author         *exportUser `json:"author"` // null — аноним
	Text           string      `json:"text"`
	Images         []string    `json:"images"`
	CommentsClosed bool        `json:"comments_closed"`
	PublishedAt    *time.Time  `json:"published_at,omitempty"`
	GrabbedAt      time.Time   `json:"grabbed_at"`
}

type exportResult struct {
	Note     exportNote       `json:"note"`
	Comments []*exportComment `json:"comments"` // корни дерева
}

// cmdExport выгружает заметку из archive.db во вложенное JSON-дерево. Полностью
// офлайн (в отличие от grab): читает только БД. Пример: `lovegw export 312750`.
func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dbPath := fs.String("db", defaultArchivePath, "путь к archive.db")
	outDir := fs.String("out", ".", "каталог для <id>.json")
	if err := fs.Parse(reorderArgs(args, fs)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		usage()
		return fmt.Errorf("export: не указан id заметки")
	}
	noteID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("export: id заметки не число: %q", fs.Arg(0))
	}

	ar, err := archive.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer ar.Close()

	note, ok, err := ar.LoadNote(ctx, noteID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("export: заметка %d не найдена в архиве %s (сначала grab)", noteID, *dbPath)
	}
	comments, err := ar.LoadComments(ctx, noteID)
	if err != nil {
		return err
	}

	roots := buildTree(comments)
	res := exportResult{
		Note: exportNote{
			ID:             note.ID,
			Author:         userPtr(note.Author),
			Text:           note.Text,
			Images:         nonNilImages(note.Images),
			CommentsClosed: note.CommentsClosed,
			PublishedAt:    ptrTime(note.PublishedAt),
			GrabbedAt:      note.GrabbedAt,
		},
		Comments: roots,
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(*outDir, fs.Arg(0)+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "экспорт заметки %d: комментариев %d, корней %d → %s\n",
		noteID, len(comments), len(roots), path)
	return nil
}

// buildTree превращает плоский список (по возрастанию id) во вложенное дерево:
// каждый комментарий подвешивается к родителю по parent_id. Сироты (родитель
// вне выборки — бывает при частичной выгрузке) становятся корнями, чтобы ничего
// не потерять. Порядок детей — по id (хронология), т.к. вход уже отсортирован.
func buildTree(comments []archive.StoredComment) []*exportComment {
	nodes := make(map[int64]*exportComment, len(comments))
	for _, c := range comments {
		nodes[c.ID] = &exportComment{
			ID: c.ID,
			Author: exportUser{
				ID: c.Author.ID, Name: c.Author.Name, Age: c.Author.Age,
				ProfileURL: c.Author.ProfileURL, AvatarURL: c.Author.AvatarURL,
			},
			PublishedAt: ptrTime(c.PublishedAt),
			Text:        c.Text,
		}
	}
	var roots []*exportComment
	for _, c := range comments {
		node := nodes[c.ID]
		if c.ParentID != 0 {
			if parent, ok := nodes[c.ParentID]; ok {
				parent.Replies = append(parent.Replies, node)
				continue
			}
		}
		roots = append(roots, node)
	}
	return roots
}

func userPtr(u *archive.User) *exportUser {
	if u == nil {
		return nil
	}
	return &exportUser{ID: u.ID, Name: u.Name, Age: u.Age, ProfileURL: u.ProfileURL, AvatarURL: u.AvatarURL}
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nonNilImages(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
