package holidays

// Общий поход к чужому календарю.
//
// Темп — один запрос в сутки на источник: заметка выходит раз в день, и
// кэшировать тут нечего. User-Agent честный, со ссылкой на площадку: мы берём
// со страницы факты, а не притворяемся браузером. Проверено 24.08.2026 —
// calend.ru отдаёт страницу и такому агенту (kakoysegodnyaprazdnik.ru при этом
// отвечает 401 и в список источников не попал: сайт, требующий притворства,
// нам не источник).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// UserAgent — как мы представляемся календарям.
const UserAgent = "lovegw/1.0 (+https://t3h.ru; утренняя заметка сообщества)"

// maxPage — потолок читаемой страницы. Календарь — это десятки килобайт;
// мегабайты означают, что мы попали не туда, и грузить их в память незачем.
const maxPage = 4 << 20

func fetchDoc(ctx context.Context, c *http.Client, url string) (*goquery.Document, error) {
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "ru,en;q=0.5")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: код %d", url, resp.StatusCode)
	}
	return goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxPage))
}

// clean приводит вытащенный из разметки текст к одной строке: чужие календари
// щедры на переводы строк и неразрывные пробелы внутри названия.
func clean(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	return strings.Join(strings.Fields(s), " ")
}
