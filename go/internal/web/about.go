package web

// «Рассказ о себе» — страница, где человек пишет о себе и кладёт фотографии
// (эпик M).
//
// СВОЯ страница, а не раздел «Моей»: здесь подписывают документ, и подпись,
// данная мимоходом под кнопкой в длинном списке настроек, подписью не является.
// Устройство то же, что у привязки мессенджера: два состояния одним шаблоном —
// сперва документ и кнопка, потом форма. Это один разговор, и разводить его по
// двум шаблонам значило бы завести второе место, где решают, что показать.
//
// Правило показа одно на обе половины: пока документ не подписан, форм нет
// вовсе, а не «есть, но отвечают отказом». Ядро при этом спрашивается ВТОРОЙ
// раз при самой записи (aboutGuard): между показом формы и нажатием человек мог
// нажать «Отозвать», и строка после отзыва была бы обработкой без основания.

import (
	"errors"
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// aboutPage — экран в двух состояниях.
type aboutPage struct {
	page
	// Doc непустой, пока согласия нет: показывается документ, а не форма.
	Doc    platform.ConsentDoc
	Signed bool
	About  platform.About
	Photos []platform.Photo
	// Free — сколько мест в альбоме свободно. Ноль убирает поле файла: кнопка,
	// отвечающая отказом, хуже её отсутствия.
	Free int
	// Limit и MaxRunes приезжают из ядра, а не пишутся в шаблоне словами: числа
	// у площадки живут в одном месте, и разошедшийся с поведением текст хуже
	// отсутствующего (тем же правилом справка перестала считать темы руками).
	Limit    int
	MaxRunes int
	// Shots — перекодировщик поднят, файл принять есть чем. Не поднялся —
	// страница живёт дальше, просто без поля файла: чужой бинарник, который не
	// отвечает, не повод закрывать рассказ о себе.
	Shots bool
	// Hidden — карточку скрыл модератор. Человек обязан увидеть это сам, а не
	// гадать, почему его страница пуста для других.
	Hidden bool
	Saved  bool
	Bad    string
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.showAbout(w, r, u, aboutPage{})
}

// showAbout собирает страницу целиком. Одна дорога на показ, на отказ и на
// удачную запись: три сборки той же страницы разошлись бы на первой же правке.
func (s *Server) showAbout(w http.ResponseWriter, r *http.Request, u platform.User, in aboutPage) {
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentProfile)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	have, err := s.auth.UserConsents(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	p := in
	p.page = s.newPage(r, "Рассказ о себе")
	p.Doc = doc
	p.Signed = have.Has(platform.ConsentProfile, doc.Version)
	p.Limit = platform.PhotoLimit
	p.MaxRunes = platform.MaxAboutRunes
	p.Shots = s.shots != nil

	if p.Signed {
		prof, err := s.st.UserProfile(r.Context(), u.ID)
		if err != nil {
			s.oops(w, r, "рассказ о себе", err)
			return
		}
		// Набранное на отказе сохраняется: форму человек уже заполнил, и
		// перечитывать её из базы значило бы стереть его работу.
		if p.About == (platform.About{}) {
			p.About = platform.About{Bio: prof.Bio, City: prof.City, Job: prof.Job}
		}
		p.Hidden = prof.AboutStatus != platform.StatusVisible
		// Свой альбом человек видит ЦЕЛИКОМ, включая скрытое модератором:
		// иначе снятая фотография выглядела бы пропавшей, и он положил бы её
		// заново.
		photos, err := s.st.ProfilePhotos(r.Context(), u.ID, true)
		if err != nil {
			s.oops(w, r, "альбом", err)
			return
		}
		p.Photos = photos
		p.Free = platform.PhotoLimit - len(photos)
	}
	s.render(w, r, http.StatusOK, "about.gohtml", p)
}

// handleAboutConsent записывает подпись и возвращает на ту же страницу — уже с
// формой. Отдельным действием от сохранения текста: подписывают документ, а не
// поле ввода.
func (s *Server) handleAboutConsent(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentProfile)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	if err := s.auth.GrantConsent(r.Context(), u.ID, doc.Kind, doc.Version, r.UserAgent()); err != nil {
		s.oops(w, r, "согласие на рассказ о себе", err)
		return
	}
	http.Redirect(w, r, "/me/about", http.StatusSeeOther)
}

func (s *Server) handleAboutSave(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	in := platform.About{
		Bio:  r.FormValue("bio"),
		City: r.FormValue("city"),
		Job:  r.FormValue("job"),
	}
	switch err := s.wr.SetAbout(r.Context(), u.ID, in); {
	case err == nil:
		s.showAbout(w, r, u, aboutPage{Saved: true})
	default:
		s.showAbout(w, r, u, aboutPage{About: in, Bad: aboutProblem(err)})
	}
}

// handlePhotoAdd принимает фотографию.
//
// Черновика в памяти, как у картинки к заметке, здесь нет намеренно: у заметки
// он спасает НАБРАННЫЙ ТЕКСТ, который иначе пропадёт вместе с отказом формы, а
// фотография профиля кладётся одна и сразу, и тридцатиминутная память в
// процессе была бы лишним риском без выигрыша.
func (s *Server) handlePhotoAdd(w http.ResponseWriter, r *http.Request) {
	if !s.postUpload(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// Право спрашивается ДО перекодирования: отказ не должен стоить ни
	// процессора, ни файла, который после отказа убирать будет некому. Тот же
	// порядок и тот же довод, что у картинки к заметке (MayPublishNote).
	if err := s.wr.MayTellAbout(r.Context(), u.ID); err != nil {
		s.showAbout(w, r, u, aboutPage{Bad: aboutProblem(err)})
		return
	}
	shot, bad := s.takeShotSide(r.Context(), r, photoSide)
	if bad != "" {
		s.showAbout(w, r, u, aboutPage{Bad: bad})
		return
	}
	switch err := s.wr.AddProfilePhoto(r.Context(), u.ID, shot); {
	case err == nil:
		http.Redirect(w, r, "/me/about", http.StatusSeeOther)
	default:
		s.showAbout(w, r, u, aboutPage{Bad: aboutProblem(err)})
	}
}

func (s *Server) handlePhotoDrop(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	pos, err := strconv.Atoi(r.FormValue("position"))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "Непонятно, какую фотографию убрать.")
		return
	}
	switch err := s.wr.RemoveProfilePhoto(r.Context(), u.ID, pos); {
	case err == nil, errors.Is(err, platform.ErrNoPhoto):
		// «Такой уже нет» — не ошибка, а нажатие дважды или возврат по истории.
		http.Redirect(w, r, "/me/about", http.StatusSeeOther)
	default:
		s.showAbout(w, r, u, aboutPage{Bad: aboutProblem(err)})
	}
}

// photoSide — длинная сторона фотографии профиля. Та же, что у картинки к
// заметке (imgconv.MaxSide): в альбоме снимок показан квадратом со стороной в
// сотню точек, но по нажатию открывается целиком, и уменьшать его до размера
// миниатюры значило бы решить за человека, что разглядывать его незачем.
const photoSide = 1600

// aboutProblem переводит отказ ядра на человеческий. Общий список, потому что
// одни и те же отказы приходят на три формы этой страницы.
func aboutProblem(err error) string {
	switch {
	case errors.Is(err, platform.ErrNoProfileConsent):
		return "Согласие отозвано. Прочитайте документ и подпишите его заново."
	case errors.Is(err, platform.ErrPhotoLimit):
		return "Все места заняты. Уберите одну фотографию, чтобы положить другую."
	case errors.Is(err, platform.ErrTooLong):
		return "Слишком длинно."
	case errors.Is(err, platform.ErrBanned):
		return "Публикации вам сейчас запрещены — рассказ о себе тоже."
	case errors.Is(err, platform.ErrNotMember):
		return "Это может только участник площадки."
	default:
		return "Не получилось сохранить. Попробуйте ещё раз."
	}
}
