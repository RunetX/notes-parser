package love

// Лента заметок мобильной версии — источник id для заметок, которых десктопная
// лента НЕ НАЗЫВАЕТ.
//
// Зачем она нужна. Заметке с запрещёнными комментариями десктопная лента не
// рисует ссылку на тред, а id заметки лежит только в ней — элемент остаётся с
// текстом, автором и датой, но без имени (см. Feed). Мобильная версия того же
// раздела устроена иначе: текст заметки там сам по себе ссылка на неё
// (`/notes/<id>`), и рисуется она независимо от того, можно ли в тред писать, —
// значит id есть у КАЖДОЙ заметки ленты. Замер 27.08.2026: десктопная лента
// показала пять заметок и назвала четыре, мобильная назвала все, включая
// 313096.
//
// Дальше этого мобильную ленту не используют: даты там относительные
// («Сегодня в 16:10»), а зеркалу нужны абсолютные, и заметку по найденному id
// оно дочитывает обычной десктопной страницей треда.
//
// Клиент должен быть создан с базой MobileBaseURL и мобильным User-Agent — с
// десктопным сайт уводит редиректом на полную версию.

import (
	"bytes"
	"context"
	"io"

	"github.com/PuerkitoBio/goquery"
)

const (
	// selMobileFeedLink — ссылка с текста заметки на саму заметку
	// (href = /notes/<id>). У части заметок таких ссылок в элементе ДВЕ
	// (замер: 11 ссылок на 8 заметок), поэтому id складываются без повторов.
	selMobileFeedLink = "a.lvmb-notes__note-text-link"

	// mobileNotesPath — лента мобильной версии. Сужения окна `limit~N`, как у
	// десктопной, она не понимает вовсе (отвечает 404), поэтому берётся
	// страница целиком: её заметок (8) с запасом хватает на окно зеркала (5).
	mobileNotesPath = "/notes/"
)

// FetchMobileFeedIDs загружает мобильную ленту и возвращает id её заметок в
// том же порядке, в каком идёт сама лента, — от новых к старым.
func (c *Client) FetchMobileFeedIDs(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx, mobileNotesPath)
	if err != nil {
		return nil, err
	}
	return ParseMobileFeedIDs(bytes.NewReader(body))
}

// ParseMobileFeedIDs разбирает мобильную ленту заметок. Пустая лента — дрейф
// вёрстки: заметки на сайте есть всегда (то же правило, что у ParseFeed).
//
// Список заметок лежит на странице ДВАЖДЫ: обычной разметкой и экранированной
// строкой внутри <script> (мобильная версия отдаёт его же скрипту). Разбор
// идёт через goquery, поэтому вторая копия остаётся текстом скрипта и в
// выборку не попадает — регулярка по href удвоила бы каждый id.
func ParseMobileFeedIDs(r io.Reader) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	var ids []string
	seen := make(map[string]bool)
	doc.Find(selMobileFeedLink).Each(func(_ int, a *goquery.Selection) {
		href, ok := a.Attr("href")
		if !ok {
			return
		}
		id := digitsOf(href)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	})
	if len(ids) == 0 {
		return nil, &MarkupError{Selector: selMobileFeedLink, Context: "мобильная лента заметок"}
	}
	return ids, nil
}
