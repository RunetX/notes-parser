package web

// Запись: заметка, ответ в тред, правка своей заметки в окне, смена своего ника.
//
// Ядро решает, МОЖНО ли (write.go в platform), морда — как об этом спросить и
// что сказать в ответ. Здесь нет ни одного правила, которого нет в ядре: иначе
// они разъедутся, и страница покажет кнопку, отвечающую отказом.
//
// Ни одна форма не теряет набранный текст при отказе — человек, у которого
// пропала реплика, второй раз её не напишет.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"lovegw/internal/platform"
)

// Writer — что морда умеет ИЗМЕНЯТЬ. Третий интерфейс рядом со Store (чтение
// публичных страниц) и Auth (вход и согласия), и разделение это не педантизм:
// список здесь — исчерпывающий ответ на вопрос «что площадка позволяет
// участнику». Правки и удаления чужого в нём нет, и это видно с одного взгляда.
type Writer interface {
	CreateNote(ctx context.Context, in platform.NewNote) (int64, error)
	CreateComment(ctx context.Context, in platform.NewComment) (int64, error)
	EditNote(ctx context.Context, userID, noteID int64, body string) error
	SetOwnNick(ctx context.Context, userID int64, nick string) error
	// SetOwnAvatar — поставить себе фото, забранное из анкеты НГС: url, откуда
	// оно взято, и сами байты. Своего файла площадка не принимает вовсе, поэтому
	// другого способа сменить здесь фото нет (Ш5д).
	SetOwnAvatar(ctx context.Context, userID int64, url string, data []byte) error
	// React — нажать, переключить или снять реакцию. Правкой чужого это не
	// является: строка своя, а объект остаётся нетронутым.
	React(ctx context.Context, in platform.NewReaction) error
}

// composePageName — один шаблон и на новую заметку, и на правку: поля те же, и
// две копии разошлись бы на первой же правке.
const composePageName = "compose.gohtml"

// composePage — форма заметки: новой или правки.
type composePage struct {
	page
	Note    platform.NoteView // при правке; нулевая у новой
	Editing bool
	// Admin — правит АДМИНИСТРАТОР, а не автор в своём окне. Экран от этого
	// меняется: условий авторского окна тут нет вовсе, зато появляется поле
	// «зачем» — оно уходит в журнал, и без него правка чужого текста осталась бы
	// записью «кто-то что-то поправил».
	Admin     bool
	Body      string
	Anonymous bool
	Problem   string
}

// ---------------------------------------------------------------- новая заметка

func (s *Server) handleNewNote(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	s.render(w, r, http.StatusOK, composePageName, composePage{
		page: s.newPage(r, "Новая заметка"),
	})
}

func (s *Server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	body := r.FormValue("body")
	anon := r.FormValue("anonymous") != ""
	id, err := s.wr.CreateNote(r.Context(), platform.NewNote{
		AuthorID: u.ID, Anonymous: anon, Body: body,
	})
	if err != nil {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "публикация заметки", err)
			return
		}
		s.render(w, r, status, composePageName, composePage{
			page: s.newPage(r, "Новая заметка"), Body: body, Anonymous: anon, Problem: problem,
		})
		return
	}
	videoWarm(id, body)
	http.Redirect(w, r, "/n/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// ---------------------------------------------------------------- правка заметки

func (s *Server) handleEditNote(w http.ResponseWriter, r *http.Request) {
	_, note, asAdmin, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, composePageName, composePage{
		page:      s.newPage(r, "Правка заметки"),
		Note:      note,
		Editing:   true,
		Admin:     asAdmin,
		Body:      note.Body,
		Anonymous: note.Anonymous,
	})
}

func (s *Server) handleUpdateNote(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, note, asAdmin, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	body := r.FormValue("body")
	var err error
	if asAdmin {
		err = s.mod.EditNoteAsAdmin(r.Context(), platform.Viewer{UserID: u.ID, Role: u.Role},
			note.ID, body, r.FormValue("reason"))
	} else {
		err = s.wr.EditNote(r.Context(), u.ID, note.ID, body)
	}
	// «Текст и так такой» отказом не считается: администратор мог открыть форму
	// и закрыть её, ничего не изменив, — то же правило, что у кнопок модерации.
	if err != nil && !errors.Is(err, platform.ErrNothingToDo) {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "правка заметки", err)
			return
		}
		s.render(w, r, status, composePageName, composePage{
			page: s.newPage(r, "Правка заметки"), Note: note, Editing: true, Admin: asAdmin,
			Body: body, Anonymous: note.Anonymous, Problem: problem,
		})
		return
	}
	// Ролик, названный в новом тексте, показывается карточкой — но только когда
	// превью уже лежит рядом. Правка подталкивает закачку ровно так же, как
	// публикация: иначе карточку увидел бы не тот, кто пришёл читать первым.
	videoWarm(note.ID, body)
	http.Redirect(w, r, "/n/"+strconv.FormatInt(note.ID, 10), http.StatusSeeOther)
}

// editTarget — заметка, которую этому человеку сейчас можно править, и КЕМ он
// её правит: автором в своём окне или администратором. Пустой последний
// результат означает «ответ уже отправлен».
//
// Одна дорога на оба случая, потому что различаются они только правом и текстом
// на экране: форма, адрес и разбор ответа общие, а две копии разошлись бы на
// первой же правке — ровно как разошлись бы правило страницы и правило ядра,
// если бы Editable считался в двух местах.
func (s *Server) editTarget(w http.ResponseWriter, r *http.Request) (platform.User, platform.NoteView, bool, bool) {
	var none platform.NoteView
	u, ok := s.me(r)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			s.fail(w, r, http.StatusUnauthorized, "Чтобы писать, нужно войти.")
		}
		return platform.User{}, none, false, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, false, false
	}
	admin := u.Role >= platform.RoleAdmin && s.mod != nil
	note, err := s.st.NoteViewByID(r.Context(), platform.Viewer{UserID: u.ID, Role: u.Role}, id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, false, false
	}
	if err != nil {
		s.oops(w, r, "чтение заметки", err)
		return u, none, false, false
	}
	// Скрытая заметка для читателя отсутствует — как и на её собственной
	// странице. Администратору скрытое модерацией видно: снять из текста лишнее
	// и вернуть его людям — это одно действие в два нажатия.
	if note.Status != platform.StatusVisible &&
		!(admin && note.Status == platform.StatusHiddenMod) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, false, false
	}

	switch {
	case note.Editable(time.Now()):
		// Своё окно. Условие считает Editable — то же самое, что проверит ядро,
		// чтобы кнопка не отвечала отказом.
		if s.wr == nil {
			s.fail(w, r, http.StatusServiceUnavailable, "Запись сейчас недоступна.")
			return u, none, false, false
		}
		return u, note, false, true
	case admin && platform.IsNative(note.ID):
		return u, note, true, true
	case admin:
		s.fail(w, r, http.StatusForbidden,
			"Эта заметка пришла с НГС — здесь её копия, и текст копии не правится. "+
				"Иначе страница молча разошлась бы с оригиналом.")
	case !note.Own:
		s.fail(w, r, http.StatusForbidden, "Это не ваша заметка.")
	default:
		s.fail(w, r, http.StatusForbidden,
			"Править заметку можно только первые десять минут и только пока под ней нет ответов. "+
				"Дальше текст остаётся как есть: под ним уже отвечают вам.")
	}
	return u, none, false, false
}

// ---------------------------------------------------------------- ответ в тред

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	noteID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || noteID <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	replyTo, _ := strconv.ParseInt(r.FormValue("reply_to"), 10, 64)
	body := r.FormValue("body")

	id, err := s.wr.CreateComment(r.Context(), platform.NewComment{
		NoteID:    noteID,
		AuthorID:  u.ID,
		Body:      body,
		ReplyToID: replyTo,
	})
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
			return
		}
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "публикация комментария", err)
			return
		}
		// Заметку перерисовываем целиком, но с набранным текстом: потерять
		// написанное — худшее, что можно сделать с ответом в живом разговоре.
		s.showNote(w, r, noteID, status, compose{Body: body, ReplyTo: replyTo, Problem: problem})
		return
	}
	// Сходить за превью ролика, если он в тексте назван. Здесь, а не на показе:
	// человек после публикации сразу оказывается на своей реплике, и ждать,
	// пока карточку «откроет» второй читатель, ему незачем (preview.go).
	videoWarm(id, body)
	http.Redirect(w, r, noteURL(noteID, s.threadLinear(w, r), 1)+"#c"+strconv.FormatInt(id, 10),
		http.StatusSeeOther)
}

// ---------------------------------------------------------------- свой ник

// handleNick — смена собственного ника.
//
// Под запрет «участник только пишет» она не попадает: это не публикация, а
// собственное имя. Площадка на переименование рассчитана с самого начала — ник
// хранится ТЕКУЩИЙ и дорисовывается везде, включая обращения к вам в чужих
// ответах, — и текст согласия обещает эту возможность прямо.
func (s *Server) handleNick(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	err := s.wr.SetOwnNick(r.Context(), u.ID, r.FormValue("nick"))
	switch {
	case err == nil:
		http.Redirect(w, r, "/me", http.StatusSeeOther)
	case errors.Is(err, platform.ErrBadNick):
		s.fail(w, r, http.StatusBadRequest,
			"Такой ник не годится: он пустой, слишком длинный или содержит невидимые знаки.")
	default:
		s.oops(w, r, "смена ника", err)
	}
}

// ---------------------------------------------------------------- общее

// writer — вошедший участник, которому можно писать. Пустой второй результат
// означает «ответ уже отправлен».
//
// Тень сюда не проходит: за ней никто не доказал владения анкетой. На практике
// это состояние встречается редко (вход доводит человека до участника), но
// проверка стоит и здесь, и в ядре — снаружи ради внятного текста, внутри ради
// того, чтобы правило нельзя было обойти вовсе.
func (s *Server) writer(w http.ResponseWriter, r *http.Request) (platform.User, bool) {
	u, ok := s.me(r)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			s.fail(w, r, http.StatusUnauthorized, "Чтобы писать, нужно войти.")
		}
		return platform.User{}, false
	}
	if s.wr == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "Запись сейчас недоступна.")
		return platform.User{}, false
	}
	if u.Kind != platform.KindMember {
		s.fail(w, r, http.StatusForbidden, "Писать может только участник площадки.")
		return platform.User{}, false
	}
	return u, true
}

// writeProblem переводит отказ ядра в текст для человека. Пустая строка
// означает «это не отказ по правилам, а поломка» — такое уходит в oops.
func writeProblem(err error) (int, string) {
	switch {
	case errors.Is(err, platform.ErrEmptyBody):
		return http.StatusBadRequest, "Пустой текст."
	case errors.Is(err, platform.ErrTooLong):
		return http.StatusBadRequest, "Текст слишком длинный."
	case errors.Is(err, platform.ErrRateLimited):
		return http.StatusTooManyRequests, "Слишком часто. Подождите немного и попробуйте снова."
	case errors.Is(err, platform.ErrThreadLocked):
		return http.StatusForbidden, "Обсуждение закрыто модератором."
	case errors.Is(err, platform.ErrBanned):
		return http.StatusForbidden, "Публикации вам сейчас запрещены."
	case errors.Is(err, platform.ErrConsentRevoked):
		return http.StatusForbidden,
			"Вы отозвали согласие. Верните его на своей странице, и можно будет писать снова — " +
				"но прежние заметки останутся без подписи: обезличивание необратимо."
	case errors.Is(err, platform.ErrConsentOutdated):
		return http.StatusForbidden,
			"Соглашения обновились: страницы площадки теперь открыты поисковым системам. " +
				"Откройте «Мою страницу» и подпишите новую редакцию — после этого можно писать дальше."
	case errors.Is(err, platform.ErrNotMember):
		return http.StatusForbidden, "Писать может только участник площадки."
	case errors.Is(err, platform.ErrBadReaction):
		return http.StatusBadRequest, "Такой реакции нет."
	case errors.Is(err, platform.ErrNotYours):
		return http.StatusForbidden, "Это не ваша запись."
	case errors.Is(err, platform.ErrNotAdmin):
		return http.StatusForbidden, "Нужны права администратора."
	case errors.Is(err, platform.ErrNotNative):
		return http.StatusForbidden,
			"Эта заметка пришла с НГС — здесь её копия, и текст копии не правится."
	case errors.Is(err, platform.ErrEditWindowClosed):
		return http.StatusForbidden,
			"Окно правки закрыто: заметку можно поправить один раз, первые десять минут и только пока под ней нет ответов."
	default:
		return http.StatusInternalServerError, ""
	}
}
