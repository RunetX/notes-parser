package web

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
	"lovegw/internal/textutil"
)

//go:embed templates
var templateFS embed.FS

// pages — по шаблону на страницу, и каждый со своей копией «базы». Так сделано
// потому, что все файлы, разобранные в один набор, делят пространство имён:
// два {{define "content"}} затёрли бы друг друга молча. Разбор идёт один раз на
// процесс, ошибка в шаблоне — это отказ старта, а не пятисотка в бою.
var pages = mustPages()

func mustPages() map[string]*template.Template {
	names, err := fs.Glob(templateFS, "templates/pages/*.gohtml")
	if err != nil || len(names) == 0 {
		panic("web: шаблоны страниц не найдены")
	}
	out := make(map[string]*template.Template, len(names))
	for _, n := range names {
		out[path.Base(n)] = template.Must(template.New("base.gohtml").Funcs(funcs).
			ParseFS(templateFS, "templates/base.gohtml", "templates/parts/*.gohtml", n))
	}
	return out
}

// tz — пояс показа. Новосибирский, как на самом НГС: время в заметках люди
// сверяют с собственными часами, а не с UTC.
var tz = mustTZ()

func mustTZ() *time.Location {
	loc, err := time.LoadLocation("Asia/Novosibirsk")
	if err != nil {
		// tzdata вшита в бинарник (time/tzdata), так что сюда попадём разве что
		// при сломанной сборке. Но UTC вместо тишины лучше паники в проде.
		return time.UTC
	}
	return loc
}

var funcs = template.FuncMap{
	"asset":       assetURL,
	"av":          newAvatar,
	"nk":          nickOf,
	"body":        noteBodyHTML,
	"doc":         docHTML,
	"commentBody": commentBodyHTML,
	"long":        isLongBody,
	"when":        whenHTML,
	"plural":      plural,
	"depth":       depthClass,
	"themes":      themeList,
	"site":        func() string { return SiteName },
	"origin":      originOf,
	"replyURL":    replyURL,
	"rx":          reactBoxOf,
	"modn":        modNote,
	"modf":        modFeedNote,
	"modc":        modComment,
	"ci":          commentItem,
	"ni":          noteItem,
	"dec":         decisionArg,
	"udec":        userDecisionsOf,
	"kindword":    kindWord,
	"smile":       smileImg,
	"rxlabel":     reactionLabel,
	"smilelist":   smileList,
	"vid":         videoCards,
	"markuphelp":  markupHelp,
	"threadURL":   func(base string) template.URL { return template.URL(base + "#reply") },
}

// replyURL — адрес «ответить вот этому». Собирается в Go и возвращается
// template.URL намеренно: подставленная в href строка с «?» и «&» проходит через
// экранирование URL-контекста и приезжает на страницу как %3f и %26, то есть
// ссылка молча перестаёт работать. Обе половины здесь наши, чужого в адрес
// попасть неоткуда.
func replyURL(base string, commentID int64) template.URL {
	id := strconv.FormatInt(commentID, 10)
	// Якорь — САМА реплика, а не форма: форма теперь стоит под ней, и человек,
	// нажавший «Ответить», обязан увидеть, КОМУ отвечает. Заодно :target
	// подсвечивает реплику и разворачивает свёрнутую простыню.
	return template.URL(base + "&reply=" + id + "#c" + id)
}

// nickArg — подпись автора: имя, его класс по полу и то, ведёт ли имя на
// страницу человека.
//
// Ссылкой ник стал 30.08.2026 по решению владельца: на НГС имя под фотографией
// кликабельно, и отдельная кнопка «автор» в полоске модератора была тем же
// переходом, но видным одному модератору и стоящим не там, где на него смотрят.
//
// Ведёт она у ВОШЕДШЕГО — страницы участников закрыты от гостя и от поисковика
// (решение владельца), — и у всех подряд, если автор ЖИТЕЛЬ (решение владельца
// 30.08.2026): персональных данных у персонажа нет, собирать в одно место
// нечего, а мордолента над лентой ведёт ровно туда же и открыта всем. Гостю
// имя живого человека остаётся текстом, а не ссылкой, уводящей на вход, — по
// общему правилу морды: не показывать кнопку, которая ответит отказом.
type nickArg struct {
	ID    int64
	Name  string
	Class string
	Link  bool
}

// nickOf собирает подпись. Решение «ссылка или текст» живёт в Go, а не в
// шаблоне, потому что мест показа три (лента, страница заметки, реплика) и
// условие у них общее: у анонима автора нет вовсе, у зеркального анонима НГС нет
// даже строки в users, а гостю страница не откроется.
func nickOf(a platform.Author, name string, anonymous, signedIn bool) nickArg {
	class := a.Gender.Class()
	if anonymous {
		class += " anon"
	}
	return nickArg{
		ID:    a.ID,
		Name:  name,
		Class: class,
		Link:  (signedIn || a.Persona) && !anonymous && a.ID != 0,
	}
}

// avatarArg — что показать на месте аватара: адрес картинки и признак того, что
// это силуэт, а не лицо. Признак нужен показу: силуэт — это ОТСУТСТВИЕ фото, и
// в тёмной теме светлый квадрат не должен светить ярче текста (style.css,
// .ava.sil). Отличать их по адресу в CSS было бы можно, но связь «папка
// /assets/profile/ означает силуэт» держалась бы только памятью.
type avatarArg struct {
	URL string
	Sil bool
}

// newAvatar — картинка аватара: своё фото, а нет его — силуэт по умолчанию
// (silhouette.go). Выбор живёт в Go, а не в шаблоне: условие «фото, иначе пол,
// но у анонима всегда аноним» на языке шаблонов читается вдвое хуже, а ошибиться
// в нём стоит показанного лица.
func newAvatar(url string, g platform.Gender, anonymous bool) avatarArg {
	if url == "" || anonymous {
		return avatarArg{URL: silhouette(g, anonymous), Sil: true}
	}
	return avatarArg{URL: url}
}

// commentBodyHTML — тело комментария вместе с обращением.
//
// Обращение живёт ребром reply_to_id, а не текстом: только так переименование и
// обезличивание по 152-ФЗ меняют подпись ВЕЗДЕ, включая чужие ответы. Значит и
// нарисовать его должен показ — из текущего ника адресата, который приехал
// самосоединением в SELECT.
func commentBodyHTML(b *addressBook, c platform.CommentView) template.HTML {
	e := eraOf(c.ID, c.PublishedAt)
	name := replyName(c.ReplyTo)
	if name == "" {
		return renderBody("", c.Body, e)
	}
	body := c.Body
	if e.siteMarkup {
		// «Для [b][i]Ник[/i][/b] » — так обращение рисовал САМ сайт осенью
		// 2013-го, до перехода на «Ник, ». Ребро у такой строки уже есть
		// (его принёс архив), поэтому оставленный в теле префикс дал бы
		// обращение дважды. Снимается он показом, а не разбором при
		// раскатке: 10,7 млн строк уже лежат в базе, и переписывать их
		// ради вида нечестно — ниже по тексту это те же байты, что отдал
		// сайт.
		if cut, ok := platform.TrimLegacyAddress(body); ok {
			body = cut
		}
	}
	// Автор назвал адресата сам — второго обращения не рисуем, а выделяем жирным
	// написанное им (address.go). «Сам назвал» решает не форма строки, а
	// свидетельство треда: одна и та же фраза перед запятой ни ником, ни
	// обращением от этого не становится.
	if token, rest, ok := leadingAddress(body); ok && b.isAddress(c, token) {
		own := template.HTML(`<b class="to">` + template.HTMLEscapeString(token) + `</b>, `)
		return renderBody(own, rest, e)
	}
	prefix := template.HTML(`<a class="to" href="#c` +
		strconv.FormatInt(c.ReplyTo.CommentID, 10) + `">` +
		template.HTMLEscapeString(name) + `</a>, `)
	return renderBody(prefix, body, e)
}

// page — общая часть каждой страницы. Встраивается в данные конкретной
// страницы, поэтому и «база», и содержимое читают поля из одного значения.
type page struct {
	Title string
	Theme string
	// Back — куда вернуть человека после смены темы, входа или выхода. Это
	// текущий адрес, и он проходит localPath на приёме: подставить сюда чужой
	// хост нельзя.
	Back string
	// SignedIn и Nick — что показать в правом верхнем углу шапки: «Вход» или
	// свой ник с выходом, как на НГС.
	SignedIn bool
	Nick     string
	// CSRF — скрытое поле для форм, которые что-то меняют. Пусто у гостя: ему
	// такие формы и не показываются.
	CSRF string
	// Moderator — показать в меню участника пункт «Модерация». Поле общей части
	// страницы, а не отдельной: очередь смотрят между делом, из любого места, и
	// искать вход в неё по памяти адреса модератор не должен.
	Moderator bool
	// Admin — показать пункт «Администрирование». Отдельно от Moderator, а не
	// «Role >= admin» в шаблоне: право это разное по существу (модератор решает
	// про слова, администратор — про людей и права), и решение о показе обязано
	// приниматься там же, где остальные, — в Go.
	Admin bool
	// Bell и Unread — колокольчик в шапке и число непрочитанного (не больше
	// platform.UnreadCap, выше показывается «99+»). Bell отдельным полем, а не
	// «Unread > 0»: значок стоит и пустым — пропадающий элемент шапки дёргает
	// вёрстку на каждом переходе, а нам его показывать ровно тогда, когда шина
	// подключена и человек вошёл.
	Bell   bool
	Unread int
	// Thread — сколько реплик у заметки, которую человек читает; ноль означает
	// «это не страница заметки», и в шапке счётчика тогда нет вовсе. Поле ОБЩЕЙ
	// части, а не страницы заметки, потому что рисует его шапка: она одна на все
	// страницы, и лезть из неё в поля конкретной значило бы уронить все
	// остальные.
	Thread int
	// Canonical — АБСОЛЮТНЫЙ адрес этой страницы для поисковика. Нужен с тех
	// пор, как площадка открыта роботам: у одной заметки адресов несколько —
	// дерево, линейный вид, его страницы, раскрытая коробка реакций, — и все они
	// показывают один и тот же разговор. Без канонического адреса поисковик
	// выбирает главный сам, и выбирает обычно не тот.
	//
	// Пусто — тега нет вовсе: у ленты страницы РАЗНЫЕ по содержанию, и сводить
	// их к первой значило бы спрятать от поиска весь архив, кроме двадцати
	// свежих записей.
	Canonical string
}

// Capped — счётчик упёрся в потолок, и точное число уже не считалось. Решение о
// показе живёт здесь, а не в шаблоне: «99+» это про потолок запроса, а не про
// вкус вёрстки.
func (p page) Capped() bool { return p.Unread >= platform.UnreadCap }

func (s *Server) newPage(r *http.Request, title string) page {
	if title == "" || title == SiteName {
		title = SiteName
	} else {
		title = title + " — " + SiteName
	}
	p := page{
		Title: title,
		Theme: s.theme(r),
		Back:  localPath(r.URL.RequestURI()),
	}
	if u, ok := s.me(r); ok {
		p.SignedIn, p.Nick, p.CSRF = true, u.Nick, csrfToken(s.session(r))
		p.Moderator = u.Role >= platform.RoleModerator && s.mod != nil
		p.Admin = u.Role >= platform.RoleAdmin && s.mod != nil
		p.Bell = s.events != nil
		if p.Bell {
			// Отдельный запрос на страницу, и это осознанная плата. Сложить его
			// в SQL сессии было бы дешевле на один поход в базу, но userColumns
			// один на все чтения человека, и колонка, заполненная ТОЛЬКО у
			// вошедшего через сессию, — это ровно тот вид поля, которое через
			// год прочитают там, где оно всегда ноль. Счёт идёт по частичному
			// индексу и упирается в потолок в сотню строк.
			//
			// Отказ счётчика страницу не рушит: колокольчик покажет пусто.
			// Непрочитанное — не то, ради чего стоит отдавать человеку 500.
			n, err := s.events.Unread(r.Context(), u.ID)
			if err != nil {
				s.log.Warn("счётчик непрочитанного", "user", u.ID, "err", err)
			}
			p.Unread = n
		}
	}
	return p
}

// render собирает страницу В БУФЕР и только потом отдаёт. Иначе ошибка шаблона
// на середине выдаёт обрубок страницы с кодом 200 — то есть врёт и человеку, и
// логам.
// commentItemData и noteItemData — точка для частей «comment» и «note_item».
//
// Пара, а не сама строка: реплике нужен контекст страницы (права, реакции,
// книга обращений), а второго аргумента у {{template}} не бывает. Тот же приём,
// что у `rx` и `modc`, и заведён он ровно затем, чтобы страница и живой добор
// рисовали строку ОДНИМ шаблоном.
type commentItemData struct {
	Page    notePage
	Comment platform.CommentView
}

type noteItemData struct {
	Page feedPage
	Note platform.NoteView
}

func commentItem(p notePage, c platform.CommentView) commentItemData {
	return commentItemData{Page: p, Comment: c}
}

func noteItem(p feedPage, n platform.NoteView) noteItemData {
	return noteItemData{Page: p, Note: n}
}

// parts — набор ТОЛЬКО из частей, без «базы» и без страниц. Через него живой
// добор рисует отдельную строку тем же шаблоном, что и страница.
//
// Отдельный набор, а не заимствование у страницы, по причине из mustPages:
// каждая страница разобрана в свою копию, и брать «ту, что под рукой» значило
// бы привязать фрагмент к случайной из них.
var parts = template.Must(template.New("parts").Funcs(funcs).
	ParseFS(templateFS, "templates/parts/*.gohtml"))

// renderPart собирает одну строку списка. Буфер здесь по той же причине, что и
// у страницы: ошибка шаблона обязана дать честные 500, а не обрубок с кодом 200.
func (s *Server) renderPart(buf *bytes.Buffer, name string, data any) error {
	if err := parts.ExecuteTemplate(buf, name, data); err != nil {
		s.log.Error("отрисовка части", "part", name, "err", err)
		return err
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, _ *http.Request, status int, name string, data any) {
	t, ok := pages[name]
	if !ok {
		s.log.Error("нет такого шаблона", "page", name)
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.log.Error("отрисовка страницы", "page", name, "err", err)
		http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Страницы не кэшируются вовсе: они разные для вошедшего и гостя, а тред —
	// ещё и меняется под ногами. Vary по куке — на случай кэша между нами и
	// человеком, о котором мы не знаем.
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fail — страница с человеческим текстом вместо голого http.Error. Причину
// подбирает вызывающий; подробностей о том, что именно сломалось внутри, здесь
// нет намеренно.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	type errPage struct {
		page
		Status  int
		Message string
	}
	title := "Ошибка"
	switch status {
	case http.StatusNotFound:
		title = "Не найдено"
	case http.StatusForbidden:
		title = "Закрыто"
	}
	s.render(w, r, status, "error.gohtml", errPage{
		page: s.newPage(r, title), Status: status, Message: msg,
	})
}

// oops — отказ по нашей вине. В лог уходит настоящая ошибка, человеку —
// извинение: текст ошибки базы на странице это подсказка тому, кто её ищет.
func (s *Server) oops(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error(what, "err", err)
	s.fail(w, r, http.StatusInternalServerError, "Что-то сломалось на нашей стороне. Мы уже знаем.")
}

// ---------------------------------------------------------------- функции шаблонов

// dateFormat — как время подписано на НГС: 14.08.2026, 18:30:04. С секундами и
// всегда с годом. Читателю «Заметок» это привычная строка, по ней он сверяет
// порядок реплик в длинном треде, поэтому «вчера в 18:30» тут было бы хуже.
const dateFormat = "02.01.2006, 15:04:05"

// whenHTML — время публикации новосибирское, как на сайте.
//
// Неточное время (published_exact = false) — это момент, когда заметку увидело
// зеркало: настоящего сайт не отдаёт. Молча подменять его нельзя (расхождение
// бывает в минуты, а на границе суток и в дату), но и «≈» перед каждой датой
// ленты ломает узнаваемость. Поэтому след остаётся, но тихий: пунктир и
// подпись при наведении.
func whenHTML(t time.Time, exact bool) template.HTML {
	local := t.In(tz)
	class, title := "date", ""
	if !exact {
		class = "date _approx"
		title = ` title="Точного времени публикации сайт не отдаёт: это момент, когда заметку увидело зеркало"`
	}
	return template.HTML(`<time class="` + class + `" datetime="` +
		local.Format(time.RFC3339) + `"` + title + `>` +
		template.HTMLEscapeString(local.Format(dateFormat)) + `</time>`)
}

// plural — русское склонение при числительном.
func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return few
	default:
		return many
	}
}

// depthClass — класс отступа по глубине ветки. Именно класс, а не inline-style:
// CSP без 'unsafe-inline' запрещает атрибут style, а ослаблять CSP ради отступа
// значит менять защиту от XSS на пиксели.
func depthClass(d int) string {
	switch {
	case d <= 1:
		return "d1"
	case d >= platform.MaxDepth:
		return "d" + itoa(platform.MaxDepth)
	default:
		return "d" + itoa(d)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// replyName — как подписать адресата в обращении. Пусто означает «обращение не
// рисуем»: у снесённого модерацией или обезличенного адресата имени нет, и
// придумывать его нечем.
func replyName(r *platform.ReplyRef) string {
	switch {
	case r == nil:
		return ""
	case r.Anonymous:
		return "Аноним"
	default:
		return strings.TrimSpace(r.Nick)
	}
}

// noteTitle — заголовок вкладки для страницы заметки: у заметок на НГС своего
// заголовка нет вовсе, поэтому берём начало текста. Знаки разметки и коды
// смайлов снимаются (stripMarkup) — там, где страница их разбирает: вкладка
// обязана показывать то же, что карточка, а показать жирное или картинку она
// не может.
func noteTitle(n platform.NoteView) string {
	plain := stripMarkup(n.Body, eraOf(n.ID, n.PublishedAt))
	return textutil.Fit(textutil.OneLine(plain), 60)
}
