package web

// «Рассказ о себе» — город, занятие, несколько слов и до трёх фотографий
// (эпик M).
//
// От страницы здесь остался ОДИН экран — документ и кнопка под ним. Сами поля и
// альбом живут на «Моей странице», там же, где их видно: 12.09.2026 владелец
// спросил, к чему отдельно «Мой профиль» и «Моя страница», и первая редакция
// эпика как раз и разводила показ с правкой по разным адресам — человек правил
// не то, что видит, а нередко и не находил вовсе.
//
// А вот ПОДПИСЬ переезжать не вправе: документ обязан стоять ДО кнопки, и
// подпись, данная мимоходом под полем ввода, подписью не является. Поэтому
// /me/about и осталась — как экран одного разговора, который кончается
// возвратом на свою страницу уже с формами. Подписавшего сюда не пускают вовсе:
// смотреть тут больше не на что.
//
// Ядро спрашивается ВТОРОЙ раз при самой записи (aboutGuard): между показом
// формы и нажатием человек мог нажать «Отозвать», и строка после отзыва была бы
// обработкой без основания.

import (
	"errors"
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// aboutPage — экран согласия, и только он.
type aboutPage struct {
	page
	Doc platform.ConsentDoc
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
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
	have, err := s.auth.UserConsents(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	if have.Has(platform.ConsentProfile, doc.Version) {
		// Уже подписано — здесь смотреть нечего: формы стоят на своей странице.
		// Сам документ при этом никуда не делся, он лежит в подвале, как и
		// остальные четыре.
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "about.gohtml", aboutPage{
		page: s.newPage(r, "Рассказ о себе"),
		Doc:  doc,
	})
}

// handleAboutConsent записывает подпись и возвращает на свою страницу — уже с
// формами. Отдельным действием от сохранения текста: подписывают документ, а не
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
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// handleAboutSave сохраняет город, занятие и рассказ.
//
// Удача не уводит редиректом, а перерисовывает ту же страницу со словом
// «Сохранено»: человек правит запись, глядя на неё, и отправлять его после
// нажатия куда-то ещё незачем. Отказ возвращает НАБРАННОЕ — перечитав поля из
// базы, мы стёрли бы его работу.
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
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{Saved: true}}})
	default:
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{About: in, Bad: aboutProblem(err)}}})
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
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{Bad: aboutProblem(err)}}})
		return
	}
	shot, bad := s.takeShotSide(r.Context(), r, photoSide)
	if bad != "" {
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{Bad: bad}}})
		return
	}
	switch err := s.wr.AddProfilePhoto(r.Context(), u.ID, shot); {
	case err == nil:
		http.Redirect(w, r, "/me", http.StatusSeeOther)
	default:
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{Bad: aboutProblem(err)}}})
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
		http.Redirect(w, r, "/me", http.StatusSeeOther)
	default:
		s.showMe(w, r, u, mePage{Prof: profileView{Edit: &profileEdit{Bad: aboutProblem(err)}}})
	}
}

// photoSide — длинная сторона фотографии профиля. Та же, что у картинки к
// заметке (imgconv.MaxSide): в альбоме снимок показан квадратом со стороной в
// сотню точек, но по нажатию открывается целиком, и уменьшать его до размера
// миниатюры значило бы решить за человека, что разглядывать его незачем.
const photoSide = 1600

// aboutProblem переводит отказ ядра на человеческий. Общий список, потому что
// одни и те же отказы приходят на три формы своей страницы.
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
