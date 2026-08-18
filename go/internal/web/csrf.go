package web

// Второй рубеж против подделки межсайтовых запросов — скрытое поле формы.
//
// Первый рубеж (Origin / Sec-Fetch-Site) стоит в postForm и ловит подавляющее
// большинство, но опирается он на заголовки, которые присылает браузер, а не на
// секрет, известный только нам. Поэтому у форм, которые что-то МЕНЯЮТ от имени
// вошедшего, есть ещё и поле. Именно поле, а не заголовок: формы площадки
// обязаны работать без JS, а заголовок без JS не поставить.
//
// Токен ВЫВОДИТСЯ из токена сессии, а не хранится отдельно. Своей куки не
// нужно, гасится он вместе с сессией сам, при выходе становится недействителен
// в тот же момент — и подделать его нельзя: сессионная кука HttpOnly, чужая
// страница её не прочтёт. Разделитель в хеше нужен затем, чтобы значение,
// которое видно в разметке страницы, нельзя было подставить обратно как токен
// сессии.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

const csrfField = "csrf"

func csrfToken(session string) string {
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("t3h-csrf|" + session))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// checkCSRF сверяет поле формы с сессией. false означает «ответ уже отправлен».
//
// У гостя токена нет вовсе, и это правильный отказ: все формы, которые его
// требуют, доступны только вошедшим.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	want := csrfToken(s.session(r))
	got := r.FormValue(csrfField)
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
		s.fail(w, r, http.StatusForbidden,
			"Форма устарела или вы вышли. Обновите страницу и попробуйте снова.")
		return false
	}
	return true
}

// postWrite — вход для форм, которые пишут: происхождение, разбор тела, токен.
// Одним местом по той же причине, что и postForm: забытая проверка — это дыра.
func (s *Server) postWrite(w http.ResponseWriter, r *http.Request) bool {
	return s.postForm(w, r) && s.checkCSRF(w, r)
}
