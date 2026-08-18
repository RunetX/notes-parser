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

// Theme — вариант оформления. Пустой идентификатор — это не тема, а
// отсутствие выбора: атрибута на странице нет, и решает prefers-color-scheme.
// Кнопки у этого состояния нет намеренно — нажать «ничего не выбрано» нельзя,
// а подсветить в нём нужную кнопку умеет CSS (.tbtn в style.css): сервер
// про системную настройку не знает вовсе.
type Theme struct {
	ID   string
	Name string
}

// themes — весь набор, и он из двух. Было четыре, но «Классика» и «Светлая»
// различались только именем: палитра у обеих одна и та же — та, что на НГС. Два
// имени на одно оформление читаются как «есть что-то ещё», а нажатие это не
// подтверждает. Идентификатор остался classic — он уже лежит в куках людей.
var themes = []Theme{
	{ID: "classic", Name: "Светлая"},
	{ID: "dark", Name: "Тёмная"},
}

// legacy — значения куки, которых в наборе больше нет. Уронить их в «решает
// браузер» нельзя: у выбравшего светлую тему страница почернела бы сама,
// если его система в тёмной.
var legacy = map[string]string{"light": "classic"}

func themeList() []Theme { return themes }

func validTheme(id string) bool {
	for _, t := range themes {
		if t.ID == id {
			return true
		}
	}
	return false
}

// theme — выбранная тема. Неизвестное значение куки читается как «выбора нет»:
// кука приходит от человека, и доверять ей нечего.
func (s *Server) theme(r *http.Request) string {
	c, err := r.Cookie(s.cookieName(themeCookie))
	if err != nil {
		return ""
	}
	if id, ok := legacy[c.Value]; ok {
		return id
	}
	if !validTheme(c.Value) {
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
	// Выбор всегда явный: пустое значение форма не отправляет вовсе, и validTheme
	// его больше не пропускает.
	s.setCookie(w, themeCookie, id, prefTTL)
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}
