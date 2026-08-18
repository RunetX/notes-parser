package web

// «Моя страница» — то немногое, что человек может сделать со своими данными без
// переписки с администратором. Права субъекта в полном объёме (выгрузка,
// обезличивание) — это Ш7; здесь исполняется то, что обязано работать НЕМЕДЛЕННО
// и без ручной проверки: отзыв согласия.

import (
	"net/http"

	"lovegw/internal/platform"
)

type mePage struct {
	page
	Member platform.Author
	Docs   []platform.ConsentDoc
	Have   platform.Consents
	Hidden bool // все публикации скрыты рубильником
	Shadow bool // вход не завершён: согласий нет
	Admin  bool
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	missing, err := s.auth.MissingConsent(r.Context(), u.ID, s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	if missing.Kind != "" {
		// Вход не завершён — доводим до конца, а не показываем половину.
		http.Redirect(w, r, "/consent", http.StatusSeeOther)
		return
	}
	docs, err := platform.CurrentConsentDocs(s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "тексты согласий", err)
		return
	}
	have, err := s.auth.UserConsents(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	card, err := s.auth.MemberCard(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "карточка участника", err)
		return
	}
	s.render(w, r, http.StatusOK, "me.gohtml", mePage{
		page:   s.newPage(r, "Моя страница"),
		Member: card,
		Docs:   docs,
		Have:   have,
		Hidden: u.HideAll,
		Shadow: u.Kind == platform.KindShadow,
		Admin:  u.Role >= platform.RoleAdmin,
	})
}

// handleMeConsent — отзыв и возврат согласия.
//
// Отзыв распространения прячет все публикации в тот же момент, без очереди к
// модератору: ч. 2 ст. 9 не оставляет места для «рассмотрим в течение недели».
// Отзыв общего согласия делает то же и вдобавок перестаёт считать человека
// участником — обрабатывать становится нечего.
func (s *Server) handleMeConsent(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	kind := r.FormValue("kind")
	if kind != platform.ConsentProcessing && kind != platform.ConsentDistribution {
		s.fail(w, r, http.StatusBadRequest, "Такого согласия нет.")
		return
	}
	if r.FormValue("action") == "grant" {
		docs, err := platform.CurrentConsentDocs(s.cfg.Operator)
		if err != nil {
			s.oops(w, r, "тексты согласий", err)
			return
		}
		for _, d := range docs {
			if d.Kind == kind {
				if err := s.auth.GrantConsent(r.Context(), u.ID, d.Kind, d.Version, r.UserAgent()); err != nil {
					s.oops(w, r, "запись согласия", err)
					return
				}
			}
		}
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	if err := s.auth.RevokeConsent(r.Context(), u.ID, kind); err != nil {
		s.oops(w, r, "отзыв согласия", err)
		return
	}
	if kind == platform.ConsentProcessing {
		// Сессии погашены вместе с согласием — куку надо снять и здесь, иначе
		// браузер будет носить мёртвый токен.
		s.setCookie(w, sessCookie, "", 0)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}
