package web

// Превращение хранимого текста в разметку.
//
// Текст лежит в базе ПЛОСКИМ и никогда как HTML — отсюда и отсутствие XSS: мы
// экранируем всё до единого знака, а разметку добавляем сами. Хранимого HTML не
// существует, значит и вычищать нечего; санитайзер, которого нет, нельзя обойти.
//
// Из форматирования — только абзацы, переносы и автоссылки. BB-коды не
// поддержаны сознательно: на самом НГС они мертвы (ноль на 61 177 живых
// комментариев), а собственного синтаксиса площадка заводить не станет.

import (
	"html"
	"html/template"
	"regexp"
	"strings"

	"lovegw/internal/textutil"
)

// linkRe ловит http/https-ссылки. Схема в шаблоне зашита намеренно: так в
// href физически не может оказаться javascript: — не потому, что мы его
// вычистили, а потому, что распознаём только две схемы.
var linkRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// linkTailCut — знаки, которые в конце ссылки почти всегда принадлежат фразе,
// а не адресу: «см. https://t3h.ru/n/1.» — точка тут не часть ссылки.
const linkTailCut = `.,;:!?…»)]"'`

// linkTextLimit — сколько от адреса показывать. Длинная ссылка в узкой колонке
// растягивает страницу по горизонтали на телефоне.
const linkTextLimit = 60

// bodyHTML — текст заметки или комментария как разметка.
func bodyHTML(text string) template.HTML { return renderBody("", text) }

// renderBody собирает абзацы, вставляя обращение внутрь ПЕРВОГО из них.
//
// Обращение «Ник, » — ребро, а не часть тела (в базе его нет), но выглядеть оно
// должно ровно так, как выглядело на сайте: началом первой фразы, а не строкой
// сверху. Отсюда и вставка внутрь абзаца.
func renderBody(prefix template.HTML, text string) template.HTML {
	paras := paragraphs(text)
	if len(paras) == 0 {
		if prefix == "" {
			return ""
		}
		return "<p>" + prefix + "</p>"
	}
	var b strings.Builder
	b.Grow(len(text) + 32*len(paras))
	for i, p := range paras {
		b.WriteString("<p>")
		if i == 0 {
			b.WriteString(string(prefix))
		}
		writeLines(&b, p)
		b.WriteString("</p>")
	}
	return template.HTML(b.String())
}

// paragraphs делит текст пустыми строками. Разделение сохраняется, потому что
// сайт делает nl2br и люди этим пользуются: пустая строка между абзацами на НГС
// работает, схлопываются только пробелы (замер 15.08.2026 по 61 177 репликам).
func paragraphs(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var (
		out  []string
		cur  []string
		push = func() {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, "\n"))
				cur = nil
			}
		}
	)
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			push()
			continue
		}
		cur = append(cur, line)
	}
	push()
	return out
}

func writeLines(b *strings.Builder, para string) {
	for i, line := range strings.Split(para, "\n") {
		if i > 0 {
			b.WriteString("<br>")
		}
		writeText(b, strings.TrimRight(line, " \t"))
	}
}

// writeText экранирует текст и превращает адреса в ссылки. Порядок важен:
// ссылки ищутся в СЫРОЙ строке, а экранируется каждый кусок отдельно — иначе
// «&amp;» из уже экранированного текста попал бы в href как есть.
func writeText(b *strings.Builder, line string) {
	idx := 0
	for _, loc := range linkRe.FindAllStringIndex(line, -1) {
		raw := line[loc[0]:loc[1]]
		addr := strings.TrimRight(raw, linkTailCut)
		if addr == "" || len(addr) < len("http://x") {
			continue
		}
		b.WriteString(html.EscapeString(line[idx:loc[0]]))
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(addr))
		// nofollow noopener ugc — стандартная разметка чужой ссылки, оставленной
		// пользователем: мы за неё не ручаемся и веса ей не передаём.
		b.WriteString(`" rel="nofollow noopener ugc">`)
		b.WriteString(html.EscapeString(textutil.Fit(addr, linkTextLimit)))
		b.WriteString(`</a>`)
		idx = loc[0] + len(addr)
	}
	b.WriteString(html.EscapeString(line[idx:]))
}
