package web

// Страница «События» и колокольчик — то, ради чего заведена шина (эпик F).
//
// Что здесь есть и чего нет. Есть список поводов со ссылкой на место в треде и
// отметка прочитанного. Нет ни настроек («какие события мне показывать»), ни
// фильтров, ни отдельных вкладок: поводов у человека единицы в сутки, и всё,
// что можно сделать с таким списком, — это прочитать его сверху вниз. Настройки
// появятся тогда, когда появятся подписки, — то есть когда список действительно
// станет длинным.
//
// Колокольчик в шапке — единственное место площадки, где мы сознательно
// расходимся с главной меркой эпика E («человек не заметил переезда»): на
// love.ngs.ru такого значка не было, переносить нечего. Отсюда правило Ш5з о
// собственных метках: своя метка обязана объясняться — заголовком при наведении
// и словами в /help.
//
// Всё работает без JS: список — обычная страница с нумерованной постраничкой,
// отметка прочитанного — форма с POST, как выход и переключение темы. Живое
// обновление счётчика (live.go) поверх этого — удобство, а не условие работы.

import (
	"context"
	"net/http"
	"strconv"

	"lovegw/internal/platform"
)

// Events — что морде нужно от шины. Шестой узкий интерфейс рядом со Store,
// Auth, Writer, Moderator и Site, и это не педантизм: каждый такой список сам по
// себе есть исчерпывающий ответ на вопрос «что площадка позволяет делать с
// данными». Здесь позволено ровно два действия, и оба — над СВОИМИ поводами.
//
// nil ⇒ ни страницы событий, ни колокольчика, ни живого канала.
type Events interface {
	Notifications(ctx context.Context, userID int64, offset, limit int) ([]platform.NotificationView, error)
	CountNotifications(ctx context.Context, userID int64) (int, error)
	Unread(ctx context.Context, userID int64) (int, error)
	MarkRead(ctx context.Context, userID, upto int64) error
}

// SetEvents подключает шину. Отдельным вызовом, а не седьмым аргументом
// конструктора: способность необязательная, а конструктор и без того на
// пределе. Маршруты при этом заведены всегда и отвечают «нет такой страницы»,
// когда шины нет, — как делает /mod без модерации.
// Живой канал подключается тут же и только если хранилище его умеет
// (type-assertion, как SiteMessenger от Site): поток — способность
// необязательная, и её отсутствие означает ровно «страница не дописывается
// сама», а не поломку.
func (s *Server) SetEvents(ev Events) {
	s.events = ev
	if live, ok := ev.(Live); ok {
		s.hub = newHub(live, s.log)
	}
}

// eventsPageSize — сколько поводов на страницу. Двадцать, как в ленте: список
// того же рода, и разнобой в числах читатель замечает раньше, чем объясняет.
const eventsPageSize = 20

type eventsPage struct {
	page
	Lines []eventLine
	Pager pager
	// Top — верхняя граница отметки «прочитано»: id самого свежего повода на
	// ЭТОЙ странице. Отмечаем то, что человек видел, а не всё подряд, — иначе
	// кнопка на пятой странице гасила бы непрочитанное, до которого он не дошёл.
	Top int64
	// Markable — есть ли что отмечать. Кнопка, которой нечего делать, хуже её
	// отсутствия.
	//
	// Имя не «Unread» намеренно: у общей части страницы (page) уже есть поле
	// Unread — число непрочитанного для колокольчика, — и одноимённое поле здесь
	// перекрывало его в шаблоне, отчего на самой странице событий колокольчик
	// показывал не число, а «true» (жалоба владельца 23.08.2026). Шаблон о таком
	// столкновении не предупреждает вовсе: он молча берёт ближнее поле.
	Markable bool
}

// eventLine — повод, готовый к показу: текст заголовка и адрес собраны в Go.
//
// В Go, а не в шаблоне, по той же причине, по которой там собирается modAct:
// правило «как назвать этот повод» одно, и разъехавшись по веткам шаблона, оно
// дало бы разные слова об одном и том же в списке и в живом уведомлении.
type eventLine struct {
	platform.NotificationView
	Title string
	URL   string
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	u, ok := s.eventReader(w, r)
	if !ok {
		return
	}
	num := pageParam(r.URL.Query().Get("page"))
	if num == 0 {
		s.fail(w, r, http.StatusBadRequest, "Неверный номер страницы.")
		return
	}
	ctx := r.Context()
	total, err := s.events.CountNotifications(ctx, u.ID)
	if err != nil {
		s.oops(w, r, "счётчик событий", err)
		return
	}
	pages := pageCount(total, eventsPageSize)
	if num > pages {
		s.fail(w, r, http.StatusNotFound, "Такой страницы событий нет.")
		return
	}
	list, err := s.events.Notifications(ctx, u.ID, (num-1)*eventsPageSize, eventsPageSize)
	if err != nil {
		s.oops(w, r, "события", err)
		return
	}
	p := eventsPage{
		page:  s.newPage(r, "События"),
		Lines: make([]eventLine, 0, len(list)),
		Pager: newPager(num, pages, eventsURL),
	}
	for _, n := range list {
		p.Lines = append(p.Lines, eventLine{
			NotificationView: n,
			Title:            eventTitle(n),
			URL:              eventURL(n),
		})
		if n.EventID > p.Top {
			p.Top = n.EventID
		}
		p.Markable = p.Markable || !n.Read
	}
	s.render(w, r, http.StatusOK, "events.gohtml", p)
}

// handleEventsRead отмечает прочитанным всё до названной границы.
func (s *Server) handleEventsRead(w http.ResponseWriter, r *http.Request) {
	if !s.postWrite(w, r) {
		return
	}
	u, ok := s.eventReader(w, r)
	if !ok {
		return
	}
	// Разбор границы намеренно нестрогий: мусор в поле означает «до самого
	// верха», а не отказ. Кнопка «прочитано» — не то место, где человеку
	// показывают ошибку формы.
	upto, _ := strconv.ParseInt(r.FormValue("upto"), 10, 64)
	if err := s.events.MarkRead(r.Context(), u.ID, max(0, upto)); err != nil {
		s.oops(w, r, "отметка прочитанного", err)
		return
	}
	http.Redirect(w, r, localPath(r.FormValue("back")), http.StatusSeeOther)
}

// eventReader — вошедший, которому есть что показать. Пустой второй результат
// означает «ответ уже отправлен».
//
// Гостю и при выключенной шине — «нет такой страницы», как у /mod: обещать
// страницу, которой у этого человека не будет, незачем.
func (s *Server) eventReader(w http.ResponseWriter, r *http.Request) (platform.User, bool) {
	u, ok := s.me(r)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			s.fail(w, r, http.StatusUnauthorized, "Чтобы смотреть события, нужно войти.")
		}
		return platform.User{}, false
	}
	if s.events == nil {
		s.fail(w, r, http.StatusNotFound, "Такой страницы нет.")
		return platform.User{}, false
	}
	return u, true
}

// eventTitle — как назвать повод человеку.
//
// Заголовки короткие и в них НЕТ чужого текста: имя действующего лица шаблон
// подставляет отдельно (и берёт его из ТЕКУЩЕГО ника самосоединением, как
// подписи в треде), а выдержка идёт отдельной строкой и гасится, если публикацию
// скрыли. У реакций имени нет вовсе — см. правило Ш5г.
func eventTitle(n platform.NotificationView) string {
	switch n.Reason {
	case platform.ReasonReplyToComment:
		return "Ответ на вашу реплику"
	case platform.ReasonReplyToNote:
		return "Ответ на вашу заметку"
	case platform.ReasonMention:
		return "Вас упомянули"
	case platform.ReasonReaction:
		return "Вашу запись отметили"
	}
	switch n.Kind {
	case platform.EventHidden:
		return "Ваша публикация скрыта"
	case platform.EventRestored:
		return "Ваша публикация возвращена"
	case platform.EventBanned:
		return "Вам запрещено публиковать"
	case platform.EventUnbanned:
		return "Запрет на публикации снят"
	}
	return "Событие"
}

// eventURL — куда ведёт повод. У запрета публикаций места в треде нет вовсе, и
// ведёт он на свою страницу: там написано, за что и до какого числа.
func eventURL(n platform.NotificationView) string {
	switch {
	case n.NoteID == 0:
		return "/me"
	case n.CommentID != 0 && !n.Hidden:
		// На скрытую реплику якорь не ставим: страница до неё не долистает,
		// потому что её там больше нет.
		return "/n/" + strconv.FormatInt(n.NoteID, 10) + "#c" + strconv.FormatInt(n.CommentID, 10)
	default:
		return "/n/" + strconv.FormatInt(n.NoteID, 10)
	}
}

func eventsURL(n int) string {
	if n <= 1 {
		return "/events"
	}
	return "/events?page=" + strconv.Itoa(n)
}
