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
	"unicode"
	"unicode/utf8"

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
	selCalendFolkLink = "a"
	selCalendArticle  = "div.maintext p"
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
	out, err := parseCalend(doc)
	if err != nil {
		return nil, err
	}
	// Приметы лежат не на странице дня, а в СТАТЬЕ народного календаря — это
	// второй запрос в сутки, и он оправдан: примета в утренней заметке
	// каноничная рубрика жанра (замер 24.08.2026 по живой заметке соседа
	// t3h.ru/n/313059), а выдумывать её нельзя ни в коем случае — это
	// непроверяемый факт ровно того сорта, ради которого и заведён пакет.
	// Отказ статьи заметку не отменяет: приметы просто не будет.
	if href := folkArticle(doc); href != "" {
		omens, err := c.fetchOmens(ctx, base, href)
		if err != nil {
			return out, nil
		}
		out = append(out, omens...)
	}
	return out, nil
}

// folkArticle — адрес статьи народного календаря с сегодняшней страницы дня.
func folkArticle(doc *goquery.Document) string {
	href, _ := doc.Find(selCalendFolk).First().Find(selCalendFolkLink).First().Attr("href")
	return href
}

func (c Calend) fetchOmens(ctx context.Context, base, href string) ([]Occasion, error) {
	url := href
	if strings.HasPrefix(href, "/") {
		url = strings.TrimSuffix(base, "/") + href
	}
	doc, err := fetchDoc(ctx, c.Client, url)
	if err != nil {
		return nil, err
	}
	return parseOmens(doc), nil
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

// Приметы народного календаря. Лежат они не списком, а внутри абзаца статьи
// («На Фотю с утра примечали: если выпал иней, значит…»), поэтому берутся
// ПРЕДЛОЖЕНИЯМИ и по признакам самой приметы, а не по вёрстке: жирным календарь
// метит одни, а не другие, и опираться на это нельзя.
//
// Берём КОРОТКИЕ приметы и не больше трёх: в промпт идёт факт, который модель
// перескажет своими словами, — тот же порядок, что у событий истории.
const (
	maxOmens     = 3
	maxOmenRunes = 160
	minOmenRunes = 20
)

// omenPara — по этим словам абзац признаётся «про приметы».
var omenPara = []string{"примечал", "примет", "верили", "считалось"}

// omenSentence — по этим словам предложение признаётся самой приметой, а не
// рассказом вокруг неё.
var omenSentence = []string{"если ", "предвещал", "сулил", "значит", "к дожд", "к ветр", "к морозу"}

func parseOmens(doc *goquery.Document) []Occasion {
	var out []Occasion
	doc.Find(selCalendArticle).EachWithBreak(func(_ int, p *goquery.Selection) bool {
		text := clean(p.Text())
		if !containsAny(strings.ToLower(text), omenPara) {
			return true
		}
		for _, sent := range splitSentences(text) {
			if len(out) >= maxOmens {
				return false
			}
			if o, ok := omenFrom(sent); ok {
				out = append(out, o)
			}
		}
		return len(out) < maxOmens
	})
	return out
}

// omenFrom — примета из предложения ("" — не примета). Зачин «На Фотю с утра
// примечали:» срезается: в заметку он не пойдёт, а модели мешает.
func omenFrom(sent string) (Occasion, bool) {
	if i := strings.Index(sent, ":"); i > 0 && i < len(sent)-1 {
		sent = strings.TrimSpace(sent[i+1:])
	}
	sent = strings.TrimSpace(strings.Trim(sent, "«»\""))
	lower := strings.ToLower(sent)
	if !containsAny(lower, omenSentence) {
		return Occasion{}, false
	}
	n := utf8.RuneCountInString(sent)
	if n < minOmenRunes || n > maxOmenRunes {
		return Occasion{}, false
	}
	sent = strings.ToUpper(string([]rune(sent)[:1])) + string([]rune(sent)[1:])
	return Occasion{Title: sent, Kind: KindOmen, Scope: ScopeWorld, Sources: []string{SourceCalend}}, true
}

// splitSentences — грубое деление на предложения: сокращений вида «т. д.» в
// приметах не бывает, а точка с большой буквы после неё — надёжная граница.
func splitSentences(text string) []string {
	var out []string
	start := 0
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '.' && runes[i] != '!' && runes[i] != '?' {
			continue
		}
		if i+2 < len(runes) && runes[i+1] == ' ' && unicode.IsUpper(runes[i+2]) {
			out = append(out, strings.TrimSpace(string(runes[start:i+1])))
			start = i + 1
		}
	}
	if s := strings.TrimSpace(string(runes[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

func containsAny(lower string, subs []string) bool {
	for _, s := range subs {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
