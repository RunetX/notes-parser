package holidays

// calend.ru — «Календарь событий». Даёт на день праздники со страной, народный
// календарь и «Хронику» знаменательных событий с годами.
//
// robots.txt сайта (проверен 24.08.2026) запрещает адреса с параметрами, поиск
// и личные кабинеты; страница дня `/day/<год>-<месяц>-<число>/` разрешена.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// SourceCalend — имя источника в Occasion.Sources и в конфиге.
const SourceCalend = "calend.ru"

// Селекторы разметки calend.ru — одним блоком, как у парсера самого сайта
// (`love/parse.go`): дрейф чужой вёрстки чинится в одном месте, а не по всему
// файлу.
//
// `:not(.famous-date)` обязателен: тем же классом `block holidays` размечены
// блоки «Ближайшие дни компаний» и «Ближайшие дни городов», а это БУДУЩИЕ даты,
// не сегодняшние. Без этого в утро 24 августа приезжает День города, до
// которого ещё неделя.
const (
	selCalendHolidays = "div.block.holidays:not(.famous-date) ul.itemsNet > li"
	selCalendAlso     = "div.block.thisDay ul.itemsNet > li"
	selCalendFolk     = "div.block.homiesCal ul.itemsNet > li"
	selCalendHistory  = "div.block.knownDates ul.itemsNet > li"
	selCalendNames    = "div.block.nameDay ul.itemsNet > li"
	selCalendTitle    = ".title"
	selCalendType     = ".caption .link a"
	selCalendYear     = ".caption .year"
)

// calendScopes — по адресу рубрики видно, чей это праздник. Адрес, а не
// подпись: подпись календарь однажды перепишет, а рубрика — часть его же
// навигации.
var calendScopes = map[string]Scope{
	"wholeworld": ScopeWorld,
	"russtate":   ScopeRussia,
	"katolic":    ScopeReligious,
	"orthodox":   ScopeReligious,
	"pravoslav":  ScopeReligious,
	"muslim":     ScopeReligious,
	"jewish":     ScopeReligious,
	"buddhism":   ScopeReligious,
	"buddhist":   ScopeReligious,
}

// Calend — источник поверх calend.ru.
type Calend struct {
	BaseURL string // пусто — боевой адрес
	Client  *http.Client
}

func (c Calend) Name() string { return SourceCalend }

func (c Calend) Fetch(ctx context.Context, day time.Time) ([]Occasion, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://www.calend.ru"
	}
	url := strings.TrimSuffix(base, "/") +
		"/day/" + strconv.Itoa(day.Year()) + "-" +
		strconv.Itoa(int(day.Month())) + "-" + strconv.Itoa(day.Day()) + "/"
	doc, err := fetchDoc(ctx, c.Client, url)
	if err != nil {
		return nil, err
	}
	return parseCalend(doc)
}

func parseCalend(doc *goquery.Document) ([]Occasion, error) {
	var out []Occasion
	// Праздники и «А также в этот день» размечены одинаково и различаются
	// только блоком: во втором лежат памятные и церковные даты, и Scope у них
	// приезжает из той же рубрики.
	for _, sel := range []string{selCalendHolidays, selCalendAlso} {
		doc.Find(sel).Each(func(_ int, li *goquery.Selection) {
			title := clean(li.Find(selCalendTitle).First().Text())
			if title == "" {
				return
			}
			out = append(out, Occasion{
				Title:   title,
				Kind:    KindHoliday,
				Scope:   calendScope(li),
				Sources: []string{SourceCalend},
			})
		})
	}
	doc.Find(selCalendFolk).Each(func(_ int, li *goquery.Selection) {
		title := clean(li.Find(selCalendTitle).First().Text())
		if title == "" {
			return
		}
		out = append(out, Occasion{
			Title: title, Kind: KindFolk, Scope: ScopeWorld,
			Sources: []string{SourceCalend},
		})
	})
	doc.Find(selCalendHistory).Each(func(_ int, li *goquery.Selection) {
		title := clean(li.Find(selCalendTitle).First().Text())
		if title == "" {
			return
		}
		year, _ := strconv.Atoi(clean(li.Find(selCalendYear).First().Text()))
		out = append(out, Occasion{
			Title: title, Kind: KindHistory, Year: year, Scope: ScopeWorld,
			Sources: []string{SourceCalend},
		})
	})
	// Именины — ОДНА строка на день, а не повод на каждое имя: их бывает под
	// два десятка (25 августа — семнадцать), и списком они вытеснили бы из
	// промпта всё остальное. Имена берём без толкований («Александр —
	// „защитник людей"»): толкование это авторский текст календаря, а правило
	// пакета — только названия (см. шапку holidays.go).
	var names []string
	doc.Find(selCalendNames).Each(func(_ int, li *goquery.Selection) {
		if name := clean(li.Find(selCalendTitle).First().Text()); name != "" {
			names = append(names, name)
		}
	})
	if len(names) > 0 {
		out = append(out, Occasion{
			Title: strings.Join(names, ", "), Kind: KindName, Scope: ScopeWorld,
			Sources: []string{SourceCalend},
		})
	}
	if len(out) == 0 {
		// Пустого дня у календаря не бывает: что-нибудь есть всегда, хоть
		// народный календарь. Ноль — это дрейф вёрстки, и молчать о нём нельзя,
		// иначе источник тихо выключится навсегда.
		return nil, &MarkupError{Source: SourceCalend, Selector: selCalendHolidays}
	}
	return out, nil
}

// calendScope — чей праздник, по адресу рубрики под названием. Рубрики нет
// (народный календарь) или она незнакомая — считаем чужой государственной:
// пропустить лишнее дешевле, чем поставить в утро чужую дату.
func calendScope(li *goquery.Selection) Scope {
	href, ok := li.Find(selCalendType).First().Attr("href")
	if !ok {
		return ScopeWorld
	}
	for slug, scope := range calendScopes {
		if strings.Contains(href, "/"+slug) {
			return scope
		}
	}
	return ScopeForeign
}
