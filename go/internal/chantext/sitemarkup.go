package chantext

// Знаки разметки, которыми пишут на площадке, — в разметку мессенджера.
//
// Зачем это здесь. Текст площадки хранится ПЛОСКИМ, а начертания в нём — знаки
// НГС: «[b]жирный[/b]». Морда их разбирает (web/bbcode.go), а приёмники канала
// экранируют строку целиком, поэтому в Telegram и MAX заметка выходила
// скобками — та самая незакрытая работа Ш5ж. Разбор стоит ЗДЕСЬ, в общем
// подмножестве каналов: там уже описано, что мессенджеру можно, и там же
// живут мера длины и обрезка, которой этот HTML потом режут.
//
// Показываем не всё, что понимает морда, а пересечение возможностей обоих
// каналов — <b> и <i>. У [u], [s] и [color] пары в этом подмножестве нет
// (chantext.ValidateHTML их не пропустит), поэтому знаки снимаются, а текст
// остаётся: потерять начертание не страшно, потерять слова — страшно.
//
// Смайлы сайта (:::popcorn:::) остаются кодами: в текст сообщения картинку не
// вставить, а подменять их эмодзи — значит выбирать за автора другой рисунок.

import (
	"html"
	"regexp"
	"strings"
)

// siteTagRe — те же знаки, что разбирает страница площадки. Набор списан с
// web/bbcode.go намеренно: расходиться этим двум разборам нельзя, иначе
// написанное в форме выглядит на странице одним, а в канале другим.
var siteTagRe = regexp.MustCompile(`(?i)\[(/?)(b|i|u|s|color)(?:=(#?[a-z0-9]{1,12}))?\]`)

// siteTagHTML — во что превращается знак. Пусто — начертания в канале нет,
// знак снимается молча.
var siteTagHTML = map[string]string{"b": "b", "i": "i"}

// siteMaxDepth — потолок вложенности. Текст пишут люди, и «[b]» сто раз подряд
// дало бы сто вложенных элементов; глубже потолка теги просто не открываются.
const siteMaxDepth = 8

// FromSiteMarkup экранирует плоский текст площадки и разбирает в нём знаки
// НГС, возвращая HTML, годный для канала (ValidateHTML его принимает).
//
// Разбирает СТЕК, а не замена по регулярке, и причина здесь жёстче, чем у
// страницы: Telegram разбирает разметку сам и на непарный тег отвечает отказом,
// то есть незакрытый <b> — это не «страница поехала», а непринятое сообщение и
// вечная пробка в очереди приёмника. Поэтому перекрёстное «[b][i]…[/b][/i]»
// раскладывается через отложенные теги, а всё оставшееся открытым закрывается
// в конце.
func FromSiteMarkup(text string) string {
	var b strings.Builder
	var st siteMarkupState
	idx := 0
	for _, m := range siteTagRe.FindAllStringSubmatchIndex(text, -1) {
		if t := text[idx:m[0]]; t != "" {
			st.flush(&b)
			b.WriteString(html.EscapeString(t))
		}
		idx = m[1]
		name := strings.ToLower(text[m[4]:m[5]])
		if m[3] > m[2] { // группа «/» непустая — закрывающий
			st.close(&b, name)
			continue
		}
		st.push(&b, name)
	}
	if t := text[idx:]; t != "" {
		st.flush(&b)
		b.WriteString(html.EscapeString(t))
	}
	st.closeAll(&b)
	return b.String()
}

// siteMarkupState — разбор одного текста: что открыто в разметке и что открыто
// по смыслу, но в разметке уже закрыто (перекрёстные теги).
type siteMarkupState struct {
	open []string // имена знаков, открытые сейчас
	pend []string // вернуть перед следующим куском текста
}

// tagOf — HTML-имя знака; пусто у тех, что канал не рисует.
func tagOf(name string) string { return siteTagHTML[name] }

func writeOpen(b *strings.Builder, name string) {
	if t := tagOf(name); t != "" {
		b.WriteString("<" + t + ">")
	}
}

func writeClose(b *strings.Builder, name string) {
	if t := tagOf(name); t != "" {
		b.WriteString("</" + t + ">")
	}
}

// flush возвращает отложенные теги ЛЕНИВО, перед самим текстом: иначе
// «[b][i]x[/b][/i]» оставил бы в сообщении пустую пару <i></i>.
func (s *siteMarkupState) flush(b *strings.Builder) {
	for _, name := range s.pend {
		writeOpen(b, name)
		s.open = append(s.open, name)
	}
	s.pend = nil
}

func (s *siteMarkupState) push(b *strings.Builder, name string) {
	if len(s.open)+len(s.pend) >= siteMaxDepth {
		return
	}
	s.flush(b)
	writeOpen(b, name)
	s.open = append(s.open, name)
}

// close закрывает знак, разбирая перекрёстную разметку: открытое позже
// закрывается тоже — иначе элементы пересекутся, — но не теряется, а уходит в
// отложенные и вернётся перед следующим куском текста.
func (s *siteMarkupState) close(b *strings.Builder, name string) {
	for i := len(s.open) - 1; i >= 0; i-- {
		if s.open[i] != name {
			continue
		}
		back := append([]string(nil), s.open[i+1:]...)
		for j := len(s.open) - 1; j >= i; j-- {
			writeClose(b, s.open[j])
		}
		s.open = s.open[:i]
		s.pend = append(back, s.pend...)
		return
	}
	// Отложенный закрывают, не выводя ничего: в разметке его сейчас нет.
	for i, t := range s.pend {
		if t == name {
			s.pend = append(s.pend[:i:i], s.pend[i+1:]...)
			return
		}
	}
	// Закрывающий без открывающего — обычное дело в живых текстах; показывать
	// его нечем.
}

// closeAll закрывает всё, что осталось открытым: непарный тег мессенджер не
// примет вовсе.
func (s *siteMarkupState) closeAll(b *strings.Builder) {
	for j := len(s.open) - 1; j >= 0; j-- {
		writeClose(b, s.open[j])
	}
	s.open, s.pend = nil, nil
}

// UTF16Len — длина строки в кодовых единицах UTF-16, то есть так, как её
// считают сами мессенджеры (предел Telegram 4096 и предел MAX 4000 наложены
// именно на неё). Отличается от рун только на знаках вне BMP — эмодзи там
// считаются за два, и на них подсчёт по рунам занижает длину вдвое.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// SourceLinkLabel — подпись ссылки на оригинал в подвале поста. Общая на все
// каналы: заметка площадки обязана выглядеть в Telegram и MAX одинаково, а
// подпись — единственное место, где расхождение появилось бы само собой.
const SourceLinkLabel = "Читать на площадке"

// SourceLink — готовая ссылка на оригинал для подвала.
func SourceLink(url string) string {
	return `<a href="` + html.EscapeString(url) + `">` + SourceLinkLabel + `</a>`
}

// ToSiteMarkup — обратный ход: HTML канала в плоский текст со знаками НГС, то
// есть в тот вид, в каком текст хранится на площадке.
//
// Нужен дайджесту: выпуск собирается в подмножестве каналов, а публикуется
// заметкой, и тело заметки — плоский текст. Пара к FromSiteMarkup неполная, и
// это свойство самого места: <b> и <i> переводятся знаками, а СССЫЛКА С
// ПОДПИСЬЮ на площадке невозможна вовсе — своего [url] мы не заводим (знаки
// берём те, что были у сайта). Поэтому «<a href="X">Y</a>» становится «Y — X»:
// подпись сохраняется, адрес остаётся видимым и кликается автоссылкой, а
// текст, у которого подпись и есть адрес, не удваивается.
func ToSiteMarkup(s string) string {
	var b strings.Builder
	var link string // href открытой сейчас ссылки
	var linkText strings.Builder
	out := func(t string) {
		if link != "" {
			linkText.WriteString(t)
		}
		b.WriteString(t)
	}
	idx := 0
	for _, m := range tagRe.FindAllStringSubmatchIndex(s, -1) {
		if t := s[idx:m[0]]; t != "" {
			out(html.UnescapeString(t))
		}
		idx = m[1]
		tag := s[m[0]:m[1]]
		name := strings.ToLower(s[m[2]:m[3]])
		closing := strings.HasPrefix(tag, "</")
		switch {
		case name == "b" || name == "i":
			if closing {
				out("[/" + name + "]")
			} else {
				out("[" + name + "]")
			}
		case name == "a" && !closing:
			if mm := anchorOpenRe.FindStringSubmatch(tag); mm != nil {
				link = html.UnescapeString(mm[1])
				linkText.Reset()
			}
		case name == "a" && closing && link != "":
			if strings.TrimSpace(linkText.String()) != link {
				b.WriteString(" — " + link)
			}
			link = ""
		}
	}
	if t := s[idx:]; t != "" {
		out(html.UnescapeString(t))
	}
	return b.String()
}
