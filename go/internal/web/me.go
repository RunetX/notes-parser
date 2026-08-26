package web

// «Моя страница» — то немногое, что человек может сделать со своими данными без
// переписки с администратором. Права субъекта в полном объёме (выгрузка,
// обезличивание) — это Ш7; здесь исполняется то, что обязано работать НЕМЕДЛЕННО
// и без ручной проверки: отзыв согласия.

import (
	"errors"
	"net/http"
	"time"

	"lovegw/internal/platform"
)

// myHiddenLimit — сколько своих скрытых публикаций показываем. Список тут не
// архив, а повод нажать «на пересмотр»: длиннее двадцати он перестаёт читаться.
const myHiddenLimit = 20

type mePage struct {
	page
	Member  platform.Author
	Docs    []platform.ConsentDoc
	Have    platform.Consents
	Shadow  bool // вход не завершён: согласий нет
	Admin   bool
	// Avatar — показывать ли кнопку «Обновить аватар». Её нет у вошедшего по
	// приглашению (анкеты НГС у него нет вовсе) и нет, когда сайт недоступен:
	// кнопка, которая заведомо ответит отказом, хуже её отсутствия.
	Avatar bool
	// Problem — что не вышло в последнем действии. Отдельным полем, а не
	// страницей ошибки: «в анкете нет фото» — это не поломка, и уводить с
	// собственной страницы ради такой строки незачем.
	Problem string
	// Hidden — свои публикации, скрытые модерацией, с причиной и кнопкой
	// «на пересмотр». Молча исчезнувшая реплика — худшее, что можно сделать с
	// сообществом, которое только что переехало, поэтому список стоит здесь, а
	// не «по запросу к администратору».
	Hidden []platform.MyCheck
	// Ban — запрет писать: до какого числа и за что. Забаненного мы НЕ выкидываем
	// из учётной записи (чтение открыто всем), ровно затем, чтобы он эту строку
	// прочитал.
	Ban    *time.Time
	Reason string
	// Jump — стоит ли «проматывать к новым» (jump.go). Предпочтение экрана, а не
	// человека, поэтому приезжает из куки, а не из карточки участника.
	Jump bool
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.showMe(w, r, u, "")
}

// showMe рисует «мою страницу» — при необходимости с сообщением о том, что
// только что не получилось.
func (s *Server) showMe(w http.ResponseWriter, r *http.Request, u platform.User, problem string) {
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
	// Свои скрытые публикации спрашиваются по ОЧЕРЕДИ модерации, а не обходом
	// комментариев по автору: у участника с 138 тыс. реплик такой обход стоит
	// 53 с и в срок веб-запроса не влезает вовсе (замер 18.08.2026).
	var hidden []platform.MyCheck
	if s.mod != nil {
		hidden, err = s.mod.MyHidden(r.Context(), u.ID, myHiddenLimit)
		if err != nil {
			s.oops(w, r, "мои скрытые публикации", err)
			return
		}
	}
	var ban *time.Time
	if u.Banned(time.Now()) {
		ban = u.BannedUntil
	}
	s.render(w, r, http.StatusOK, "me.gohtml", mePage{
		page:    s.newPage(r, "Моя страница"),
		Member:  card,
		Docs:    docs,
		Have:    have,
		Shadow:  u.Kind == platform.KindShadow,
		Admin:   u.Role >= platform.RoleAdmin,
		Avatar:  s.site != nil && platform.IsNGS(u.ID),
		Problem: problem,
		Hidden:  hidden,
		Ban:     ban,
		Reason:  u.BanReason,
		Jump:    s.jumpFresh(r),
	})
}

// handleAvatar — «Обновить аватар»: сходить в анкету НГС за фото ещё раз.
//
// Зачем кнопка. Аватар приносит на площадку ЗЕРКАЛО, вместе с комментарием, — а
// комментариев на НГС нет с 17.08.2026, значит само оно здесь не обновится уже
// никогда: сменивший фото в анкете остался бы с прошлогодним навсегда. Замер по
// боевому зеркалу показал, насколько это живое: у одной участницы 56 разных
// файлов за четыре недели, случалось по три за сутки, — то есть перенос по
// просьбе через администратора был бы ежедневной просьбой.
//
// Своего файла площадка не принимает вовсе, и это не экономия: чужая картинка —
// это премодерация, хранилище и другой разговор о согласии (Ш5д). Здесь ровно
// перенос того, что человек и так показывает на НГС.
//
// Живёт в me.go, а не в write.go, потому что половина её ответов — это «моя
// страница» с объяснением: отказ чужого сайта не повод уводить человека на
// страницу ошибки.
func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	if s.site == nil {
		s.fail(w, r, http.StatusServiceUnavailable,
			"Площадка сейчас не может сходить на НГС за фото.")
		return
	}
	if !platform.IsNGS(u.ID) {
		s.fail(w, r, http.StatusBadRequest,
			"Ваш вход не связан с анкетой НГС — фото брать неоткуда.")
		return
	}
	prof, err := s.site.Profile(r.Context(), u.ID)
	switch {
	case errors.Is(err, ErrNoProfile):
		s.showMe(w, r, u, "НГС не отдал вашу анкету: она скрыта целиком или удалена. Фото осталось прежним.")
		return
	case err != nil:
		// Отказ ЧУЖОГО сайта не наша поломка, и 500 на своей странице тут врал бы.
		s.log.Warn("анкета НГС для обновления фото", "user", u.ID, "err", err)
		s.showMe(w, r, u, "НГС сейчас не отвечает. Фото осталось прежним — попробуйте позже.")
		return
	}
	if prof.AvatarURL == "" {
		// Фото в анкете нет (силуэт по умолчанию клиент НГС сюда не пропускает).
		// Своё при этом НЕ снимаем: файлов площадка не принимает, вернуть его
		// было бы неоткуда, а «нажал обновить и остался без фото» — это потеря
		// по нажатию кнопки.
		s.showMe(w, r, u, "В анкете НГС сейчас нет фото — здесь всё осталось как было.")
		return
	}
	data, err := s.site.Avatar(r.Context(), prof.AvatarURL)
	if err != nil {
		s.log.Warn("фото анкеты НГС", "user", u.ID, "err", err)
		s.showMe(w, r, u, "Фото из анкеты сейчас не забралось. Попробуйте позже.")
		return
	}
	if err := s.wr.SetOwnAvatar(r.Context(), u.ID, prof.AvatarURL, data); err != nil {
		s.oops(w, r, "смена фото", err)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// revokePage — экран «что произойдёт, если отозвать».
type revokePage struct {
	page
	Kind string
	// Title — заголовок отзываемого документа, ровно тот, что человек подписывал.
	Title string
	// Processing — отзывается ОБЩЕЕ согласие: оно вдобавок закрывает вход.
	Processing bool
}

// handleMeConsent — отзыв и возврат согласия.
//
// Отзыв исполняется в тот же момент, без очереди к модератору: ч. 2 ст. 9 не
// оставляет места для «рассмотрим в течение недели». Распространение — это
// обезличивание заметок (имя уходит, тексты остаются), общее согласие — то же
// плюс закрытый вход: обрабатывать становится нечего.
//
// Но СПЕРВА экран подтверждения, и это не вежливость. Пока отзыв прятал
// публикации, он был обратим — «вернёте согласие, и они появятся снова», — и
// одной кнопки хватало. Обезличивание необратимо по построению: соответствие
// «кто → какая могила» не хранится нигде, и вернуть подпись не может уже никто,
// включая администратора. Необратимое действие в одно нажатие — это ловушка.
func (s *Server) handleMeConsent(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
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
	if r.FormValue("confirm") != "1" {
		docs, err := platform.CurrentConsentDocs(s.cfg.Operator)
		if err != nil {
			s.oops(w, r, "тексты согласий", err)
			return
		}
		title := kind
		for _, d := range docs {
			if d.Kind == kind {
				title = d.Title
			}
		}
		s.render(w, r, http.StatusOK, "revoke.gohtml", revokePage{
			page:       s.newPage(r, "Отзыв согласия"),
			Kind:       kind,
			Title:      title,
			Processing: kind == platform.ConsentProcessing,
		})
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
