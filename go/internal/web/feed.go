package web

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"lovegw/internal/platform"
)

// feedPageSize — 20 заметок на страницу, как на НГС (`/notes/page~2/limit~20/`).
const feedPageSize = 20

// countTTL — сколько живёт посчитанное число заметок.
//
// Счётчик нужен постраничке (сколько всего страниц), и считается он по всей
// ленте — после раскатки архива это 117 тысяч строк на КАЖДЫЙ заход в ленту,
// самый частый запрос площадки и самая дешёвая мишень. Полминуты устаревания
// стоят ровно одного: последняя страница на полминуты может не знать про
// свежую заметку. Сама заметка при этом видна сразу — она приходит первой
// строкой ленты, а не изменением её длины.
const countTTL = 30 * time.Second

// feedCount — тот самый счётчик. Живёт в памяти процесса: пережить рестарт ему
// незачем, а делить его между мордой и демоном нечем и не нужно.
//
// Слотов ДВА, потому что и лент две: у читателя в ней только видимое, у
// модератора ещё и скрытое (platform.feedModQuery). Один счётчик на обоих врал
// бы номерами страниц тому, у кого строк больше, — и последние заметки ленты
// уезжали бы за край последней страницы. Слот выбирается тем же вопросом, что и
// запрос: «видит ли этот человек скрытое».
type feedCount struct {
	mu sync.Mutex
	n  [2]int
	at [2]time.Time
}

// countNotes — сколько заметок в ленте этого человека, не чаще раза в countTTL.
func (s *Server) countNotes(ctx context.Context, v platform.Viewer) (int, error) {
	i := 0
	if v.CanModerate() {
		i = 1
	}
	s.notes.mu.Lock()
	defer s.notes.mu.Unlock()
	if now := time.Now(); now.Sub(s.notes.at[i]) < countTTL {
		return s.notes.n[i], nil
	}
	n, err := s.st.CountNotes(ctx, v)
	if err != nil {
		return 0, err
	}
	s.notes.n[i], s.notes.at[i] = n, time.Now()
	return n, nil
}

type feedPage struct {
	page
	// Notes — первой страницей идут закреплённые, дальше хронология. Один
	// список, а не два: для читателя это одна лента, а закреплённое он узнаёт
	// по метке на самой заметке, а не по тому, что оно стоит в другом блоке.
	Notes []platform.NoteView
	Pager pager
	// CanWrite — показывать ли «Написать заметку». Гостю не показываем: читать
	// можно всем, писать — только вошедшим, и кнопка, ведущая к отказу, хуже её
	// отсутствия.
	CanWrite bool
	// CanModerate — под каждой заметкой ленты видны кнопки: скрыть и вернуть,
	// закрепить и открепить, замок треда, ссылка на автора. Модератор работает
	// ТАМ, ГДЕ ЧИТАЕТ, а читают ленту: до 26.08.2026 за каждым решением
	// приходилось открывать страницу заметки, а скрытая заметка пропадала у него
	// вместе со всеми — вернуть её можно было только из очереди.
	CanModerate bool
	// CanEdit — смотрящий администратор, и под НАТИВНЫМИ заметками ленты стоит
	// «Поправить». Отдельно от CanModerate: правка чужого текста выше
	// модераторской двери (см. platform.EditNoteAsAdmin).
	CanEdit bool
	// FreshOK и FreshAfter — живой добор ленты (fresh.go). Граница непрозрачна
	// для страницы: она её только печатает, а разбирает и двигает сервер.
	FreshOK    bool
	FreshAfter string
	// Shots — иллюстрация заметки, по её номеру. Картой, а не полем у NoteView:
	// картинки живут в своей таблице и спрашиваются ОДНИМ запросом на всю
	// страницу, а тащить их в общий запрос ленты значило бы утяжелять самый
	// частый запрос площадки ради двух процентов заметок, у которых они есть.
	//
	// Показываются они и у зеркальных, и у своих: разделять тут — значит
	// завести в одном списке ровно ту разницу между чужим и своим, которой у
	// картинки нет вовсе.
	Shots map[int64]platform.Media
	// NGSSent — какие из показанных заметок уже унесены на НГС (третье состояние
	// метки происхождения). Отдельным запросом и картой по тем же двум причинам,
	// что Shots: лишняя колонка в самом частом запросе площадки и то, что живёт
	// это в своей таблице.
	NGSSent map[int64]bool
	// Origins — ОРИГИНАЛ для каждого показанного двойника, по его номеру.
	// Картой и отдельным запросом по тем же двум причинам, что и Shots: тащить
	// соединение в общий запрос ленты значило бы платить им за все 117 тысяч
	// заметок ради нескольких, а без цитаты карточка двойника — служебная
	// строка без предмета, ровно то, на что владелец и пожаловался.
	Origins map[int64]platform.SynthOrigin
	// SentToNGS — человек только что опубликовал заметку, и она ушла НА САЙТ, а
	// не сюда: своей строки у неё нет вовсе, приедет она зеркалом через минуту с
	// небольшим. Без этой строки полторы минуты выглядят как пропажа текста —
	// нажал «Опубликовать», а в ленте пусто.
	//
	// Признак из адреса, а не из базы: он про ОДИН показ, сразу после нажатия, и
	// заводить ради него состояние в сессии было бы дороже, чем он стоит.
	SentToNGS bool
}

// origins — оригиналы для двойников этой страницы. Отказ ленту не роняет, как и
// у иллюстраций: без цитаты двойник остаётся собой, а без ленты цитата не нужна.
func (s *Server) origins(ctx context.Context, notes []platform.NoteView) map[int64]platform.SynthOrigin {
	var ids []int64
	for _, n := range notes {
		if n.SynthOf != 0 {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	got, err := s.st.SynthOrigins(ctx, ids)
	if err != nil {
		s.log.Error("оригиналы смежных обсуждений", "err", err)
		return nil
	}
	return got
}

// sentToNGS — какие из показанных заметок уехали на НГС. Отказ ленту не роняет,
// как и у иллюстраций: без метки заметка остаётся собой.
func (s *Server) sentToNGS(ctx context.Context, notes []platform.NoteView) map[int64]bool {
	if len(notes) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.ID)
	}
	sent, err := s.st.NGSSentObjects(ctx, platform.NGSNote, ids)
	if err != nil {
		s.log.Error("унесённое на НГС", "err", err)
		return nil
	}
	return sent
}

// thumbs — иллюстрации показанных заметок. Отказ чтения не роняет ленту: без
// картинок она остаётся лентой, а без ленты картинки не нужны.
func (s *Server) thumbs(ctx context.Context, notes []platform.NoteView) map[int64]platform.Media {
	if len(notes) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.ID)
	}
	shots, err := s.st.NoteThumbs(ctx, ids)
	if err != nil {
		s.log.Error("иллюстрации ленты", "err", err)
		return nil
	}
	return shots
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	num := pageParam(r.URL.Query().Get("page"))
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx, v := r.Context(), s.viewer(r)

	total, err := s.countNotes(ctx, v)
	if err != nil {
		s.oops(w, r, "счётчик ленты", err)
		return
	}
	pages := pageCount(total, feedPageSize)
	if num > pages {
		s.fail(w, r, http.StatusNotFound, "Такой страницы в ленте нет.")
		return
	}
	notes, err := s.st.Feed(ctx, v, (num-1)*feedPageSize, feedPageSize)
	if err != nil {
		s.oops(w, r, "лента", err)
		return
	}
	// Граница живого добора берётся ДО того, как сверху лягут закреплённые:
	// они стоят вне хронологии, и «самая свежая» среди них — не граница, а
	// случайное старое время, с которого добор принёс бы половину ленты.
	fresh := feedCursor(time.Now(), 0)
	if len(notes) > 0 {
		fresh = feedCursor(notes[0].PublishedAt, notes[0].ID)
	}
	// Закреплённое — только на первой странице. На остальных оно было бы
	// шапкой, которая едет за читателем: он листает ленту как раз затем, чтобы
	// уйти от начала. Лишнего запроса на страницах 2…5933 при этом нет вовсе.
	//
	// С мордолентой вышло иначе (просьба владельца 30.08.2026): она стоит на
	// ВСЕХ страницах чтения, включая вторую страницу ленты, — на НГС это часть
	// раздела, а не украшение его начала. Платы за это нет: полоса читается из
	// кэша на полминуты (faces.go).
	if num == 1 {
		pinned, err := s.st.PinnedNotes(ctx, v)
		if err != nil {
			s.oops(w, r, "закреплённые", err)
			return
		}
		notes = append(pinned, notes...)
	}
	me, signedIn := s.me(r)
	s.render(w, r, http.StatusOK, "feed.gohtml", feedPage{
		page:     s.readingPage(r, ""),
		Notes:    notes,
		Pager:    newPager(num, pages, feedURL),
		CanWrite: signedIn && me.Kind == platform.KindMember && s.wr != nil,
		// Только вошедшему и только на первой странице: подделать адрес может
		// кто угодно, а строка эта — ответ на СВОЁ нажатие, и постороннему она
		// сказала бы о чужой заметке.
		SentToNGS: signedIn && num == 1 && r.URL.Query().Get("ngs") == "1",
		// Скрытое ядро отдало по роли смотрящего (platform.Feed), а кнопки под
		// ним показываются, только если модерация к морде вообще подключена:
		// нарисовать «Вернуть» там, где звать некого, — это кнопка, отвечающая
		// отказом.
		CanModerate: v.CanModerate() && s.mod != nil,
		CanEdit:     me.Role >= platform.RoleAdmin && s.mod != nil,
		// Дописывается только первая страница: остальные — срез истории, и
		// новая заметка сверху сдвинула бы человеку то, что он читает.
		FreshOK:    num == 1,
		FreshAfter: fresh,
		Shots:      s.thumbs(ctx, notes),
		NGSSent:    s.sentToNGS(ctx, notes),
		Origins:    s.origins(ctx, notes),
	})
}

func feedURL(n int) string {
	if n <= 1 {
		return "/"
	}
	return "/?page=" + strconv.Itoa(n)
}
