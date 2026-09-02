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
	// CreateNote публикует заметку. shot — уже ПЕРЕКОДИРОВАННЫЕ байты картинки
	// (nil, если её нет). Морда отдаёт байты, а не путь к файлу: класть их в
	// хранилище обязан тот же код, что у зеркала и у аватара, иначе три места
	// начнут по-разному решать, что считать картинкой. Тот же довод, что у
	// SetOwnAvatar, и та же дорога.
	CreateNote(ctx context.Context, in platform.NewNote, shot *Shot) (int64, error)
	// MayPublishNote — можно ли этому человеку сейчас публиковать. Спрашивается
	// ДО перекодирования: отказ не должен стоить ни процессора, ни файла,
	// который после отката транзакции убрать будет некому (см. shot.go).
	MayPublishNote(ctx context.Context, userID int64) error
	CreateComment(ctx context.Context, in platform.NewComment) (int64, error)
	EditNote(ctx context.Context, userID int64, in platform.NoteEdit) error
	SetOwnNick(ctx context.Context, userID int64, nick string) error
	// SetOwnAvatar — поставить себе фото, забранное из анкеты НГС: url, откуда
	// оно взято, и сами байты. Своего файла площадка не принимает вовсе, поэтому
	// другого способа сменить здесь фото нет (Ш5д).
	SetOwnAvatar(ctx context.Context, userID int64, url string, data []byte) error
	// ClearOwnAvatar — снять с себя фото и остаться без него. Вторая дорога
	// рядом с SetOwnAvatar, потому что первая пустую анкету НГС за причину
	// снять фото не считает: чужая рука фото не отнимает, своя — вправе.
	ClearOwnAvatar(ctx context.Context, userID int64) error
	// SetNGSSend — уносить ли написанное здесь на love.ngs.ru. По умолчанию
	// ВЫКЛЮЧЕНО: публикуя здесь, человек соглашался на публикацию ЗДЕСЬ, и унести
	// его слова на чужой сайт под его именем можно только по осознанно нажатой
	// галочке. Отправляет потом демон и СЕССИЕЙ САМОГО ЧЕЛОВЕКА — морда ключа от
	// сессий не имеет вовсе.
	SetNGSSend(ctx context.Context, userID int64, on bool) error
	NGSSendOn(ctx context.Context, userID int64) (bool, error)
	// NGSStuck — сколько записей не ушло из-за сессии сайта и с каких пор. Ноль
	// значит «сейчас работает». Нужен затем, чтобы галочка не обещала того, чего
	// не делает: сессия живёт у демона, и морда о ней узнать иначе не может —
	// зато видит СЛЕД, оставленный им в очереди.
	NGSStuck(ctx context.Context, userID int64) (int, time.Time, error)
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
	Admin bool
	// TextLocked — заметка ЗЕРКАЛЬНАЯ, и администратор открыл форму ради одной
	// картинки: текст копии не правится вообще (см. platform.ErrNotNative), и
	// поля для него на экране нет вовсе — поле, отвечающее отказом, хуже его
	// отсутствия.
	TextLocked bool
	Body       string
	Anonymous  bool
	// Stage и CanStage — ПЕСОЧНИЦА (эпик «народ»): заметка, в которой говорят
	// жители, а люди только читают. Галочка стоит на форме НОВОЙ заметки и
	// только у администратора: заводить песочницу вправе он и сам житель
	// (platform.stageGuard), а житель формой не пользуется вовсе.
	//
	// Полей два, а не одно, по тому же доводу, что у Anonymous рядом с CanShot:
	// первое — что человек выбрал (и что обязано пережить отказ), второе —
	// показывать ли этот выбор.
	Stage    bool
	CanStage bool
	Problem  string
	// CanShot — площадка сейчас принимает файлы (есть перекодировщик). Нет —
	// поля файла в форме нет вовсе.
	CanShot bool
	// HasShot — у правимой заметки есть картинка, значит есть и «снять».
	HasShot bool
	// LostShot — отказ случился, когда файл ехал ТЕЛОМ формы. Браузер его не
	// возвращает, и человеку надо сказать об этом прямо, а не оставить гадать,
	// почему поле опустело.
	//
	// Со скриптом такого не бывает вовсе: файл уже лежит черновиком на сервере
	// и приезжает обратно в Draft — ради этого предзагрузка и заводилась.
	LostShot bool
	// Draft — приложенная картинка, ждущая своей заметки (shotdraft.go). Стоит
	// на форме и после отказа: отказ ядра не должен стоить человеку файла.
	Draft *shotDraftView
}

// ---------------------------------------------------------------- новая заметка

func (s *Server) handleNewNote(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	s.render(w, r, http.StatusOK, composePageName, s.newNoteForm(r, "", false, false))
}

// newNoteForm — форма новой заметки В ОДНОМ МЕСТЕ: её собирают и показ, и
// каждый отказ. Тот же довод, что у editForm ниже, и он не про опрятность:
// разойдясь, две копии разойдутся МОЛЧА — форму отказа видит не тот, кто её
// писал, а тот, кому отказали.
func (s *Server) newNoteForm(r *http.Request, body string, anon, stage bool) composePage {
	return composePage{
		page:      s.newPage(r, "Новая заметка"),
		Body:      body,
		Anonymous: anon,
		Stage:     stage,
		CanStage:  s.canStage(r),
		CanShot:   s.takesShots(),
	}
}

// canStage — вправе ли этот человек завести песочницу.
//
// Спрашивается ТОЛЬКО ради показа галочки, и настоящей проверкой не является:
// та стоит в ядре (stageGuard) и идёт той же транзакцией, что и публикация.
// Здесь же она повторена по общему правилу морды — не рисовать кнопку, которая
// ответит отказом.
//
// Права модератора к этому отношения не имеют, поэтому и от s.mod показ не
// зависит: песочница — вопрос о том, кто здесь ГОВОРИТ, а не о словах.
func (s *Server) canStage(r *http.Request) bool {
	u, ok := s.me(r)
	if !ok {
		return false
	}
	return platform.Viewer{UserID: u.ID, Role: u.Role}.CanAdmin()
}

// handleCreateNote — публикация заметки, с картинкой или без.
//
// Порядок проверок здесь — это порядок их ЦЕНЫ, а не порядок важности.
// Происхождение читается из заголовков и не стоит ничего; вошедший уже лежит в
// контексте; право публиковать — один дешёвый запрос. И только потом слот
// очереди, чтение десяти мегабайт тела и запуск ffmpeg.
//
// Так сделано ради файла: байты ложатся на диск ДО транзакции, а уборки каталога
// у площадки нет вовсе, поэтому каждый отказ, случившийся ПОСЛЕ перекодирования,
// оставлял бы файл навсегда.
func (s *Server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	multipart := isMultipart(r)
	if !multipart {
		// Форма без файла ходит прежней дорогой: она работает и из старой
		// вкладки, и когда картинки площадка не принимает вовсе.
		if !s.postWrite(w, r) {
			return
		}
	} else if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	// Дешёвый отказ до всякой работы. Настоящая проверка всё равно стоит внутри
	// транзакции и остаётся единственной, которой можно верить.
	if multipart {
		if err := s.wr.MayPublishNote(r.Context(), u.ID); err != nil {
			// Цена дешёвого отказа названа честно: тело ещё не прочитано, а
			// значит форма перерисуется без набранного текста и без номера
			// черновика. Читать десять мегабайт ради того, кому мы уже
			// отказали, дороже — см. шапку MayPublishNote в ядре.
			s.composeProblem(w, r, err, s.newNoteForm(r, "", false, false), true, nil)
			return
		}
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
	}

	body := r.FormValue("body")
	anon := r.FormValue("anonymous") != ""
	stage := r.FormValue("stage") != ""

	shot, draft, problem := s.pickShot(r.Context(), r, u.ID)
	if problem != "" {
		// Заметка НЕ публикуется. Опубликовать текст молча, без картинки, значит
		// решить за человека, который её осознанно прикладывал.
		form := s.newNoteForm(r, body, anon, stage)
		form.Problem, form.LostShot = problem, hadFilePart(r)
		s.render(w, r, http.StatusBadRequest, composePageName, form)
		return
	}

	// Stage уходит в ядро КАК ЕСТЬ, без проверки прав здесь: её делает
	// stageGuard той же транзакцией, что и саму публикацию. Подделанная
	// страница получит оттуда честный отказ, а не тихо заведённую песочницу.
	id, err := s.wr.CreateNote(r.Context(), platform.NewNote{
		AuthorID: u.ID, Anonymous: anon, Body: body, Stage: stage,
	}, shot)
	if err != nil {
		s.composeProblem(w, r, err, s.newNoteForm(r, body, anon, stage),
			hadFilePart(r) && draft == "", draftView(shot, draft))
		return
	}
	// Черновик прожил ровно до заметки. Убирается ПОСЛЕ удачи, а не при чтении:
	// отказ ядра (бан, частота, истёкшая редакция согласия) картинки стоить не
	// должен — на перерисованной форме она остаётся приложенной.
	s.dropDraft(draft)
	videoWarm(id, body)
	http.Redirect(w, r, "/n/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// composeProblem перерисовывает форму с отказом, не теряя ни набранного текста,
// ни приложенной картинки: предзагруженная возвращается карточкой, приехавшая
// телом формы — увы, только словами о том, что её надо выбрать заново.
func (s *Server) composeProblem(w http.ResponseWriter, r *http.Request, err error, form composePage, lost bool, draft *shotDraftView) {
	status, problem := writeProblem(err)
	if problem == "" {
		s.oops(w, r, "публикация заметки", err)
		return
	}
	form.Problem, form.LostShot, form.Draft = problem, lost, draft
	s.render(w, r, status, composePageName, form)
}

// ---------------------------------------------------------------- правка заметки

func (s *Server) handleEditNote(w http.ResponseWriter, r *http.Request) {
	_, note, mode, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, composePageName, s.editForm(r, note, mode, note.Body, ""))
}

// editForm — форма правки в одном месте: её собирают и показ, и каждый отказ, а
// две копии разошлись бы на первом же новом поле.
//
// Картинку администратор ставит ЛЮБОЙ заметке, поэтому поле файла у него есть и
// у зеркальной; «снять» — только там, где ядро это позволит (у зеркальной
// иллюстрация вернулась бы сверкой через пять минут, см. SetNoteImageAsAdmin),
// и кнопки, ведущей к отказу, мы не рисуем.
func (s *Server) editForm(r *http.Request, note platform.NoteView, mode editMode, body, problem string) composePage {
	admin := mode != editOwn
	has := s.noteHasImage(r.Context(), note.ID)
	return composePage{
		page:       s.newPage(r, "Правка заметки"),
		Note:       note,
		Editing:    true,
		Admin:      admin,
		TextLocked: mode == editShot,
		Body:       body,
		Anonymous:  note.Anonymous,
		Problem:    problem,
		CanShot:    admin && s.takesShots(),
		HasShot:    has && (mode == editOwn || platform.IsNative(note.ID)),
		Stage:      note.Stage,
		// Песочницу предлагаем только там, где ядро согласится: пока под
		// заметкой не сказано ни слова. Заметка ЛЮБАЯ, в том числе зеркальная,
		// — старая запись с НГС без единой реплики и есть готовая сцена.
		//
		// Счётчик считает ВИДИМОЕ, а ядро — все реплики подряд, поэтому у
		// заметки, где единственная реплика скрыта модератором, галочка
		// покажется и ответит отказом. Случай редкий, и отказ у него внятный
		// (ErrStageHasThread): выбирать между этим и запросом «а есть ли
		// скрытые» на каждую форму правки — размен в пользу запроса, которого
		// нет.
		CanStage: admin && note.CommentCount == 0,
	}
}

// noteHasImage — есть ли у заметки картинка. Спрашивается только формой правки:
// «снять картинку» нельзя предлагать там, где снимать нечего, а отказ на кнопку
// хуже её отсутствия. Ошибку чтения считаем за «нет»: показать лишнюю галочку
// хуже, чем не показать её на странице, которая иначе откроется.
func (s *Server) noteHasImage(ctx context.Context, id int64) bool {
	imgs, err := s.st.NoteImages(ctx, id)
	return err == nil && len(imgs) > 0
}

func (s *Server) handleUpdateNote(w http.ResponseWriter, r *http.Request) {
	multipart := isMultipart(r)
	if !multipart {
		// Прежняя дорога (urlencoded) остаётся рабочей: она работает и из старой
		// вкладки, и когда картинки площадка не принимает вовсе.
		if !s.postWrite(w, r) {
			return
		}
	} else if !sameOrigin(r) {
		s.fail(w, r, http.StatusForbidden, "Запрос пришёл не с нашей страницы.")
		return
	}
	u, note, mode, ok := s.editTarget(w, r)
	if !ok {
		return
	}
	if multipart {
		// Слот перекодирования занимается ДО чтения тела, как и у публикации:
		// память в контейнере одна на всех, и кто прислал файл — участник или
		// администратор, — ей безразлично. Право уже проверено выше, и это тот
		// самый дешёвый отказ, ради которого порядок и выстроен.
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
	}
	body := r.FormValue("body")
	if mode == editShot {
		// Текст копии не правится вообще, поэтому чужое поле из формы не
		// читается вовсе: подложить его подделанной страницей нельзя, если оно
		// никуда не идёт.
		body = note.Body
	}
	// Снятие картинки — часть той же правки, а не отдельное действие: разведи их
	// на две транзакции, и отказ второй оставит человека с закрытым навсегда
	// окном и с картинкой, которую он снимал.
	dropShot := r.FormValue("drop_shot") != ""
	// Песочница — решение администратора и только его. У автора в своём окне
	// поля нет вовсе, поэтому чужое значение из формы здесь не читается: тот же
	// приём, что и с телом зеркальной заметки строкой выше.
	stage := note.Stage
	if mode != editOwn {
		stage = r.FormValue("stage") != ""
	}

	shot, draft, problem := s.pickShot(r.Context(), r, u.ID)
	if problem != "" {
		// Ничего не меняется вовсе: сохранить текст молча, без картинки, значит
		// решить за того, кто её осознанно прикладывал.
		page := s.editForm(r, note, mode, body, problem)
		page.Stage = stage
		page.LostShot = hadFilePart(r)
		s.render(w, r, http.StatusBadRequest, composePageName, page)
		return
	}

	var err error
	switch {
	case mode == editOwn:
		err = s.wr.EditNote(r.Context(), u.ID, platform.NoteEdit{
			NoteID: note.ID, Body: body, DropImage: dropShot,
		})
	default:
		actor := platform.Viewer{UserID: u.ID, Role: u.Role}
		if mode == editAdmin {
			err = s.mod.EditNoteAsAdmin(r.Context(), actor, note.ID, body, r.FormValue("reason"))
		}
		// Картинка — ОТДЕЛЬНОЕ действие ядра, а не часть правки текста, и
		// потому отдельная транзакция: у них разные двери (текст копии с НГС не
		// правится вообще, картинка ставится любой заметке) и разные записи в
		// журнале. Порядок «сперва текст» выбран так, чтобы отказ на картинке не
		// отменял уже сохранённых слов, — а сказано об отказе будет в любом
		// случае.
		if err == nil || errors.Is(err, platform.ErrNothingToDo) {
			if shot != nil || dropShot {
				err = s.mod.SetNoteImageAsAdmin(r.Context(), actor, note.ID, shot, r.FormValue("reason"))
			}
		}
		// Песочница — третье отдельное действие ядра и третья запись в журнале,
		// по тому же доводу, что и картинка: у неё своя дверь (заметка ЛЮБАЯ, но
		// только пока в ней не говорили) и свой отказ. Последней в цепочке,
		// потому что она решает не про текст, а про то, кто в этой заметке
		// вправе говорить, — и отказ здесь не должен отменять уже сохранённых
		// слов и картинки.
		if stage != note.Stage && (err == nil || errors.Is(err, platform.ErrNothingToDo)) {
			err = s.mod.SetNoteStageAsAdmin(r.Context(), actor, note.ID, stage, r.FormValue("reason"))
		}
	}
	// «Текст и так такой» отказом не считается: администратор мог открыть форму
	// и закрыть её, ничего не изменив, — то же правило, что у кнопок модерации.
	if err != nil && !errors.Is(err, platform.ErrNothingToDo) {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "правка заметки", err)
			return
		}
		page := s.editForm(r, note, mode, body, problem)
		page.Stage = stage
		page.LostShot = hadFilePart(r) && draft == ""
		page.Draft = draftView(shot, draft)
		s.render(w, r, status, composePageName, page)
		return
	}
	s.dropDraft(draft)
	// Ролик, названный в новом тексте, показывается карточкой — но только когда
	// превью уже лежит рядом. Правка подталкивает закачку ровно так же, как
	// публикация: иначе карточку увидел бы не тот, кто пришёл читать первым.
	videoWarm(note.ID, body)
	http.Redirect(w, r, "/n/"+strconv.FormatInt(note.ID, 10), http.StatusSeeOther)
}

// editMode — чем человек правит открытую форму. Трёх состояний, а не двух,
// потому что у администратора их два: НАТИВНУЮ заметку он правит целиком, а у
// зеркальной вправе поменять одну картинку — текст копии не правится вообще.
type editMode int

const (
	editOwn   editMode = iota // автор в своём окне
	editAdmin                 // администратор: текст нативной заметки и картинка
	editShot                  // администратор у зеркальной: только картинка
)

// editTarget — заметка, которую этому человеку сейчас можно править, и КАК он
// её правит: автором в своём окне, администратором целиком или администратором
// ради одной картинки. Пустой последний результат означает «ответ уже
// отправлен».
//
// Одна дорога на все случаи, потому что различаются они только правом и текстом
// на экране: форма, адрес и разбор ответа общие, а две копии разошлись бы на
// первой же правке — ровно как разошлись бы правило страницы и правило ядра,
// если бы Editable считался в двух местах.
func (s *Server) editTarget(w http.ResponseWriter, r *http.Request) (platform.User, platform.NoteView, editMode, bool) {
	var none platform.NoteView
	u, ok := s.me(r)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			s.fail(w, r, http.StatusUnauthorized, "Чтобы писать, нужно войти.")
		}
		return platform.User{}, none, editOwn, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, editOwn, false
	}
	admin := u.Role >= platform.RoleAdmin && s.mod != nil
	note, err := s.st.NoteViewByID(r.Context(), platform.Viewer{UserID: u.ID, Role: u.Role}, id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, editOwn, false
	}
	if err != nil {
		s.oops(w, r, "чтение заметки", err)
		return u, none, editOwn, false
	}
	// Скрытая заметка для читателя отсутствует — как и на её собственной
	// странице. Администратору скрытое модерацией видно: снять из текста лишнее
	// и вернуть его людям — это одно действие в два нажатия.
	if note.Status != platform.StatusVisible &&
		!(admin && note.Status == platform.StatusHiddenMod) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return u, none, editOwn, false
	}

	switch {
	case note.Editable(time.Now()):
		// Своё окно. Условие считает Editable — то же самое, что проверит ядро,
		// чтобы кнопка не отвечала отказом.
		if s.wr == nil {
			s.fail(w, r, http.StatusServiceUnavailable, "Запись сейчас недоступна.")
			return u, none, editOwn, false
		}
		return u, note, editOwn, true
	case admin && platform.IsNative(note.ID):
		return u, note, editAdmin, true
	case admin:
		// Зеркальная. Форма открывается ради КАРТИНКИ: у копии с НГС она
		// смертна (в базе лежит ссылка на чужой хост), и поставить свой файл —
		// единственный способ её вернуть. Текста форма не покажет вовсе.
		return u, note, editShot, true
	case !note.Own:
		s.fail(w, r, http.StatusForbidden, "Это не ваша заметка.")
	default:
		s.fail(w, r, http.StatusForbidden,
			"Править заметку можно только первые десять минут и только пока под ней нет ответов. "+
				"Дальше текст остаётся как есть: под ним уже отвечают вам.")
	}
	return u, none, editOwn, false
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
		// Что именно изменилось, говорит сам документ на экране подписи, и
		// здесь это не повторяется дословно: редакция меняется, а текст отказа
		// иначе устаревает молча — так уже вышло с прошлой формулировкой про
		// поисковые системы.
		return http.StatusForbidden,
			"Соглашения обновились. Откройте «Мою страницу» и подпишите новую редакцию — " +
				"после этого можно писать дальше."
	case errors.Is(err, platform.ErrNotMember):
		return http.StatusForbidden, "Писать может только участник площадки."
	case errors.Is(err, platform.ErrBadReaction):
		return http.StatusBadRequest, "Такой реакции нет."
	case errors.Is(err, platform.ErrNotYours):
		return http.StatusForbidden, "Это не ваша запись."
	case errors.Is(err, platform.ErrStageHasThread):
		return http.StatusForbidden,
			"В заметке уже есть реплики — песочницу из неё больше не сделать и обратно не вернуть. " +
				"Правило одно на обе стороны: разговор, который начали люди, не отдают жителям, " +
				"а сказанное жителями не выпускают наружу."
	case errors.Is(err, platform.ErrStageClosed):
		return http.StatusForbidden, "Завести песочницу может только администратор."
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
