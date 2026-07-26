package maxx

// Композиция HTML-текстов для MAX. Порт композеров tgx: поддерживаемые MAX
// теги (<b>, <a href>) совпадают с используемыми, поэтому перенос дословный,
// включая экранирование (сырой текст в HTML-режиме — латентный баг
// Python-версии). Отличие от tgx: ветвления «влезает в подпись» нет — текст
// и вложение в MAX живут в одном сообщении; комментарий не режем — лимит
// длины сообщения MAX не задокументирован (бриф, R2).

import (
	"fmt"
	"html"
	"strings"

	"lovegw/internal/store"
)

// ComposeNoteMessage собирает HTML-текст поста заметки.
func ComposeNoteMessage(baseURL, signature string, n store.Note) string {
	name := html.EscapeString(n.AuthorName)
	var b strings.Builder
	if n.AuthorID == "" || n.AuthorID == "0" {
		fmt.Fprintf(&b, "<b>%s:</b>\n", name)
	} else {
		fmt.Fprintf(&b, `<b><a href="%s/profile/%s">%s:</a></b>%s`, baseURL, n.AuthorID, name, "\n")
	}
	b.WriteString(html.EscapeString(n.Text))
	if signature != "" {
		b.WriteString("\n\n")
		b.WriteString(signature)
	}
	return b.String()
}

// ComposeCommentMessage собирает HTML комментария с заголовком-ссылкой автора.
func ComposeCommentMessage(c store.Comment) string {
	return fmt.Sprintf(`<b><a href="%s">%s, %s:</a></b>%s%s`,
		c.AuthorLink, html.EscapeString(c.AuthorName), html.EscapeString(c.AuthorAge),
		"\n", html.EscapeString(c.Text))
}
