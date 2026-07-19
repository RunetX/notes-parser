package love

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Все селекторы вёрстки сайта — в одном месте: при дрейфе вёрстки
// правки локализуются здесь и в testdata-фикстурах.
const (
	selNoteItem        = ".lv-notes__note-item"
	selCommentLink     = ".lv-notes__comment-link"       // attr name = id заметки
	selNickname        = ".lv-people__nickname"
	selNoteText        = ".lv-notes__note-text"
	selNoteAuthorPic   = ".lv-notes__note-author .avatar" // src аватара автора заметки
	selNoteImage       = ".note_images a"                 // href — полноразмерная иллюстрация
	selCommentItem = ".lv-note__comment-item"
	selCommentID   = "a[id^=anchor-]" // id вида anchor-63167742; в Python это же
	                                  // резал магический [7:] — len("anchor-") == 7
	selAvatar      = ".avatar"        // src; alt = "Имя, N лет"
	selCommentDate = ".lv-comment__pubdate"
	selCommentText = ".lv-comment__text"
)

// commentAnchorPrefix — префикс id якоря комментария.
const commentAnchorPrefix = "anchor-"

// commentsClosedMarker — текст, которым сайт помечает заметку, закрытую для
// новых комментариев («не актуальна»), в ссылке-счётчике ленты вместо обычного
// «Комментарии». Это единственный признак заморозки в серверном HTML: на самой
// странице комментариев (и в ленте по классам/атрибутам) состояние не отражено
// — баннер и форма ответа рисуются на клиенте JS, а мы скрапим сырой HTML.
// Признак живёт в одном месте; сменят формулировку — заметка просто не
// архивируется досрочно и уйдёт в архив по недельному правилу (мягкая деградация).
const commentsClosedMarker = "не актуальна"

// dateLayout — формат даты комментария, время новосибирское.
const dateLayout = "02.01.2006, 15:04:05"

var nsk = loadNSK()

func loadNSK() *time.Location {
	if loc, err := time.LoadLocation("Asia/Novosibirsk"); err == nil {
		return loc
	}
	return time.FixedZone("NOVT", 7*3600)
}

// ParseNotes разбирает страницу ленты заметок. Пустая лента считается
// дрейфом вёрстки: на сайте заметки есть всегда.
func ParseNotes(r io.Reader) ([]Note, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, fmt.Errorf("разбор HTML ленты: %w", err)
	}
	items := doc.Find(selNoteItem)
	if items.Length() == 0 {
		return nil, &MarkupError{Selector: selNoteItem, Context: "лента заметок"}
	}

	var notes []Note
	var parseErr error
	items.EachWithBreak(func(i int, s *goquery.Selection) bool {
		n, ok, err := parseFeedNote(s)
		if err != nil {
			parseErr = err
			return false
		}
		if ok {
			notes = append(notes, n)
		}
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return notes, nil
}

// parseFeedNote разбирает один элемент ленты. Второй результат false —
// заметку без id пропускаем (паритет с Python-версией), это не ошибка.
func parseFeedNote(s *goquery.Selection) (Note, bool, error) {
	link := s.Find(selCommentLink).First()
	id, ok := link.Attr("name")
	if !ok || id == "" {
		return Note{}, false, nil
	}
	n := Note{
		ID: id, AuthorID: "0", AuthorName: "Анонимно",
		CommentsClosed: strings.Contains(link.Text(), commentsClosedMarker),
	}
	if nick := s.Find(selNickname).First(); nick.Length() > 0 {
		if href, ok := nick.Attr("href"); ok {
			n.AuthorID = digitsOf(href)
			n.AuthorName = strings.TrimSpace(nick.Text())
		}
	}
	text := s.Find(selNoteText).First()
	if text.Length() == 0 {
		return Note{}, false, &MarkupError{Selector: selNoteText, Context: "заметка " + id}
	}
	n.Text = strings.TrimSpace(text.Text())

	if src, ok := s.Find(selNoteAuthorPic).First().Attr("src"); ok {
		n.AuthorAvatarURL = strings.TrimSpace(src)
	}
	s.Find(selNoteImage).Each(func(_ int, a *goquery.Selection) {
		if href, ok := a.Attr("href"); ok {
			if href = strings.TrimSpace(href); href != "" {
				n.Images = append(n.Images, href)
			}
		}
	})
	return n, true, nil
}

// ParseComments разбирает страницу комментариев заметки. Ноль комментариев —
// законная ситуация (свежая заметка), а вот сломанный элемент — дрейф вёрстки.
func ParseComments(r io.Reader, baseURL string) ([]Comment, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, fmt.Errorf("разбор HTML комментариев: %w", err)
	}

	var comments []Comment
	var parseErr error
	doc.Find(selCommentItem).EachWithBreak(func(i int, s *goquery.Selection) bool {
		ctx := fmt.Sprintf("комментарий #%d на странице", i)

		idAttr, ok := s.Find(selCommentID).First().Attr("id")
		if !ok {
			parseErr = &MarkupError{Selector: selCommentID, Context: ctx}
			return false
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(idAttr, commentAnchorPrefix), 10, 64)
		if err != nil {
			parseErr = &MarkupError{Selector: selCommentID, Context: ctx + ": id=" + idAttr}
			return false
		}
		ctx = fmt.Sprintf("комментарий %d", id)

		href, ok := s.Find(selNickname).First().Attr("href")
		if !ok {
			parseErr = &MarkupError{Selector: selNickname, Context: ctx}
			return false
		}

		avatar := s.Find(selAvatar).First()
		src, okSrc := avatar.Attr("src")
		alt, okAlt := avatar.Attr("alt")
		if !okSrc || !okAlt {
			parseErr = &MarkupError{Selector: selAvatar, Context: ctx}
			return false
		}
		name, age := splitNameAge(alt)

		dateText := strings.TrimSpace(s.Find(selCommentDate).First().Text())
		published, err := time.ParseInLocation(dateLayout, dateText, nsk)
		if err != nil {
			parseErr = &MarkupError{Selector: selCommentDate, Context: ctx + ": " + dateText}
			return false
		}

		text := s.Find(selCommentText).First()
		if text.Length() == 0 {
			parseErr = &MarkupError{Selector: selCommentText, Context: ctx}
			return false
		}

		comments = append(comments, Comment{
			ID:          id,
			AuthorName:  name,
			AuthorAge:   age,
			AuthorLink:  absolutize(baseURL, href),
			AvatarURL:   absolutize(baseURL, src),
			PublishedAt: published,
			Text:        strings.TrimSpace(text.Text()),
		})
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	return comments, nil
}

// digitsOf выбирает из строки все цифры: "/anketa376712/" -> "376712".
func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "0"
	}
	return b.String()
}

// splitNameAge делит alt-текст аватара "Имя,Возраст" по последней запятой:
// имя само может содержать запятые.
func splitNameAge(alt string) (name, age string) {
	if i := strings.LastIndex(alt, ","); i >= 0 {
		return strings.TrimSpace(alt[:i]), strings.TrimSpace(alt[i+1:])
	}
	return strings.TrimSpace(alt), ""
}

// absolutize достраивает относительные ссылки сайта до абсолютных.
func absolutize(baseURL, link string) string {
	if strings.HasPrefix(link, "/") {
		return strings.TrimSuffix(baseURL, "/") + link
	}
	return link
}
