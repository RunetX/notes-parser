package web

// Живой добор: страница дописывает себя сама, без перезагрузки и без нажатий.
//
// Устроено это в ДВА шага, и разделение принципиальное.
//
//	сигнал  — /live говорит «в этом треде новое» и ничего больше (см. live.go);
//	добор   — страница приходит СЮДА за готовой строкой.
//
// Почему не отдать разметку прямо в поток: хаб один на процесс и никого не
// знает, а строка у каждого своя — модератор видит скрытое и кнопки под ним,
// автор видит свою реакцию, гость не видит «Ответить». Рисовать её в хабе
// значило бы рисовать столько раз, сколько открытых окон, и с чужими правами.
// Здесь же строку рисует обычный запрос обычного человека, со своей сессией.
//
// А почему не собирать её скриптом из JSON — это и есть главное: разметку
// площадки собирает ОДИН шаблон (parts/comment.gohtml, parts/note_item.gohtml),
// и он же рисует страницу. Второй способ превратить текст в HTML — это вторая
// поверхность для XSS и второе место, где однажды забудут про эпоху разметки,
// смайлы, обращение из ребра и маскирование анонима.
//
// Курсор для клиента НЕПРОЗРАЧЕН: он получает строку в data-fresh, возвращает
// её в ?after= и заменяет тем, что придёт в X-Fresh-After. Что внутри — дело
// сервера: у треда это id реплики, у ленты пара «время, id». Так формат можно
// менять, не трогая скрипт.

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

// freshLimit — сколько строк отдаём за один добор. Больше пришло — хвост
// приедет следующим запросом: курсор сдвинулся, а сигнал придёт снова.
const freshLimit = platform.FreshLimit

// handleFresh — новые реплики треда. Отвечает КУСКОМ разметки, а не страницей:
// заголовки и «база» здесь были бы мусором, который клиент выбросит.
func (s *Server) handleFresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	after, ok := parseThreadCursor(r.URL.Query().Get("after"))
	if !ok {
		s.fail(w, r, http.StatusBadRequest, "Неверная граница добора.")
		return
	}

	ctx, v := r.Context(), s.viewer(r)
	note, err := s.st.NoteViewByID(ctx, v, id)
	if err != nil {
		// Заметки нет, она скрыта или база отказала — во всех случаях
		// дописывать нечего. Пустой ответ, а не 500: живой добор — удобство
		// поверх страницы, и своей поломкой он не имеет права её ронять.
		s.freshEmpty(w, r, "чтение заметки для добора", err)
		return
	}
	canMod := v.CanModerate() && s.mod != nil
	if note.Status != platform.StatusVisible && !(canMod && note.Status == platform.StatusHiddenMod) {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}

	comments, err := s.st.CommentsSince(ctx, v, id, after, freshLimit)
	if err != nil {
		s.freshEmpty(w, r, "новые реплики", err)
		return
	}

	// Вторая половина добора — ПЕРЕЕЗДЫ: строки, которые на странице уже стоят,
	// но с тех пор сменили место. Дерево под открытой страницей перестраивается
	// постоянно (зеркало ставит ребро по обращению «Ник, …», обход мобильной
	// версии заменяет его настоящим), а по границе id такая строка не приезжает
	// никогда — id у неё прежний. Без этого читатель видит ветку, выросшую не
	// там, и правильную — только после обновления.
	//
	// Отказ здесь новых реплик не отменяет: место строки хуже, чем её
	// отсутствие, и терять из-за него живой добор целиком незачем.
	moved, movedAfter, err := s.st.CommentsMoved(ctx, v, id, after.Moved, freshLimit)
	if err != nil {
		s.log.Warn("живой добор", "что", "переезды", "заметка", id, "err", err)
		moved, movedAfter = nil, after.Moved
	}
	after.Moved = movedAfter
	moved = withoutSeen(moved, comments)

	me, signedIn := s.me(r)
	// Контекст страницы собирается ЗАНОВО и такой же, как у настоящей: иначе
	// дописанная реплика отличалась бы от нарисованной обновлением — то есть
	// ровно тем, чего живой добор и должен избежать.
	//
	// Общей части (шапка, тема, колокольчик) здесь нет намеренно: во фрагменте
	// её некуда девать, а запрос непрочитанного она стоит на каждую реплику.
	p := notePage{
		Note:        note,
		Linear:      strings.EqualFold(r.URL.Query().Get("view"), "linear"),
		CanWrite:    canWriteIn(note, me, signedIn, s.wr != nil),
		CanModerate: canMod,
		// Реакций у только что появившейся реплики нет ни у кого, и спрашивать
		// их отдельным запросом незачем: первое же нажатие перерисует коробку
		// обычным переходом.
		Reactions: map[int64][]platform.Reaction{},
		ReactOpen: -1,
		// Ответов у неё тоже нет: «Ответы N» появится при следующем обновлении.
		Replies: map[int64]int{},
		PageNum: 1,
	}
	// SignedIn нужен подписи автора: у вошедшего имя — ссылка на его страницу.
	// Без этой строки дописанная реплика отличалась бы от нарисованной
	// обновлением, то есть ровно тем, чего живой добор и должен избежать.
	p.SignedIn = signedIn
	if signedIn {
		p.CSRF = csrfToken(s.session(r))
	}
	p.ReplyBase = noteURL(id, p.Linear, 1)
	// Книга обращений строится по ДОБРАННЫМ репликам, а не по всему треду, и это
	// честная цена за то, чтобы не читать дерево целиком на каждую реплику.
	// Свидетельства в трёх строках меньше, чем в девятистах, — но касается это
	// только текстов, где обращение стоит В ТЕЛЕ, то есть НГС до 2014 года и
	// переименовавшихся. Свежая реплика ни тем, ни другим не бывает: у своей
	// обращение это ребро, у зеркальной его срезал приёмник.
	// Новое и переехавшее рисуются одним проходом и одним шаблоном: разница
	// между ними не в разметке, а в том, что страница с ними сделает.
	rows := append(append(make([]platform.CommentView, 0, len(comments)+len(moved)),
		comments...), moved...)
	p.Book = newAddressBook(note, rows)

	// УНЕСЁННОЕ НА НГС — по добранным строкам. Спрашивается здесь, а не только
	// на полной странице, потому что смена этой метки и есть один из поводов
	// добора: реплика уезжает на сайт через секунды после публикации, и
	// перерисовать её надо тем же путём, что переезд ветки. Без этой строки
	// добранная реплика приходила бы со значком «написано здесь» и спорила бы с
	// той же репликой после обновления страницы.
	if len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, c := range rows {
			ids = append(ids, c.ID)
		}
		if sent, err := s.st.NGSSentObjects(ctx, platform.NGSComment, ids); err != nil {
			// Отказ гасит метку, а не добор: реплика важнее значка.
			s.log.Warn("унесённое на НГС", "заметка", id, "err", err)
		} else {
			p.NGSSent = sent
		}
	}

	var buf bytes.Buffer
	for _, c := range rows {
		if err := s.renderPart(&buf, "comment", commentItem(p, c)); err != nil {
			http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
			return
		}
	}
	// Граница двигается ПО КАЖДОЙ полосе отдельно, а не «последней строкой
	// порции»: порция склеена из двух диапазонов, и её хвост — максимум одной
	// полосы, а не обеих.
	for _, c := range comments {
		after.Seen(c.ID)
	}
	// Какие из присланных строк — переезды, а не новое. Заголовком, а не меткой
	// в разметке: рисует строку ТОТ ЖЕ шаблон, что и страницу, и заводить в нём
	// поле ради одного вызывающего значило бы завести второй вид реплики.
	// Страница по этому списку снимает строку со старого места; всё остальное,
	// что у неё уже стоит, она по-прежнему пропускает — этим гасится эхо
	// собственной реплики.
	if len(moved) > 0 {
		w.Header().Set("X-Fresh-Moved", idList(moved))
	}
	s.sendFresh(w, &buf, threadCursor(after), note.CommentCount)
}

// withoutSeen выбрасывает из переездов то, что уже едет новым. Строка, которая
// и появилась впервые, и успела переехать, годится в обоих качествах — берём её
// как новую: на странице у неё ещё нет места, которое надо исправлять.
func withoutSeen(moved, fresh []platform.CommentView) []platform.CommentView {
	if len(moved) == 0 || len(fresh) == 0 {
		return moved
	}
	seen := make(map[int64]bool, len(fresh))
	for _, c := range fresh {
		seen[c.ID] = true
	}
	kept := moved[:0]
	for _, c := range moved {
		if !seen[c.ID] {
			kept = append(kept, c)
		}
	}
	return kept
}

// idList — номера строк через запятую, как их ждёт заголовок X-Fresh-Moved.
func idList(cs []platform.CommentView) string {
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(c.ID, 10))
	}
	return b.String()
}

// handleFreshFeed — новые заметки ленты. Только первая страница: остальные суть
// исторический срез, и дописывать их сверху значило бы сдвигать человеку то,
// что он читает.
func (s *Server) handleFreshFeed(w http.ResponseWriter, r *http.Request) {
	at, id, ok := parseFeedCursor(r.URL.Query().Get("after"))
	if !ok {
		s.fail(w, r, http.StatusBadRequest, "Неверная граница добора.")
		return
	}
	ctx, v := r.Context(), s.viewer(r)
	notes, err := s.st.NotesSince(ctx, v, at, id, freshLimit)
	if err != nil {
		s.freshEmpty(w, r, "новые заметки", err)
		return
	}
	me, signedIn := s.me(r)
	// Контекст страницы собирается заново и такой же, как у настоящей ленты:
	// дописанная заметка обязана прийти с теми же кнопками, что нарисовало бы
	// обновление, — иначе у модератора первая же дописанная строка окажется без
	// полоски действий, и он решит, что кнопки пропали.
	p := feedPage{
		CanWrite:    signedIn && me.Kind == platform.KindMember && s.wr != nil,
		CanModerate: v.CanModerate() && s.mod != nil,
		CanEdit:     me.Role >= platform.RoleAdmin && s.mod != nil,
		Shots:       s.thumbs(ctx, notes),
		Origins:     s.origins(ctx, notes),
	}
	p.SignedIn = signedIn // подпись автора: у вошедшего имя — ссылка
	if signedIn {
		p.CSRF = csrfToken(s.session(r))
		p.Back = "/"
	}

	// Запрос отдаёт от НОВЫХ к старым — тем же порядком, что и лента. Клиент
	// вставляет строки сверху, поэтому идти по ним надо С КОНЦА: иначе три
	// пришедшие разом заметки встанут в ленту задом наперёд.
	var buf bytes.Buffer
	for i := len(notes) - 1; i >= 0; i-- {
		if err := s.renderPart(&buf, "note_item", noteItem(p, notes[i])); err != nil {
			http.Error(w, "внутренняя ошибка", http.StatusInternalServerError)
			return
		}
	}
	cursor := feedCursor(at, id)
	if len(notes) > 0 {
		cursor = feedCursor(notes[0].PublishedAt, notes[0].ID)
	}
	s.sendFresh(w, &buf, cursor, -1)
}

// sendFresh отдаёт кусок разметки. Не кэшируется и не индексируется по тем же
// причинам, что и страницы: он и есть их часть.
func (s *Server) sendFresh(w http.ResponseWriter, buf *bytes.Buffer, cursor string, count int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Vary", "Cookie")
	h.Set("X-Fresh-After", cursor)
	// Число комментариев считает СЕРВЕР, а не скрипт по числу вставленных строк:
	// у заголовка над тредом должно стоять то же, что покажет обновление, а
	// добор мог принести не всё (потолок порции) или ничего (реплику успели
	// скрыть).
	if count >= 0 {
		h.Set("X-Fresh-Count", strconv.Itoa(count))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// freshEmpty — отказ, который не должен выглядеть отказом. Пустой кусок разметки
// и прежний курсор: страница останется какой была, а человек ничего не заметит.
func (s *Server) freshEmpty(w http.ResponseWriter, r *http.Request, what string, err error) {
	if err != nil {
		s.log.Warn("живой добор", "что", what, "err", err)
	}
	s.sendFresh(w, &bytes.Buffer{}, r.URL.Query().Get("after"), -1)
}

// feedCursor и parseFeedCursor — граница ленты. Пара, а не одно число: лента
// упорядочена по времени публикации, а id при равном времени лишь доразрешает
// порядок; по одному id граница разъехалась бы на полосах идентификаторов —
// зеркальная заметка НГС имеет номер МЕНЬШЕ любой нативной, будучи новее.
func feedCursor(at time.Time, id int64) string {
	return strconv.FormatInt(at.UnixNano(), 10) + "," + strconv.FormatInt(id, 10)
}

func parseFeedCursor(s string) (time.Time, int64, bool) {
	ns, ids, ok := strings.Cut(s, ",")
	if !ok {
		return time.Time{}, 0, false
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	id, err := strconv.ParseInt(ids, 10, 64)
	if err != nil || id < 0 {
		return time.Time{}, 0, false
	}
	return time.Unix(0, n), id, true
}

// threadCursor и parseThreadCursor — граница треда на проводе. Пара, как и у
// ленты, и по той же причине: одним числом полосы идентификаторов не
// упорядочиваются (см. platform.FreshAfter). Клиент её не разбирает — получает
// в data-fresh, возвращает в ?after=, — поэтому формат наш и менять его можно,
// не трогая скрипт.
// Составляющих четыре: две полосы («что появилось») и пара переездов («что
// переехало», platform.MovedAfter). Пары разной природы, и одной их не заменить:
// у появления ключ — номер строки, у переезда номер прежний, и заметен он только
// по отметке времени.
func threadCursor(a platform.FreshAfter) string {
	s := strconv.FormatInt(a.NGS, 10) + "," + strconv.FormatInt(a.Native, 10)
	if a.Moved.On() {
		s += "," + strconv.FormatInt(a.Moved.At.UnixNano(), 10) +
			"," + strconv.FormatInt(a.Moved.ID, 10)
	}
	return s
}

func parseThreadCursor(s string) (platform.FreshAfter, bool) {
	ngs, rest, ok := strings.Cut(s, ",")
	if !ok {
		// Граница ПРЕЖНЕГО вида — одно число. Её возвращают страницы, открытые
		// до выкатки, и отвечать им отказом незачем: кладём число в его
		// собственную полосу, а соседнюю оставляем закрытой. Что на такой
		// странице уже стоит из другой полосы, число не говорит, а принести её с
		// нуля значило бы выложить в открытый тред полсотни старых реплик.
		// Обновление страницы даёт нормальную границу.
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 0 {
			return platform.FreshAfter{}, false
		}
		if platform.IsNGS(id) {
			// Максимум оказался ngs'ным — значит своих реплик страница не
			// показывала вовсе, и нативная полоса честно начинается с нуля.
			return platform.FreshAfter{NGS: id}, true
		}
		return platform.FreshAfter{NGS: platform.NativeIDBase - 1, Native: id}, true
	}
	n, err := strconv.ParseInt(ngs, 10, 64)
	if err != nil || n < 0 {
		return platform.FreshAfter{}, false
	}
	native, mv, hasMoved := strings.Cut(rest, ",")
	m, err := strconv.ParseInt(native, 10, 64)
	if err != nil || m < 0 {
		return platform.FreshAfter{}, false
	}
	a := platform.FreshAfter{NGS: n, Native: m}
	if !hasMoved {
		// Граница БЕЗ переездов — со страницы, открытой до выкатки этой правки.
		// Такой странице их не носят: нулевая граница означала бы «неси всё, что
		// когда-либо переезжало», то есть переставить читателю пол-треда разом.
		// Обновление даёт нормальную границу.
		return a, true
	}
	ns, id, ok := strings.Cut(mv, ",")
	if !ok {
		return platform.FreshAfter{}, false
	}
	at, err := strconv.ParseInt(ns, 10, 64)
	if err != nil || at < 0 {
		return platform.FreshAfter{}, false
	}
	mid, err := strconv.ParseInt(id, 10, 64)
	if err != nil || mid < 0 {
		return platform.FreshAfter{}, false
	}
	a.Moved = platform.MovedAfter{At: time.Unix(0, at), ID: mid}
	return a, true
}
