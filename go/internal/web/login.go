package web

// Вход.
//
// До 18.08.2026 этот файл был воротами: без общего ключа площадка не отдавала
// ни одной страницы, и пустой ключ означал «не пускать никого». Решение
// владельца — открыть чтение всем, а вход показывать кнопкой в шапке, как на
// НГС. Вместе с воротами ушло и то правило: теперь пустой ключ означает «войти
// пока некуда», а читать можно и так.
//
// Ключ общий и потому временный: он не различает людей, ничего не знает про
// согласия и не годится ни для записи, ни для показа «моего». Всё это приносит
// Ш4 — код в поле «о себе» анкеты НГС, — и вместе с ним ключ уходит. Пока же
// вход даёт ровно одно: метку «свой» в шапке.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

// authCookie — имя куки входа. Через cookieName получает префикс __Host- на
// https: он запрещает браузеру принимать её с другого пути и без Secure.
const authCookie = "in"

// authTTL — на сколько хватает одного ввода ключа. Месяц: это временная мера, и
// заставлять владельца вводить ключ каждый день незачем.
const authTTL = 30 * 24 * time.Hour

// authDigest — то, с чем сравнивается кука. В памяти держим только хеш ключа:
// сам ключ нужен ровно один раз, на проверке ввода.
func authDigest(key string) []byte {
	if key == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func (s *Server) signedIn(r *http.Request) bool {
	if len(s.gate) == 0 {
		return false
	}
	c, err := r.Cookie(s.cookieName(authCookie))
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(c.Value)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, s.gate) == 1
}

type loginPage struct {
	page
	To    string
	Wrong bool
	// Open — можно ли вообще войти. Ключ не задан — формы нет: поле, к которому
	// не подходит ни одно значение, хуже честного «пока нельзя».
	Open bool
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.signedIn(r) {
		http.Redirect(w, r, localPath(r.URL.Query().Get("to")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.gohtml", loginPage{
		page: s.newPage(r, "Вход"),
		To:   localPath(r.URL.Query().Get("to")),
		Open: len(s.gate) > 0,
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	if len(s.gate) == 0 {
		s.fail(w, r, http.StatusForbidden, "Вход пока закрыт.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return
	}
	to := localPath(r.FormValue("to"))
	if subtle.ConstantTimeCompare(authDigest(r.FormValue("key")), s.gate) != 1 {
		// Отдельного счётчика попыток нет: ключ длиной в 128+ бит перебирать
		// по сети бессмысленно, а счётчик по адресу требовал бы хранить адреса.
		s.log.Warn("вход: неверный ключ")
		s.render(w, r, http.StatusUnauthorized, "login.gohtml", loginPage{
			page: s.newPage(r, "Вход"), To: to, Wrong: true, Open: true,
		})
		return
	}
	s.setCookie(w, authCookie, hex.EncodeToString(s.gate), authTTL)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleLogout — POST, а не ссылка: выход меняет состояние, а GET-ссылку на
// такое браузер нажимает сам в любом префетче.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return
	}
	s.setCookie(w, authCookie, "", 0)
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}

// cookieName добавляет префикс __Host- там, где он работает. Префикс — это не
// украшение: он запрещает браузеру принять такую куку без Secure, с чужого
// пути и с Domain, то есть закрывает подсадку куки соседним поддоменом.
func (s *Server) cookieName(name string) string {
	if s.secure {
		return "__Host-" + name
	}
	return name
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	c := &http.Cookie{
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
