package web

// Страницы модератора и жалоба участника.
//
// Мера здесь одна, и она из решения по Ш7: очередь должна ЧИТАТЬСЯ ЗА МИНУТУ.
// Владелец сказал, что следить за тредами ему некогда, — значит интерфейс, ради
// которого приходится «заходить и разбираться», ничем не лучше отсутствия
// интерфейса. Отсюда одна страница со всем сразу, действия кнопками прямо в
// строке и никаких экранов подтверждения: скрытие обратимо, а лишнее нажатие в
// пересчёте на сутки дороже ошибки.
//
// Второе правило — модератор работает ТАМ, ГДЕ ЧИТАЕТ. Кнопки «скрыть» и
// «вернуть» стоят под каждой репликой на странице заметки, и скрытое ему видно
// в общем дереве (platform.Thread отдаёт модератору статус 2). Иначе решение
// принималось бы по цитате в очереди, без разговора вокруг, — а именно разговор
// вокруг и отличает ссору от угрозы.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"lovegw/internal/platform"
)

// Moderator — что морда умеет по части модерации. Четвёртый интерфейс рядом со
// Store (чтение публичных страниц), Auth (данные одного человека) и Writer
// (что участник вправе изменить), и разделение это не педантизм: список здесь —
// исчерпывающий ответ на вопрос «что площадка позволяет модератору». Правки
// чужого текста в нём нет, и это видно с одного взгляда.
//
// nil означает «модерации нет»: страницы /mod не существует, кнопок под
// репликами нет. Так морда поднимается и там, где ядро отдано ей только на
// чтение.
type Moderator interface {
	ReviewQueue(ctx context.Context, limit int) ([]platform.ReviewItem, error)
	AutoHidden(ctx context.Context, limit int) ([]platform.ReviewItem, error)
	ModerationStats(ctx context.Context) (platform.ModerationStats, error)
	AuditTail(ctx context.Context, limit int) ([]platform.AuditEntry, error)

	HideSubject(ctx context.Context, actor platform.Viewer, s platform.Subject, category, reason string) error
	RestoreSubject(ctx context.Context, actor platform.Viewer, s platform.Subject, reason string) error
	Decide(ctx context.Context, actor platform.Viewer, s platform.Subject, d platform.Decision, reason string) error
	SetThreadLocked(ctx context.Context, actor platform.Viewer, noteID int64, locked bool, reason string) error
	SetNotePinned(ctx context.Context, actor platform.Viewer, noteID int64, pinned bool, reason string) error

	BanUser(ctx context.Context, actor platform.Viewer, userID int64, until time.Time, reason string) error
	UnbanUser(ctx context.Context, actor platform.Viewer, userID int64, reason string) error
	SetRole(ctx context.Context, actor platform.Viewer, id int64, role platform.Role) error
	UserByID(ctx context.Context, id int64) (platform.User, error)

	// AddReport и Appeal — не модераторские действия, а входы УЧАСТНИКА в
	// модерацию: жалоба и просьба о пересмотре. Живут здесь, потому что ходят в
	// ту же очередь; Writer остаётся списком того, что человек публикует.
	AddReport(ctx context.Context, reporterID int64, s platform.Subject, reason string) error
	Appeal(ctx context.Context, userID int64, s platform.Subject) error
	MyHidden(ctx context.Context, userID int64, limit int) ([]platform.MyCheck, error)
}

// queueLimit — сколько строк показываем на странице очереди. Больше сотни она
// перестаёт быть очередью и становится отчётом, который никто не читает.
const queueLimit = 100

// auditLimit — сколько записей журнала на странице.
const auditLimit = 200

type modPage struct {
	page
	Stats      platform.ModerationStats
	Queue      []platform.ReviewItem
	AutoHidden []platform.ReviewItem
}

// handleMod — очередь модератора.
func (s *Server) handleMod(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.moderator(w, r); !ok {
		return
	}
	ctx := r.Context()
	stats, err := s.mod.ModerationStats(ctx)
	if err != nil {
		s.oops(w, r, "сводка модерации", err)
		return
	}
	queue, err := s.mod.ReviewQueue(ctx, queueLimit)
	if err != nil {
		s.oops(w, r, "очередь модерации", err)
		return
	}
	auto, err := s.mod.AutoHidden(ctx, queueLimit)
	if err != nil {
		s.oops(w, r, "скрытое автоматом", err)
		return
	}
	s.render(w, r, http.StatusOK, "mod.gohtml", modPage{
		page:       s.newPage(r, "Модерация"),
		Stats:      stats,
		Queue:      queue,
		AutoHidden: auto,
	})
}

type modLogPage struct {
	page
	Entries []platform.AuditEntry
}

// handleModLog — журнал действий.
//
// Показывается модератору, а не только администратору, и это осознанно:
// собственная работа обязана быть видна тому, кто её делает, иначе «кто это
// скрыл» выясняется перепиской. Решения автомата в том же списке — без них
// нельзя понять, что он натворил, пока никто не смотрел.
func (s *Server) handleModLog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.moderator(w, r); !ok {
		return
	}
	entries, err := s.mod.AuditTail(r.Context(), auditLimit)
	if err != nil {
		s.oops(w, r, "журнал модерации", err)
		return
	}
	s.render(w, r, http.StatusOK, "mod_log.gohtml", modLogPage{
		page:    s.newPage(r, "Журнал модерации"),
		Entries: entries,
	})
}

// handleModAct — одно действие модератора над публикацией или тредом.
//
// Одна ручка на все действия, потому что различаются они только глаголом, а
// проверки, разбор объекта и возврат на прежнюю страницу у них общие. Возврат
// именно на прежнюю (localPath от поля back): модератор работает и из очереди, и
// со страницы заметки, и выбрасывать его после каждого нажатия в одно место
// значило бы заставлять искать место заново.
func (s *Server) handleModAct(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.moderator(w, r)
	if !ok {
		return
	}
	actor := platform.Viewer{UserID: u.ID, Role: u.Role}
	subj, ok := subjectOf(r)
	if !ok && !noteAction[r.FormValue("do")] {
		s.fail(w, r, http.StatusBadRequest, "Непонятно, к чему относится действие.")
		return
	}
	ctx, reason := r.Context(), r.FormValue("reason")
	var err error
	switch r.FormValue("do") {
	case "hide":
		err = s.mod.HideSubject(ctx, actor, subj, r.FormValue("category"), reason)
	case "restore":
		err = s.mod.RestoreSubject(ctx, actor, subj, reason)
	case "keep":
		err = s.mod.Decide(ctx, actor, subj, platform.DecisionKeep, reason)
	case "drop":
		err = s.mod.Decide(ctx, actor, subj, platform.DecisionHide, reason)
	case "lock", "unlock":
		err = s.mod.SetThreadLocked(ctx, actor, noteFormID(r), r.FormValue("do") == "lock", reason)
	case "pin", "unpin":
		err = s.mod.SetNotePinned(ctx, actor, noteFormID(r), r.FormValue("do") == "pin", reason)
	default:
		s.fail(w, r, http.StatusBadRequest, "Неизвестное действие.")
		return
	}
	s.afterModAction(w, r, err)
}

// noteAction — действия над ТРЕДОМ целиком: объект у них не «заметка или
// комментарий» из subjectOf, а номер заметки в отдельном поле формы.
var noteAction = map[string]bool{"lock": true, "unlock": true, "pin": true, "unpin": true}

func noteFormID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.FormValue("note"), 10, 64)
	return id
}

// afterModAction — общий ответ на действие. «Состояние уже такое» отказом не
// считается: два модератора, нажавших одно и то же, не должны видеть ошибку.
func (s *Server) afterModAction(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil, errors.Is(err, platform.ErrNothingToDo):
		http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
	case errors.Is(err, platform.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "Этой записи больше нет.")
	case errors.Is(err, platform.ErrNotModerator):
		s.fail(w, r, http.StatusForbidden, "Нужны права модератора.")
	case errors.Is(err, platform.ErrTooManyPinned):
		// Потолок закреплённых — правило ленты, а не поломка: говорим прямо,
		// сколько их бывает, чтобы модератор снял лишнее, а не гадал.
		s.fail(w, r, http.StatusBadRequest, err.Error()+". Сначала открепите лишнее.")
	default:
		s.oops(w, r, "действие модератора", err)
	}
}

// ---------------------------------------------------------------- участник

type modUserPage struct {
	page
	Member platform.User
	Banned bool
	// Admin — смотрящий вправе раздавать роли. Право скрывать чужие слова не
	// должно размножаться само, поэтому выдаёт его только администратор.
	Admin bool
}

// handleModUser — карточка участника: запрет писать и права.
//
// Отдельной страницы участника у площадки нет вовсе (на НГС её роль играет
// анкета, а своей мы не заводили), поэтому карточка живёт под /mod и видна
// только модератору. Показывать её всем значило бы завести профиль, которого в
// эпике E нет, — а заодно выставить напоказ запреты и роли.
func (s *Server) handleModUser(w http.ResponseWriter, r *http.Request) {
	me, ok := s.moderator(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такого участника нет.")
		return
	}
	member, err := s.mod.UserByID(r.Context(), id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такого участника нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "карточка участника", err)
		return
	}
	s.render(w, r, http.StatusOK, "mod_user.gohtml", modUserPage{
		page:   s.newPage(r, "Участник"),
		Member: member,
		Banned: member.Banned(time.Now()),
		Admin:  me.Role >= platform.RoleAdmin,
	})
}

// banDays — на сколько суток запрещаем писать по умолчанию. Тридцать — не наша
// выдумка: ровно такой срок ставит НГС, и в архиве это видно прямо (у одного
// участника 18 запретов подряд по 30 суток). Преемственность здесь дороже
// оригинальности.
const banDays = 30

// maxBanDays — потолок срока. Год — это уже не «остыть», а «уйти», и такое
// решение принимается не нажатием кнопки.
const maxBanDays = 365

func (s *Server) handleModUserAct(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.moderator(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такого участника нет.")
		return
	}
	actor := platform.Viewer{UserID: u.ID, Role: u.Role}
	ctx, reason := r.Context(), r.FormValue("reason")
	switch r.FormValue("do") {
	case "ban":
		days, _ := strconv.Atoi(r.FormValue("days"))
		if days <= 0 {
			days = banDays
		}
		if days > maxBanDays {
			days = maxBanDays
		}
		err = s.mod.BanUser(ctx, actor, id, time.Now().AddDate(0, 0, days), reason)
	case "unban":
		err = s.mod.UnbanUser(ctx, actor, id, reason)
	case "role":
		// Роли раздаёт только администратор: право скрывать чужие слова не
		// должно размножаться само.
		if u.Role < platform.RoleAdmin {
			s.fail(w, r, http.StatusForbidden, "Права выдаёт администратор.")
			return
		}
		role, ok := roleOf(r.FormValue("role"))
		if !ok {
			s.fail(w, r, http.StatusBadRequest, "Такой роли нет.")
			return
		}
		err = s.mod.SetRole(ctx, actor, id, role)
	default:
		s.fail(w, r, http.StatusBadRequest, "Неизвестное действие.")
		return
	}
	s.afterModAction(w, r, err)
}

func roleOf(s string) (platform.Role, bool) {
	switch s {
	case "user":
		return platform.RoleUser, true
	case "moderator":
		return platform.RoleModerator, true
	case "admin":
		return platform.RoleAdmin, true
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------- жалоба

type reportPage struct {
	page
	Subject platform.Subject
	NoteID  int64
	Problem string
}

// handleReport — форма жалобы.
//
// Отдельной страницей, а не раскрывающимся блоком под репликой: жалоба это
// текст, а поле ввода под каждой из девятисот реплик — то же самое, чего мы
// избежали у реакций. Заодно страница объясняет, что произойдёт дальше, — без
// этого «пожаловаться» читается как «удалить».
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if s.mod == nil || u.Kind != platform.KindMember {
		s.fail(w, r, http.StatusForbidden, "Жаловаться может только участник площадки.")
		return
	}
	subj, ok := subjectOf(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Непонятно, на что жалоба.")
		return
	}
	noteID, _ := strconv.ParseInt(r.FormValue("note"), 10, 64)
	s.render(w, r, http.StatusOK, "report.gohtml", reportPage{
		page:    s.newPage(r, "Жалоба"),
		Subject: subj,
		NoteID:  noteID,
	})
}

func (s *Server) handleReportSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	if s.mod == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Жалобы сейчас не принимаются.")
		return
	}
	subj, ok := subjectOf(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Непонятно, на что жалоба.")
		return
	}
	noteID, _ := strconv.ParseInt(r.FormValue("note"), 10, 64)
	err := s.mod.AddReport(r.Context(), u.ID, subj, r.FormValue("reason"))
	problem := ""
	switch {
	case err == nil, errors.Is(err, platform.ErrNothingToDo):
		// Повторная жалоба на то же — не ошибка: человек просто не помнит, что
		// уже жаловался, и говорить ему «отказано» незачем.
		http.Redirect(w, r, noteURL(noteID, false, 1), http.StatusSeeOther)
		return
	case errors.Is(err, platform.ErrNotFound):
		problem = "Этой записи больше нет."
	case errors.Is(err, platform.ErrSelfReport):
		problem = "Это ваша собственная запись."
	case errors.Is(err, platform.ErrRateLimited):
		problem = "У вас уже слишком много нерассмотренных жалоб. Дождитесь ответа по прежним."
	case errors.Is(err, platform.ErrBanned):
		problem = "Вам сейчас запрещены публикации."
	case errors.Is(err, platform.ErrHiddenAll):
		// Жалоба заводит работу живому человеку, поэтому цена входа у неё та же,
		// что у собственных слов, — и отказ здесь ровно тот же, что при
		// публикации, а не наша поломка.
		problem = "Вы отозвали согласие на распространение. Верните его на своей странице."
	case errors.Is(err, platform.ErrNotMember):
		problem = "Жаловаться может только участник площадки."
	default:
		s.oops(w, r, "жалоба", err)
		return
	}
	s.render(w, r, http.StatusBadRequest, "report.gohtml", reportPage{
		page:    s.newPage(r, "Жалоба"),
		Subject: subj,
		NoteID:  noteID,
		Problem: problem,
	})
}

// ---------------------------------------------------------------- пересмотр

// handleAppeal — «прошу человека посмотреть ещё раз».
//
// Кнопка обязательна, а не любезна: автомат ошибается, и молча исчезнувшая
// реплика — худшее, что можно сделать с сообществом, которое только что
// переехало.
func (s *Server) handleAppeal(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		s.fail(w, r, http.StatusUnauthorized, "Чтобы просить пересмотра, нужно войти.")
		return
	}
	if s.mod == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Пересмотр сейчас недоступен.")
		return
	}
	subj, ok := subjectOf(r)
	if !ok {
		s.fail(w, r, http.StatusNotFound, "Непонятно, что пересматривать.")
		return
	}
	err := s.mod.Appeal(r.Context(), u.ID, subj)
	if err != nil && !errors.Is(err, platform.ErrNoAppeal) {
		s.oops(w, r, "просьба о пересмотре", err)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// ---------------------------------------------------------------- общее

// moderator — вошедший с правами модератора. Пустой второй результат означает
// «ответ уже отправлен».
//
// Гостю отвечаем «такой страницы нет», а не «нужны права»: существование
// закрытой двери — само по себе сведения, и постороннему их знать незачем.
func (s *Server) moderator(w http.ResponseWriter, r *http.Request) (platform.User, bool) {
	u, ok := s.me(r)
	if !ok || u.Role < platform.RoleModerator || s.mod == nil {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return platform.User{}, false
	}
	return u, true
}

// modAct — аргумент шаблона «modbar»: всё, что нужно полоске действий под одной
// публикацией. Собирается в Go, а не в шаблоне, потому что решение «что кому
// показать» одно на заметку и на комментарий: разъехавшись, эти две ветки дали
// бы модератору кнопку под заметкой и ничего под репликой.
type modAct struct {
	Subject     platform.Subject
	NoteID      int64
	AuthorID    int64
	Hidden      bool
	CanModerate bool
	CanReport   bool
	CSRF        string
	Back        string
}

// modNote — полоска под самой заметкой.
func modNote(p notePage) modAct {
	return modAct{
		Subject:     platform.NoteSubject(p.Note.ID),
		NoteID:      p.Note.ID,
		AuthorID:    p.Note.Author.ID,
		Hidden:      p.Note.Status == platform.StatusHiddenMod,
		CanModerate: p.CanModerate,
		// Пожаловаться на СВОЮ публикацию нельзя, и «пожаловаться» под чужой
		// показываем только тому, кто вправе писать: жалоба заводит работу
		// живому человеку, и цена входа у неё та же, что у собственных слов.
		CanReport: p.CanWrite && !p.CanModerate && !p.Note.Own,
		CSRF:      p.CSRF,
		Back:      p.Back,
	}
}

// modComment — полоска под комментарием.
func modComment(p notePage, c platform.CommentView) modAct {
	return modAct{
		Subject:     platform.CommentSubject(c.ID),
		NoteID:      c.NoteID,
		AuthorID:    c.Author.ID,
		Hidden:      c.Status == platform.StatusHiddenMod,
		CanModerate: p.CanModerate,
		CanReport:   p.CanWrite && !p.CanModerate && !c.Own,
		CSRF:        p.CSRF,
		Back:        p.Back,
	}
}

// decArg — аргумент кнопок решения в очереди: объект плюс то, что нужно любой
// пишущей форме. Собирается функцией, потому что шаблон принимает одно значение,
// а нужны три.
type decArg struct {
	Subject platform.Subject
	CSRF    string
	Back    string
}

func decisionArg(p modPage, it platform.ReviewItem) decArg {
	return decArg{Subject: it.Subject, CSRF: p.CSRF, Back: p.Back}
}

// subjectOf собирает объект действия из формы или строки запроса. Пусто —
// объект не назван или назван неизвестным видом.
func subjectOf(r *http.Request) (platform.Subject, bool) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return platform.Subject{}, false
	}
	s := platform.Subject{Kind: r.FormValue("kind"), ID: id}
	return s, s.Valid()
}
