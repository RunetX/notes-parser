package love

import (
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Все селекторы вёрстки сайта — в одном месте: при дрейфе вёрстки
// правки локализуются здесь и в testdata-фикстурах.
const (
	selNoteItem      = ".lv-notes__note-item"
	selCommentLink   = ".lv-notes__comment-link" // attr name = id заметки
	selNickname      = ".lv-people__nickname"
	selNoteText      = ".lv-notes__note-text"
	selNoteAuthorPic = ".lv-notes__note-author .avatar" // src аватара автора заметки
	selNoteImage     = ".note_images a"                 // href — полноразмерная иллюстрация
	selCommentItem   = ".lv-note__comment-item"
	selCommentID     = "a[id^=anchor-]" // id вида anchor-63167742; в Python это же
	// резал магический [7:] — len("anchor-") == 7
	selAvatar      = ".avatar" // src; alt = "Имя, N лет"
	selCommentDate = ".lv-comment__pubdate"
	selCommentText = ".lv-comment__text"
	// Эмодзи сайт хранит текстом, а показывает картинкой; сам знак стоит в alt.
	// Селектор НЕОБЯЗАТЕЛЬНЫЙ: реплика без эмодзи — обычное дело, и ноль
	// совпадений здесь не дрейф вёрстки (см. bodyText).
	selEmoji = "img.emojione"

	// Шапка заметки на её же странице комментариев (/notes/comments/<id>/).
	// Классы отличаются от ленты — lv-note__ вместо lv-notes__. Это
	// единственный источник текста/автора для старых заметок, выпавших из
	// ленты (limit~5).
	selNotePageItem = ".lv-note__note-item"
	selNotePageText = ".lv-note__note-text"
	selNotePageDate = ".lv-notes__note-date"
	selNoteComments = ".lv-note__comments" // блок списка комментариев (для признака заморозки)
	// selCommentsCount — счётчик комментариев в шапке списка на самой
	// странице треда («Комментарии 325»). Считает ВЕСЬ тред, а страница
	// отдаёт последние 30, поэтому равенства с числом разобранных элементов
	// не бывает — счётчик отвечает ровно на один вопрос: «ноль тут законный
	// или источник замолчал».
	selCommentsCount = ".lv-note__comments-count"

	// Список гостей своей анкеты (/guests/page~N/limit~24/, только под своей
	// сессией). Ярлык у времени — «Был:/Была:», но это ВРЕМЯ ВИЗИТА, а не
	// последний выход человека на сайт: у гостей находятся комментарии,
	// написанные позже метки (проверено 13.08.2026, у Axeinos на пять суток).
	// Разметка просто переиспользует карточку из общих списков.
	selGuestItem = ".lv-people__item"
	selGuestLink = ".lv-people__link"         // атрибут data-userid
	selGuestNick = ".lv-user-info__nick_main" // атрибут title — ник
	selGuestTime = ".lv-people__time"         // «Был: вчера в 19:38»

	// attrParentComment — id родительского комментария; непусто только в
	// древовидном виде (?view=tree), в линейном всегда "".
	attrParentComment = "data-parent-comment-id"
)

// commentAnchorPrefix — префикс id якоря комментария.
const commentAnchorPrefix = "anchor-"

// commentsClosedMarker — текст, которым сайт помечает заметку, закрытую для
// новых комментариев («не актуальна»), в ссылке-счётчике ленты вместо обычного
// «Комментарии». На самой странице комментариев то же состояние сайт печатает
// СЕРВЕРОМ и другими словами — commentsForbiddenMarker ниже.
//
// Запрет при этом живёт ТОЛЬКО в отрисовке: форму ответа сайт не рисует, а
// POST в /notes/comments/<id> принимает — проверено концом в конец 25.08.2026:
// ответ из мессенджера встал в тред закрытой заметки и вернулся оттуда обычным
// обходом зеркала (одного 200 в доказательства не годится, см. drainOK в post.go).
// Поэтому признак справочный, гейта на него нет ни в bridge, ни в pulpit, ни у
// `account say`, и заводить его решено НЕ: отметка стоит у 62 % заметок зеркала.
// Признак живёт в одном месте; сменят формулировку — заметка просто не
// архивируется досрочно и уйдёт в архив по недельному правилу (мягкая деградация).
const commentsClosedMarker = "не актуальна"

// commentsForbiddenMarker — текст на самой странице комментариев для заметки,
// закрытой для новых ответов. В ленте тот же смысл несёт commentsClosedMarker
// («не актуальна»), но на странице комментариев формулировка другая.
const commentsForbiddenMarker = "Комментарии запрещены"

// noteDeletedRe — страница снесённой заметки. Сайт отвечает на неё обычным
// 200 и целым каркасом, в котором вместо заметки одна фраза «Заметка 313038
// удалена.»; ни блока заметки, ни счётчика комментариев там нет. Без этого
// признака удаление неотличимо от дрейфа вёрстки: демон бесконечно опрашивает
// мёртвый адрес и на каждом такте пишет «шапка не разобрана».
var noteDeletedRe = regexp.MustCompile(`Заметка\s+\d+\s+удалена`)

// ErrNoteDeleted — заметку снесли на сайте. Для обходов это рабочий случай, а
// не сбой: опрос такого адреса надо прекращать, а не тревожить админа. Отдельно
// от ErrNotFound, потому что сайт отдаёт не 404, а 200 со страницей-заглушкой.
var ErrNoteDeleted = errors.New("заметка удалена на сайте")

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
	n.Text = bodyText(text)

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

	return parseCommentsDoc(doc, baseURL)
}

// parseCommentsDoc — та же работа по готовому DOM: страницу комментариев
// разбирают дважды (сами комментарии и шапка заметки), и строить дерево два
// раза на каждом такте опроса каждой заметки незачем.
func parseCommentsDoc(doc *goquery.Document, baseURL string) ([]Comment, error) {
	var comments []Comment
	var parseErr error
	doc.Find(selCommentItem).EachWithBreak(func(i int, s *goquery.Selection) bool {
		c, err := parseCommentItem(s, i, baseURL)
		if err != nil {
			parseErr = err
			return false
		}
		comments = append(comments, c)
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	// Пустой тред законен (свежая заметка), а вот «счётчик говорит N, а
	// элементов ноль» — это молчащий источник: так выглядит и дрейф классов, и
	// переезд комментариев на клиентский рендер (17.08.2026 сайт стал отдавать
	// каркас с пустым lv-note__comments-list). Без этой проверки поломка
	// неотличима от пустой заметки и обнаруживается по жалобам людей.
	if len(comments) == 0 {
		if n, ok := commentsCount(doc); ok && n > 0 {
			return nil, &MarkupError{
				Selector: selCommentItem,
				Context:  fmt.Sprintf("счётчик обещает %d комментариев, разобрано ноль", n),
			}
		}
	}
	return comments, nil
}

// commentsCount читает счётчик комментариев треда из шапки списка. Второй
// результат false — счётчика на странице нет вовсе: тогда утверждать нечего,
// и молчаливый ноль остаётся законным (сам счётчик тоже могут увезти на
// клиент, и придумывать по его отсутствию поломку значило бы менять тихий
// отказ на ложную тревогу).
func commentsCount(doc *goquery.Document) (int, bool) {
	sel := doc.Find(selCommentsCount).First()
	if sel.Length() == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(digitsOf(sel.Text()))
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseCommentItem разбирает один элемент списка комментариев. Любой
// отсутствующий обязательный кусок — MarkupError (дрейф вёрстки), и обход
// останавливается: разбирать остаток страницы по сломанным селекторам смысла
// нет.
func parseCommentItem(s *goquery.Selection, i int, baseURL string) (Comment, error) {
	ctx := fmt.Sprintf("комментарий #%d на странице", i)

	anchor := s.Find(selCommentID).First()
	idAttr, ok := anchor.Attr("id")
	if !ok {
		return Comment{}, &MarkupError{Selector: selCommentID, Context: ctx}
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(idAttr, commentAnchorPrefix), 10, 64)
	if err != nil {
		return Comment{}, &MarkupError{Selector: selCommentID, Context: ctx + ": id=" + idAttr}
	}
	ctx = fmt.Sprintf("комментарий %d", id)

	// parent_id есть только в древовидном виде; пустой/непарсибельный —
	// корень заметки (0), это не ошибка вёрстки.
	var parentID int64
	if p, ok := anchor.Attr(attrParentComment); ok {
		if p = strings.TrimSpace(p); p != "" {
			parentID, _ = strconv.ParseInt(p, 10, 64)
		}
	}

	href, ok := s.Find(selNickname).First().Attr("href")
	if !ok {
		return Comment{}, &MarkupError{Selector: selNickname, Context: ctx}
	}

	avatar := s.Find(selAvatar).First()
	src, okSrc := avatar.Attr("src")
	alt, okAlt := avatar.Attr("alt")
	if !okSrc || !okAlt {
		return Comment{}, &MarkupError{Selector: selAvatar, Context: ctx}
	}
	name, age := splitNameAge(alt)

	dateText := strings.TrimSpace(s.Find(selCommentDate).First().Text())
	published, err := time.ParseInLocation(dateLayout, dateText, nsk)
	if err != nil {
		return Comment{}, &MarkupError{Selector: selCommentDate, Context: ctx + ": " + dateText}
	}

	text := s.Find(selCommentText).First()
	if text.Length() == 0 {
		return Comment{}, &MarkupError{Selector: selCommentText, Context: ctx}
	}

	return Comment{
		ID:          id,
		ParentID:    parentID,
		AuthorID:    digitsOf(href),
		AuthorName:  name,
		AuthorAge:   age,
		AuthorLink:  absolutize(baseURL, href),
		AvatarURL:   absolutize(baseURL, src),
		PublishedAt: published,
		Text:        bodyText(text),
	}, nil
}

// ParseNoteFromCommentsPage разбирает шапку самой заметки на её странице
// комментариев. В отличие от ленты (limit~5), эта страница доступна и для
// старых заметок, выпавших из ленты, — единственный способ получить их текст,
// автора и дату. Поле ID не заполняется (id знает вызывающий — это аргумент
// граббера); картинки абсолютизируются. Отсутствие блока/текста — дрейф вёрстки.
func ParseNoteFromCommentsPage(r io.Reader, baseURL string) (Note, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return Note{}, fmt.Errorf("разбор HTML комментариев: %w", err)
	}
	return parseNoteDoc(doc, baseURL)
}

// ParseCommentsPage разбирает страницу комментариев целиком по ОДНОМУ дереву:
// и сами комментарии, и шапку заметки. Отдельные ParseComments и
// ParseNoteFromCommentsPage строили DOM каждая своим, а страница опрашивается
// на каждом такте каждой живой заметки. Ошибка шапки не валит комментарии:
// шапка нужна зеркалу только для необязательных обновлений (свежие
// иллюстрации, «комментарии запрещены»), и её дрейф не должен останавливать
// зеркалирование — возвращается nil.
func ParseCommentsPage(r io.Reader, baseURL string) (CommentsPage, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return CommentsPage{}, fmt.Errorf("разбор HTML комментариев: %w", err)
	}
	comments, err := parseCommentsDoc(doc, baseURL)
	if err != nil {
		return CommentsPage{}, err
	}
	page := CommentsPage{Comments: comments}
	page.Total, _ = commentsCount(doc)
	n, err := parseNoteDoc(doc, baseURL)
	if err != nil {
		// Дрейф шапки — не повод ронять комментарии, а вот удаление заметки
		// касается вызывающего напрямую: опрашивать этот адрес больше незачем.
		if errors.Is(err, ErrNoteDeleted) {
			return CommentsPage{}, err
		}
		return page, nil
	}
	page.Note = &n
	return page, nil
}

func parseNoteDoc(doc *goquery.Document, baseURL string) (Note, error) {
	item := doc.Find(selNotePageItem).First()
	if item.Length() == 0 {
		if noteDeletedRe.MatchString(doc.Text()) {
			return Note{}, ErrNoteDeleted
		}
		return Note{}, &MarkupError{Selector: selNotePageItem, Context: "шапка заметки на странице комментариев"}
	}

	n := Note{AuthorID: "0", AuthorName: "Анонимно"}
	if nick := item.Find(selNickname).First(); nick.Length() > 0 {
		n.AuthorName = strings.TrimSpace(nick.Text())
		if href, ok := nick.Attr("href"); ok {
			n.AuthorID = digitsOf(href)
		}
	}

	text := item.Find(selNotePageText).First()
	if text.Length() == 0 {
		return Note{}, &MarkupError{Selector: selNotePageText, Context: "текст заметки на странице комментариев"}
	}
	n.Text = bodyText(text)

	if src, ok := item.Find(selNoteAuthorPic).First().Attr("src"); ok {
		n.AuthorAvatarURL = strings.TrimSpace(src)
	}
	item.Find(selNoteImage).Each(func(_ int, a *goquery.Selection) {
		if href, ok := a.Attr("href"); ok {
			if href = strings.TrimSpace(href); href != "" {
				n.Images = append(n.Images, absolutize(baseURL, href))
			}
		}
	})
	if dateText := strings.TrimSpace(item.Find(selNotePageDate).First().Text()); dateText != "" {
		if t, err := time.ParseInLocation(dateLayout, dateText, nsk); err == nil {
			n.PublishedAt = t
		}
	}
	// «Комментарии запрещены» — прямой текстовый узел блока списка (не внутри
	// самих комментариев), поэтому берём только собственный текст контейнера,
	// чтобы не поймать эту фразу в чьём-то комментарии.
	n.CommentsClosed = strings.Contains(ownText(doc.Find(selNoteComments).First()), commentsForbiddenMarker)
	return n, nil
}

// bodyText — текст заметки или реплики вместе с эмодзи.
//
// Простой Text() их ТЕРЯЕТ: сайт подменяет эмодзи картинками
// (<img class="emojione" alt="😉">), а у картинки текстового узла нет вовсе.
// Замечено 23.08.2026 на живом треде: ответ, состоявший из одних эмодзи,
// приехал к нам телом «Анна,» — от реплики уцелело одно обращение, — и страница
// площадки нарисовала ник дважды (своё обращение из ребра плюс уцелевшее в
// теле). То есть терялись не «значки»: терялась вся реплика.
//
// Знак берём из alt — там стоит он сам, а не его имя (в src лежит 1F609.png, по
// которому знак пришлось бы собирать из имени файла). Пустой alt пропускаем:
// поставить вместо картинки пустоту мы и так умеем, а вот выдумывать знак — нет.
//
// Возвращается ТЕКСТ, а не разметка: тело у нас всюду плоское, и картинку с
// чужого хоста в него класть незачем — на площадке эмодзи и так рисует шрифт.
func bodyText(s *goquery.Selection) string {
	s.Find(selEmoji).Each(func(_ int, img *goquery.Selection) {
		alt, ok := img.Attr("alt")
		if !ok || alt == "" {
			return
		}
		// ReplaceWithHtml разбирает строку как разметку, поэтому знак
		// экранируется: alt в принципе может содержать «<».
		img.ReplaceWithHtml(html.EscapeString(alt))
	})
	return strings.TrimSpace(s.Text())
}

// ownText возвращает только прямые текстовые узлы выборки, без текста вложенных
// элементов.
func ownText(s *goquery.Selection) string {
	var b strings.Builder
	s.Contents().Each(func(_ int, c *goquery.Selection) {
		if goquery.NodeName(c) == "#text" {
			b.WriteString(c.Text())
		}
	})
	return b.String()
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

// absolutize достраивает относительные ссылки сайта до абсолютных. Схему, кроме
// http(s), отбрасывает: строка приезжает атрибутом чужой вёрстки, а уходит и в
// href поста канала, и в загрузчик медиа — javascript:/data: там не нужны
// никогда. Пустая строка на выходе означает «ссылки нет», и вызывающие это уже
// умеют (ComposeSubNotice, fetchMedia).
func absolutize(baseURL, link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	// Ссылка от корня сайта — включая «//host/path»: она приклеится к базе как
	// путь и останется на сайте, менять это поведение незачем.
	if strings.HasPrefix(link, "/") {
		return strings.TrimSuffix(baseURL, "/") + link
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return link
}
