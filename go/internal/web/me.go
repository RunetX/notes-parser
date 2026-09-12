package web

// «Моя страница» — всё, что человек может сделать со своими данными без
// переписки с администратором. Права субъекта в полном объёме (выгрузка,
// обезличивание) — это Ш7; здесь исполняется то, что обязано работать НЕМЕДЛЕННО
// и без ручной проверки: отзыв согласия.
//
// Вкладок ДВЕ, и заведены они 12.09.2026 решением владельца: «к чему отдельно
// „Мой профиль" и „Моя страница"… сделай общий вид и отдельно вкладку с
// настройками, где можно переключить всякие опции». До этого дня пунктов меню
// было два и вели они на РАЗНОЕ: своя страница показывала список тумблеров,
// чужая — карточку с рассказом о себе, — и человек, искавший свой профиль,
// попадал в настройки.
//
// Имя «Моя страница» при этом не тронуто, и не из осторожности: так её зовут
// все пять опубликованных согласий («отозвать можно на „Моей странице"»), а
// опубликованная редакция неизменяема. Поэтому настройки не отдельная страница,
// а ВКЛАДКА этой же: отзыв согласия по-прежнему живёт на «Моей странице».

import (
	"errors"
	"net/http"
	"time"

	"lovegw/internal/platform"
)

// myHiddenLimit — сколько своих скрытых публикаций показываем. Список тут не
// архив, а повод нажать «на пересмотр»: длиннее двадцати он перестаёт читаться.
const myHiddenLimit = 20

// mePage — вкладка ПРОФИЛЬ: тот же профиль, что видят другие, плюс правка
// прямо на нём.
type mePage struct {
	page
	// Prof — ровно то же, что на /u/<id>: собрано тем же кодом и нарисовано тем
	// же шаблоном. Расходиться этим двум показам нельзя — человек правит то,
	// что видит.
	Prof profileView
	// Avatar — показывать ли кнопку «Обновить аватар». Её нет у вошедшего по
	// приглашению (анкеты НГС у него нет вовсе) и нет, когда сайт недоступен:
	// кнопка, которая заведомо ответит отказом, хуже её отсутствия.
	Avatar bool
	// Problem — что не вышло в последнем действии. Отдельным полем, а не
	// страницей ошибки: «в анкете нет фото» — это не поломка, и уводить с
	// собственной страницы ради такой строки незачем.
	Problem string
	// Ban — запрет писать: до какого числа и за что. Забаненного мы НЕ выкидываем
	// из учётной записи (чтение открыто всем), ровно затем, чтобы он эту строку
	// прочитал.
	Ban    *time.Time
	Reason string
	// Hidden — свои публикации, скрытые модерацией, с причиной и кнопкой
	// «на пересмотр». Молча исчезнувшая реплика — худшее, что можно сделать с
	// сообществом, которое только что переехало, поэтому список стоит здесь, а
	// не «по запросу к администратору». Место ему на ПРОФИЛЕ, а не в
	// настройках: это про написанное, а не про то, как ведёт себя площадка.
	Hidden []platform.MyCheck
}

// settingsPage — вкладка НАСТРОЙКИ: всё, что переключается.
//
// Разведены вкладки по вопросу, на который отвечают. Профиль отвечает «кто я и
// что я написал», настройки — «как площадка себя ведёт и чем я здесь владею»:
// вынос на НГС, прокрутка, двери входа, переписка, сессии, согласия. Прежде это
// лежало одним списком под шапкой, и найти в нём что-либо было нечем.
type settingsPage struct {
	page
	Member  platform.Author
	Problem string
	Docs    []platform.ConsentDoc
	Have    platform.Consents
	// Jump — стоит ли «проматывать к новым» (jump.go). Предпочтение экрана, а не
	// человека, поэтому приезжает из куки, а не из карточки участника.
	Jump bool
	// NGSSend — уносить ли написанное здесь на love.ngs.ru, и показывать ли эту
	// галочку вообще. Показывается она только тому, у кого есть анкета НГС:
	// вошедшему по приглашению уносить нечем и некуда — кнопки, отвечающей
	// отказом, площадка не рисует (тот же довод, что у «Обновить аватар»).
	NGSSend     bool
	NGSSendable bool
	// NGSStuck — сколько записей застряло из-за отсутствующей сессии сайта и с
	// каких пор. Ноль означает «уходит»: строка гаснет сама, как только
	// следующая запись уезжает.
	NGSStuck   int
	NGSStuckAt time.Time
	// NGSPending — сколько заметок ещё в пути на сайт. У человека с галочкой
	// заметка не заводится здесь вовсе, и полторы минуты между нажатием и
	// появлением в ленте иначе выглядят как пропажа текста.
	NGSPending int
	// Blocked — кому закрыта переписка. Пустой список раздела не рисует вовсе:
	// заголовок над пустотой отвечает на вопрос, которого не задавали.
	Blocked []platform.Author
	// Bindings — привязанные мессенджеры: ВТОРАЯ дверь, не зависящая от НГС.
	// Показываются вместе с согласиями и выносом, потому что вопрос у них один
	// и тот же — «чем я владею на этой площадке».
	Bindings []platform.Binding
	// NGSDoor — есть ли у человека анкета НГС (номер лежит в полосе НГС).
	// SiteUp — доступен ли сам сайт: код входа читается со страницы анкеты, и
	// без клиента НГС эта дверь не работает ни у кого. Состояний, стало быть,
	// ТРИ, а не два, и различать их обязана страница: «анкеты нет вовсе» и
	// «анкета есть, но сайт молчит» — разные ответы на один вопрос, и второй
	// нельзя объявлять входом по приглашению.
	NGSDoor bool
	SiteUp  bool
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.showMe(w, r, u, mePage{})
}

// meReady — прихожая обеих вкладок: вход не завершён, пока не подписаны
// обязательные согласия, и половины страницы в этом состоянии не показываем.
func (s *Server) meReady(w http.ResponseWriter, r *http.Request, u platform.User) bool {
	missing, err := s.auth.MissingConsent(r.Context(), u.ID, s.cfg.Operator)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return false
	}
	if missing.Kind != "" {
		// Вход не завершён — доводим до конца, а не показываем половину.
		http.Redirect(w, r, "/consent", http.StatusSeeOther)
		return false
	}
	return true
}

// showMe рисует вкладку профиля — при необходимости с сообщением о том, что
// только что не получилось или что сохранилось.
//
// Один сборщик на показ, на отказ формы и на удачную запись: три сборки той же
// страницы разошлись бы на первой же правке.
func (s *Server) showMe(w http.ResponseWriter, r *http.Request, u platform.User, in mePage) {
	if !s.meReady(w, r, u) {
		return
	}
	card, err := s.auth.MemberCard(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "карточка участника", err)
		return
	}
	// Профиль читается из той же строки и тем же запросом, что у чужой
	// страницы. Отказ её НЕ роняет: своя страница нужна человеку и ради списка
	// скрытого, и ради дороги в настройки, а без этого запроса остаётся
	// карточка входа — ник, фото и пол у неё те же самые.
	member, err := s.st.UserProfile(r.Context(), u.ID)
	if err != nil {
		s.log.Warn("своя карточка не прочитана", "user", u.ID, "err", err)
		member = platform.Profile{ID: u.ID, Kind: u.Kind, Role: u.Role}
	}
	member.Nick, member.AvatarURL, member.Gender = card.Nick, card.AvatarURL, card.Gender
	have, err := s.auth.UserConsents(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentProfile)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	edit := &profileEdit{
		CSRF:     csrfToken(s.session(r)),
		Signed:   have.Has(platform.ConsentProfile, doc.Version),
		Limit:    platform.PhotoLimit,
		MaxRunes: platform.MaxAboutRunes,
		Shots:    s.shots != nil,
		About:    in.Prof.Edit.about(),
		Saved:    in.Prof.Edit.saved(),
		Bad:      in.Prof.Edit.bad(),
	}
	prof, ok := s.profileBody(w, r, u, member, edit)
	if !ok {
		return
	}
	// Свободные места считаются от ПОЛНОГО альбома, включая скрытое
	// модератором: своё человек видит всё (так решает profileBody по признаку
	// «смотрю на себя»), и снятая модератором фотография иначе выглядела бы
	// пропавшей — он положил бы её заново, заняв второе место из трёх.
	edit.Free = platform.PhotoLimit - len(prof.Photos)
	// Набранное на отказе сохраняется: форму человек уже заполнил, и
	// перечитывать её из базы значило бы стереть его работу.
	if edit.About == (platform.About{}) {
		edit.About = platform.About{Bio: member.Bio, City: member.City, Job: member.Job}
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
		Prof:    prof,
		Avatar:  s.site != nil && platform.IsNGS(u.ID),
		Problem: in.Problem,
		Ban:     ban,
		Reason:  u.BanReason,
		Hidden:  hidden,
	})
}

// about, saved и bad читают указатель, которого может не быть: вызывающие
// приходят с пустым mePage чаще, чем с заполненным, и три проверки на nil в
// сборщике читались бы хуже трёх методов.
func (e *profileEdit) about() platform.About {
	if e == nil {
		return platform.About{}
	}
	return e.About
}

func (e *profileEdit) saved() bool { return e != nil && e.Saved }

func (e *profileEdit) bad() string {
	if e == nil {
		return ""
	}
	return e.Bad
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := s.me(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.showSettings(w, r, u, "")
}

// showSettings рисует вкладку настроек.
func (s *Server) showSettings(w http.ResponseWriter, r *http.Request, u platform.User, problem string) {
	if !s.meReady(w, r, u) {
		return
	}
	docs, err := platform.RequiredConsentDocs(s.cfg.Operator)
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
	// Галочка выноса на НГС. Её отказ страницу не роняет: настройки нужны
	// человеку и ради согласий, и ради дверей входа, а состояние одной галочки
	// этого не стоит.
	var (
		ngsSend    bool
		ngsStuck   int
		ngsStuckAt time.Time
		ngsPending int
	)
	if s.wr != nil && platform.IsNGS(u.ID) {
		if on, err := s.wr.NGSSendOn(r.Context(), u.ID); err == nil {
			ngsSend = on
		} else {
			s.log.Warn("состояние отправки на НГС не прочитано", "user", u.ID, "err", err)
		}
		// Спрашиваем ТОЛЬКО у включивших: у остальных очередь пуста по замыслу,
		// и лишний запрос на каждый заход был бы платой ни за что.
		if ngsSend {
			if n, since, err := s.wr.NGSStuck(r.Context(), u.ID); err == nil {
				ngsStuck, ngsStuckAt = n, since
			} else {
				s.log.Warn("застрявшее на НГС не прочитано", "user", u.ID, "err", err)
			}
			if n, err := s.wr.NGSDraftsPending(r.Context(), u.ID); err == nil {
				ngsPending = n
			} else {
				s.log.Warn("заметки в пути не прочитаны", "user", u.ID, "err", err)
			}
		}
	}
	// Привязки читаются ВСЕГДА: это единственное место, где человек видит, каким
	// ключом от его записи владеет мессенджер, — и единственное, где чужая
	// привязка (подсунутый код) бросится в глаза.
	bindings, err := s.auth.UserBindings(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "привязки", err)
		return
	}
	// Чёрный список переписки. Здесь он потому, что снять запрет больше негде:
	// закрытая переписка ушла из списка писем, а на странице закрытого стои́т
	// причина без кнопки — сама причина и отсылает сюда. Спрашивается независимо
	// от согласия: отозвавший его вправе разбирать свои прежние запреты.
	//
	// Отказ страницу не роняет, как и у галочки выноса: настройки нужны ради
	// согласий и дверей входа, и одного раздела это не стоит.
	var blocked []platform.Author
	if s.mail != nil {
		if list, err := s.mail.BlockedList(r.Context(), u.ID); err == nil {
			blocked = list
		} else {
			s.log.Warn("чёрный список переписки не прочитан", "user", u.ID, "err", err)
		}
	}
	s.render(w, r, http.StatusOK, "settings.gohtml", settingsPage{
		page:        s.newPage(r, "Настройки"),
		Member:      card,
		Problem:     problem,
		Docs:        docs,
		Have:        have,
		Jump:        s.jumpFresh(r),
		NGSSend:     ngsSend,
		NGSSendable: platform.IsNGS(u.ID) && u.Kind == platform.KindMember,
		NGSStuck:    ngsStuck,
		NGSStuckAt:  ngsStuckAt,
		NGSPending:  ngsPending,
		Bindings:    bindings,
		Blocked:     blocked,
		// Дверь эта работает, только пока жив САЙТ: код читается со страницы
		// анкеты. Нет клиента НГС — /login про неё и не говорит, и обещать её
		// здесь значило бы нарисовать кнопку, отвечающую отказом. Тот же довод,
		// по которому на профиле прячется «Обновить аватар».
		NGSDoor: platform.IsNGS(u.ID),
		SiteUp:  s.site != nil,
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
// Своего файла площадка на аватар не принимает, и это не экономия: аватар стои́т
// под каждой репликой человека за тринадцать лет, а фотография в альбоме
// разовая и каждую смотрит модератор.
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
		s.showMe(w, r, u, mePage{Problem: "НГС не отдал вашу анкету: она скрыта целиком или удалена. Фото осталось прежним."})
		return
	case err != nil:
		// Отказ ЧУЖОГО сайта не наша поломка, и 500 на своей странице тут врал бы.
		s.log.Warn("анкета НГС для обновления фото", "user", u.ID, "err", err)
		s.showMe(w, r, u, mePage{Problem: "НГС сейчас не отвечает. Фото осталось прежним — попробуйте позже."})
		return
	}
	if prof.AvatarURL == "" {
		// Фото в анкете нет (силуэт по умолчанию клиент НГС сюда не пропускает).
		// Своё при этом НЕ снимаем: аватара из своего файла площадка не
		// принимает, вернуть его было бы неоткуда, а «нажал обновить и остался
		// без фото» — это потеря по нажатию кнопки.
		s.showMe(w, r, u, mePage{Problem: "В анкете НГС сейчас нет фото — здесь всё осталось как было."})
		return
	}
	data, err := s.site.Avatar(r.Context(), prof.AvatarURL)
	if err != nil {
		s.log.Warn("фото анкеты НГС", "user", u.ID, "err", err)
		s.showMe(w, r, u, mePage{Problem: "Фото из анкеты сейчас не забралось. Попробуйте позже."})
		return
	}
	if err := s.wr.SetOwnAvatar(r.Context(), u.ID, prof.AvatarURL, data); err != nil {
		s.oops(w, r, "смена фото", err)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// handleAvatarClear — «Убрать фото»: снять аватар и остаться без него.
//
// Зачем вторая кнопка рядом с первой. «Обновить аватар» пустую анкету НГС за
// причину снять фото не считает: аватара из своего файла площадка не принимает,
// и потеря по нажатию кнопки была бы невозвратной. Но у стёршего фото В АНКЕТЕ
// из этого выходил тупик — на НГС фото уже нет, само оно сюда больше не
// приезжает, а кнопка отвечает «здесь всё осталось как было» (жалоба владельца,
// 28.08.2026). Разница между двумя кнопками не в осторожности, а в том, ЧЬЯ это
// рука.
//
// На НГС этот путь не ходит вовсе, и отсюда два следствия: стоит он как обычная
// запись (costWrite, см. goesToNGS), а работает и при мёртвом сайте — то есть
// переживёт закрытие НГС, в отличие от соседней кнопки.
//
// Байты из хранилища не удаляются (platform.ClearAvatar): имя файла есть его
// содержимое, и на ту же картинку ссылаются чужие строки.
func (s *Server) handleAvatarClear(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	if err := s.wr.ClearOwnAvatar(r.Context(), u.ID); err != nil {
		s.oops(w, r, "снятие фото", err)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// handleNGSSend переключает вынос написанного на love.ngs.ru.
//
// Формой с POST, а не ссылкой: это изменение согласия на распространение
// собственных слов на чужом сайте, и делаться оно должно нажатием, которое
// нельзя получить чужой ссылкой. Тот же приём, что у выхода и смены темы.
func (s *Server) handleNGSSend(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.writer(w, r)
	if !ok {
		return
	}
	// Значение приходит явным полем, а не «переключи на противоположное»: две
	// вкладки, открытые на настройках, иначе переключали бы галочку друг у
	// друга, и человек получил бы включённую отправку, нажав «выключить».
	on := r.FormValue("on") == "1"
	if err := s.wr.SetNGSSend(r.Context(), u.ID, on); err != nil {
		s.oops(w, r, "переключение отправки на НГС", err)
		return
	}
	http.Redirect(w, r, "/me/settings", http.StatusSeeOther)
}

// revokePage — экран «что произойдёт, если отозвать».
type revokePage struct {
	page
	Kind string
	// DocTitle — заголовок отзываемого документа, ровно тот, что человек
	// подписывал. Не Title: так зовётся заголовок ВКЛАДКИ у каркаса, и поле
	// страницы затенило бы его (page_shadow_test.go).
	DocTitle string
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
	// НЕОБЯЗАТЕЛЬНОЕ согласие на привязку ОТЗЫВАЕТСЯ здесь же, а вот ДАЁТСЯ
	// только на своём экране (/me/bind): подпись имеет смысл рядом с текстом и
	// действием, ради которого её просят, а «дать снова» в общем списке
	// завело бы согласие, под которым нет никакой привязки.
	switch {
	case kind == platform.ConsentProcessing, kind == platform.ConsentDistribution:
	case kind == platform.ConsentBinding && r.FormValue("action") == "revoke":
	// Согласие на переписку — той же породы: отзывается здесь, а даётся на
	// своём экране (/mail/consent), где перед кнопкой стои́т документ. Отзыв
	// вдобавок ЗАКРЫВАЕТ ВХОДЯЩИЕ (решение владельца 11.09.2026), то есть это не
	// «перестать писать», а «перестать получать», — и «дать снова» в общем
	// списке завело бы согласие мимо текста, который оно подтверждает.
	case kind == platform.ConsentTalks && r.FormValue("action") == "revoke":
	// Рассказ о себе — той же породы и по тому же доводу: подписывают его на
	// своём экране (/me/about), где документ стои́т ДО кнопки, а отзыв уносит
	// город, занятие, текст и все снимки вместе с байтами.
	case kind == platform.ConsentProfile && r.FormValue("action") == "revoke":
	default:
		s.fail(w, r, http.StatusBadRequest, "Такого согласия нет.")
		return
	}
	if r.FormValue("action") == "grant" {
		docs, err := platform.RequiredConsentDocs(s.cfg.Operator)
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
		http.Redirect(w, r, "/me/settings", http.StatusSeeOther)
		return
	}
	if r.FormValue("confirm") != "1" {
		// Заголовок ищется среди ВСЕХ документов, а не среди обязательных:
		// необязательного среди них нет по построению, и прежний поиск оставлял
		// человеку сырое слово «binding» вместо названия.
		doc, err := platform.ConsentDocOf(s.cfg.Operator, kind)
		if err != nil {
			s.oops(w, r, "тексты согласий", err)
			return
		}
		s.render(w, r, http.StatusOK, "revoke.gohtml", revokePage{
			page:       s.newPage(r, "Отзыв согласия"),
			Kind:       kind,
			DocTitle:   doc.Title,
			Processing: kind == platform.ConsentProcessing,
			// Список последствий выбирает ШАБЛОН, по виду документа: экран
			// обезличивания годится только двум обязательным. У необязательных
			// последствия совсем другие, и общий список обещал бы то, чего не
			// случится, — четыре страшных пункта подряд, ни один из которых не
			// сбудется, это не строгость, а неправда. Первым такую неправду
			// получал отзыв переписки: ему обещали, что имя уйдёт со всех
			// заметок.
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
	http.Redirect(w, r, "/me/settings", http.StatusSeeOther)
}
