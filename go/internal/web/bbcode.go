package web

// Разметка ранних лет НГС: [b], [i], [u], [s], [color=…].
//
// На самом сайте эти теги давно мертвы и печатаются буквально (ноль на 61 177
// живых комментариев, замер 15.08.2026). Но 18.08.2026 в базу приехал ВЕСЬ
// архив, а там разметка живая — и для ЧУЖОГО текста вопрос не «наш или НГС», а
// «показывал ли её сайт В ТОТ ДЕНЬ». Полоса идентификаторов на это не отвечает:
// у комментария 2016 года id тоже сайта, а «[b]» в нём стоял буквально.
//
// В СВОЁМ тексте (нативная полоса) разметка разбирается всегда — решение
// владельца 21.08.2026. Своего синтаксиса площадка при этом не выдумывает:
// знаки те же, что были на сайте, потому что они у переехавших в пальцах.
//
// Разбирает СТЕК, а не замена по регулярке, и это не педантизм. В живых текстах
// разметка перекрёстная — «[b][i][color=red]…[/color][/b][/i]», так писали
// объявления КПН, — и незакрытая: у «[/color]» в выгрузках 379 закрытий против
// 224 открытий. Замена пары «[b]» → «<b>» оставила бы на такой строке
// незакрытый элемент, а разметку страницы мы собираем строкой: утёкший <b>
// сделал бы жирным всё, что идёт после карточки.

import (
	"html/template"
	"regexp"
	"strings"
	"time"
)

// bbRe — теги разметки. Значение цвета ограничено буквами: сайт понимал и
// «#ff0000», но у нас цвет приезжает КЛАССОМ (CSP без 'unsafe-inline'
// запрещает атрибут style), а класс должен существовать в стилях.
var bbRe = regexp.MustCompile(`(?i)\[(/?)(b|i|u|s|color)(?:=(#?[a-z0-9]{1,12}))?\]`)

// bbColors — цвета, которые площадка умеет показать. Список закрытый и снят
// замером по выгрузкам архива (18.08.2026): red 59, green 49, purple 35,
// blue 35, orange 27, pink 19 — других в текстах нет. Незнакомый цвет не
// ошибка и не повод печатать тег буквально: он просто не красит.
var bbColors = map[string]bool{
	"red": true, "green": true, "blue": true,
	"purple": true, "orange": true, "pink": true,
}

// bbColorNames — те же цвета по-русски и в порядке частоты в архиве: справочник
// под формой набирают глазами, а «pink» в списке кодов ничего не говорит тому,
// кто пришёл написать заметку. Список ОДИН на два дела (разбор и подсказку) —
// разъехавшись, они пообещали бы человеку цвет, которого страница не красит,
// и это стережёт TestMarkupHelpCoversEveryColor.
var bbColorNames = []struct{ Code, Name string }{
	{"red", "красный"},
	{"green", "зелёный"},
	{"purple", "фиолетовый"},
	{"blue", "синий"},
	{"orange", "оранжевый"},
	{"pink", "розовый"},
}

// markupRow — строка справочника разметки под формой: как набирают и что
// выйдет. Образец не написан в шаблоне руками, а ПРОГНАН через тот же разбор,
// каким собирается страница: подсказка, разошедшаяся с поведением, хуже
// отсутствующей — то же правило, что у чисел в справке.
type markupRow struct {
	Code   string
	Sample template.HTML
}

// markupHelp — справочник целиком. Пять строк: четыре начертания и цвет со
// всеми доступными значениями сразу — в тёмных темах они другие, и увидеть их
// человек должен теми же, какими они выйдут у него на экране.
func markupHelp() []markupRow {
	rows := []markupRow{
		{Code: "[b]жирный[/b]"},
		{Code: "[i]курсив[/i]"},
		{Code: "[u]подчёркнутый[/u]"},
		{Code: "[s]зачёркнутый[/s]"},
		{Code: "[color=red]цвет[/color]"},
	}
	for i := range rows[:4] {
		rows[i].Sample = markupSample(rows[i].Code)
	}
	var colors strings.Builder
	for i, c := range bbColorNames {
		if i > 0 {
			colors.WriteString(" ")
		}
		colors.WriteString("[color=" + c.Code + "]" + c.Name + "[/color]")
	}
	rows[4].Sample = markupSample(colors.String())
	return rows
}

// markupSample прогоняет образец тем же разбором, что и тело заметки, но без
// абзаца: строка справочника — это строка списка, а не текст.
func markupSample(text string) template.HTML {
	var b strings.Builder
	st := bbState{era: era{markup: true}}
	writeLines(&b, text, &st)
	st.endParagraph(&b)
	return template.HTML(b.String())
}

// bbMaxDepth — потолок вложенности. Тексты писали люди, и «[b]» сто раз подряд
// дало бы сто вложенных элементов на каждой странице заметки; глубже потолка
// теги просто не открываются.
const bbMaxDepth = 8

// bbSunset — день, когда сайт перестал показывать разметку. Дата не на глаз:
// замер 18.08.2026 по 1 642 652 комментариям и 2 625 заметкам из 56 выгрузок
// досье. Обращение «Для [b][i]Ник[/i][/b]» рисовал САМ сайт, и оно обрывается
// 02.06.2014 в 13:07:44 UTC — с 03.06 идёт «Ник, ». Ровно там же кончается и
// разметка людей: в комментариях 2013–2014 её тысячи, дальше единицы в месяц
// (2015 — 3, 2016 — 2, 2017–2026 — ноль), а в заметках 2010–2014 размечено от
// 6 до 48 % — и ровно ноль начиная с 2015-го. То есть в тот день сайт сменил
// разбор, и с тех пор «[b]» у него просто текст; таким он остаётся и у нас — но
// только в ЕГО текстах: рубеж этот про НГС, а не про написанное здесь.
var bbSunset = time.Date(2014, 6, 3, 0, 0, 0, 0, time.UTC)

type bbTag struct {
	name  string // b, i, u, s, color
	color string // только у color; пусто — тег ничего не рисует
}

func (t bbTag) openTag() string {
	if t.name == "color" {
		if t.color == "" {
			return ""
		}
		return `<span class="bb-` + t.color + `">`
	}
	return "<" + t.name + ">"
}

func (t bbTag) closeTag() string {
	switch {
	case t.name != "color":
		return "</" + t.name + ">"
	case t.color == "":
		return ""
	}
	return "</span>"
}

// bbState — разбор на протяжении ОДНОГО текста.
//
// Состояние переживает границу абзаца: «[b]», открытый в первом абзаце и
// закрытый в третьем, в архиве обычное дело, а <p> в HTML пересекать нельзя.
// Поэтому на границе абзаца открытое закрывается и возвращается в следующем.
type bbState struct {
	era  era     // что из знаков сайта разбирать: у разметки и смайлов рубежи разные
	open []bbTag // открыты сейчас в разметке
	pend []bbTag // открыты по смыслу, в разметке закрыты — вернуть перед выводом
}

// flush возвращает отложенные теги. Возврат ЛЕНИВЫЙ, перед самим текстом:
// иначе «[b][i]x[/b][/i]» оставил бы на странице пустую пару <i></i>.
func (s *bbState) flush(b *strings.Builder) {
	for _, t := range s.pend {
		b.WriteString(t.openTag())
		s.open = append(s.open, t)
	}
	s.pend = nil
}

// line пишет строку текста, разбирая теги. Выключенное состояние — прежний
// путь слово в слово: свои тексты и документы разметки не знают.
func (s *bbState) line(b *strings.Builder, line string) {
	if !s.era.markup {
		writeText(b, line, s.era.smiles, s.era.links)
		return
	}
	idx := 0
	for _, m := range bbRe.FindAllStringSubmatchIndex(line, -1) {
		if txt := line[idx:m[0]]; txt != "" {
			s.flush(b)
			writeText(b, txt, s.era.smiles, s.era.links)
		}
		idx = m[1]
		name := strings.ToLower(line[m[4]:m[5]])
		if m[3] > m[2] { // группа «/» непустая
			s.close(b, name)
			continue
		}
		var color string
		if m[6] >= 0 {
			if c := strings.ToLower(line[m[6]:m[7]]); bbColors[c] {
				color = c
			}
		}
		s.push(b, bbTag{name: name, color: color})
	}
	if txt := line[idx:]; txt != "" {
		s.flush(b)
		writeText(b, txt, s.era.smiles, s.era.links)
	}
}

func (s *bbState) push(b *strings.Builder, t bbTag) {
	if len(s.open)+len(s.pend) >= bbMaxDepth {
		return
	}
	s.flush(b)
	b.WriteString(t.openTag())
	s.open = append(s.open, t)
}

// close закрывает тег, разбирая перекрёстную разметку: всё, что открыто позже,
// закрывается тоже — иначе элементы пересекутся, — но не теряется, а уходит в
// отложенные и вернётся перед следующим куском текста.
func (s *bbState) close(b *strings.Builder, name string) {
	for i := len(s.open) - 1; i >= 0; i-- {
		if s.open[i].name != name {
			continue
		}
		back := append([]bbTag(nil), s.open[i+1:]...)
		for j := len(s.open) - 1; j >= i; j-- {
			b.WriteString(s.open[j].closeTag())
		}
		s.open = s.open[:i]
		s.pend = append(back, s.pend...)
		return
	}
	// Отложенный закрывают, не выводя ничего: в разметке его сейчас нет.
	for i, t := range s.pend {
		if t.name == name {
			s.pend = append(s.pend[:i:i], s.pend[i+1:]...)
			return
		}
	}
	// Закрывающий без открывающего — в архиве обычное дело («[/color]» без
	// «[color=…]» встречается чаще, чем с ним). Показывать его нечем.
}

// endParagraph закрывает разметку на границе абзаца, не теряя смысла: открытое
// уходит в отложенные и вернётся в следующем абзаце. Он же закрывает разметку в
// конце текста — незакрытых элементов на странице не остаётся в любом случае.
func (s *bbState) endParagraph(b *strings.Builder) {
	for j := len(s.open) - 1; j >= 0; j-- {
		b.WriteString(s.open[j].closeTag())
	}
	s.pend = append(append([]bbTag(nil), s.open...), s.pend...)
	s.open = nil
}

// stripMarkup убирает знаки разметки и коды смайлов из текста, который
// показывается ПЛОСКИМ — сегодня это заголовок вкладки. Ни начертания, ни
// картинки там показать нечем, а «[b]Хотелки[/b]» в названии страницы человек
// читает как поломку: на самой странице этих скобок нет (жалоба владельца
// 23.08.2026).
//
// Эпоха решает ровно то же, что и в теле: снимаем там, где страница эти знаки
// РАЗБИРАЕТ. В чужом тексте после заката сайт печатал их буквально, страница
// печатает так же — и заголовок обязан совпадать с ней, иначе вкладка обещает
// один текст, а карточка показывает другой.
//
// Набор знаков тот же (bbRe, smileyRe): второго списка тегов не заводится.
// Незнакомый код смайла остаётся текстом — как оставался на сайте и как
// остаётся в теле.
func stripMarkup(text string, e era) string {
	if e.markup {
		text = bbRe.ReplaceAllString(text, "")
	}
	if e.smiles {
		text = smileyRe.ReplaceAllStringFunc(text, func(m string) string {
			if _, ok := smiles[strings.Trim(m, ":")]; ok {
				return ""
			}
			return m
		})
	}
	return text
}
