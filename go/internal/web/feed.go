package web

import (
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// feedPageSize — 20 заметок на страницу, как на НГС (`/notes/page~2/limit~20/`).
const feedPageSize = 20

type feedPage struct {
	page
	Notes []platform.NoteView
	Pager pager
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	num := pageParam(r.URL.Query().Get("page"))
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx, v := r.Context(), s.viewer(r)

	total, err := s.st.CountNotes(ctx)
	if err != nil {
		s.oops(w, r, "счётчик ленты", err)
		return
	}
	pages := pageCount(total, feedPageSize)
	if num > pages {
		s.fail(w, r, http.StatusNotFound, "Такой страницы в ленте нет.")
		return
	}
	notes, err := s.st.Feed(ctx, v, (num-1)*feedPageSize, feedPageSize)
	if err != nil {
		s.oops(w, r, "лента", err)
		return
	}
	s.render(w, r, http.StatusOK, "feed.gohtml", feedPage{
		page:  s.newPage(r, ""),
		Notes: notes,
		Pager: newPager(num, pages, feedURL),
	})
}

func feedURL(n int) string {
	if n <= 1 {
		return "/"
	}
	return "/?page=" + strconv.Itoa(n)
}
