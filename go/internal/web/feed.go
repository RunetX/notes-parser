package web

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"lovegw/internal/platform"
)

// feedPageSize — 20 заметок на страницу, как на НГС (`/notes/page~2/limit~20/`).
const feedPageSize = 20

// countTTL — сколько живёт посчитанное число заметок.
//
// Счётчик нужен постраничке (сколько всего страниц), и считается он по всей
// ленте — после раскатки архива это 117 тысяч строк на КАЖДЫЙ заход в ленту,
// самый частый запрос площадки и самая дешёвая мишень. Полминуты устаревания
// стоят ровно одного: последняя страница на полминуты может не знать про
// свежую заметку. Сама заметка при этом видна сразу — она приходит первой
// строкой ленты, а не изменением её длины.
const countTTL = 30 * time.Second

// feedCount — тот самый счётчик. Живёт в памяти процесса: пережить рестарт ему
// незачем, а делить его между мордой и демоном нечем и не нужно.
type feedCount struct {
	mu sync.Mutex
	n  int
	at time.Time
}

// countNotes — сколько заметок в ленте, не чаще раза в countTTL.
func (s *Server) countNotes(ctx context.Context) (int, error) {
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	if now := time.Now(); now.Sub(s.notes.at) < countTTL {
		return s.notes.n, nil
	}
	n, err := s.st.CountNotes(ctx)
	if err != nil {
		return 0, err
	}
	s.notes.n, s.notes.at = n, time.Now()
	return n, nil
}

type feedPage struct {
	page
	// Notes — первой страницей идут закреплённые, дальше хронология. Один
	// список, а не два: для читателя это одна лента, а закреплённое он узнаёт
	// по метке на самой заметке, а не по тому, что оно стоит в другом блоке.
	Notes []platform.NoteView
	Pager pager
	// CanWrite — показывать ли «Написать заметку». Гостю не показываем: читать
	// можно всем, писать — только вошедшим, и кнопка, ведущая к отказу, хуже её
	// отсутствия.
	CanWrite bool
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	num := pageParam(r.URL.Query().Get("page"))
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx, v := r.Context(), s.viewer(r)

	total, err := s.countNotes(ctx)
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
	// Закреплённое — только на первой странице. На остальных оно было бы
	// шапкой, которая едет за читателем: он листает ленту как раз затем, чтобы
	// уйти от начала. Лишнего запроса на страницах 2…5933 при этом нет вовсе.
	if num == 1 {
		pinned, err := s.st.PinnedNotes(ctx, v)
		if err != nil {
			s.oops(w, r, "закреплённые", err)
			return
		}
		notes = append(pinned, notes...)
	}
	me, signedIn := s.me(r)
	s.render(w, r, http.StatusOK, "feed.gohtml", feedPage{
		page:     s.newPage(r, ""),
		Notes:    notes,
		Pager:    newPager(num, pages, feedURL),
		CanWrite: signedIn && me.Kind == platform.KindMember && s.wr != nil,
	})
}

func feedURL(n int) string {
	if n <= 1 {
		return "/"
	}
	return "/?page=" + strconv.Itoa(n)
}
