package web

// Согласия — по одному на экран.
//
// Не форма с двумя галочками: ч. 1 ст. 10.1 152-ФЗ требует брать согласие на
// распространение ОТДЕЛЬНО от общего и запрещает считать согласием молчание или
// заранее проставленную галочку. Экран с двумя чекбоксами — ровно то, что этим
// запрещено, поэтому здесь два шага, два нажатия и две строки в базе.
//
// Отказ — не тупик и не пустая кнопка: он ОТКАТЫВАЕТ вход. Человек возвращается
// в то состояние, в каком был до него (тень зеркала), сессия гасится, связь с
// анкетой снимается. Иначе в базе оставались бы участники, которые ни на что не
// соглашались, — и объяснить, на каком основании они там, было бы нечем.

import (
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

type consentPage struct {
	page
	Doc  platform.ConsentDoc
	Step int
	Of   int
	Nick string
}

func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	doc, err := s.auth.MissingConsent(r.Context(), u.ID, s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	if doc.Kind == "" {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	step := 1
	if doc.Kind == platform.ConsentDistribution {
		step = 2
	}
	s.render(w, r, http.StatusOK, "consent.gohtml", consentPage{
		page: s.newPage(r, "Вход"),
		Doc:  doc, Step: step, Of: 2, Nick: u.Nick,
	})
}

func (s *Server) handleConsentGrant(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// Что именно подписано, решает сервер, а не форма: иначе подделанное поле
	// записало бы согласие на документ, которого человек не видел, — а строка в
	// consents и есть доказательство.
	doc, err := s.auth.MissingConsent(r.Context(), u.ID, s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	if doc.Kind == "" {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	if r.FormValue("kind") != doc.Kind || r.FormValue("version") != strconv.Itoa(doc.Version) {
		// Экран разъехался с состоянием — например, вторая вкладка. Показываем
		// то, что действительно требуется сейчас.
		http.Redirect(w, r, "/consent", http.StatusSeeOther)
		return
	}
	if err := s.auth.GrantConsent(r.Context(), u.ID, doc.Kind, doc.Version, r.UserAgent()); err != nil {
		s.oops(w, r, "запись согласия", err)
		return
	}
	http.Redirect(w, r, "/consent", http.StatusSeeOther)
}

// handleConsentRefuse откатывает незавершённый вход целиком.
func (s *Server) handleConsentRefuse(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.auth.AbortLogin(r.Context(), u.ID); err != nil {
		s.oops(w, r, "отказ от входа", err)
		return
	}
	s.setCookie(w, sessCookie, "", 0)
	s.setCookie(w, codeCookie, "", 0)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
