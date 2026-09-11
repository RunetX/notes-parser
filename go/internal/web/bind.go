package web

// Привязка мессенджера со стороны морды: половина дела, и именно та, где сидит
// человек, чью учётную запись привязывают.
//
// Здесь рождается КОД (platform.StartBinding) — в живой сессии и только в ней.
// Гасит его бот, потому что только там известно, кто собеседник. Почему
// направление именно такое и чем кончается подсунутый код — в шапке
// platform/binding.go; здесь важно одно следствие: на этой странице человек
// обязан прочитать, что код делает, ДО того как он появится на экране.

import (
	"errors"
	"net/http"
	"time"

	"lovegw/internal/platform"
)

// bindPage — экран привязки в двух состояниях. Оба живут одним шаблоном, потому
// что это один разговор: сперва документ и кнопка, потом код и куда его слать.
type bindPage struct {
	page
	Doc platform.ConsentDoc
	// Code пуст на первом шаге. Непустой означает, что согласие уже записано, а
	// код живёт Mins минут.
	Code string
	Mins int
	Bots []botLink
	// Bound — что уже привязано: повторная привязка того же мессенджера
	// законна («проверю, что работает»), но человек должен видеть, что она есть.
	Bound []platform.Binding
}

// handleBind — первый шаг: показать документ. Отдельным экраном, а не абзацем
// на «Моей странице», по той же причине, по которой отдельны экраны согласий на
// входе: подпись, данная мимоходом под кнопкой, подписью не является.
func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentBinding)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	bound, err := s.auth.UserBindings(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "привязки", err)
		return
	}
	s.render(w, r, http.StatusOK, "bind.gohtml", bindPage{
		page: s.newPage(r, "Привязка мессенджера"),
		Doc:  doc, Bots: s.botLinks(), Bound: bound,
	})
}

// handleBindStart — второй шаг: записать согласие и показать код.
//
// Код показывается ПРЯМО В ОТВЕТЕ на POST, а не после перехода, — тем же
// приёмом и по тому же доводу, что код приглашения на /mod/admin: редирект унёс
// бы его в адресную строку, то есть в историю браузера и в лог Caddy.
func (s *Server) handleBindStart(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentBinding)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	code, expires, err := s.auth.StartBinding(r.Context(), u.ID, r.UserAgent())
	if err != nil {
		s.oops(w, r, "код привязки", err)
		return
	}
	bound, err := s.auth.UserBindings(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "привязки", err)
		return
	}
	// Минуты считаем от полученного срока, а не пишем словом: число живёт в
	// ядре (platform.BindTTL), и разойдясь с ним текст стал бы врать.
	s.render(w, r, http.StatusOK, "bind.gohtml", bindPage{
		page: s.newPage(r, "Привязка мессенджера"),
		Doc:  doc, Code: code, Bots: s.botLinks(), Bound: bound,
		Mins: int(time.Until(expires).Round(time.Minute) / time.Minute),
	})
}

// handleUnbind снимает привязку. Возврат на «Мою страницу» с объяснением, а не
// на экран привязки: отвязка — это конец разговора, а не его середина.
func (s *Server) handleUnbind(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	switch err := s.auth.Unbind(r.Context(), u.ID, r.FormValue("messenger")); {
	case err == nil:
		http.Redirect(w, r, "/me", http.StatusSeeOther)
	// Отвязка того, чего нет, и отвязка неизвестной породы для человека значат
	// одно: «уже не привязано». Страницей ошибки отвечать здесь не на что —
	// нажали дважды, вернулись по истории.
	case errors.Is(err, platform.ErrNoBinding), errors.Is(err, platform.ErrUnknownMessenger):
		s.showMe(w, r, u, "Этот мессенджер к вашей записи не привязан.")
	default:
		s.oops(w, r, "отвязка мессенджера", err)
	}
}

// handleLogoutAll гасит ВСЕ сессии человека — «выйти на всех устройствах».
//
// Появилась вместе со скользящим сроком сессии (platform.SessionUser) и не для
// полноты: пока окно было абсолютным, украденная кука умирала сама через три
// месяца, а теперь живёт, пока ею пользуются. Это и есть цена продления, и
// заплатить её обязана кнопка, а не надежда.
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := s.auth.RevokeUserSessions(r.Context(), u.ID); err != nil {
		s.oops(w, r, "выход на всех устройствах", err)
		return
	}
	// Своя кука снимается тоже: гасятся ВСЕ сессии, включая эту, и оставить в
	// браузере мёртвый токен значило бы показать человеку «вы вошли» до первого
	// же запроса.
	s.setCookie(w, sessCookie, "", 0)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// messengerName — как сеть зовут вслух. Отдельная функция, а не поле в
// platform.Binding: ядру всё равно, как это пишется на экране, а вариант
// написания у MAX ровно один и заглавными.
func messengerName(kind string) string {
	switch kind {
	case platform.IdentityTelegram:
		return "Telegram"
	case platform.IdentityMAX:
		return "MAX"
	}
	return kind
}
