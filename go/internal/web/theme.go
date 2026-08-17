package web

// Темы. Механика ровно та же, что у токенов CSS: выбор человека живёт в куке и
// приезжает на страницу атрибутом data-theme, а его отсутствие означает «как в
// системе» — тогда решает prefers-color-scheme.
//
// Переключатель — форма с POST, а не ссылка: смена темы меняет состояние, а
// GET-ссылка на такое сама себя нажимает в любом префетче браузера. И не JS:
// страница обязана работать без него, а с ним ничего не улучшается.

import (
	"net/http"
	"time"
)

const (
	themeCookie = "theme"
	// prefTTL — срок жизни кук с предпочтениями (тема, вид треда). Год: это не
	// доступ и не личные данные, а выбор оформления, и переспрашивать его
	// каждую неделю незачем.
	prefTTL = 365 * 24 * time.Hour
)

// Theme — вариант оформления. Пустой идентификатор — «как в системе»: атрибута
// на странице нет, и тему выбирает сам браузер.
type Theme struct {
	ID   string
	Name string
}

// themes — весь набор. «Классика» стоит первой не из ностальгии: людям,
// переезжающим с НГС, узнаваемая палитра говорит «это то самое место» быстрее
// любого текста на главной.
var themes = []Theme{
	{ID: "", Name: "Как в системе"},
	{ID: "classic", Name: "Классика"},
	{ID: "light", Name: "Светлая"},
	{ID: "dark", Name: "Тёмная"},
}

func themeList() []Theme { return themes }

func validTheme(id string) bool {
	for _, t := range themes {
		if t.ID == id {
			return true
		}
	}
	return false
}

// theme — выбранная тема. Неизвестное значение куки читается как «как в
// системе»: кука приходит от человека, и доверять ей нечего.
func (s *Server) theme(r *http.Request) string {
	c, err := r.Cookie(s.cookieName(themeCookie))
	if err != nil || !validTheme(c.Value) {
		return ""
	}
	return c.Value
}

func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return
	}
	id := r.FormValue("theme")
	if !validTheme(id) {
		s.fail(w, r, http.StatusBadRequest, "Такой темы нет.")
		return
	}
	// Пустая тема — это удаление куки: «как в системе» не значение, а его
	// отсутствие, и хранить его отдельным словом незачем.
	s.setCookie(w, themeCookie, id, prefTTL)
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}
