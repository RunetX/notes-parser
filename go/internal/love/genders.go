package love

// Пол участников со страницы комментариев.
//
// Сайт красит ники по полу (`_female` / `_male` на ссылке ника), и это не
// украшение: в треде на четыре сотни реплик цвет — первое, чем взгляд отделяет
// собеседников. Пол при этом достаётся ДАРОМ, попутно с обходом тредов: он
// стоит прямо в разметке рядом с номером анкеты, и отдельно ходить в каждый
// профиль (как это делает `personas gender`) не нужно — полторы тысячи запросов
// превращаются в ноль.
//
// Разбор намеренно снисходительный: страница без единого ника — не ошибка, а
// заметка без комментариев. Дрейф вёрстки ловится там, где он важен, — в
// parse.go на обязательных селекторах.

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Значения пола те же, что у profile.go (GenderMale / GenderFemale): пол один и
// тот же, и заводить второй словарь ради второго источника незачем.

// nickLinkSelector — ссылка ника. Именно ник, а не аватар: у аватара класс пола
// тоже есть, но такие же ссылки стоят в верхней ленте анкет наверху страницы,
// и по ним пол приехал бы от людей, к заметке отношения не имеющих.
const nickLinkSelector = "a.lv-people__nickname[href^='/profile/']"

// ParseGenders собирает «номер анкеты → пол» со страницы комментариев. В выборку
// попадают и автор заметки, и все комментаторы.
func ParseGenders(r io.Reader) (map[int64]string, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string)
	doc.Find(nickLinkSelector).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		id := profileIDFromHref(href)
		if id == 0 {
			return
		}
		class, _ := s.Attr("class")
		switch {
		case strings.Contains(class, "_female"):
			out[id] = GenderFemale
		case strings.Contains(class, "_male"):
			out[id] = GenderMale
		}
	})
	return out, nil
}

// profileIDFromHref вытаскивает номер анкеты из «/profile/1281493/».
func profileIDFromHref(href string) int64 {
	const prefix = "/profile/"
	if !strings.HasPrefix(href, prefix) {
		return 0
	}
	rest := strings.Trim(href[len(prefix):], "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// FetchGenders загружает страницу комментариев обычной (десктопной) версии и
// возвращает пол всех, чьи ники на ней есть. Клиент нужен ДЕСКТОПНЫЙ — в
// мобильной версии пола в разметке нет вовсе (проверено 18.08.2026).
func (c *Client) FetchGenders(ctx context.Context, noteID string) (map[int64]string, error) {
	body, err := c.get(ctx, "/notes/comments/"+noteID+"/")
	if err != nil {
		return nil, err
	}
	return ParseGenders(bytes.NewReader(body))
}
