package web

// Превращение хранимого текста в разметку.
//
// Текст лежит в базе ПЛОСКИМ и никогда как HTML — отсюда и отсутствие XSS: мы
// экранируем всё до единого знака, а разметку добавляем сами. Хранимого HTML не
// существует, значит и вычищать нечего; санитайзер, которого нет, нельзя обойти.
//
// Из форматирования — абзацы, переносы, автоссылки и разметка ранних лет НГС
// (bbcode.go). Последняя разбирается только там, где её показывал САМ САЙТ, то
// есть до 02.06.2014: сегодня она на НГС мертва (ноль на 61 177 живых
// комментариев), но в приехавшем архиве живая. Своего синтаксиса площадка
// по-прежнему не заводит — написанное здесь показывается как есть.

import (
	"html"
	"html/template"
	"regexp"
	"strings"
	"time"

	"lovegw/internal/platform"
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

// linkMode — какие адреса в этом тексте становятся ссылками.
//
// Три состояния, а не флаг, потому что вопросов здесь два и они разные: «можно
// ли вообще» и «чьи». Решение владельца 18.08.2026 гасило ЧУЖИЕ адреса (в базе
// 10,7 млн чужих реплик за тринадцать лет, инструмента контроля нет, а домены
// за эти годы меняли владельцев — «ссылка была приличной в 2011-м» сегодня не
// значит ничего), и 22.08.2026 оно уточнено: адреса САМОЙ ПЛОЩАДКИ кликаются
// везде. Своя ссылка никуда не уводит — она ведёт на соседнюю страницу, за
// которую мы отвечаем целиком, — а понадобилась она с выпуском дайджеста:
// сводка недели состоит из ссылок на заметки, и набирать их руками никто не
// станет.
//
// Чужой адрес по-прежнему остаётся ТЕКСТОМ и виден целиком: человек волен
// скопировать его сам, и это осознанное действие вместо случайного клика.
type linkMode int

const (
	linkOwn  linkMode = iota // только адреса площадки (обычные тексты)
	linkAll                  // все (тексты согласий — документ пишем мы сами)
	linkNone                 // никакие
)

// ownLinkPrefix — адрес площадки, по которому и опознаётся «своя» ссылка.
// Ставится один раз при создании сервера (web.New) и дальше только читается:
// сервер в процессе один, а сделать префикс аргументом рендера нечем — шаблоны
// разбираются один раз на процесс с общей FuncMap.
var ownLinkPrefix string

// setOwnLinkPrefix запоминает адрес площадки. Пустой base_url значит «своих
// ссылок не бывает» — тогда кликаться не будет ничего, как до 22.08.2026.
func setOwnLinkPrefix(baseURL string) {
	ownLinkPrefix = strings.TrimSuffix(baseURL, "/")
}

// isOwnLink — ведёт ли адрес на саму площадку. Сравнивается ПРЕФИКС вместе со
// схемой и хостом: «https://t3h.ru.evil.example/» начинается с «https://t3h.ru»
// по буквам, поэтому за префиксом обязан идти конец строки, «/», «?» или «#».
func isOwnLink(addr string) bool {
	if ownLinkPrefix == "" || !strings.HasPrefix(addr, ownLinkPrefix) {
		return false
	}
	rest := addr[len(ownLinkPrefix):]
	return rest == "" || strings.HasPrefix(rest, "/") ||
		strings.HasPrefix(rest, "?") || strings.HasPrefix(rest, "#")
}

// era — что из знаков сайта показывать в этом тексте. Решает ДАТА, а не полоса
// идентификаторов: полоса отделяет «НГС» от «наше», а нужен другой рубеж —
// «сайт это показывал» против «печатал буквально». Рубежей два, и они разные:
// разметка умерла 02.06.2014 (bbSunset), смайлы дожили до сентября 2017
// (smileySunset). Написанное ЗДЕСЬ не попадает ни под один — у него свои
// правила, и они шире: своё мы показываем так, как человек его написал.
type era struct {
	markup, smiles bool
	links          linkMode

	// siteMarkup — разметку в ЭТОМ тексте рисовал сам сайт, а не мы. Отдельный
	// флаг нужен ровно одному месту (commentBodyHTML): обращение образца 2013
	// года «Для [b][i]Ник[/i][/b] текст» — работа САЙТА, и снимать его в своём
	// тексте не за чем. Пока разметка была только у НГС, это различие совпадало
	// с markup; с 21.08.2026 уже нет.
	siteMarkup bool
}

func eraOf(id int64, t time.Time) era {
	// Написанное на площадке разбирается ЦЕЛИКОМ — и смайлы, и разметка.
	//
	// Смайлы площадка предлагает сама (выбиралка под формой), и код в своём
	// тексте обязан стать картинкой, иначе человек нажимает кнопку, а получает
	// «:::popcorn:::». С разметкой 21.08.2026 вышло то же самое, только с
	// другого конца: владелец написал здесь «[b]Хотелки[/b]» и увидел скобки.
	// Знаки эти у переехавших в пальцах — тринадцать лет НГС их показывал, —
	// и заводить ВМЕСТО них свой синтаксис значило бы переучивать людей ради
	// чистоты замысла. Поэтому у площадки та же разметка, что была у сайта:
	// [b], [i], [u], [s], [color=…].
	//
	// Рубежи от этого не отменяются, а становятся правилом про ЧУЖОЙ текст:
	// комментарий 2016 года со скобками так и останется со скобками — сайт
	// печатал их буквально, и человек, читавший ту страницу, видел именно их.
	native := platform.IsNative(id)
	if t.IsZero() {
		return era{markup: native, smiles: native, links: linkOwn}
	}
	site := !native && t.Before(bbSunset)
	return era{
		markup:     native || site,
		smiles:     native || t.Before(smileySunset),
		links:      linkOwn,
		siteMarkup: site,
	}
}

// noteBodyHTML — тело заметки. Пара к commentBodyHTML: разметку (bbcode.go)
// разбираем в своём тексте и в чужом до заката — то есть там, где её показывал
// сам сайт.
func noteBodyHTML(n platform.NoteView) template.HTML {
	return renderBody("", n.Body, eraOf(n.ID, n.PublishedAt))
}

// docHTML — текст согласия как разметка. Отдельно от bodyHTML, потому что
// документ — это документ: у него есть подзаголовки и перечни, и читать его
// сплошной простынёй человек не станет, а прочесть он обязан.
//
// Хранится документ всё равно ПЛОСКИМ текстом: именно эти байты подписаны и
// именно их хеш лежит в consent_docs. Разметка тут — способ показа, и подмена
// «##» на <h2> ничего в подписанном тексте не меняет.
func docHTML(text string) template.HTML {
	var b strings.Builder
	// Документ пишем мы сами: разметки НГС в нём нет, а вот ссылки живые. Гашение
	// адресов (linksClickable) касается ЧУЖОГО текста — согласие с неработающей
	// ссылкой на оператора было бы издевательством над правом, которое оно даёт.
	st := bbState{era: era{links: linkAll}}
	for i, para := range paragraphs(text) {
		if i == 0 {
			// Первая строка файла и есть заголовок документа — тот же, что
			// лежит в ConsentDoc.Title. Рисуем его здесь, чтобы страница не
			// печатала название дважды.
			b.WriteString("<h1>")
			b.WriteString(html.EscapeString(strings.TrimSpace(para)))
			b.WriteString("</h1>")
			continue
		}
		if head, ok := strings.CutPrefix(para, "## "); ok {
			b.WriteString("<h2>")
			b.WriteString(html.EscapeString(strings.TrimSpace(head)))
			b.WriteString("</h2>")
			continue
		}
		b.WriteString("<p>")
		writeLines(&b, para, &st)
		b.WriteString("</p>")
	}
	return template.HTML(b.String())
}

// renderBody собирает абзацы, вставляя обращение внутрь ПЕРВОГО из них.
//
// Обращение «Ник, » — ребро, а не часть тела (в базе его нет), но выглядеть оно
// должно ровно так, как выглядело на сайте: началом первой фразы, а не строкой
// сверху. Отсюда и вставка внутрь абзаца.
func renderBody(prefix template.HTML, text string, e era) template.HTML {
	paras := paragraphs(text)
	if len(paras) == 0 {
		if prefix == "" {
			return ""
		}
		return "<p>" + prefix + "</p>"
	}
	var b strings.Builder
	b.Grow(len(text) + 32*len(paras))
	st := bbState{era: e}
	for i, p := range paras {
		b.WriteString("<p>")
		if i == 0 {
			b.WriteString(string(prefix))
		}
		writeLines(&b, p, &st)
		// Разметка закрывается на границе абзаца и возвращается в следующем:
		// <p> в HTML пересекать нельзя, а «[b]» через абзац в архиве бывает.
		st.endParagraph(&b)
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

func writeLines(b *strings.Builder, para string, st *bbState) {
	for i, line := range strings.Split(para, "\n") {
		if i > 0 {
			b.WriteString("<br>")
		}
		st.line(b, strings.TrimRight(line, " \t"))
	}
}

// writeText экранирует текст, превращает адреса в ссылки, а коды смайлов — в
// картинки. Порядок важен: ссылки ищутся в СЫРОЙ строке, а экранируется каждый
// кусок отдельно — иначе «&amp;» из уже экранированного текста попал бы в href
// как есть. Смайлы разбираются только в кусках ВНЕ ссылки: «:::» внутри адреса
// это часть адреса. Какие адреса вообще становятся ссылками, решает links
// (см. linkMode): чужие остаются текстом, свои кликаются.
func writeText(b *strings.Builder, line string, smiles bool, links linkMode) {
	write := func(s string) {
		if smiles {
			writeSmileys(b, s)
			return
		}
		b.WriteString(html.EscapeString(s))
	}
	if links == linkNone {
		write(line)
		return
	}
	idx := 0
	for _, loc := range linkRe.FindAllStringIndex(line, -1) {
		raw := line[loc[0]:loc[1]]
		addr := strings.TrimRight(raw, linkTailCut)
		if addr == "" || len(addr) < len("http://x") {
			continue
		}
		if links == linkOwn && !isOwnLink(addr) {
			continue // чужой адрес остаётся текстом и виден целиком
		}
		write(line[idx:loc[0]])
		b.WriteString(`<a href="`)
		b.WriteString(html.EscapeString(addr))
		// nofollow noopener ugc — стандартная разметка чужой ссылки, оставленной
		// пользователем: мы за неё не ручаемся и веса ей не передаём.
		b.WriteString(`" rel="nofollow noopener ugc">`)
		b.WriteString(html.EscapeString(textutil.Fit(addr, linkTextLimit)))
		b.WriteString(`</a>`)
		idx = loc[0] + len(addr)
	}
	write(line[idx:])
}
