package maxx

// Композиция HTML-текстов для MAX. Порт композеров tgx: поддерживаемые MAX
// теги (<b>, <a href>) совпадают с используемыми, поэтому перенос дословный,
// включая экранирование (сырой текст в HTML-режиме — латентный баг
// Python-версии). Отличие от tgx: ветвления «влезает в подпись» нет — текст
// и вложение в MAX живут в одном сообщении.

import (
	"fmt"
	"html"
	"strings"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

// Предел одного сообщения MAX. Меряется ГОТОВАЯ строка вместе с разметкой, а не
// видимый текст: на комментарий с текстом в 4472 знака сервер ответил
// «Field 'text' size (4549) must be at most 4000», а 4549 — ровно длина
// собранного HTML (12 + ссылка 35 + 2 + имя 9 + 2 + возраст 7 + 9 + перевод
// строки + текст). Этим MAX отличается от Telegram, который считает видимую
// длину, — бюджет tgx сюда не переносится.
//
// Цена промаха несимметрична: зеркало шлёт комментарии треда строго по порядку
// и обрывается на первой ошибке, поэтому одно непринятое сообщение — вечная
// пробка (заметка 312886, 06.08.2026: 389 комментариев в Telegram против 18 в
// MAX). Отсюда запас в сто единиц и подсчёт в UTF-16.
const (
	messageLimit  = 4000
	messageBudget = messageLimit - 100
)

// apiLen — длина строки так, как её считает MAX: в кодовых единицах UTF-16.
// Чем именно меряет сервер — рунами или единицами — неизвестно; единиц всегда
// не меньше, поэтому уложившись в них, укладываемся в любом случае. На
// кириллице и латинице обе меры совпадают, расходятся только на эмодзи.
func apiLen(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF { // вне BMP — суррогатная пара
			n++
		}
	}
	return n
}

// escapeFit экранирует сырой текст, укладывая результат в budget единиц. Руна
// добавляется вместе со своим экранированием целиком, поэтому сущность
// (&amp;) не может разорваться пополам — обрезать уже экранированную строку
// было бы нельзя именно по этой причине. Обрезанное помечается многоточием.
func escapeFit(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	esc := html.EscapeString(text)
	if apiLen(esc) <= budget {
		return esc
	}
	const ellipsis = "…"
	room := budget - apiLen(ellipsis)
	var b strings.Builder
	used := 0
	for _, r := range text {
		piece := html.EscapeString(string(r))
		n := apiLen(piece)
		if used+n > room {
			break
		}
		b.WriteString(piece)
		used += n
	}
	// Пробелы в хвосте безопасно снимать: сущность кончается на «;», а не на
	// пробеле, так что разметку это не заденет.
	return strings.TrimRight(b.String(), " \t\n") + ellipsis
}

// fitHTML укладывает в бюджет уже готовый HTML (дайджест, новости, доставка
// личной переписки) — там разметку ставили не мы и рвать её нельзя.
// chantext.Truncate режет по ВИДИМЫМ рунам и сам закрывает теги, а MAX считает
// сырую строку; прямой формулы между этими мерами нет, потому что разметка
// приходится на заранее неизвестные места. Поэтому подбираем видимую длину
// двоичным поиском по фактической длине результата.
//
// Второе значение — признак того, что кусок текста потерян. Для дайджеста это
// не мелочь (выпуск режется по живому), поэтому вызывающий обязан сказать об
// этом в лог: молча терять раздел выпуска хуже, чем шумно.
func fitHTML(s string) (string, bool) {
	if apiLen(s) <= messageBudget {
		return s, false
	}
	lo, hi, best := 0, chantext.VisibleLen(s), ""
	for lo <= hi {
		mid := (lo + hi) / 2
		if cand := chantext.Truncate(s, mid); apiLen(cand) <= messageBudget {
			best, lo = cand, mid+1
		} else {
			hi = mid - 1
		}
	}
	if best == "" {
		// Разметка не оставила места под текст — случай теоретический, но
		// молчать нельзя: пустой текст сервер не примет, и выйдет та же пробка.
		return "…", true
	}
	return best, true
}

// Потолок видимых полей автора в шапке: имя и возраст приходят с сайта, и без
// потолка непомерно длинный ник съел бы весь бюджет сообщения.
const authorFieldLimit = 100

// ComposeNoteMessage собирает HTML-текст поста заметки. Текст заметки сайтом не
// ограничен, поэтому укладывается в бюджет MAX; шапка и подпись в бюджет
// заложены.
func ComposeNoteMessage(baseURL, signature string, n store.Note) string {
	name := html.EscapeString(truncateRunes(n.AuthorName, authorFieldLimit))
	var head strings.Builder
	if n.AuthorID == "" || n.AuthorID == "0" {
		fmt.Fprintf(&head, "<b>%s:</b>\n", name)
	} else {
		fmt.Fprintf(&head, `<b><a href="%s">%s:</a></b>%s`,
			html.EscapeString(baseURL+"/profile/"+n.AuthorID), name, "\n")
	}
	tail := ""
	if signature != "" {
		tail = "\n\n" + signature
	}
	return head.String() + escapeFit(n.Text, messageBudget-apiLen(head.String())-apiLen(tail)) + tail
}

// ComposeCommentMessage собирает HTML комментария с заголовком-ссылкой автора,
// укладывая результат в бюджет MAX: непринятое сообщение остановило бы очередь
// комментариев заметки навсегда.
func ComposeCommentMessage(c store.Comment) string {
	inner := fmt.Sprintf("%s, %s:",
		html.EscapeString(truncateRunes(c.AuthorName, authorFieldLimit)),
		html.EscapeString(truncateRunes(c.AuthorAge, authorFieldLimit)))
	if c.AuthorLink != "" {
		inner = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(c.AuthorLink), inner)
	}
	head := "<b>" + inner + "</b>\n"
	return head + escapeFit(c.Text, messageBudget-apiLen(head))
}

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
const (
	subNoticeCommentLimit = 400
	subNoticeNoteLimit    = 120
)

// ComposeSubNotice собирает HTML уведомления подписчика: повод (reason —
// готовая строка от mirror.SubEvent), кто написал (ссылкой на профиль), под
// чьей заметкой и выдержка текста. Раньше уходила строка «Новый комментарий…»
// с одной ссылкой — по ней нельзя было понять ни автора, ни повод.
// Нулевой комментарий (c.ID == 0) — повод «новая заметка автора»: цитировать
// нечего, показываем саму заметку. Порт tgx.ComposeSubNotice.
func ComposeSubNotice(reason string, n store.Note, c store.Comment, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", html.EscapeString(reason))

	if c.ID == 0 {
		fmt.Fprintf(&b, "<b>%s</b>:\n%s", html.EscapeString(n.AuthorName),
			html.EscapeString(truncateRunes(n.Text, subNoticeCommentLimit)))
		fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть заметку</a>", html.EscapeString(link))
		return b.String()
	}

	author := html.EscapeString(c.AuthorName)
	if c.AuthorAge != "" {
		author += ", " + html.EscapeString(c.AuthorAge)
	}
	if c.AuthorLink != "" {
		author = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(c.AuthorLink), author)
	}
	fmt.Fprintf(&b, "<b>%s</b> в заметке <i>%s</i>", author,
		html.EscapeString(truncateRunes(oneLine(n.AuthorName+": "+n.Text), subNoticeNoteLimit)))
	if !c.PublishedAt.IsZero() {
		fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("02.01 15:04"))
	}
	b.WriteString(":\n")
	b.WriteString(html.EscapeString(truncateRunes(c.Text, subNoticeCommentLimit)))
	fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть комментарий</a>", html.EscapeString(link))
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
