package web

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"lovegw/internal/platform"
)

const (
	// feedPageSize — сколько заметок на странице ленты.
	feedPageSize = 30
	// feedExcerpt — сколько знаков заметки показывать в ленте. Заметки на НГС
	// короткие, и почти все влезают целиком; обрезка нужна редким простыням.
	feedExcerpt = 900
)

type feedPage struct {
	page
	Notes []platform.NoteView
	Limit int
	// More — адрес следующей страницы. Пусто — лента кончилась.
	More string
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	cur, ok := feedCursor(r.URL.Query())
	if !ok {
		s.fail(w, r, http.StatusBadRequest, "Неверная ссылка на страницу ленты.")
		return
	}
	notes, next, err := s.st.Feed(r.Context(), s.viewer(r), cur, feedPageSize)
	if err != nil {
		s.oops(w, r, "лента", err)
		return
	}
	s.render(w, r, http.StatusOK, "feed.gohtml", feedPage{
		page:  s.newPage(r, ""),
		Notes: notes,
		Limit: feedExcerpt,
		More:  feedMoreURL(next),
	})
}

// feedCursor разбирает место, с которого продолжать ленту: время и id последней
// показанной заметки. Пролистывание идёт по ключу, а не по номеру страницы,
// потому что лента живая: OFFSET на ней и дублирует строки, и теряет их.
//
// Время едет через UnixMicro — ровно та точность, которую хранит timestamptz,
// поэтому значение возвращается в базу без искажения. Секунд бы не хватило: в
// бэкфилле у соседних заметок время совпадает до секунды.
func feedCursor(q url.Values) (platform.FeedCursor, bool) {
	ts, id := q.Get("t"), q.Get("id")
	if ts == "" && id == "" {
		return platform.FeedCursor{}, true
	}
	micro, err1 := strconv.ParseInt(ts, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil || n <= 0 {
		return platform.FeedCursor{}, false
	}
	return platform.FeedCursor{PublishedAt: time.UnixMicro(micro), ID: n}, true
}

func feedMoreURL(c platform.FeedCursor) string {
	if c.IsZero() {
		return ""
	}
	return "/?t=" + strconv.FormatInt(c.PublishedAt.UnixMicro(), 10) +
		"&id=" + strconv.FormatInt(c.ID, 10)
}
