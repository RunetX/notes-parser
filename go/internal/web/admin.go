package web

// Раздел администратора. Пока в нём одно дело — ПРИГЛАШЕНИЯ, и заведён он ради
// него (24.08.2026, просьба владельца: «не хочу лезть в консоль на
// production»).
//
// Довод тот же, по которому в морде появилась очередь модерации: впустить
// человека — работа обычная и частая, а до сих пор она требовала ssh на боевой
// хост и команды с флагами. Лишний повод открыть там консоль дороже ошибки в
// форме: ошибку видно на месте, и она отзывается кнопкой.
//
// Отдельная страница, а не пункт в /mod: модератор решает про СЛОВА,
// администратор — про то, кто их здесь пишет, и смешивать эти два стола значило
// бы показывать модератору дверь, в которую ему нельзя. Постороннему (включая
// модератора) страницы не существует — «нет такой страницы», а не «нужны
// права»: существование закрытой двери само по себе сведения.
//
// Чего здесь нет и не будет: показа уже выданного кода. В базе лежит его
// sha256, «покажите ещё раз» невозможно физически — и это свойство, а не
// неудобство. Потерянный код отзывается и выдаётся заново.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

const (
	// inviteDays — срок приглашения по умолчанию. Месяц: код диктуют в
	// переписке, а переезжает человек не в тот же вечер.
	inviteDays = 30
	// maxInviteDays — потолок поля. Дальше срок режет уже ядро, здесь он стоит
	// затем, чтобы опечатка в поле не превратилась в вечный ключ.
	maxInviteDays = 365
)

type adminPage struct {
	page
	Invites []inviteRow
	// Code — ТОЛЬКО ЧТО выданный код. Живёт ровно один показ: перезагрузка
	// страницы его не покажет, потому что в базе его нет.
	Code string
	// CodeFor — кому он выдан, словами: страница обязана назвать это вслух,
	// иначе опечатка в номере обнаружится, когда код уже уехал в переписку.
	CodeFor string
	// CodeWarn — привязка к тому, кто уже входил: код даёт доступ к живой
	// учётной записи, и сказать об этом надо на экране выдачи, а не в справке.
	CodeWarn bool
	// InviteURL — куда нести код. Печатается рядом, чтобы администратор
	// копировал строку целиком, а не вспоминал адрес страницы входа.
	InviteURL string
	Days      int
	Problem   string
	// Personas — жители площадки. Правится здесь одно: ФОТО. Ник и биографию
	// пишет владелец в карточке (data/narod/cards), а на площадку их приносит
	// `narod enroll` — редактировать характер из веб-формы значило бы завести
	// второе место, где он живёт.
	Personas []platform.PersonaRow
	// CanShot — площадка сейчас принимает файлы. Нет перекодировщика — нет и
	// поля выбора: кнопка, ведущая к отказу, хуже её отсутствия.
	CanShot bool
	// Told — что сделано с фото: короткая строка над списком. Отдельно от
	// Problem, потому что это удача, а не отказ.
	Told string
}

// inviteRow — строка списка. Обёртка над platform.Invite, потому что странице
// нужны две вещи, которых у ядра нет: чем НАЗВАТЬ строку в форме отзыва и жива
// ли она СЕЙЧАС. Оба ответа считаются в Go, а не в шаблоне: «сейчас» — это одно
// время на всю страницу, а не своё у каждой кнопки.
type inviteRow struct {
	platform.Invite
	Key  string
	Live bool
}

// inviteKeyFormat — как форма называет строку приглашения. Время выдачи, а не
// идентификатор: своего у приглашения нет, а хеш кода показывать нельзя (по
// нему код подбирается перебором — см. platform.RevokeInvite).
const inviteKeyFormat = time.RFC3339Nano

// admin — вошедший с правами администратора. Пустой второй результат означает
// «ответ уже отправлен».
func (s *Server) admin(w http.ResponseWriter, r *http.Request) (platform.User, bool) {
	u, ok := s.me(r)
	if !ok || u.Role < platform.RoleAdmin || s.mod == nil {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return platform.User{}, false
	}
	return u, true
}

// handleAdmin — страница раздела.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.admin(w, r); !ok {
		return
	}
	s.renderAdmin(w, r, http.StatusOK, adminPage{})
}

// renderAdmin дорисовывает общую часть и список. Одним местом, потому что
// страница показывается трижды — обычным заходом, после выдачи (с кодом) и
// после отказа (с объяснением), — и список в ней каждый раз свежий.
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, status int, p adminPage) {
	p.page = s.newPage(r, "Администрирование")
	if p.Days == 0 {
		p.Days = inviteDays
	}
	p.InviteURL = strings.TrimSuffix(s.cfg.BaseURL, "/") + "/login/invite"
	p.CanShot = s.takesShots()
	// Отказ списка жителей страницу не рушит и — главное — не отменяет только
	// что выданный код: он в этом ответе единственный раз. Тот же довод, что у
	// приглашений строкой ниже.
	people, err := s.mod.Personas(r.Context())
	if err != nil {
		s.log.Error("список жителей", "err", err)
		if p.Problem == "" {
			p.Problem = "Список жителей прочитать не удалось — это про список, а не про остальное."
		}
	}
	p.Personas = people
	list, err := s.mod.Invites(r.Context(), platform.InviteListLimit)
	if err != nil {
		// Отказ списка — не повод потерять только что выданный код: он в этом
		// ответе единственный раз и восстановлению не подлежит. Но и молчать
		// нельзя: пустой список читается как «не выдавали», то есть врёт.
		s.log.Error("список приглашений", "err", err)
		if p.Problem == "" {
			p.Problem = "Список выданных прочитать не удалось — это про список, а не про выдачу."
		}
	}
	now := time.Now()
	p.Invites = make([]inviteRow, 0, len(list))
	for _, in := range list {
		p.Invites = append(p.Invites, inviteRow{
			Invite: in,
			Key:    in.CreatedAt.UTC().Format(inviteKeyFormat),
			Live:   in.Live(now),
		})
	}
	s.render(w, r, status, "mod_admin.gohtml", p)
}

// handleAdminAvatar — фото жителя: поставить или снять.
//
// Своим маршрутом, а не веткой в handleAdminAct, по устройству net/http: форма
// с файлом приезжает multipart, а ParseForm на нём выходит без ошибки, оставляя
// r.Form пустой картой, — то есть общий вход для обеих форм не «менее удобен»,
// а сломан молча (см. postUpload).
func (s *Server) handleAdminAvatar(w http.ResponseWriter, r *http.Request) {
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	// Слот перекодирования занимается ДО чтения тела, как и у публикации:
	// память в контейнере одна на всех, и кто прислал файл — участник или
	// администратор, — ей безразлично.
	release, ok := s.takeShotSlot(w, r)
	if !ok {
		return
	}
	defer release()
	if !s.postUpload(w, r) {
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusBadRequest, "Непонятно, кому меняем фото.")
		return
	}
	actor := platform.Viewer{UserID: u.ID, Role: u.Role}
	if r.FormValue("do") == "avatar_off" {
		s.afterAvatar(w, r, s.mod.SetPersonaAvatar(r.Context(), actor, id, nil, r.FormValue("reason")), "Фото снято.")
		return
	}
	shot, problem := s.takeShotSide(r.Context(), r, avatarSide)
	switch {
	case problem != "":
		s.renderAdmin(w, r, http.StatusBadRequest, adminPage{Problem: problem})
		return
	case shot == nil:
		s.renderAdmin(w, r, http.StatusBadRequest, adminPage{Problem: "Файл не выбран."})
		return
	}
	s.afterAvatar(w, r, s.mod.SetPersonaAvatar(r.Context(), actor, id, shot, r.FormValue("reason")), "Фото поставлено.")
}

// afterAvatar — общий ответ обеих кнопок. Страницей, а не переходом: список
// жителей на ней и есть подтверждение, а отказ надо показать словами.
func (s *Server) afterAvatar(w http.ResponseWriter, r *http.Request, err error, told string) {
	switch {
	case err == nil:
		s.renderAdmin(w, r, http.StatusOK, adminPage{Told: told})
	case errors.Is(err, platform.ErrNotPersona):
		s.renderAdmin(w, r, http.StatusForbidden, adminPage{
			Problem: "Это анкета живого человека. Фото ему ставит он сам — со своей страницы, из анкеты НГС."})
	case errors.Is(err, platform.ErrNotFound):
		s.renderAdmin(w, r, http.StatusNotFound, adminPage{Problem: "Такого жителя нет."})
	case errors.Is(err, platform.ErrNotAdmin):
		s.fail(w, r, http.StatusForbidden, "Фото жителям ставит администратор.")
	default:
		s.oops(w, r, "фото жителя", err)
	}
}

// handleAdminAct — выдать или отозвать приглашение.
func (s *Server) handleAdminAct(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.admin(w, r)
	if !ok {
		return
	}
	actor := platform.Viewer{UserID: u.ID, Role: u.Role}
	switch r.FormValue("do") {
	case "issue":
		s.issueInvite(w, r, actor)
	case "revoke":
		at, err := time.Parse(inviteKeyFormat, r.FormValue("issued"))
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "Непонятно, какое приглашение отзывать.")
			return
		}
		// afterModAction, а не свой ответ: «состояние уже такое» (успели
		// воспользоваться, отозвал второй администратор) отказом не считается.
		s.afterModAction(w, r, s.mod.RevokeInvite(r.Context(), actor, at))
	default:
		s.fail(w, r, http.StatusBadRequest, "Неизвестное действие.")
	}
}

// issueInvite — выдача. Ответом идёт САМА страница, а не переход на неё:
// показать код можно только здесь и только сейчас, а редирект унёс бы его в
// адресную строку, то есть в историю браузера и в логи Caddy.
func (s *Server) issueInvite(w http.ResponseWriter, r *http.Request, actor platform.Viewer) {
	days, _ := strconv.Atoi(r.FormValue("days"))
	switch {
	case days <= 0:
		days = inviteDays
	case days > maxInviteDays:
		days = maxInviteDays
	}
	refuse := func(problem string) {
		s.renderAdmin(w, r, http.StatusBadRequest, adminPage{Problem: problem, Days: days})
	}
	var bind int64
	if raw := strings.TrimSpace(r.FormValue("bind")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			refuse("Номер участника — это число. Оставьте поле пустым, если человек на площадке ещё не появлялся.")
			return
		}
		bind = id
	}
	code, err := s.mod.IssueInvite(r.Context(), actor, bind,
		r.FormValue("label"), time.Duration(days)*24*time.Hour)
	switch {
	case errors.Is(err, platform.ErrNotFound):
		refuse("Такого участника на площадке нет. Проверьте номер: у переехавшего с НГС он равен номеру его анкеты.")
		return
	case errors.Is(err, platform.ErrAnonymized):
		refuse("Данные этого участника обезличены по его требованию — привязать к ним никого нельзя.")
		return
	case errors.Is(err, platform.ErrNotAdmin):
		s.fail(w, r, http.StatusForbidden, "Приглашения выдаёт администратор.")
		return
	case err != nil:
		s.oops(w, r, "выдача приглашения", err)
		return
	}
	who, member := s.inviteTarget(r, bind)
	s.renderAdmin(w, r, http.StatusOK, adminPage{
		Code: code, CodeFor: who, CodeWarn: member, Days: days,
	})
}

// inviteTarget — кому выдан код: словами и с признаком «этот уже входил».
//
// Ходит в базу за ником, потому что в форме стоял НОМЕР, а сверять
// администратор будет с человеком, которого знает по имени. Второй ответ — не
// запрет: привязка к участнику это штатный способ вернуть доступ потерявшему
// анкету на НГС, но код в этом случае открывает живую учётную запись, и сказать
// об этом надо на экране выдачи.
func (s *Server) inviteTarget(r *http.Request, bind int64) (string, bool) {
	if bind == 0 {
		return "без привязки — пришедший заведётся на площадке с нуля и сам назовёт ник", false
	}
	num := "№" + strconv.FormatInt(bind, 10)
	u, err := s.mod.UserByID(r.Context(), bind)
	if err != nil || u.Nick == "" {
		return "участнику " + num, false
	}
	return u.Nick + " (" + num + ")", u.Kind == platform.KindMember
}
