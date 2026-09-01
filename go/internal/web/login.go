package web

// Вход. Экраны; устройство и обоснование — в шапке platform/auth.go.
//
//	/login            номер анкеты
//	POST /login       читаем анкету, заводим код и ДОСТАВЛЯЕМ его
//	POST /login/check сверяем: введённый код (личка) или содержимое «о себе»
//	/consent          два согласия по одному на экран
//	/me               свой угол
//
// Канал доставки кода ОДИН: код показывается на экране, человек вставляет его в
// поле «о себе» на НГС, и проверка становится двусторонней (кода в публичной
// анкете мало — нужна ещё кука того, кто проверку начал). Второй канал, код
// личным сообщением на НГС, убран 01.09.2026 вместе с удалённым служебным
// аккаунтом; почему убран целиком, а не выключен — в шапке platform/auth.go.
//
// Отсюда устройство экрана проверки: поля ввода кода на нём НЕТ вовсе, есть
// только кнопка «Проверить». Это не экономия вёрстки, а та самая защита —
// показанный код, принятый введённым обратно, отдаёт чужую анкету в одно
// нажатие.
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
	// codeCookie — какая проверка сейчас идёт: «<канал>:<анкета>:<код>».
	//
	// Код здесь и есть вторая половина доказательства: анкета говорит «код тут»,
	// кука — «проверку начал я», и обе нужны, потому что код в чужой публичной
	// анкете видит кто угодно.
	//
	// Метка канала осталась, хотя канал теперь один: ею отсекаются куки,
	// выданные до 01.09.2026 каналом лички, — у тех кода в куке нет вовсе, и
	// человек получит честное «начните заново» вместо невнятного отказа сверки.
	codeCookie = "code"
)

// Канал доставки кода в куке проверки.
const chanProfile = "p" // код показан на экране, ждём его в поле «о себе»

// Номер анкеты из того, что человек вставил в поле. Люди копируют адрес из
// браузера чаще, чем переписывают цифры, поэтому разбираются обе формы.
var (
	// profileLinkRe — номер внутри ссылки: /profile/1493279/ и старая форма
	// /anketa1493279. Спрашивается ПЕРВЫМ, потому что у адреса бывает хвост
	// (/photos/6/, ?page=2), и «последнее число» тогда не анкета.
	profileLinkRe = regexp.MustCompile(`(?:/profile/|/anketa)(\d+)`)
	// digitsRe — просто числа, если ссылки не оказалось.
	digitsRe = regexp.MustCompile(`\d+`)
)

func parseProfileID(s string) int64 {
	if m := profileLinkRe.FindStringSubmatch(s); m != nil {
		return validProfileID(m[1])
	}
	// Иначе берём САМОЕ ДЛИННОЕ число, а не последнее: номер анкеты пяти-
	// семизначный, а рядом в строке попадаются короткие хвосты — номер
	// страницы, год, возраст. 18.08.2026 такой хвост увёл человека на
	// /profile/6/, и он получил «НГС не отвечает» вместо «проверьте номер».
	best := ""
	for _, d := range digitsRe.FindAllString(s, -1) {
		if len(d) > len(best) {
			best = d
		}
	}
	return validProfileID(best)
}

// validProfileID — число, годное в номер анкеты. Длина сверяется ДО разбора:
// иначе слишком длинное «число» просто не поместится в int64.
func validProfileID(s string) int64 {
	if s == "" || len(s) > 12 {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
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
				case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					// Человек ушёл со страницы, не дождавшись её, либо запрос
					// выбрал свой срок. Это не поломка, а самый частый исход
					// под наплывом: строка ERROR на каждый оборванный запрос
					// превратила бы лог в ту же нагрузку, от которой стоят
					// потолки (guard.go).
					s.log.Debug("сессия не прочитана: запрос прерван", "err", err)
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
	// Bot — адрес РюмкинЪа. Пусто означает «про этот путь не говорим»: звать в
	// бота, которого не назвали, некуда.
	Bot     string
	Problem string
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
		Bot:       s.cfg.Contacts.Bot,
		Problem:   problem,
	})
}

// codePage — экран «это вы?»: ник анкеты и что делать дальше, то есть вставить
// показанный код в поле «о себе» на НГС.
type codePage struct {
	page
	ProfileID int64
	Nick      string
	Avatar    string // наш путь /media/…, если этого человека знает зеркало
	// Gender — из анкеты НГС, и нужен он ровно для силуэта: фото зеркало знает
	// далеко не про всех, а входящий должен узнать на этом экране СЕБЯ.
	Gender platform.Gender
	// Code показывается на экране — и потому НЕ принимается введённым обратно
	// ни на одном экране: сверяется он чтением анкеты, а в форме проверки поля
	// для него нет вовсе.
	Code    string
	Problem string
}

// handleLoginStart читает анкету, заводит код и доставляет его.
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
		s.renderLogin(w, r, http.StatusNotFound,
			"Анкеты с номером "+strconv.FormatInt(id, 10)+" на НГС нет. "+
				"Нужны цифры из адреса вашей страницы — например, love.ngs.ru/profile/1493279/.")
		return
	}
	if err != nil {
		s.log.Error("чтение анкеты НГС", "profile", id, "err", err)
		s.renderLogin(w, r, http.StatusBadGateway,
			"НГС сейчас не отвечает. Попробуйте позже — или войдите по приглашению.")
		return
	}
	s.startByProfileField(w, r, id, prof)
}

// startByProfileField — единственный канал: код показывается на экране и его
// надо вставить в поле «о себе». Проверка двусторонняя, поэтому код кладётся и
// в куку: анкета докажет «анкета моя», кука — «проверку начал я».
func (s *Server) startByProfileField(w http.ResponseWriter, r *http.Request, id int64, prof SiteProfile) {
	ch, err := s.auth.StartProfileChallenge(r.Context(), id)
	if err != nil {
		s.oops(w, r, "выдача кода входа", err)
		return
	}
	s.setCookie(w, codeCookie,
		chanProfile+":"+strconv.FormatInt(id, 10)+":"+ch.Code, time.Until(ch.ExpiresAt))
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
		Gender:    prof.Gender,
		Code:      code,
		Problem:   problem,
	})
}

// handleLoginCheck сверяет код тем способом, каким его доставили.
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
			"Проверка устарела или начиналась в другом браузере. Начните заново.")
		return
	}
	prof, err := s.site.Profile(r.Context(), id)
	if err != nil {
		s.log.Error("чтение анкеты НГС", "profile", id, "err", err)
		s.renderCode(w, r, http.StatusBadGateway, id, SiteProfile{}, code,
			"НГС сейчас не отвечает. Код ещё жив — попробуйте через минуту.")
		return
	}
	if !s.checkCode(w, r, id, code, prof) {
		return
	}
	s.finishNGSLogin(w, r, id, prof)
}

// checkCode — сверка. false означает «ответ уже отправлен».
//
// Код берётся ИЗ КУКИ, а не из формы, и второго источника здесь быть не должно:
// принять показанный код введённым обратно — значит отдать чужую анкету в одно
// нажатие (см. шапку platform/auth.go).
func (s *Server) checkCode(w http.ResponseWriter, r *http.Request,
	id int64, code string, prof SiteProfile,
) bool {
	err := s.auth.VerifyProfileChallenge(r.Context(), id, code, prof.AboutMe)
	switch {
	case err == nil:
		return true
	case errors.Is(err, platform.ErrCodeMismatch):
		s.renderCode(w, r, http.StatusUnauthorized, id, prof, code,
			"Код не совпал. Проверьте, что вставили его целиком, вместе с «T3H-».")
	case errors.Is(err, platform.ErrCodeNotFound):
		// «Приватность» тут ни при чём: она прячет активность, а не «о себе»
		// (замер 18.08.2026, см. love.Profile.Hidden). А вот модерация — при чём:
		// правку анкеты НГС сначала одобряет человек.
		s.renderCode(w, r, http.StatusUnauthorized, id, prof, code,
			"В поле «о себе» кода пока нет. Правку анкеты НГС проверяет модератор — "+
				"возможно, она ещё не одобрена.")
	case errors.Is(err, platform.ErrNoChallenge):
		s.renderLogin(w, r, http.StatusUnauthorized,
			"Код устарел или был заменён новым. Начните заново.")
	case errors.Is(err, platform.ErrTooManyAttempts):
		s.renderCode(w, r, http.StatusTooManyRequests, id, prof, code,
			"Слишком много проверок подряд. Возьмите новый код через час.")
	default:
		s.oops(w, r, "проверка кода входа", err)
	}
	return false
}

// finishNGSLogin заводит участника и выдаёт сессию.
func (s *Server) finishNGSLogin(w http.ResponseWriter, r *http.Request, id int64, prof SiteProfile) {
	userID, err := s.auth.CompleteNGSLogin(r.Context(),
		platform.MirroredAuthor{ID: id, Nick: prof.Nick, AvatarURL: prof.AvatarURL}, prof.Gender)
	if errors.Is(err, platform.ErrAnonymized) {
		// Тупик, а не повод пробовать снова: код сверен, вход невозможен по
		// исполненному требованию субъекта. Поэтому отдельная страница, а не
		// экран проверки с кнопкой «ещё раз».
		s.fail(w, r, http.StatusForbidden,
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

// pendingCode разбирает куку проверки: «<канал>:<анкета>:<код>».
//
// Кука без кода не годится ни при каком канале: код в ней — вторая половина
// доказательства. Так же отсекается и кука канала лички, выданная до
// 01.09.2026: у неё метка «t» и кода нет вовсе.
func (s *Server) pendingCode(r *http.Request) (id int64, code string) {
	c, err := r.Cookie(s.cookieName(codeCookie))
	if err != nil {
		return 0, ""
	}
	channel, rest, ok := strings.Cut(c.Value, ":")
	if !ok || channel != chanProfile {
		return 0, ""
	}
	idStr, code, _ := strings.Cut(rest, ":")
	id, err = strconv.ParseInt(idStr, 10, 64)
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

// ---------------------------------------------------------------- вход из бота

// handleBotLogin впускает по одноразовой ссылке, выданной РюмкинЪом.
//
// Права на анкету здесь не проверяются и проверить их нечем: доказательством
// служит живая сессия НГС, которую бот держит у себя, а ключ — лишь её
// предъявление. Вся строгость поэтому в ядре: ключ одноразовый (гасится
// удалением строки), живёт десять минут и не годится обезличенному.
//
// Ключ уходит из адресной строки СРАЗУ, редиректом: иначе он остался бы в
// истории браузера и в логе прокси. Погашенный, он там уже безвреден, но
// приучать себя к секретам в URL не стоит — тот же довод, по которому код
// приглашения показывается в ответе на POST, а не после перехода.
func (s *Server) handleBotLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.me(r); ok {
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	userID, err := s.auth.RedeemBotLogin(r.Context(), r.URL.Query().Get("key"))
	switch {
	case err == nil:
	case errors.Is(err, platform.ErrBotKeyInvalid):
		// Три причины — не найден, истёк, уже использован — человеку значат одно
		// и то же, и разводить их в ответе значило бы рассказывать постороннему,
		// угадал он ключ или нет.
		s.renderLogin(w, r, http.StatusUnauthorized,
			"Ссылка не сработала: она одноразовая и живёт десять минут. "+
				"Попросите у РюмкинЪа новую — командой /site.")
		return
	default:
		s.oops(w, r, "вход по ссылке из бота", err)
		return
	}
	if _, err := s.auth.CompleteBotLogin(r.Context(), userID); err != nil {
		if errors.Is(err, platform.ErrAnonymized) {
			s.fail(w, r, http.StatusForbidden,
				"Данные этой анкеты обезличены по требованию её владельца. "+
					"Автоматически вернуть их нельзя — напишите администратору.")
			return
		}
		s.oops(w, r, "завершение входа из бота", err)
		return
	}
	if err := s.signIn(w, r, userID); err != nil {
		s.oops(w, r, "выдача сессии", err)
		return
	}
	http.Redirect(w, r, "/consent", http.StatusSeeOther)
}
