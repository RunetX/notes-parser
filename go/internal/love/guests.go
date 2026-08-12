package love

// Список гостей своей анкеты — единственный известный след действия модератора.
// Наказание он ставит молча, но перед ним открывает анкету, и визит виден
// владельцу: бан 12.08.2026 в 19:38 Нск и визит Гадёныша в 19:38 сошлись
// минута в минуту.
//
// Ограничение сайта, из-за которого это надо СНИМАТЬ, а не читать по случаю:
// строка на человека одна, только последний визит. Пришёл повторно — прежняя
// запись затёрта навсегда, и улика по прошлому разу исчезла.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Guest — запись из списка гостей.
type Guest struct {
	ID        int64     // анкета гостя
	Nick      string    // ник на момент снятия
	VisitedAt time.Time // время визита (Нск); нулевое — формат не разобран
	Raw       string    // исходная строка времени, чтобы не терять нераспознанное
}

// GuestsLimit — сколько записей отдаётся на странице. Порядок сегментов в пути
// строгий: сначала page, потом limit. И `/guests/page~2/` без limit, и
// `/guests/limit~30/page~2/` молча возвращают первую страницу.
const GuestsLimit = 24

// nbsp — сайт разделяет ник, возраст и время неразрывными пробелами.
const nbsp = " "

// Guests загружает страницу списка гостей под сессией владельца анкеты.
// page нумеруется с единицы; за концом списка сайт отдаёт последнюю страницу
// повторно, поэтому обход останавливают по совпадению с уже виденными id.
func (c *Client) Guests(ctx context.Context, cookies []*http.Cookie, page int) ([]Guest, error) {
	if page < 1 {
		page = 1
	}
	path := fmt.Sprintf("/guests/page~%d/limit~%d/", page, GuestsLimit)
	body, err := c.get(ctx, path, cookies...)
	if err != nil {
		return nil, err
	}
	return ParseGuests(bytes.NewReader(body), time.Now().In(nsk))
}

// ParseGuests разбирает список гостей. Пустой список — законная ситуация
// (в анкету никто не заходил), поэтому дрейфом вёрстки он не считается.
func ParseGuests(r io.Reader, now time.Time) ([]Guest, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, fmt.Errorf("разбор HTML гостей: %w", err)
	}
	var out []Guest
	var parseErr error
	doc.Find(selGuestItem).EachWithBreak(func(i int, s *goquery.Selection) bool {
		idAttr, ok := s.Find(selGuestLink).First().Attr("data-userid")
		if !ok {
			parseErr = &MarkupError{Selector: selGuestLink, Context: fmt.Sprintf("гость #%d", i)}
			return false
		}
		id, err := strconv.ParseInt(strings.TrimSpace(idAttr), 10, 64)
		if err != nil {
			parseErr = &MarkupError{Selector: selGuestLink, Context: "нечисловой data-userid " + idAttr}
			return false
		}
		nick, _ := s.Find(selGuestNick).First().Attr("title")
		g := Guest{ID: id, Nick: normalizeSpaces(nick)}
		g.Raw = normalizeSpaces(s.Find(selGuestTime).First().Text())
		g.VisitedAt = parseVisitTime(g.Raw, now)
		out = append(out, g)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return out, nil
}

// ruMonths — родительный падеж, как пишет сайт («7 августа в 16:21»).
var ruMonths = map[string]time.Month{
	"января": time.January, "февраля": time.February, "марта": time.March,
	"апреля": time.April, "мая": time.May, "июня": time.June,
	"июля": time.July, "августа": time.August, "сентября": time.September,
	"октября": time.October, "ноября": time.November, "декабря": time.December,
}

// parseVisitTime разбирает «сегодня в 14:05», «вчера в 23:53», «7 августа в
// 16:21» (ярлык «Был:/Была:» в начале не мешает — он не число и не месяц).
// Года в разметке нет: берём текущий, а дату из будущего относим к прошлому
// году. Нулевое время — формат незнакомый, строка остаётся в Raw.
func parseVisitTime(s string, now time.Time) time.Time {
	fields := strings.Fields(strings.ToLower(normalizeSpaces(s)))
	if len(fields) < 2 {
		return time.Time{}
	}
	t, err := time.Parse("15:04", fields[len(fields)-1])
	if err != nil {
		return time.Time{}
	}
	day, ok := visitDay(fields, now)
	if !ok {
		return time.Time{}
	}
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
}

// visitDay — на какой день указывает строка визита.
func visitDay(fields []string, now time.Time) (time.Time, bool) {
	if contains(fields, "сегодня") {
		return now, true
	}
	if contains(fields, "вчера") {
		return now.AddDate(0, 0, -1), true
	}
	for i, f := range fields { // «7 августа в 16:21»
		mon, ok := ruMonths[f]
		if !ok || i == 0 {
			continue
		}
		num, err := strconv.Atoi(fields[i-1])
		if err != nil {
			continue
		}
		day := time.Date(now.Year(), mon, num, 0, 0, 0, 0, now.Location())
		if day.After(now.AddDate(0, 0, 1)) {
			day = day.AddDate(-1, 0, 0) // список уходит вглубь через Новый год
		}
		return day, true
	}
	return time.Time{}, false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func normalizeSpaces(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, nbsp, " ")), " ")
}
