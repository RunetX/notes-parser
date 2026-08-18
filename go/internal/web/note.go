package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"lovegw/internal/platform"
)

// linearPageSize — 30 комментариев на страницу линейного вида, как на НГС
// (`/notes/comments/312696/page~2/limit~30/`).
const linearPageSize = 30

// viewCookie — запомненный вид треда. Кука, а не только адрес: переключатель на
// сайте живой, им пользуются, и заново выбирать вид на каждой заметке — это
// раздражение на ровном месте.
const viewCookie = "thread"

// compose — состояние формы ответа на странице заметки. Живёт отдельно от
// notePage, потому что приходит с двух сторон: из адреса (нажали «Ответить») и
// из отказа при публикации (тогда в ней ещё и набранный текст).
type compose struct {
	Body    string
	ReplyTo int64
	Problem string
}

type notePage struct {
	page
	Note     platform.NoteView
	Images   []platform.Media
	Comments []platform.CommentView
	// Linear — тред показан линейно (новые сверху), а не деревом.
	Linear bool
	// Replies — сколько ответов в ветке каждого комментария. Дерево показывается
	// целиком, поэтому число нужно не для «загрузить», а для того, чтобы ветку
	// можно было свернуть, зная её размер.
	Replies map[int64]int
	TreeURL string
	FlatURL string
	// ReplyBase — адрес этой же страницы, к которому дописывается выбор
	// адресата. Считается один раз здесь, потому что в нём и вид треда, и номер
	// страницы: «Ответить» не должно уводить человека из линейного вида в дерево.
	ReplyBase string
	Pager     pager
	// CanWrite — форма ответа показывается вошедшему участнику. Гостю вместо неё
	// приглашение войти: «читать можно всем» не значит «писать могут все».
	CanWrite bool
	// Editable — своя заметка ещё в окне правки.
	Editable bool
	Compose  compose
	// ReplyTo — адресат готовящегося ответа, если он выбран и ещё жив.
	ReplyTo *platform.CommentView
	// Reactions — реакции заметки (ключ 0) и её комментариев: один запрос на
	// страницу, а не по одному на реплику.
	Reactions map[int64][]platform.Reaction
	// ReactOpen — под каким объектом раскрыт выбор реакции (0 — под заметкой,
	// −1 — ни под кем). Раскрыт всегда не больше одного: выбиралка под каждой из
	// девятисот реплик это пять тысяч кнопок на странице.
	ReactOpen int64
	// PageNum — номер страницы линейного вида: с ним нажатие возвращает человека
	// туда же, где он был.
	PageNum int
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	replyTo, _ := strconv.ParseInt(r.URL.Query().Get("reply"), 10, 64)
	s.showNote(w, r, id, http.StatusOK, compose{ReplyTo: replyTo})
}

// showNote рисует страницу заметки. Отдельно от обработчика, потому что тем же
// путём страница возвращается после неудачной публикации ответа — вместе с
// набранным текстом и причиной отказа.
func (s *Server) showNote(w http.ResponseWriter, r *http.Request, id int64, status int, form compose) {
	ctx, v := r.Context(), s.viewer(r)

	note, err := s.st.NoteViewByID(ctx, v, id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "чтение заметки", err)
		return
	}
	// Скрытая заметка для читателя просто отсутствует. Говорить «она есть, но
	// спрятана» — значит показывать работу модерации посторонним, и это же
	// правило действует при публикации комментария в ядре.
	if note.Status != platform.StatusVisible {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}

	images, err := s.st.NoteImages(ctx, id)
	if err != nil {
		s.oops(w, r, "иллюстрации заметки", err)
		return
	}

	linear := s.threadLinear(w, r)
	me, signedIn := s.me(r)
	// Реакции читаются одним запросом на всю страницу — и заметки, и треда. Свои
	// узнаются по id читателя; у гостя он нулевой, и «моих» не бывает.
	reactions, err := s.st.NoteReactions(ctx, me.ID, id)
	if err != nil {
		s.oops(w, r, "реакции заметки", err)
		return
	}
	p := notePage{
		page:     s.newPage(r, noteTitle(note.Body)),
		Note:     note,
		Images:   images,
		Linear:   linear,
		TreeURL:  noteURL(id, false, 1),
		FlatURL:  noteURL(id, true, 1),
		CanWrite: signedIn && me.Kind == platform.KindMember && !note.Locked && s.wr != nil,
		Editable: note.Editable(time.Now()),
		Compose:  form,

		Reactions: reactions,
		ReactOpen: reactTarget(r),
		PageNum:   1,
	}

	if linear {
		num := pageParam(r.URL.Query().Get("page"))
		if num == 0 {
			s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
			return
		}
		pages := pageCount(note.CommentCount, linearPageSize)
		if num > pages {
			s.fail(w, r, http.StatusNotFound, "Такой страницы в обсуждении нет.")
			return
		}
		comments, err := s.st.Flat(ctx, v, id, (num-1)*linearPageSize, linearPageSize)
		if err != nil {
			s.oops(w, r, "комментарии заметки", err)
			return
		}
		p.Comments = comments
		p.Pager = newPager(num, pages, func(n int) string { return noteURL(id, true, n) })
		p.ReplyBase = noteURL(id, true, num)
		p.PageNum = num
	} else {
		// Дерево отдаётся ЦЕЛИКОМ. Постранички у него нет и не должно быть:
		// ветка, обрезанная на середине, перестаёт быть веткой, а «дальше»
		// внутри разговора означает «конец разговора на следующей странице».
		comments, err := s.st.Thread(ctx, v, id)
		if err != nil {
			s.oops(w, r, "тред заметки", err)
			return
		}
		p.Comments = comments
		p.Replies = replyCounts(comments)
		p.ReplyBase = noteURL(id, false, 1)
	}
	// Адресат берётся из уже загруженного треда, а не отдельным запросом: он там
	// есть по определению, а лишний поход в базу на каждое «Ответить» — это цена
	// без выгоды. Не нашёлся (снесён, на другой странице линейного вида) — форма
	// просто становится ответом в корень.
	p.ReplyTo = findComment(p.Comments, form.ReplyTo)
	s.render(w, r, status, "note.gohtml", p)
}

func findComment(cs []platform.CommentView, id int64) *platform.CommentView {
	if id == 0 {
		return nil
	}
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

// replyCounts считает размер ветки под каждым комментарием. Дерево приходит
// обходом в глубину, поэтому потомки — это идущие следом строки с большей
// глубиной, и одного прохода достаточно.
func replyCounts(cs []platform.CommentView) map[int64]int {
	out := make(map[int64]int, len(cs))
	for i, c := range cs {
		n := 0
		for j := i + 1; j < len(cs) && cs[j].Depth > c.Depth; j++ {
			n++
		}
		if n > 0 {
			out[c.ID] = n
		}
	}
	return out
}

// threadLinear решает, каким показать тред: явный выбор в адресе сильнее куки, и
// он же куку и обновляет — переключатель обязан запоминаться, иначе им
// пользоваться невозможно. По умолчанию дерево: на НГС оно тоже основное.
func (s *Server) threadLinear(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Query().Get("view") {
	case "linear":
		s.setCookie(w, viewCookie, "linear", prefTTL)
		return true
	case "tree":
		s.setCookie(w, viewCookie, "tree", prefTTL)
		return false
	}
	c, err := r.Cookie(s.cookieName(viewCookie))
	return err == nil && c.Value == "linear"
}

func noteURL(id int64, linear bool, page int) string {
	u := "/n/" + strconv.FormatInt(id, 10) + "?view="
	if linear {
		u += "linear"
	} else {
		u += "tree"
	}
	if page > 1 {
		u += "&page=" + strconv.Itoa(page)
	}
	return u
}
