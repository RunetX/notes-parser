package web

// Письма — личная переписка участников (эпик L, Ш2: морда за гейтом).
//
// Седьмой узкий интерфейс рядом со Store, Auth, Writer, Moderator, Site и
// Events, и подключается он отдельным вызовом SetMail — по тому же доводу, что
// шина: способность необязательная, а конструктор New и без того на пределе.
//
// nil ⇒ ни страниц, ни пункта меню, ни кнопки «Написать», то есть написать
// письмо нечем. Это и есть гейт `platform.mail.enabled`, и природа у него своя:
// пока не закрыт Ш0 (ответ юриста по ч. 4 ст. 10.1 и уведомление РКН —
// docs/mail-149fz.md), принимать чужие письма площадка не вправе ВОВСЕ, а не
// «вправе, но мы пока не показываем». Поэтому выключенная переписка отвечает
// «нет такой страницы», а не «войдите» и не «скоро откроем».
//
// Маршруты при этом заведены ВСЕГДА и отказывают внутри обработчика — как /mod
// без модерации: второй список маршрутов, собираемый по условию, однажды
// разошёлся бы с первым, и разошёлся бы молча.
//
// Чего здесь нет намеренно. Жалобы на письмо (Ш4) и чёрного списка (Ш3): их
// кнопки появятся вместе с их ядром, а кнопка, отвечающая отказом, хуже
// отсутствующей. Живого обновления (Ш5): страница дописывает себя только
// перезагрузкой, и сказано об этом на самой странице, а не умолчано. Пятого
// значка в шапке: он уже ломал телефон 23.08.2026 (довод стоит прямо в
// base.gohtml), а о новом письме говорит колокольчик — пункт меню несёт число
// и этого довольно.

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// Mail — что морде нужно от переписки.
//
// Список этот сам по себе есть исчерпывающий ответ на вопрос «что площадка
// позволяет делать с чужими письмами», и потому он короткий. Прочесть можно
// только СВОЮ переписку (ядро отвечает постороннему ErrNotFound), отправить —
// только от своего имени, а метода, который отдаёт чужую переписку целиком,
// здесь нет и не будет: его нет и в ядре, и это единственная надёжная форма
// обещания «модератор видит только процитированное в жалобе».
type Mail interface {
	// CanWriteTo — можно ли написать этому человеку и есть ли уже переписка.
	// ОДНО правило на ядро и на кнопку: второй список условий в шаблоне
	// однажды нарисовал бы кнопку, отвечающую отказом.
	CanWriteTo(ctx context.Context, meID, peerID int64) (int64, error)
	SendMessage(ctx context.Context, senderID, recipientID int64, body string) (platform.MessageSent, error)
	Dialogs(ctx context.Context, userID int64, offset, limit int) ([]platform.DialogView, error)
	CountDialogs(ctx context.Context, userID int64) (int, error)
	Dialog(ctx context.Context, userID, dialogID int64, offset, limit int) (platform.DialogHead, []platform.MessageView, error)
	MarkDialogRead(ctx context.Context, userID, dialogID, uptoID int64) error
	UnreadMail(ctx context.Context, userID int64) (int, error)
	HideDialog(ctx context.Context, userID, dialogID int64) error
	// Чёрный список. Решение ЛИЧНОЕ, а не модераторское: «мне от этого
	// человека писем не нужно» — не обвинение и разбирательства не требует,
	// поэтому в audit_log ядро об этом не пишет ни строки. Для жалобы есть
	// отдельная дорога (Ш4), и путать их нельзя: закрытая переписка никого не
	// зовёт разбираться, а жалоба зовёт.
	BlockUser(ctx context.Context, userID, blockedID int64) error
	UnblockUser(ctx context.Context, userID, blockedID int64) error
	BlockedList(ctx context.Context, userID int64) ([]platform.Author, error)
}

// SetMail подключает переписку. Отдельным вызовом, а не седьмым аргументом
// конструктора (образец SetEvents).
func (s *Server) SetMail(m Mail) { s.mail = m }

const (
	// mailPageSize — переписок на страницу. Двадцать, как поводов и заметок:
	// списки одного рода, и разнобой в числах читатель замечает раньше, чем
	// объясняет.
	mailPageSize = 20
	// letterPageSize — писем на страницу переписки. Полсотни, как в линейном
	// виде треда: это порция по индексу (dialog_id, id), а не дерево.
	letterPageSize = 50
)

// mailEra — как разбирать тело письма.
//
// Своя, а не общий eraOf, и это НЕ украшение. eraOf решает по полосе
// идентификатора (platform.IsNative), а у писем своя последовательность с
// единицы — то есть по номеру каждое письмо лежит в полосе НГС и считалось бы
// ЧУЖИМ текстом: ни разметки, ни смайлов. Человек нажал бы кнопку смайла под
// формой и получил «:::popcorn:::», а «[b]» осталось бы скобками — ровно то, на
// что владелец жаловался 21.08.2026 про свою заметку.
//
// Это четвёртый укус той же полосы (см. id-band-order-breaks-cursors), и лечится
// он не правкой eraOf, а тем, что у письма вопроса «чей это текст» НЕТ ВОВСЕ:
// письмо написано здесь всегда, других писем у площадки не бывает. Ссылки при
// этом остаются своими (linkOwn), как везде: чужой адрес в личном письме тем
// более не наше дело — инструмента контроля у нас нет, а уводить читателя по
// ссылке, за которую мы не отвечаем, нельзя и в переписке.
func mailEra() era {
	return era{markup: true, smiles: true, links: linkOwn}
}

// letterBodyHTML — тело письма. Отдельная функция, а не commentBodyHTML:
// обращения «Ник, » у письма нет вовсе (адресат один и известен), значит нет и
// книги адресатов, ради которой та берёт лишний аргумент.
func letterBodyHTML(m platform.MessageView) template.HTML {
	return renderBody("", m.Body, mailEra())
}

type mailPage struct {
	page
	Dialogs []platform.DialogView
	Pager   pager
}

type dialogPage struct {
	page
	Head    platform.DialogHead
	Letters []platform.MessageView
	Pager   pager
	Compose compose
	// Top — верхняя граница отметки «прочитано»: номер самого свежего письма на
	// ЭТОЙ странице. Отмечаем то, что человек видел, а не всё подряд, — тот же
	// довод, что у кнопки на странице событий.
	Top int64
	// Markable — есть ли что отмечать. Кнопка, которой нечего делать, хуже её
	// отсутствия.
	Markable bool
	// CanWrite и Why — можно ли ответить и почему нельзя. Спрашивается это у
	// ЯДРА (CanWriteTo), а не выводится из шапки: причин отказа больше, чем
	// чёрный список (собеседник отозвал согласие, обезличился, его забанили), и
	// второй их список здесь разошёлся бы с первым.
	CanWrite bool
	Why      string
	// Blocked — закрыл ли переписку Я САМ. Берётся из той же ошибки ядра
	// (ErrBlockedByYou), а не отдельным вопросом: кнопка «Закрыть» у уже
	// закрытой переписки — мёртвая, а площадка мёртвых кнопок не печатает.
	// Обратный случай (закрыли МЕНЯ) кнопки не даёт вовсе: снять чужой запрет
	// нельзя, и предлагать это значило бы врать.
	Blocked bool
}

type mailNewPage struct {
	page
	Peer    platform.Author
	Compose compose
	// Unanswered — сколько писем подряд можно написать тому, кто не ответил.
	// Числом ИЗ ЯДРА, а не словом в шаблоне: правило проекта оплачено справкой,
	// которая до 08.09.2026 обещала тридцать реплик в час при девяноста
	// настоящих, — страница, разошедшаяся с поведением кнопки, хуже
	// отсутствующей.
	Unanswered int
}

type mailConsentPage struct {
	page
	Doc platform.ConsentDoc
	// To — кому человек шёл писать, когда его отвели сюда. Ноль означает «ни к
	// кому»: подпись со страницы писем возвращает в список.
	To int64
}

// mailReader — вошедший, которому есть что показать. Пустой второй результат
// означает «ответ уже отправлен».
//
// Гейт спрашивается ПЕРВЫМ, до входа, и это не мелочь: при выключенной переписке
// страницы нет ни у кого, и гость обязан получить тот же ответ, что и
// участник, — иначе «войдите» обещало бы ему страницу, которой за дверью нет.
func (s *Server) mailReader(w http.ResponseWriter, r *http.Request) (platform.User, bool) {
	if s.mail == nil {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return platform.User{}, false
	}
	u, ok := s.me(r)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			s.fail(w, r, http.StatusUnauthorized, "Чтобы читать письма, нужно войти.")
		}
		return platform.User{}, false
	}
	return u, true
}

// handleMail — список переписок.
func (s *Server) handleMail(w http.ResponseWriter, r *http.Request) {
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	num := pageParam(r.URL.Query().Get("page"))
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx := r.Context()
	total, err := s.mail.CountDialogs(ctx, u.ID)
	if err != nil {
		s.oops(w, r, "счётчик переписок", err)
		return
	}
	pages := pageCount(total, mailPageSize)
	if num > pages {
		s.fail(w, r, http.StatusNotFound, "Такой страницы писем нет.")
		return
	}
	list, err := s.mail.Dialogs(ctx, u.ID, (num-1)*mailPageSize, mailPageSize)
	if err != nil {
		s.oops(w, r, "переписки", err)
		return
	}
	s.render(w, r, http.StatusOK, "mail.gohtml", mailPage{
		page:    s.newPage(r, "Письма"),
		Dialogs: list,
		Pager:   newPager(num, pages, mailURL),
	})
}

// handleDialog — одна переписка.
//
// Открывается она на ПОСЛЕДНЕЙ странице, а не на первой: письма идут по
// возрастанию, как разговор, и человек приходит читать его конец. Цена приёма
// названа прямо — у переписки длиннее полусотни писем запросов выходит два:
// первый узнаёт, сколько их всего (это же и есть страница у короткой переписки,
// то есть у всех сегодня), второй берёт последнюю порцию.
func (s *Server) handleDialog(w http.ResponseWriter, r *http.Request) {
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	id, ok := s.dialogID(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("page")
	num := pageParam(raw)
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx := r.Context()
	head, letters, err := s.mail.Dialog(ctx, u.ID, id, (num-1)*letterPageSize, letterPageSize)
	if err != nil {
		s.dialogFail(w, r, err)
		return
	}
	pages := pageCount(head.Total, letterPageSize)
	if raw == "" && pages > 1 {
		num = pages
		head, letters, err = s.mail.Dialog(ctx, u.ID, id, (num-1)*letterPageSize, letterPageSize)
		if err != nil {
			s.dialogFail(w, r, err)
			return
		}
	}
	if num > pages {
		s.fail(w, r, http.StatusNotFound, "Такой страницы переписки нет.")
		return
	}
	s.showDialog(w, r, u, head, letters, num, pages, compose{})
}

// showDialog рисует переписку. Отдельно от обработчика, потому что рисуют её
// двое: сам показ и отказ отправки, у которого на руках уже набранный текст.
func (s *Server) showDialog(w http.ResponseWriter, r *http.Request, u platform.User,
	head platform.DialogHead, letters []platform.MessageView, num, pages int, c compose) {
	p := dialogPage{
		page:    s.newPage(r, "Переписка с "+head.Peer.Nick),
		Head:    head,
		Letters: letters,
		Pager:   newPager(num, pages, dialogURL(head.DialogID)),
		Compose: c,
	}
	for _, l := range letters {
		if l.ID > p.Top {
			p.Top = l.ID
		}
	}
	p.Markable = head.Unread > 0 && p.Top > head.LastReadID
	// Право ответить спрашивается у ядра — одним вызовом, тем же самым, каким
	// решается кнопка «Написать» на странице участника.
	if _, err := s.mail.CanWriteTo(r.Context(), u.ID, head.Peer.ID); err != nil {
		_, p.Why = mailProblem(err)
		p.Blocked = errors.Is(err, platform.ErrBlockedByYou)
	} else {
		p.CanWrite = true
	}
	status := http.StatusOK
	if c.Problem != "" {
		status = http.StatusUnprocessableEntity
	}
	s.render(w, r, status, "mail_thread.gohtml", p)
}

// handleDialogReply — ответ в существующей переписке.
func (s *Server) handleDialogReply(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	id, ok := s.dialogID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	body := r.FormValue("body")
	// Шапка нужна до отправки: в ней адресат, а письмо отправляется ЧЕЛОВЕКУ, а
	// не в переписку. Номер собеседника из формы брать нельзя — подделанная
	// страница отправила бы письмо кому угодно от чужого имени.
	head, letters, err := s.mail.Dialog(ctx, u.ID, id, 0, letterPageSize)
	if err != nil {
		s.dialogFail(w, r, err)
		return
	}
	if _, err := s.mail.SendMessage(ctx, u.ID, head.Peer.ID, body); err != nil {
		_, problem := mailProblem(err)
		if problem == "" {
			s.oops(w, r, "письмо", err)
			return
		}
		// Отказ рисует ТУ ЖЕ страницу с набранным текстом в форме: письмо,
		// пропавшее вместе со страницей отказа, — худшее, что можно сделать с
		// человеком, который его только что написал.
		pages := pageCount(head.Total, letterPageSize)
		if pages > 1 {
			head, letters, err = s.mail.Dialog(ctx, u.ID, id, (pages-1)*letterPageSize, letterPageSize)
			if err != nil {
				s.dialogFail(w, r, err)
				return
			}
		}
		s.showDialog(w, r, u, head, letters, pages, pages, compose{Body: body, Problem: problem})
		return
	}
	http.Redirect(w, r, dialogPath(id), http.StatusSeeOther)
}

// handleMailNew — первое письмо незнакомому человеку.
func (s *Server) handleMailNew(w http.ResponseWriter, r *http.Request) {
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if err != nil || to <= 0 {
		s.fail(w, r, http.StatusNotFound, "Некому писать.")
		return
	}
	ctx := r.Context()
	dialog, err := s.mail.CanWriteTo(ctx, u.ID, to)
	switch {
	case errors.Is(err, platform.ErrNoTalkConsent):
		// Не подписан документ — ведём к нему, а не прячем форму: спрятанное
		// ничего не объясняет.
		http.Redirect(w, r, "/mail/consent?to="+strconv.FormatInt(to, 10), http.StatusSeeOther)
		return
	case err != nil:
		code, problem := mailProblem(err)
		if problem == "" {
			s.oops(w, r, "проверка адресата", err)
			return
		}
		s.fail(w, r, code, problem)
		return
	case dialog != 0:
		// Переписка уже есть — писать надо в неё, а не заводить второй экран
		// того же разговора.
		http.Redirect(w, r, dialogPath(dialog), http.StatusSeeOther)
		return
	}
	peer, err := s.auth.MemberCard(ctx, to)
	if err != nil {
		s.oops(w, r, "карточка участника", err)
		return
	}
	s.render(w, r, http.StatusOK, "mail_new.gohtml", mailNewPage{
		page:       s.newPage(r, "Письмо"),
		Peer:       peer,
		Unanswered: platform.UnansweredMax,
	})
}

// handleMailSend — отправка первого письма.
func (s *Server) handleMailSend(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	to, err := strconv.ParseInt(r.FormValue("to"), 10, 64)
	if err != nil || to <= 0 {
		s.fail(w, r, http.StatusNotFound, "Некому писать.")
		return
	}
	body := r.FormValue("body")
	sent, err := s.mail.SendMessage(r.Context(), u.ID, to, body)
	if err != nil {
		code, problem := mailProblem(err)
		if problem == "" {
			s.oops(w, r, "письмо", err)
			return
		}
		peer, cerr := s.auth.MemberCard(r.Context(), to)
		if cerr != nil {
			s.fail(w, r, code, problem)
			return
		}
		s.render(w, r, http.StatusUnprocessableEntity, "mail_new.gohtml", mailNewPage{
			page:       s.newPage(r, "Письмо"),
			Peer:       peer,
			Compose:    compose{Body: body, Problem: problem},
			Unanswered: platform.UnansweredMax,
		})
		return
	}
	http.Redirect(w, r, dialogPath(sent.DialogID), http.StatusSeeOther)
}

// handleMailRead — отметить переписку прочитанной.
//
// Формой с POST, а не самим открытием страницы, и это не педантизм: GET-адрес
// браузер нажимает сам в префетче — тот же довод, по которому формой сделаны
// выход и смена темы. Цена названа: пока человек не нажал, колокольчик горит.
func (s *Server) handleMailRead(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	id, ok := s.dialogID(w, r)
	if !ok {
		return
	}
	// Разбор границы нестрогий: мусор означает «до самого верха», а не отказ.
	// Кнопка «прочитано» — не то место, где человеку показывают ошибку формы.
	upto, _ := strconv.ParseInt(r.FormValue("upto"), 10, 64)
	if err := s.mail.MarkDialogRead(r.Context(), u.ID, id, max(0, upto)); err != nil {
		s.dialogFail(w, r, err)
		return
	}
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}

// handleMailHide — убрать переписку из своего списка.
func (s *Server) handleMailHide(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	id, ok := s.dialogID(w, r)
	if !ok {
		return
	}
	if err := s.mail.HideDialog(r.Context(), u.ID, id); err != nil {
		s.dialogFail(w, r, err)
		return
	}
	http.Redirect(w, r, "/mail", http.StatusSeeOther)
}

// handleMailBlock и handleMailUnblock — чёрный список.
//
// Отдельно от «убрать у себя», и различие это НЕ оттенок. Скрытие прячет
// переписку в своём списке и ничего не отнимает у собеседника — новое письмо
// поднимет её обратно; закрытие отнимает у него право писать, и он видит
// прямой отказ, а не тишину. Молчаливой блокировки у площадки нет вовсе
// (решение владельца 11.09.2026): она копит у отправителя переписку, которой
// никто не читает, — та же травля, только невидимая, и притом обоюдная.
//
// Адресат берётся из формы, и подделать его нечем: ядро кладёт запрет от имени
// вошедшего, то есть подставленный номер закроет переписку с кем-то ещё — себе
// же. Чужого запрета этим не снять и чужого не поставить.
func (s *Server) handleMailBlock(w http.ResponseWriter, r *http.Request) {
	s.setBlock(w, r, true)
}

func (s *Server) handleMailUnblock(w http.ResponseWriter, r *http.Request) {
	s.setBlock(w, r, false)
}

func (s *Server) setBlock(w http.ResponseWriter, r *http.Request, on bool) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	to, err := strconv.ParseInt(r.FormValue("to"), 10, 64)
	if err != nil || to <= 0 {
		s.fail(w, r, http.StatusNotFound, "Некого закрывать.")
		return
	}
	act := s.mail.UnblockUser
	if on {
		act = s.mail.BlockUser
	}
	if err := act(r.Context(), u.ID, to); err != nil {
		code, problem := mailProblem(err)
		if problem == "" {
			s.oops(w, r, "чёрный список", err)
			return
		}
		s.fail(w, r, code, problem)
		return
	}
	// Возврат туда, откуда нажали: закрывают из переписки, снимают чаще со
	// своей страницы. Адрес проверяется localPath — своим, а не любым.
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}

// handleMailConsent — четвёртый документ.
//
// Своим экраном, а не абзацем под формой письма: подпись, данная мимоходом под
// кнопкой, подписью не является (тот же довод, что у /me/bind).
func (s *Server) handleMailConsent(w http.ResponseWriter, r *http.Request) {
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentTalks)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	have, err := s.auth.UserConsents(r.Context(), u.ID)
	if err != nil {
		s.oops(w, r, "согласия", err)
		return
	}
	to, _ := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if have.Has(platform.ConsentTalks, doc.Version) {
		// Подписано — показывать документ второй раз незачем, человек шёл
		// писать.
		http.Redirect(w, r, mailNext(to), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "mail_consent.gohtml", mailConsentPage{
		page: s.newPage(r, "Согласие на переписку"),
		Doc:  doc, To: to,
	})
}

// handleMailConsentGrant — подпись.
func (s *Server) handleMailConsentGrant(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.mailReader(w, r)
	if !ok {
		return
	}
	doc, err := platform.ConsentDocOf(s.cfg.Operator, platform.ConsentTalks)
	if err != nil {
		s.oops(w, r, "текст согласия", err)
		return
	}
	if err := s.auth.GrantConsent(r.Context(), u.ID, doc.Kind, doc.Version, r.UserAgent()); err != nil {
		s.oops(w, r, "запись согласия", err)
		return
	}
	to, _ := strconv.ParseInt(r.FormValue("to"), 10, 64)
	http.Redirect(w, r, mailNext(to), http.StatusSeeOther)
}

// mailNext — куда вести после подписи: к тому, кому человек шёл писать, а нет
// такого — в список писем.
func mailNext(to int64) string {
	if to > 0 {
		return "/mail/new?to=" + strconv.FormatInt(to, 10)
	}
	return "/mail"
}

// dialogID — номер переписки из адреса.
func (s *Server) dialogID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой переписки нет.")
		return 0, false
	}
	return id, true
}

// dialogFail — отказ на чтении переписки.
//
// Чужая переписка и несуществующая отвечаются ОДИНАКОВО, потому что так же
// отвечает ядро: существование чужой переписки — само по себе сведения.
func (s *Server) dialogFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "Такой переписки нет.")
	case errors.Is(err, platform.ErrNoTalkConsent):
		http.Redirect(w, r, "/mail/consent", http.StatusSeeOther)
	default:
		s.oops(w, r, "переписка", err)
	}
}

// mailProblem переводит отказ ядра в текст для человека. Пустая строка означает
// «это не отказ по правилам, а поломка» — такое уходит в oops.
//
// Своих случаев ровно столько, сколько своих ошибок у переписки; всё остальное
// (частота, бан, отозванное согласие, пустое и длинное тело) отдаётся общему
// writeProblem, потому что правила там ОДНИ И ТЕ ЖЕ — второй перевод тех же
// ошибок разошёлся бы с первым словами, а не смыслом.
func mailProblem(err error) (int, string) {
	switch {
	case errors.Is(err, platform.ErrNoRecipient):
		// Причина не называется, и это решение ядра, а не скупость показа:
		// перебором номеров иначе выясняется, кто завёл переписку, а кто её
		// выключил.
		return http.StatusForbidden,
			"Этот участник не принимает личных писем."
	case errors.Is(err, platform.ErrSelfMessage):
		return http.StatusBadRequest, "Написать самому себе нельзя."
	case errors.Is(err, platform.ErrNoTalkConsent):
		return http.StatusForbidden,
			"Личная переписка требует отдельного согласия — его подписывают на своей странице."
	case errors.Is(err, platform.ErrBlockedByPeer):
		// Говорим ПРЯМО (решение владельца 11.09.2026): молчаливая блокировка
		// копит у отправителя переписку, которой никто не читает, — это та же
		// травля, только невидимая.
		return http.StatusForbidden,
			"Этот участник закрыл от вас переписку. Письмо не отправлено."
	case errors.Is(err, platform.ErrBlockedByYou):
		// Место, где снимать, здесь НЕ называется: кнопка стои́т рядом с этой
		// строкой везде, где строка показывается, — и в переписке, и на
		// странице участника. Назови мы одну страницу, текст спорил бы с
		// соседней кнопкой.
		return http.StatusForbidden,
			"Вы закрыли переписку с этим участником: писать ему нельзя, пока не снят запрет."
	case errors.Is(err, platform.ErrUnanswered):
		n := platform.UnansweredMax
		return http.StatusTooManyRequests,
			"Вы уже написали " + strconv.Itoa(n) + " " + plural(n, "письмо", "письма", "писем") +
				" подряд, а ответа не было. Подождите ответа — так честнее и вам, и собеседнику."
	case errors.Is(err, platform.ErrRateLimited):
		// Отказ по частоте перехватывается ЗДЕСЬ, а не отдаётся общему
		// writeProblem, ради одного слова: у писем свои правила частоты
		// (platform.MessageWindow и соседи), а общий текст называл письмо
		// публикацией — то есть говорил про заметки там, где считались письма.
		return http.StatusTooManyRequests, rateProblem(err, rateLetters)
	}
	return writeProblem(err)
}

func mailURL(n int) string {
	if n <= 1 {
		return "/mail"
	}
	return "/mail?page=" + strconv.Itoa(n)
}

func dialogPath(id int64) string { return "/mail/" + strconv.FormatInt(id, 10) }

func dialogURL(id int64) func(int) string {
	return func(n int) string {
		if n <= 1 {
			return dialogPath(id)
		}
		return dialogPath(id) + "?page=" + strconv.Itoa(n)
	}
}

// plainLetter — начало письма для СПИСКА переписок: без разметки и без кодов
// смайлов.
//
// Не разбор, а снятие знаков, и это разные вещи. Выдержка приезжает из базы
// ОБРЕЗАННОЙ (left(body, N)), то есть в ней может не хватать закрывающей скобки
// и половины кода смайла; разбирать такое значит рисовать в списке то, чего в
// письме нет. Приём тот же, что у заголовка вкладки (stripMarkup): там, где
// разметке взяться неоткуда, её знаки не показывают, а снимают.
func plainLetter(s string) string { return stripMarkup(s, mailEra()) }
