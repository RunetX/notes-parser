package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

// linearPageSize — 30 комментариев на страницу линейного вида, как на НГС
// (`/notes/comments/312696/page~2/limit~30/`).
const linearPageSize = 30

// viewCookie — запомненный вид треда. Кука, а не только адрес: переключатель на
// сайте живой, им пользуются, и заново выбирать вид на каждой заметке — это
// раздражение на ровном месте.
const viewCookie = "thread"

// compose — состояние формы ответа на странице заметки. Живёт отдельно от
// canWriteIn — показывать ли под этой заметкой форму ответа.
//
// Правило вынесено в одно место, потому что мест ПОКАЗА три: сама страница,
// форма ответа, приезжающая без перезагрузки, и живой добор. Разъехавшись, они
// дали бы худшее из возможного — кнопку, которая отвечает отказом; тот же довод,
// по которому NoteView.Editable считается один раз на ядро и страницу.
//
// Три причины молчания, и все три разные:
//
//   - СКРЫТАЯ заметка: формы нет ни у кого, включая модератора, — ядро такой
//     комментарий не примет, для пишущего скрытая заметка просто отсутствует;
//   - ЗАМОК: разговор кончен для всех;
//   - ПЕСОЧНИЦА (эпик «народ»): разговор идёт, но не с нами — в ней говорят
//     жители, и участник её только читает. Администратор может: он вправе войти
//     в разговор своих жителей, не открывая песочницу всем, и то же самое
//     говорит ядро (platform.stageGuard). Модератору при этом нельзя — он решает
//     про слова, а не участвует.
//
// Формы гость не получает ни при какой причине, а вот ОБЪЯСНЕНИЕ получает:
// молчащее место под тредом читается поломкой (см. parts/reply.gohtml).
func canWriteIn(n platform.NoteView, me platform.User, signedIn, hasWriter bool) bool {
	switch {
	case !signedIn || !hasWriter || me.Kind != platform.KindMember:
		return false
	case n.Locked || n.Status != platform.StatusVisible:
		return false
	case n.Stage && me.Role < platform.RoleAdmin:
		return false
	}
	return true
}

// notePage, потому что приходит с двух сторон: из адреса (нажали «Ответить») и
// из отказа при публикации (тогда в ней ещё и набранный текст).
type compose struct {
	Body    string
	ReplyTo int64
	Problem string
}

type notePage struct {
	page
	Note     platform.NoteView
	Images   []platform.Media
	Comments []platform.CommentView
	// Linear — тред показан линейно (новые сверху), а не деревом.
	Linear bool
	// Replies — сколько ответов в ветке каждого комментария. Дерево показывается
	// целиком, поэтому число нужно не для «загрузить», а для того, чтобы ветку
	// можно было свернуть, зная её размер.
	Replies map[int64]int
	TreeURL string
	FlatURL string
	// ReplyBase — адрес этой же страницы, к которому дописывается выбор
	// адресата. Считается один раз здесь, потому что в нём и вид треда, и номер
	// страницы: «Ответить» не должно уводить человека из линейного вида в дерево.
	ReplyBase string
	Pager     pager
	// CanWrite — форма ответа показывается вошедшему участнику. Гостю вместо неё
	// приглашение войти: «читать можно всем» не значит «писать могут все».
	// Считается одной функцией canWriteIn на все три места показа.
	CanWrite bool
	// CanModerate — под каждой репликой видны кнопки «скрыть» / «вернуть», а в
	// дереве — уже скрытое. Модератор работает там, где читает: решать по цитате
	// в очереди, не видя разговора вокруг, — значит путать ссору с угрозой.
	CanModerate bool
	// Editable — своя заметка ещё в окне правки.
	Editable bool
	// AdminEdit — заметку вправе поправить администратор, и авторское окно тут
	// ни при чём: у площадки есть свои заметки (объявления, выпуск дайджеста),
	// а опечатку в них видно и через сутки. Только НАТИВНАЯ: зеркальную писали
	// на НГС, и текст копии не правится.
	AdminEdit bool
	Compose   compose
	// ReplyTo — адресат готовящегося ответа, если он выбран и ещё жив.
	ReplyTo *platform.CommentView
	// Reactions — реакции заметки (ключ 0) и её комментариев: один запрос на
	// страницу, а не по одному на реплику.
	Reactions map[int64][]platform.Reaction
	// ReactOpen — под каким объектом раскрыт выбор реакции (0 — под заметкой,
	// −1 — ни под кем). Раскрыт всегда не больше одного: выбиралка под каждой из
	// девятисот реплик это пять тысяч кнопок на странице.
	ReactOpen int64
	// PageNum — номер страницы линейного вида: с ним нажатие возвращает человека
	// туда же, где он был.
	PageNum int
	// Synth — СМЕЖНОЕ обсуждение этой заметки: тот же материал, о котором
	// говорят жители, но в своём треде и отдельной страницей (эпик «народ»).
	// Ноль в поле — двойника нет; у самого двойника его не бывает вовсе, и
	// запроса за ним не делается.
	Synth platform.NoteSynth
	// Origin — у самого ДВОЙНИКА: заметка, о которой здесь говорят. Её текст и
	// подпись автора рисуются цитатой прямо в карточке — без них страница
	// открывалась бы девяноста репликами о неизвестно чём, и узнать предмет
	// можно было бы только переходом (жалоба владельца 31.08.2026).
	//
	// Пусто у обычной заметки и у двойника, чей оригинал скрыт модератором:
	// показать убранный текст на соседней странице значило бы вернуть его на
	// вид. Карточка тогда остаётся с одной подписью — это честно.
	Origin platform.SynthOrigin
	// FreshOK и FreshAfter — живой добор: страница дописывает новые реплики сама
	// (fresh.go). Флаг отдельно от границы, потому что граница бывает нулевой у
	// пустого треда — а он-то как раз дописываться должен.
	//
	// Выключен добор на страницах линейного вида, кроме первой: там срез
	// истории, и дописывать в него хвост разговора значит врать о том, что
	// человек читает.
	FreshOK    bool
	FreshAfter string
	// Jump — «проматывать к новым»: страница встаёт на пришедшую реплику
	// (jump.go). Поле рядом с добором намеренно — работают они парой, и без
	// добора прыгать не к чему.
	Jump bool
	// FreshKnown — границу удалось спросить у ядра. Её отказ выключает добор, а
	// не страницу: дописываться она перестанет, читаться — нет.
	FreshKnown bool
	// Book — свидетельство этого треда о том, какие слова в нём ники (address.go).
	// Собирается по показанным репликам и нужно ровно для одного: не дорисовать
	// второе обращение там, где автор уже назвал адресата сам.
	Book *addressBook
}

func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	replyTo, _ := strconv.ParseInt(r.URL.Query().Get("reply"), 10, 64)
	s.showNote(w, r, id, http.StatusOK, compose{ReplyTo: replyTo})
}

// showNote рисует страницу заметки. Отдельно от обработчика, потому что тем же
// путём страница возвращается после неудачной публикации ответа — вместе с
// набранным текстом и причиной отказа.
func (s *Server) showNote(w http.ResponseWriter, r *http.Request, id int64, status int, form compose) {
	ctx, v := r.Context(), s.viewer(r)

	note, err := s.st.NoteViewByID(ctx, v, id)
	if errors.Is(err, platform.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	if err != nil {
		s.oops(w, r, "чтение заметки", err)
		return
	}
	// Скрытая заметка для читателя просто отсутствует. Говорить «она есть, но
	// спрятана» — значит показывать работу модерации посторонним, и это же
	// правило действует при публикации комментария в ядре.
	//
	// Модератору скрытое МОДЕРАЦИЕЙ видно: иначе вернуть его можно было бы только
	// из очереди, а после решения по очереди страница отвечала бы ему «нет такой
	// заметки». Скрытое АВТОРОМ (отзыв согласия) и обезличенное не показываются
	// никому — это исполнение права субъекта, а не спор о содержании.
	canMod := v.CanModerate() && s.mod != nil
	if note.Status != platform.StatusVisible && !(canMod && note.Status == platform.StatusHiddenMod) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}

	// Оригинал двойника берётся ДО сборки страницы: из него же складывается
	// заголовок вкладки. Отказ страницу не роняет — как у иллюстраций.
	var origin platform.SynthOrigin
	if note.SynthOf != 0 {
		if got, err := s.st.SynthOrigins(ctx, []int64{id}); err != nil {
			s.log.Warn("оригинал смежного обсуждения", "заметка", id, "err", err)
		} else {
			origin = got[id]
		}
	}

	images, err := s.st.NoteImages(ctx, id)
	if err != nil {
		s.oops(w, r, "иллюстрации заметки", err)
		return
	}

	linear := s.threadLinear(w, r)
	me, signedIn := s.me(r)
	// Реакции читаются одним запросом на всю страницу — и заметки, и треда. Свои
	// узнаются по id читателя; у гостя он нулевой, и «моих» не бывает.
	reactions, err := s.st.NoteReactions(ctx, me.ID, id)
	if err != nil {
		s.oops(w, r, "реакции заметки", err)
		return
	}
	p := notePage{
		page:        s.readingPage(r, synthTitle(note, origin)),
		Note:        note,
		Origin:      origin,
		Images:      images,
		Linear:      linear,
		TreeURL:     noteURL(id, false, 1),
		FlatURL:     noteURL(id, true, 1),
		CanWrite:    canWriteIn(note, me, signedIn, s.wr != nil),
		CanModerate: canMod,
		Editable:    note.Editable(time.Now()),
		// У ЛЮБОЙ заметки, включая зеркальную: текст копии не правится, но
		// картинку администратор ставит и ей (27.08.2026). Что именно откроется
		// в форме, решает editTarget, а подпись ссылке — modNote.
		AdminEdit: me.Role >= platform.RoleAdmin && s.mod != nil,
		Compose:   form,

		Reactions: reactions,
		ReactOpen: reactTarget(r),
		PageNum:   1,
	}
	// Счётчик в шапку: она липкая, и из середины треда до заголовка
	// «Комментарии N» уже не домотать. Признак отдельно от числа — счётчик стоит
	// и при нуле, иначе живому добору нечего подкручивать (см. page.InThread).
	p.InThread, p.Thread = true, note.CommentCount
	// Смежное обсуждение. Спрашивается ТОЛЬКО у обычной заметки: у двойника его
	// не бывает по правилу ядра, и лишнего попадания в индекс на его странице не
	// делаем. Отказ страницу не роняет — как у иллюстраций и мордоленты: без
	// ссылки страница остаётся страницей.
	if note.SynthOf == 0 {
		if twin, ok, err := s.st.SynthTwin(ctx, id); err != nil {
			s.log.Warn("смежное обсуждение", "заметка", id, "err", err)
		} else if ok {
			p.Synth = twin
		}
	}
	// Канонический адрес — ДЕРЕВО без параметров: линейный вид и его страницы
	// показывают те же реплики, что и оно, только иначе разложенные, а дерево
	// среди них единственное полное.
	if base := strings.TrimRight(s.cfg.BaseURL, "/"); base != "" {
		p.Canonical = base + "/n/" + strconv.FormatInt(id, 10)
	}

	// Граница живого добора берётся ДО чтения реплик и не по показанным строкам:
	// в линейном виде на странице окно из тридцати самых свежих, и полоса, в него
	// не попавшая, получила бы границу «с начала» — см. ThreadFreshAfter. Отказ
	// здесь страницу не роняет: без границы она просто не дописывается сама.
	if after, err := s.st.ThreadFreshAfter(ctx, id); err != nil {
		s.log.Warn("граница живого добора", "заметка", id, "err", err)
	} else {
		p.FreshAfter = threadCursor(after)
		p.FreshKnown = true
	}
	p.Jump = s.jumpFresh(r)

	if linear {
		num := pageParam(r.URL.Query().Get("page"))
		if num == 0 {
			s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
			return
		}
		pages := pageCount(note.CommentCount, linearPageSize)
		if num > pages {
			s.fail(w, r, http.StatusNotFound, "Такой страницы в обсуждении нет.")
			return
		}
		comments, err := s.st.Flat(ctx, v, id, (num-1)*linearPageSize, linearPageSize)
		if err != nil {
			s.oops(w, r, "комментарии заметки", err)
			return
		}
		p.Comments = comments
		p.Pager = newPager(num, pages, func(n int) string { return noteURL(id, true, n) })
		p.ReplyBase = noteURL(id, true, num)
		p.PageNum = num
		p.FreshOK = p.FreshKnown && num == 1
	} else {
		// Дерево отдаётся ЦЕЛИКОМ. Постранички у него нет и не должно быть:
		// ветка, обрезанная на середине, перестаёт быть веткой, а «дальше»
		// внутри разговора означает «конец разговора на следующей странице».
		comments, err := s.st.Thread(ctx, v, id)
		if err != nil {
			s.oops(w, r, "тред заметки", err)
			return
		}
		p.Comments = comments
		p.Replies = replyCounts(comments)
		p.ReplyBase = noteURL(id, false, 1)
		p.FreshOK = p.FreshKnown
	}
	// Книга обращений строится по ПОКАЗАННЫМ репликам: свидетельство о том, что
	// такое-то слово в этом треде ник, есть ровно в них (address.go). В линейном
	// виде свидетельства меньше — там на странице тридцать реплик, — и это
	// честная цена: не узнав ник, показ дорисует обращение, как раньше.
	p.Book = newAddressBook(note, p.Comments)
	// Адресат берётся из уже загруженного треда, а не отдельным запросом: он там
	// есть по определению, а лишний поход в базу на каждое «Ответить» — это цена
	// без выгоды. Не нашёлся (снесён, на другой странице линейного вида) — форма
	// просто становится ответом в корень.
	p.ReplyTo = findComment(p.Comments, form.ReplyTo)
	s.render(w, r, status, "note.gohtml", p)
}

func findComment(cs []platform.CommentView, id int64) *platform.CommentView {
	if id == 0 {
		return nil
	}
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

// replyCounts считает размер ветки под каждым комментарием. Дерево приходит
// обходом в глубину, поэтому потомки — это идущие следом строки с большей
// глубиной, и одного прохода достаточно.
func replyCounts(cs []platform.CommentView) map[int64]int {
	out := make(map[int64]int, len(cs))
	for i, c := range cs {
		n := 0
		for j := i + 1; j < len(cs) && cs[j].Depth > c.Depth; j++ {
			n++
		}
		if n > 0 {
			out[c.ID] = n
		}
	}
	return out
}

// threadLinear решает, каким показать тред: явный выбор в адресе сильнее куки, и
// он же куку и обновляет — переключатель обязан запоминаться, иначе им
// пользоваться невозможно. По умолчанию дерево: на НГС оно тоже основное.
func (s *Server) threadLinear(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Query().Get("view") {
	case "linear":
		s.setCookie(w, viewCookie, "linear", prefTTL)
		return true
	case "tree":
		s.setCookie(w, viewCookie, "tree", prefTTL)
		return false
	}
	c, err := r.Cookie(s.cookieName(viewCookie))
	return err == nil && c.Value == "linear"
}

func noteURL(id int64, linear bool, page int) string {
	u := "/n/" + strconv.FormatInt(id, 10) + "?view="
	if linear {
		u += "linear"
	} else {
		u += "tree"
	}
	if page > 1 {
		u += "&page=" + strconv.Itoa(page)
	}
	return u
}
