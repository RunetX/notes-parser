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
	"lovegw/internal/subnotice"
	"lovegw/internal/textutil"
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
// личной переписки) — там разметку ставили не мы и рвать её нельзя. Алгоритм
// общий (chantext.FitMeasured), от MAX здесь только его мера длины.
func fitHTML(s string) (string, bool) {
	return chantext.FitMeasured(s, messageBudget, apiLen)
}

// Потолок видимых полей автора в шапке: имя и возраст приходят с сайта, и без
// потолка непомерно длинный ник съел бы весь бюджет сообщения.
const authorFieldLimit = 100

// ComposeNoteMessage собирает HTML-текст поста заметки. Текст заметки сайтом не
// ограничен, поэтому укладывается в бюджет MAX; шапка и подпись в бюджет
// заложены.
func ComposeNoteMessage(baseURL, signature string, n store.Note) string {
	name := html.EscapeString(textutil.TruncateTrim(n.AuthorName, authorFieldLimit))
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
	name := html.EscapeString(textutil.TruncateTrim(c.AuthorName, authorFieldLimit))
	// Возраст пустой у реплики, написанной на площадке: анкетных полей она не
	// заводит вовсе. Без этой ветки шапка вышла бы «Ник, :».
	inner := name + ":"
	if c.AuthorAge != "" {
		inner = fmt.Sprintf("%s, %s:", name,
			html.EscapeString(textutil.TruncateTrim(c.AuthorAge, authorFieldLimit)))
	}
	if c.AuthorLink != "" {
		inner = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(c.AuthorLink), inner)
	}
	head := "<b>" + inner + "</b>\n"
	return head + escapeFit(c.Text, messageBudget-apiLen(head))
}

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
// ComposeSubNotice собирает HTML уведомления подписчика (общий композер —
// subnotice.Compose; здесь только подпись ссылки: в MAX она ведёт прямо на
// сообщение комментария).
func ComposeSubNotice(reason string, n store.Note, c store.Comment, link string) string {
	return subnotice.Compose(reason, n, c, link, "Открыть комментарий")
}
