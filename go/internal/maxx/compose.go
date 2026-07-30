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

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
const (
	subNoticeCommentLimit = 400
	subNoticeNoteLimit    = 120
)

// composeSubNotice собирает HTML уведомления подписчика: сработавшее слово,
// кто написал (ссылкой на профиль), под чьей заметкой и выдержка текста.
// Раньше уходила строка «Новый комментарий…» с одной ссылкой — по ней нельзя
// было понять ни автора, ни повод. Порт tgx.ComposeSubNotice.
func composeSubNotice(keyword string, n store.Note, c store.Comment, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 Ключевое слово <b>%s</b>\n\n", html.EscapeString(keyword))

	author := html.EscapeString(c.AuthorName)
	if c.AuthorAge != "" {
		author += ", " + html.EscapeString(c.AuthorAge)
	}
	if c.AuthorLink != "" {
		author = fmt.Sprintf(`<a href="%s">%s</a>`, c.AuthorLink, author)
	}
	fmt.Fprintf(&b, "<b>%s</b> в заметке <i>%s</i>", author,
		html.EscapeString(truncateRunes(oneLine(n.AuthorName+": "+n.Text), subNoticeNoteLimit)))
	if !c.PublishedAt.IsZero() {
		fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("02.01 15:04"))
	}
	b.WriteString(":\n")
	b.WriteString(html.EscapeString(truncateRunes(c.Text, subNoticeCommentLimit)))
	fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть комментарий</a>", link)
	return b.String()
}

// oneLine сводит текст в одну строку (заметка упоминается одной строкой).
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes режет текст по границе руны, добавляя многоточие.
func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit])) + "…"
	}
	return s
}
