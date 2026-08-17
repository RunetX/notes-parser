package web

import (
	"errors"
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// linearPageSize — 30 комментариев на страницу линейного вида, как на НГС
// (`/notes/comments/312696/page~2/limit~30/`).
const linearPageSize = 30

// viewCookie — запомненный вид треда. Кука, а не только адрес: переключатель на
// сайте живой, им пользуются, и заново выбирать вид на каждой заметке — это
// раздражение на ровном месте.
const viewCookie = "thread"

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
	Pager   pager
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
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
	p := notePage{
		page:    s.newPage(r, noteTitle(note.Body)),
		Note:    note,
		Images:  images,
		Linear:  linear,
		TreeURL: noteURL(id, false, 1),
		FlatURL: noteURL(id, true, 1),
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
	}
	s.render(w, r, http.StatusOK, "note.gohtml", p)
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
