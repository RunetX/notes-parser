package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"lovegw/internal/platform"
)

// commentPageSize — сколько комментариев на странице треда. Сто, потому что
// треды бывают на 848 реплик, и попытка отдать такой целиком воспроизводимо
// роняет сам НГС — повторять его ошибку незачем.
const commentPageSize = 100

// viewCookie — запомненный вид треда. Кука, а не только адрес: переключатель на
// сайте живой, им пользуются, и заново выбирать вид на каждой заметке — это
// раздражение на ровном месте.
const viewCookie = "thread"

// maxPathLen — потолок длины курсора-пути. Двенадцать сегментов по 13 знаков
// плюс точки; всё, что длиннее, пришло не от нас.
const maxPathLen = platform.MaxDepth * 14

type notePage struct {
	page
	Note     platform.NoteView
	Images   []platform.Media
	Comments []platform.CommentView
	// Flat — тред показан по времени, а не деревом.
	Flat bool
	// TreeURL / FlatURL — переключатель вида; More — следующая страница треда.
	TreeURL string
	FlatURL string
	More    string
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

	flat := s.threadFlat(w, r)
	after := r.URL.Query().Get("after")
	p := notePage{
		page:    s.newPage(r, noteTitle(note.Body)),
		Note:    note,
		Images:  images,
		Flat:    flat,
		TreeURL: noteURL(id, false, ""),
		FlatURL: noteURL(id, true, ""),
	}

	if flat {
		var afterID int64
		if after != "" {
			afterID, err = strconv.ParseInt(after, 10, 64)
			if err != nil || afterID < 0 {
				s.fail(w, r, http.StatusBadRequest, "Неверная ссылка на страницу треда.")
				return
			}
		}
		comments, next, err := s.st.Flat(ctx, v, id, afterID, commentPageSize)
		if err != nil {
			s.oops(w, r, "комментарии заметки", err)
			return
		}
		p.Comments = comments
		if next != 0 {
			p.More = noteURL(id, true, strconv.FormatInt(next, 10))
		}
	} else {
		if !validPathCursor(after) {
			s.fail(w, r, http.StatusBadRequest, "Неверная ссылка на страницу треда.")
			return
		}
		comments, next, err := s.st.Thread(ctx, v, id, after, commentPageSize)
		if err != nil {
			s.oops(w, r, "тред заметки", err)
			return
		}
		p.Comments = comments
		if next != "" {
			p.More = noteURL(id, false, next)
		}
	}
	s.render(w, r, http.StatusOK, "note.gohtml", p)
}

// threadFlat решает, каким показать тред: явный выбор в адресе сильнее куки, и
// он же куку и обновляет — переключатель обязан запоминаться, иначе им
// пользоваться невозможно.
func (s *Server) threadFlat(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Query().Get("view") {
	case "flat":
		s.setCookie(w, viewCookie, "flat", prefTTL)
		return true
	case "tree":
		s.setCookie(w, viewCookie, "tree", prefTTL)
		return false
	}
	c, err := r.Cookie(s.cookieName(viewCookie))
	return err == nil && c.Value == "flat"
}

func noteURL(id int64, flat bool, after string) string {
	var b strings.Builder
	b.WriteString("/n/")
	b.WriteString(strconv.FormatInt(id, 10))
	b.WriteString("?view=")
	if flat {
		b.WriteString("flat")
	} else {
		b.WriteString("tree")
	}
	if after != "" {
		b.WriteString("&after=")
		b.WriteString(url.QueryEscape(after))
	}
	return b.String()
}

// validPathCursor — курсор дерева похож на путь: только цифры и точки. Проверка
// не про безопасность (значение уходит параметром запроса), а про честный ответ:
// мусор в адресе должен давать 400, а не пустую страницу треда.
func validPathCursor(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > maxPathLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}
