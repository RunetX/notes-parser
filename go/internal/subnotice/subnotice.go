// Пакет subnotice — HTML уведомления подписчику: повод, кто написал, под чьей
// заметкой и выдержка текста. Общий для обоих мессенджеров: поддерживаемое
// подмножество тегов (<b>, <i>, <a href>) у Telegram и MAX совпадает, а
// расходятся они только подписью ссылки — её задаёт вызывающий.
package subnotice

import (
	"fmt"
	"html"
	"strings"

	"lovegw/internal/store"
	"lovegw/internal/textutil"
)

// Пределы выдержек: комментарий показываем щедро, заметку — строкой-подписью.
const (
	commentLimit = 400
	noteLimit    = 120
)

// Compose собирает уведомление. reason — готовая строка повода (её даёт
// mirror.SubEvent), linkLabel — подпись ссылки внизу («Открыть в обсуждении»
// в Telegram, «Открыть комментарий» в MAX). Нулевой комментарий (c.ID == 0) —
// повод «новая заметка автора»: цитировать нечего, показываем саму заметку.
func Compose(reason string, n store.Note, c store.Comment, link, linkLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", html.EscapeString(reason))

	if c.ID == 0 {
		fmt.Fprintf(&b, "<b>%s</b>:\n%s", html.EscapeString(n.AuthorName),
			html.EscapeString(textutil.TruncateTrim(n.Text, commentLimit)))
		fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть заметку</a>", html.EscapeString(link))
		return b.String()
	}

	author := html.EscapeString(c.AuthorName)
	if c.AuthorAge != "" {
		author += ", " + html.EscapeString(c.AuthorAge)
	}
	// Ссылкой на анкету НГС имя было до 27.08.2026: ссылок на НГС проект не
	// ставит нигде (решение владельца). Поле AuthorLink при этом живо — по нему
	// дайджест зеркала опознаёт человека.
	fmt.Fprintf(&b, "<b>%s</b> в заметке <i>%s</i>", author,
		html.EscapeString(textutil.TruncateTrim(textutil.OneLine(n.AuthorName+": "+n.Text), noteLimit)))
	if !c.PublishedAt.IsZero() {
		fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("02.01 15:04"))
	}
	b.WriteString(":\n")
	b.WriteString(html.EscapeString(textutil.TruncateTrim(c.Text, commentLimit)))
	fmt.Fprintf(&b, "\n\n<a href=\"%s\">%s</a>", html.EscapeString(link), html.EscapeString(linkLabel))
	return b.String()
}
