package web

// Страница участника: карточка, биография жителя и последние публикации.
//
// Своей страницы у человека на площадке не было вовсе — её роль на НГС играет
// анкета, а анкеты мы не заводили. Понадобилась она жителям (эпик «народ»: у
// персонажа есть биография, и показать её было негде), но вышла общей: две
// страницы про человека, одна для живых и одна для придуманных, разошлись бы на
// первом же новом поле, а различие между ними и так называется на самой
// странице.
//
// ВОШЕДШИМ, а не всем (решение владельца 30.08.2026). Читать площадку может кто
// угодно, и это правило Ш3 — но страница участника собирает в одно место то, что
// иначе разбросано по тредам, и собранное закрыто и от гостя, и от поисковика
// (см. privateRoots). Отсюда же ник, который у гостя остаётся текстом: ссылка,
// ведущая к отказу, хуже её отсутствия.
//
// Решения модератора живут ЗДЕСЬ, а не на отдельной странице под /mod, и это
// тот же довод, что у полоски под репликой: модератор работает там, где читает.
// Прежняя карточка /mod/u/<id> осталась одним переходом сюда — двум страницам
// про одного человека нечего делить.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"lovegw/internal/platform"
)

type userPage struct {
	page
	Member   platform.Profile
	Banned   bool
	Notes    []platform.PubNote
	Comments []platform.PubComment
	// CanModerate и CanAdmin — показывать ли решения. Два поля, а не одно:
	// запрет писать ставит модератор, роли раздаёт только администратор, и это
	// то самое различие «про слова / про людей», ради которого двери разведены.
	CanModerate bool
	CanAdmin    bool
	// Me — своя собственная страница. Нужно ровно затем, чтобы не предлагать
	// модератору забанить самого себя.
	Me bool
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	me, ok := s.me(r)
	if !ok {
		// Страница ЕСТЬ, и войдя, человек её увидит, — поэтому вход, а не «нет
		// такой страницы». Скрывать существование чужого профиля незачем: на
		// него ссылается каждая реплика в треде.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такого участника нет.")
		return
	}
	member, err := s.st.UserProfile(r.Context(), id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такого участника нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "страница участника", err)
		return
	}
	notes, err := s.st.AuthorNotes(r.Context(), id, 0)
	if err != nil {
		s.oops(w, r, "заметки участника", err)
		return
	}
	comments, err := s.st.AuthorComments(r.Context(), id, 0)
	if err != nil {
		s.oops(w, r, "реплики участника", err)
		return
	}
	s.render(w, r, http.StatusOK, "user.gohtml", userPage{
		page:        s.newPage(r, userTitle(member)),
		Member:      member,
		Banned:      member.Banned(time.Now()),
		Notes:       notes,
		Comments:    comments,
		CanModerate: me.Role >= platform.RoleModerator && s.mod != nil,
		CanAdmin:    me.Role >= platform.RoleAdmin && s.mod != nil,
		Me:          me.ID == member.ID,
	})
}

// userTitle — заголовок вкладки. Ник, а нет его (обезличенный, тень без имени)
// — номер: пустая вкладка в ряду из двадцати не отличима ни от чего.
func userTitle(m platform.Profile) string {
	if m.Nick != "" {
		return m.Nick
	}
	return "Участник №" + strconv.FormatInt(m.ID, 10)
}

// userDecisions — аргумент шаблона «moddec»: решения модератора и
// администратора об одном человеке.
//
// Собирается в Go по той же причине, что и modAct у полоски под репликой:
// «что кому показать» — решение, и приниматься оно обязано там же, где все
// остальные.
type userDecisions struct {
	ID          int64
	Nick        string
	Banned      bool
	BannedUntil *time.Time
	BanReason   string
	Role        platform.Role
	CanAdmin    bool
	CSRF        string
	Back        string
}

func userDecisionsOf(p userPage) userDecisions {
	return userDecisions{
		ID:          p.Member.ID,
		Nick:        p.Member.Nick,
		Banned:      p.Banned,
		BannedUntil: p.Member.BannedUntil,
		BanReason:   p.Member.BanReason,
		Role:        p.Member.Role,
		CanAdmin:    p.CanAdmin,
		CSRF:        p.CSRF,
		Back:        p.Back,
	}
}

// kindWord — чем человек здесь является, словами.
//
// Житель назван первым и вслух: читатель вправе знать, что реплики этого автора
// пишет машина, а справка (/help#narod) имён не называет намеренно — список из
// десяти ников устареет через неделю, признак у анкеты не устареет.
func kindWord(m platform.Profile) string {
	switch {
	case m.AnonymizedAt != nil:
		return "обезличенная запись"
	case m.Persona:
		return "житель площадки"
	case m.Kind == platform.KindShadow:
		return "ещё не переехал сюда с НГС"
	case m.Kind == platform.KindService:
		return "служебная анкета"
	default:
		return "участник"
	}
}
