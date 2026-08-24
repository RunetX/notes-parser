package holidays

// Русская Википедия, страница дня («24 августа»). Второй источник, и берут его
// не ради полноты, а ради СОГЛАСИЯ: повод, названный и здесь, и календарём,
// вернее одиночного.
//
// Разбираем ПО ЗАГОЛОВКАМ, а не по секциям, хотя сегодня страница отдаётся
// разметкой Parsoid, где у каждой секции есть свой `<section>` с готовым
// `aria-labelledby`. Заголовки переживут возврат к прежней вёрстке, а секции —
// нет; цена — десяток строк обхода. Идентификатор заголовка при этом ищется в
// двух местах: у самого `h2`/`h3` (новая разметка) и у вложенного
// `span.mw-headline` (прежняя).
//
// Подзаголовок — ГОТОВАЯ разметка «чей это праздник» («Национальные»,
// «Профессиональные», «Религиозные»), и брать её надо оттуда, а не угадывать по
// словам названия.

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// SourceWiki — имя источника в Occasion.Sources и в конфиге.
const SourceWiki = "wikipedia"

// Идентификаторы разделов страницы дня. Разделы «Родились» и «Скончались» не
// берутся вовсе: чей-то день рождения — не повод дня, а «скончались» тем более.
const (
	headHolidays = "Праздники_и_памятные_дни"
	headEvents   = "События"
)

// wikiScopes — подзаголовок раздела праздников, и список ЗАКРЫТЫЙ: незнакомый
// подзаголовок пропускается целиком. Так из поводов ушли «Именины» — под ними
// лежит поимённый список святых («мученик Гаиан;», «Сусанна Римская, мученица,
// дева»), который разбирается как два десятка отдельных праздников и засоряет
// всё, что ниже (замер 24.08.2026).
var wikiScopes = map[string]Scope{
	"Международные":    ScopeWorld,
	"Всемирные":        ScopeWorld,
	"Общие":            ScopeWorld,
	"Профессиональные": ScopeWorld,
	"Национальные":     ScopeForeign,
	"Религиозные":      ScopeReligious,
}

// wikiMonths — родительный падеж для адреса страницы («24 августа»).
var wikiMonths = [...]string{
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

// reWikiEvent — «79 — извержение…», «1853 — изобретены чипсы». Год до нашей эры
// отбрасывается вместе со строкой: дата дня у такой древности условна, а «в
// этот день» про неё звучит как насмешка над точностью.
var reWikiEvent = regexp.MustCompile(`^(\d{1,4})\s*(до н\.?\s*э\.?)?\s*[—–-]\s*(.+)$`)

// reWikiRef — сноска «[4]», остающаяся после удаления <sup>.
var reWikiRef = regexp.MustCompile(`\[\d+\]`)

// Wiki — источник поверх ru.wikipedia.org.
type Wiki struct {
	BaseURL string // пусто — боевой адрес
	Client  *http.Client
}

func (w Wiki) Name() string { return SourceWiki }

func (w Wiki) Fetch(ctx context.Context, day time.Time) ([]Occasion, error) {
	base := w.BaseURL
	if base == "" {
		base = "https://ru.wikipedia.org"
	}
	page := strconv.Itoa(day.Day()) + "_" + wikiMonths[int(day.Month())-1]
	addr := strings.TrimSuffix(base, "/") + "/wiki/" + url.PathEscape(page) + "?action=render"
	doc, err := fetchDoc(ctx, w.Client, addr)
	if err != nil {
		return nil, err
	}
	return parseWiki(doc)
}

func parseWiki(doc *goquery.Document) ([]Occasion, error) {
	var out []Occasion
	var h2, h3 string
	doc.Find("h2, h3, ul").Each(func(_ int, s *goquery.Selection) {
		switch goquery.NodeName(s) {
		case "h2":
			h2, h3 = headingID(s), ""
			return
		case "h3":
			h3 = headingID(s)
			return
		}
		// Вложенные списки идут вторым заходом — их пункты уже разобраны вместе
		// с родительским.
		if s.ParentsFiltered("ul").Length() > 0 {
			return
		}
		switch h2 {
		case headHolidays:
			s.ChildrenFiltered("li").Each(func(_ int, li *goquery.Selection) {
				if o, ok := wikiHoliday(liText(li), h3); ok {
					out = append(out, o)
				}
			})
		case headEvents:
			s.ChildrenFiltered("li").Each(func(_ int, li *goquery.Selection) {
				if o, ok := wikiEvent(liText(li)); ok {
					out = append(out, o)
				}
			})
		}
	})
	if len(out) == 0 {
		return nil, &MarkupError{Source: SourceWiki, Selector: headHolidays}
	}
	return out, nil
}

// headingID — идентификатор заголовка: сперва свой (нынешняя разметка), потом
// у вложенного span.mw-headline (прежняя).
func headingID(s *goquery.Selection) string {
	if id, ok := s.Attr("id"); ok && id != "" {
		return id
	}
	if id, ok := s.Find("span.mw-headline").First().Attr("id"); ok {
		return id
	}
	return ""
}

// liText — текст пункта без сносок и без служебной вёрстки. Сноска ссылается на
// источник самой Википедии («День внутренних войск[4]»), а <style> шаблонов
// отдаёт .Text() целым листом CSS — на живом дне 24.08.2026 так в повод 1919
// года приехало полкилобайта правил `.mw-parser-output`.
func liText(li *goquery.Selection) string {
	c := li.Clone()
	c.Find("sup, style, script").Remove()
	return reWikiRef.ReplaceAllString(clean(c.Text()), "")
}

// wikiHoliday разбирает «Украина — День независимости.» и «Католицизм: День
// святого Варфоломея.»: слева страна или конфессия, справа само название.
func wikiHoliday(text, sub string) (Occasion, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Occasion{}, false
	}
	prefix, title := splitPrefix(text)
	title = strings.TrimRight(strings.TrimSpace(title), ".")
	if title == "" {
		return Occasion{}, false
	}
	scope, known := wikiScopes[sub]
	if !known {
		return Occasion{}, false
	}
	switch {
	case isRussia(prefix):
		scope = ScopeRussia
	case prefix != "" && scope == ScopeWorld:
		// Подзаголовок обещал общий праздник, но страна названа — значит он
		// всё-таки чужой («Кыргызстан — День внутренних войск» в разделе
		// «Профессиональные»).
		scope = ScopeForeign
	}
	return Occasion{Title: title, Kind: KindHoliday, Scope: scope, Sources: []string{SourceWiki}}, true
}

// splitPrefix отделяет «Страна — » или «Конфессия: » от названия. Разделитель
// ищется только в начале строки (первые несколько слов): тире внутри самого
// названия встречается и значит другое.
func splitPrefix(text string) (prefix, rest string) {
	for _, sep := range []string{" — ", " – ", ": "} {
		if i := strings.Index(text, sep); i > 0 && i <= maxPrefixBytes {
			return strings.TrimSpace(text[:i]), text[i+len(sep):]
		}
	}
	return "", text
}

// maxPrefixBytes — сколько байт от начала строки считаем возможной страной.
// «Соединённые Штаты Америки» в UTF-8 занимают полсотни.
const maxPrefixBytes = 60

func isRussia(prefix string) bool {
	p := strings.ToLower(prefix)
	return p == "россия" || p == "российская федерация" || p == "рф"
}

// wikiEvent разбирает «1853 — изобретены картофельные чипсы».
func wikiEvent(text string) (Occasion, bool) {
	m := reWikiEvent.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil || m[2] != "" { // до нашей эры не берём
		return Occasion{}, false
	}
	year, err := strconv.Atoi(m[1])
	if err != nil {
		return Occasion{}, false
	}
	title := strings.TrimRight(strings.TrimSpace(m[3]), ".")
	if title == "" {
		return Occasion{}, false
	}
	return Occasion{
		Title: title, Kind: KindHistory, Year: year, Scope: ScopeWorld,
		Sources: []string{SourceWiki},
	}, true
}
