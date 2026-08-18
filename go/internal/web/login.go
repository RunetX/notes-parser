package web

// Вход.
//
// Основной путь — код в поле «о себе» анкеты НГС (устройство и обоснование
// двусторонней проверки — в шапке platform/auth.go). Здесь только его экраны:
//
//	/login            номер анкеты
//	POST /login       читаем анкету, показываем «это вы?» и код
//	POST /login/check читаем анкету снова и сверяем код
//	/consent          два согласия по одному на экран
//	/me               свой угол
//
// Второй путь — приглашение (`/login/invite`): анкету могли снести, а сайт может
// исчезнуть целиком, и тогда доказать владение нечем. Инвайт переживает и то,
// и другое.
//
// Пароля НГС мы не спрашиваем ни на одном экране и не будем: приучить сообщество
// вводить пароль сайта на чужом домене — значит подарить любой подделке нашего
// адреса пароли всех сразу.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

const (
	// sessCookie — токен сессии. Через cookieName получает префикс __Host- на
	// https: он запрещает браузеру принять такую куку без Secure, с чужого пути
	// и с Domain, то есть закрывает подсадку куки соседним поддоменом.
	sessCookie = "sess"
	// codeCookie — код текущей проверки анкеты. Это вторая половина
	// доказательства: анкета говорит «код тут», кука — «проверку начал я».
	// Обе нужны, потому что код в чужом «о себе» видит кто угодно.
	codeCookie = "code"
)

// profileIDRe вытаскивает номер анкеты и из голого числа, и из ссылки вида
// love.ngs.ru/profile/1493279/ — люди копируют адрес из браузера чаще, чем
// переписывают цифры.
var profileIDRe = regexp.MustCompile(`\d+`)

func parseProfileID(s string) int64 {
	// Число берётся ЦЕЛИКОМ (\d+, а не \d{1,10}): иначе слишком длинное «число»
	// разрезалось бы на куски, и последний из них прошёл бы проверку полосы.
	m := profileIDRe.FindAllString(s, -1)
	if len(m) == 0 {
		return 0
	}
	// Из ссылки берём ПОСЛЕДНЕЕ число: в «https://love.ngs.ru/profile/1493279/»
	// первым иначе оказался бы кусок домена.
	last := m[len(m)-1]
	if len(last) > 12 {
		return 0
	}
	id, err := strconv.ParseInt(last, 10, 64)
	if err != nil || !platform.IsNGS(id) {
		return 0
	}
	return id
}

// ---------------------------------------------------------------- сессия

// session — токен из куки. Пусто, если не вошёл.
func (s *Server) session(r *http.Request) string {
	c, err := r.Cookie(s.cookieName(sessCookie))
	if err != nil {
		return ""
	}
	return c.Value
}

// userKey — где в контексте запроса лежит вошедший. Сессия читается ОДИН раз за
// запрос (withViewer), потому что её спрашивают трое: шапка, права на странице
// и запросы к ядру, — а три обращения к базе на страницу это уже заметно.
type ctxKey int

const userKey ctxKey = 1

// withViewer разрешает куку сессии в человека и кладёт его в контекст.
func (s *Server) withViewer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Статика и медиа приходят с теми же куками, но человека не показывают
		// вовсе: платить за них запросом к базе не за что.
		if s.auth != nil && !strings.HasPrefix(r.URL.Path, "/assets/") &&
			!strings.HasPrefix(r.URL.Path, "/media/") {
			if token := s.session(r); token != "" {
				u, err := s.auth.SessionUser(r.Context(), token)
				switch {
				case err == nil:
					r = r.WithContext(context.WithValue(r.Context(), userKey, u))
				case !errors.Is(err, platform.ErrNotFound):
					s.log.Error("чтение сессии", "err", err)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// me — вошедший человек. Второе значение false означает «гость»: истёкшая,
// отозванная и несуществующая сессия для показа страницы — одно и то же.
func (s *Server) me(r *http.Request) (platform.User, bool) {
	u, ok := r.Context().Value(userKey).(platform.User)
	return u, ok
}

// signIn выдаёт сессию и ставит куку.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, expires, err := s.auth.CreateSession(r.Context(), userID, r.UserAgent())
	if err != nil {
		return err
	}
	s.setCookie(w, sessCookie, token, time.Until(expires))
	s.setCookie(w, codeCookie, "", 0)
	return nil
}

// ---------------------------------------------------------------- страница входа

type loginPage struct {
	page
	// ByProfile — доступен ли вход по анкете. Нет клиента сайта (или сайт
	// закрылся) — показывать форму, которая не сработает, хуже, чем сказать это.
	ByProfile bool
	Problem   string
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.me(r); ok {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, http.StatusOK, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, problem string) {
	s.render(w, r, status, "login.gohtml", loginPage{
		page:      s.newPage(r, "Вход"),
		ByProfile: s.site != nil,
		Problem:   problem,
	})
}

// codePage — экран «это вы?»: ник анкеты, код и что с ним делать.
type codePage struct {
	page
	ProfileID int64
	Nick      string
	Avatar    string // наш путь /media/…, если этого человека знает зеркало
	Code      string
	Problem   string
}

// handleLoginStart читает анкету и выдаёт код.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	if s.site == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Вход по анкете сейчас недоступен.")
		return
	}
	id := parseProfileID(r.FormValue("profile"))
	if id == 0 {
		s.renderLogin(w, r, http.StatusBadRequest,
			"Это не похоже на номер анкеты. Нужны цифры из адреса вашей страницы на НГС.")
		return
	}
	prof, err := s.site.Profile(r.Context(), id)
	if errors.Is(err, ErrNoProfile) {
		s.renderLogin(w, r, http.StatusNotFound, "Анкеты с таким номером на НГС нет.")
		return
	}
	if err != nil {
		s.log.Error("чтение анкеты НГС", "profile", id, "err", err)
		s.renderLogin(w, r, http.StatusBadGateway,
			"НГС сейчас не отвечает. Попробуйте позже — или войдите по приглашению.")
		return
	}
	ch, err := s.auth.StartProfileChallenge(r.Context(), id)
	if err != nil {
		s.oops(w, r, "выдача кода входа", err)
		return
	}
	// Кука с кодом — вторая половина доказательства, и живёт она ровно столько
	// же, сколько сам код.
	s.setCookie(w, codeCookie, strconv.FormatInt(id, 10)+":"+ch.Code, time.Until(ch.ExpiresAt))
	s.renderCode(w, r, http.StatusOK, id, prof, ch.Code, "")
}

func (s *Server) renderCode(w http.ResponseWriter, r *http.Request, status int,
	id int64, prof SiteProfile, code, problem string,
) {
	// Аватар берём СВОЙ, из зеркала: CSP запрещает картинки с чужих хостов, и
	// это тот случай, когда запрет полезен — иначе страница входа сообщала бы
	// hsmedia.ru о каждом, кто собрался переезжать.
	avatar := ""
	if card, err := s.auth.MemberCard(r.Context(), id); err == nil {
		avatar = card.AvatarURL
	}
	s.render(w, r, status, "login_code.gohtml", codePage{
		page:      s.newPage(r, "Вход"),
		ProfileID: id,
		Nick:      prof.Nick,
		Avatar:    avatar,
		Code:      code,
		Problem:   problem,
	})
}

// handleLoginCheck перечитывает анкету и сверяет код.
func (s *Server) handleLoginCheck(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	if s.site == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Вход по анкете сейчас недоступен.")
		return
	}
	id, code := s.pendingCode(r)
	if id == 0 {
		s.renderLogin(w, r, http.StatusUnauthorized,
			"Код устарел или проверка начиналась в другом браузере. Начните заново.")
		return
	}
	prof, err := s.site.Profile(r.Context(), id)
	if err != nil {
		s.log.Error("чтение анкеты НГС", "profile", id, "err", err)
		s.renderCode(w, r, http.StatusBadGateway, id, SiteProfile{}, code,
			"НГС сейчас не отвечает. Код ещё жив — попробуйте нажать «Проверить» через минуту.")
		return
	}
	switch err := s.auth.VerifyProfileChallenge(r.Context(), id, code, prof.AboutMe); {
	case err == nil:
	case errors.Is(err, platform.ErrCodeNotFound):
		// «Приватность» тут ни при чём: она прячет активность, а не «о себе»
		// (замер 18.08.2026, см. love.Profile.Hidden), — значит причина одна.
		s.renderCode(w, r, http.StatusUnauthorized, id, prof, code,
			"В поле «о себе» кода нет. Сохранили анкету на НГС?")
		return
	case errors.Is(err, platform.ErrNoChallenge):
		s.renderLogin(w, r, http.StatusUnauthorized,
			"Код устарел или был заменён новым. Начните заново.")
		return
	case errors.Is(err, platform.ErrTooManyAttempts):
		s.renderCode(w, r, http.StatusTooManyRequests, id, prof, code,
			"Слишком много проверок подряд. Возьмите новый код через час.")
		return
	default:
		s.oops(w, r, "проверка кода входа", err)
		return
	}

	userID, err := s.auth.CompleteNGSLogin(r.Context(),
		platform.MirroredAuthor{ID: id, Nick: prof.Nick, AvatarURL: prof.AvatarURL}, prof.Gender)
	if errors.Is(err, platform.ErrAnonymized) {
		s.renderCode(w, r, http.StatusForbidden, id, prof, code,
			"Данные этой анкеты обезличены по требованию её владельца. "+
				"Автоматически вернуть их нельзя — напишите администратору.")
		return
	}
	if err != nil {
		s.oops(w, r, "завершение входа", err)
		return
	}
	if err := s.signIn(w, r, userID); err != nil {
		s.oops(w, r, "выдача сессии", err)
		return
	}
	http.Redirect(w, r, "/consent", http.StatusSeeOther)
}

// pendingCode разбирает куку проверки: «<номер анкеты>:<код>».
func (s *Server) pendingCode(r *http.Request) (int64, string) {
	c, err := r.Cookie(s.cookieName(codeCookie))
	if err != nil {
		return 0, ""
	}
	idStr, code, ok := strings.Cut(c.Value, ":")
	if !ok {
		return 0, ""
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || !platform.IsNGS(id) || code == "" {
		return 0, ""
	}
	return id, code
}

// ---------------------------------------------------------------- приглашение

const (
	invitePageName  = "login_invite.gohtml"
	invitePageTitle = "Приглашение"
)

type invitePage struct {
	page
	Problem string
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.me(r); ok {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, invitePageName, invitePage{page: s.newPage(r, invitePageTitle)})
}

func (s *Server) handleInviteSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	refuse := func(status int, problem string) {
		s.render(w, r, status, invitePageName, invitePage{
			page: s.newPage(r, invitePageTitle), Problem: problem,
		})
	}
	nick := strings.TrimSpace(r.FormValue("nick"))
	userID, err := s.auth.RedeemInvite(r.Context(), r.FormValue("code"), nick)
	switch {
	case errors.Is(err, platform.ErrInviteInvalid):
		refuse(http.StatusUnauthorized, "Приглашение не найдено, уже использовано или истекло.")
		return
	case errors.Is(err, platform.ErrAnonymized):
		refuse(http.StatusForbidden,
			"Данные этого участника обезличены по его требованию. Напишите администратору.")
		return
	case err != nil:
		s.oops(w, r, "приглашение", err)
		return
	}
	if err := s.signIn(w, r, userID); err != nil {
		s.oops(w, r, "выдача сессии", err)
		return
	}
	http.Redirect(w, r, "/consent", http.StatusSeeOther)
}

// ---------------------------------------------------------------- выход

// handleLogout — POST, а не ссылка: выход меняет состояние, а GET-ссылку на
// такое браузер нажимает сам в любом префетче.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	if token := s.session(r); token != "" && s.auth != nil {
		if err := s.auth.RevokeSession(r.Context(), token); err != nil {
			s.log.Error("выход", "err", err)
		}
	}
	s.setCookie(w, sessCookie, "", 0)
	s.setCookie(w, codeCookie, "", 0)
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
