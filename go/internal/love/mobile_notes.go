package love

// Дерево ответов с мобильной версии сайта.
//
// Зачем оно нужно. На десктопной странице заметки родитель реплики лежит в
// `data-parent-comment-id` — и это КОРЕНЬ ВЕТКИ, а не та реплика, которой
// отвечают: дерево там ровно двухуровневое. Мобильная версия рендерит
// настоящее дерево вложенными <ul>, глубиной до семи, и её родитель совпадает
// с адресатом обращения «Ник, …» в 92 % против 48 % у десктопной (замер на
// заметке 312870, 248 комментариев).
//
// Почему на неё не переехало зеркало. Мобильная отдаёт тред целиком одной
// страницей, без пагинации, и на длинных ветках это ложится: заметка 312866
// (848 комментариев) воспроизводимо отвечает 500 после минуты ожидания —
// как, впрочем, и десктопный ?view=tree. Живой опрос раз в 30 секунд на таком
// источнике держаться не может, поэтому дерево снимается отдельным проходом
// обогащения уже выкачанного архива. Второе препятствие для зеркала: даты
// здесь относительные («Сегодня в 11:43»), тогда как десктоп отдаёт
// абсолютные. Обогащению это безразлично — оно берёт только пары id.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Селекторы мобильной версии. Отдельный блок от десктопных: вёрстка своя
// (префикс lvmb-), и дрейфовать они будут независимо.
const (
	// selMobileComment — элемент комментария; id вида notes-comment-63207290.
	// Родитель реплики — ближайший такой предок, отдельного атрибута нет.
	selMobileComment = `li[id^="notes-comment-"]`
	// selMobileNoteText — текст заметки (и каждого комментария): опора «это
	// вообще страница заметки», а не заглушка или редирект.
	selMobileNoteText = ".lvmb-notes__note-text"

	mobileCommentIDPrefix = "notes-comment-"
)

// mobileNotePathFmt — страница заметки мобильной версии. Слэш на конце
// обязателен: без него сайт отвечает 301.
const mobileNotePathFmt = "/notes/%s/"

// FetchNoteReplyTree загружает страницу заметки мобильной версии и возвращает
// дерево ответов: id комментария → id того, которому он отвечает (0 — реплика
// верхнего уровня, то есть ответ самой заметке). Клиент должен быть создан с
// базой MobileBaseURL и мобильным User-Agent — с десктопным сайт уводит
// редиректом на полную версию.
func (c *Client) FetchNoteReplyTree(ctx context.Context, noteID string) (map[int64]int64, error) {
	body, err := c.get(ctx, fmt.Sprintf(mobileNotePathFmt, noteID))
	if err != nil {
		return nil, err
	}
	return ParseMobileReplyTree(bytes.NewReader(body), noteID)
}

// ParseMobileReplyTree разбирает дерево ответов со страницы заметки мобильной
// версии. Пустое дерево — законная ситуация (заметка без комментариев);
// страница без единого текста заметки — дрейф вёрстки (MarkupError).
//
// Список комментариев на странице лежит ДВАЖДЫ: один раз обычной разметкой в
// теле, второй — экранированной строкой внутри JS (<li…). Разбор идёт
// через goquery, поэтому вторая копия остаётся текстом скрипта и в выборку не
// попадает; на случай, если сайт это изменит, победитель — первое вхождение
// (копии структурно одинаковы).
func ParseMobileReplyTree(r io.Reader, noteID string) (map[int64]int64, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	if doc.Find(selMobileNoteText).Length() == 0 {
		return nil, &MarkupError{
			Selector: selMobileNoteText,
			Context:  "мобильная страница заметки " + noteID,
		}
	}

	tree := make(map[int64]int64)
	var parseErr error
	doc.Find(selMobileComment).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		id, ok := mobileCommentID(s)
		if !ok {
			parseErr = &MarkupError{
				Selector: selMobileComment,
				Context:  "мобильная заметка " + noteID + ": нечисловой id комментария",
			}
			return false
		}
		if _, seen := tree[id]; seen {
			return true
		}
		// Родителем становится ближайший предок-комментарий; его отсутствие —
		// реплика верхнего уровня.
		var parent int64
		if p := s.ParentsFiltered(selMobileComment).First(); p.Length() > 0 {
			parent, _ = mobileCommentID(p)
		}
		tree[id] = parent
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return tree, nil
}

// mobileCommentID достаёт числовой id из атрибута id="notes-comment-<n>".
func mobileCommentID(s *goquery.Selection) (int64, bool) {
	raw, ok := s.Attr("id")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(raw, mobileCommentIDPrefix), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
