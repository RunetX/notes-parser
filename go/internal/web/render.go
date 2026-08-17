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
	"body":        bodyHTML,
	"commentBody": commentBodyHTML,
	"when":        whenHTML,
	"plural":      plural,
	"depth":       depthClass,
	"themes":      themeList,
	"site":        func() string { return SiteName },
}

// avatarArg — то, что нужно шаблону аватара. Шаблон принимает одно значение,
// а нужны два (адрес и подпись), поэтому пара собирается функцией.
type avatarArg struct {
	URL     string
	Initial string
}

func newAvatar(url, name string) avatarArg {
	a := avatarArg{URL: url, Initial: "?"}
	for _, r := range name {
		a.Initial = strings.ToUpper(string(r))
		break
	}
	return a
}

// commentBodyHTML — тело комментария вместе с обращением.
//
// Обращение живёт ребром reply_to_id, а не текстом: только так переименование и
// обезличивание по 152-ФЗ меняют подпись ВЕЗДЕ, включая чужие ответы. Значит и
// нарисовать его должен показ — из текущего ника адресата, который приехал
// самосоединением в SELECT.
func commentBodyHTML(c platform.CommentView) template.HTML {
	name := replyName(c.ReplyTo)
	if name == "" {
		return bodyHTML(c.Body)
	}
	prefix := template.HTML(`<a class="to" href="#c` +
		strconv.FormatInt(c.ReplyTo.CommentID, 10) + `">` +
		template.HTMLEscapeString(name) + `</a>, `)
	return renderBody(prefix, c.Body)
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
	// SignedIn — что показать в правом верхнем углу: «Вход» или «Выход».
	// Больше этот признак пока ничего не решает — до Ш4 вход не различает людей.
	SignedIn bool
}

func (s *Server) newPage(r *http.Request, title string) page {
	if title == "" || title == SiteName {
		title = SiteName
	} else {
		title = title + " — " + SiteName
	}
	return page{
		Title:    title,
		Theme:    s.theme(r),
		Back:     localPath(r.URL.RequestURI()),
		SignedIn: s.signedIn(r),
	}
}

// render собирает страницу В БУФЕР и только потом отдаёт. Иначе ошибка шаблона
// на середине выдаёт обрубок страницы с кодом 200 — то есть врёт и человеку, и
// логам.
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
// заголовка нет вовсе, поэтому берём начало текста.
func noteTitle(body string) string {
	return textutil.Fit(textutil.OneLine(body), 60)
}
