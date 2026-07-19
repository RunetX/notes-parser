package love

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Пути и поля форм сайта. Точный паритет с Python-версией
// (poster.py: love_comment_data, ryumkin.py: love_note_data).
const (
	commentPostPathFmt = "/notes/comments/%s"
	notePostPath       = "/notes/add/"
)

// PostComment отправляет комментарий к заметке от имени пользователя.
// comAPIID — id комментария, на который отвечаем; пустая строка —
// ответ в корень заметки.
func (c *Client) PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error {
	form := url.Values{
		"noteId":   {noteID},
		"comId":    {"0"},
		"comApiId": {comAPIID},
		"reason":   {""},
		"content":  {text},
	}
	resp, err := c.postForm(ctx, fmt.Sprintf(commentPostPathFmt, noteID), form, cookies)
	if err != nil {
		return fmt.Errorf("отправка комментария к заметке %s: %w", noteID, err)
	}
	if err := drainOK(resp); err != nil {
		return fmt.Errorf("отправка комментария к заметке %s: %w", noteID, err)
	}
	return nil
}

// PostNote публикует новую заметку от имени пользователя.
func (c *Client) PostNote(ctx context.Context, cookies []*http.Cookie, text string, anonymous bool) error {
	hideMe := "0"
	if anonymous {
		hideMe = "1"
	}
	form := url.Values{
		"action_note[lid]":    {"0"},
		"action_note[href]":   {""},
		"action_note[hideme]": {hideMe},
		"action_note[nocom]":  {"0"},
		"action_note[rules]":  {"1"},
		"id":                  {""},
		"category_note":       {"0"},
		"letter":              {text},
	}
	resp, err := c.postForm(ctx, notePostPath, form, cookies)
	if err != nil {
		return fmt.Errorf("публикация заметки: %w", err)
	}
	if err := drainOK(resp); err != nil {
		return fmt.Errorf("публикация заметки: %w", err)
	}
	return nil
}

// drainOK вычитывает и закрывает тело ответа (для переиспользования
// соединения) и проверяет статус. Паритет с Python: сайт отвечает 200 и
// HTML-страницей и на успех, и требует авторизацию через куки сессии.
func drainOK(resp *http.Response) error {
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("статус %d", resp.StatusCode)
	}
	return nil
}
