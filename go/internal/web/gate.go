package web

// Ворота: общий ключ доступа до настоящего входа (Ш4).
//
// Площадка стоит на публичном домене и показывает зеркало переписки живых
// людей. Значит открытой она быть не может ни дня — ни «пока никто не знает
// адреса», ни «пока там нечего смотреть»: адрес попадёт в чей-нибудь чат
// первым же вечером. Отсюда правило, которое стоит понимать буквально: ПУСТОЙ
// КЛЮЧ ЗАКРЫВАЕТ ВСЁ. Забытая настройка обязана выглядеть как запертая дверь, а
// не как открытая.
//
// Ключ общий и потому временный: он не различает людей, ничего не знает про
// согласия и не годится ни для записи, ни для показа «моего». Всё это приносит
// Ш4, и вместе с ним ворота уходят.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"time"
)

// gateCookie — имя куки доступа. Через cookieName получает префикс __Host- на
// https: он запрещает браузеру принимать её с другого пути и без Secure.
const gateCookie = "view"

// gateTTL — на сколько хватает одного ввода ключа. Месяц: это временная мера, и
// заставлять владельца вводить ключ каждый день незачем.
const gateTTL = 30 * 24 * time.Hour

// gateDigest — то, с чем сравнивается кука. В памяти держим только хеш ключа:
// сам ключ нужен ровно один раз, на проверке ввода.
func gateDigest(key string) []byte {
	if key == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func (s *Server) withGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case len(s.gate) == 0:
			s.fail(w, r, http.StatusForbidden,
				"Площадка ещё закрыта. Вход появится вместе с проверкой анкеты.")
		case s.gatePassed(r):
			next.ServeHTTP(w, r)
		default:
			http.Redirect(w, r, "/gate?to="+url.QueryEscape(localPath(r.URL.RequestURI())),
				http.StatusSeeOther)
		}
	})
}

func (s *Server) gatePassed(r *http.Request) bool {
	if len(s.gate) == 0 {
		return false
	}
	c, err := r.Cookie(s.cookieName(gateCookie))
	if err != nil {
		return false
	}
	got, err := hex.DecodeString(c.Value)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, s.gate) == 1
}

type gatePage struct {
	page
	To    string
	Wrong bool
}

func (s *Server) handleGate(w http.ResponseWriter, r *http.Request) {
	if len(s.gate) == 0 {
		s.fail(w, r, http.StatusForbidden,
			"Площадка ещё закрыта. Вход появится вместе с проверкой анкеты.")
		return
	}
	if s.gatePassed(r) {
		http.Redirect(w, r, localPath(r.URL.Query().Get("to")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "gate.gohtml", gatePage{
		page: s.newPage(r, "Вход"),
		To:   localPath(r.URL.Query().Get("to")),
	})
}

func (s *Server) handleGateSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	if len(s.gate) == 0 {
		s.fail(w, r, http.StatusForbidden, "Площадка ещё закрыта.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "Форма не разобралась.")
		return
	}
	to := localPath(r.FormValue("to"))
	if subtle.ConstantTimeCompare(gateDigest(r.FormValue("key")), s.gate) != 1 {
		// Отдельного счётчика попыток нет: ключ длиной в 128+ бит перебирать
		// по сети бессмысленно, а счётчик по адресу требовал бы хранить адреса.
		s.log.Warn("ворота: неверный ключ")
		s.render(w, r, http.StatusUnauthorized, "gate.gohtml", gatePage{
			page: s.newPage(r, "Вход"), To: to, Wrong: true,
		})
		return
	}
	s.setCookie(w, gateCookie, hex.EncodeToString(s.gate), gateTTL)
	http.Redirect(w, r, to, http.StatusSeeOther)
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
