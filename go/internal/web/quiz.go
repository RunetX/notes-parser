package web

// Пятничный вопрос на странице заметки (эпик J, ядро — platform/quiz.go).
//
// Варианты стоят прямо под репликой, которая их задаёт, и работают БЕЗ скрипта:
// каждый вариант — форма с POST, после неё возврат к тому же месту треда. Тот же
// довод, что у реакций: без JS площадка обязана работать целиком, иначе однажды
// и «Выход» окажется такой же кнопкой.
//
// ГОСТЬ ОТВЕЧАЕТ, НО НЕ ГОЛОСУЕТ — главное решение этого файла.
//
// Рубрика затевалась ради тех, кто ещё не вошёл: читать можно всем, а войти
// стоит недели (вход по коду ждёт модерации НГС). Значит нажать вариант обязан и
// гость. Но голос, который считается, требует строки в `users`: без неё «один
// ответ с человека» не удержать ничем, и проценты под вопросом превратились бы в
// число нажатий — то есть в ничто.
//
// Отсюда развилка: ответ вошедшего идёт в базу и в счёт, ответ гостя живёт в
// КУКЕ этого браузера и не идёт никуда. Гость видит разгадку и ссылку на
// настоящий тред — всё, ради чего рубрика и существует, — и не видит процентов.
// Это же самый честный повод войти, какой у площадки был: не «зарегистрируйтесь,
// чтобы читать», а «ваш ответ не попал в общий счёт».

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/platform"
)

// quizCookie — ответы гостя: «id:вариант» через запятую.
//
// Кука, а не сессия: у гостя её нет вовсе, а заводить ради викторины серверное
// состояние для неопознанного человека — это ровно та новая категория данных,
// из-за которой пришлось бы переиздавать согласия (память
// avoid-new-consent-revisions). Здесь же не хранится ничего: значение живёт в
// браузере и дальше него не идёт.
const quizCookie = "quiz"

// quizCookieTTL — сколько браузер помнит ответы. Месяц: вопросы задаются раз в
// неделю, и человек, вернувшийся через две, не должен увидеть их снова
// неотвеченными.
const quizCookieTTL = 30 * 24 * time.Hour

// quizCookieMax — потолок размера куки. Она уходит с КАЖДЫМ запросом к
// площадке, включая картинки, поэтому расти без края ей нельзя: при шести
// вопросах в неделю это около года ответов, а старые вытесняются (см. addGuestAnswer).
const quizCookieMax = 700

// quizBox — что шаблон рисует под репликой-вопросом.
type quizBox struct {
	Quiz platform.Quiz
	// Answered — ответ уже дан: вошедшим (ядро) или гостем (кука). Только после
	// этого в разметку попадают правильный вариант, разгадка и счёт — до того их
	// там нет вовсе, иначе вопрос решается просмотром исходника страницы.
	Answered bool
	// Choice — что выбрано; -1, если не отвечали.
	Choice int
	// Shares — доли вариантов в процентах, в порядке вариантов. Пусто у гостя:
	// его ответ в счёт не идёт, и показывать ему чужие проценты значило бы
	// обещать, что он в них есть.
	Shares []int
	// Total — сколько вошедших ответило.
	Total int
	// PostURL — куда уходит форма.
	PostURL string
	// CSRF — пусто у гостя: токен выводится из сессии, а у него её нет.
	CSRF string
	// Page — номер страницы треда, чтобы вернуться туда же.
	Page int
	// Guest — ответ засчитан только в этом браузере. Строка про вход показывается
	// ровно здесь и ровно тогда, когда она к месту: человек уже играет.
	Guest bool
}

// quizBoxOf собирает вопрос для показа. Возвращает nil, если у этой реплики
// вариантов нет — то есть почти всегда.
func quizBoxOf(p notePage, commentID int64) *quizBox {
	q, ok := p.Quiz[commentID]
	if !ok {
		return nil
	}
	box := &quizBox{
		Quiz:    q,
		Choice:  -1,
		PostURL: "/n/" + strconv.FormatInt(p.Note.ID, 10) + "/quiz",
		CSRF:    p.CSRF,
		Page:    p.Pager.Cur,
	}
	switch {
	case q.Answered():
		box.Answered = true
		box.Choice = q.MyChoice
		box.Total = q.Total
		box.Shares = quizShares(q)
	default:
		if c, ok := p.QuizGuest[commentID]; ok {
			box.Answered = true
			box.Choice = c
			box.Guest = true
		}
	}
	return box
}

// quizShares — доли вариантов в целых процентах.
//
// Считаются ЗДЕСЬ, а не в шаблоне: деление на ноль в шаблоне даёт не ошибку, а
// страницу с «NaN %», и увидит её тот, кто ответил первым.
func quizShares(q platform.Quiz) []int {
	out := make([]int, len(q.Options))
	if q.Total == 0 {
		return out
	}
	for i, o := range q.Options {
		out[i] = int(float64(o.Votes)/float64(q.Total)*100 + 0.5)
	}
	return out
}

// Right — правильный ли это вариант. Метод на коробке, а не поле у варианта,
// потому что шаблон обходит варианты по индексу.
func (b *quizBox) Right(i int) bool { return b.Answered && i == b.Quiz.Right }

// Mine — этот вариант выбрал смотрящий.
func (b *quizBox) Mine(i int) bool { return b.Answered && i == b.Choice }

// Hit — смотрящий угадал.
func (b *quizBox) Hit() bool { return b.Answered && b.Choice == b.Quiz.Right }

// Share — доля варианта. Отдельным методом, чтобы шаблон не лез в срез по
// индексу: у гостя срез пуст, и обращение по индексу уронило бы страницу.
func (b *quizBox) Share(i int) int {
	if i < 0 || i >= len(b.Shares) {
		return 0
	}
	return b.Shares[i]
}

// Bar — ширина полосы в классе: доля, округлённая до пяти процентов.
//
// Округление здесь не косметика, а следствие CSP. Атрибут style запрещён так же,
// как inline-скрипт, поэтому ширина едет классом (как глубина ветки — d1…d12), а
// сто один класс в таблице стилей — это таблица стилей ради одной полосы.
// Точное число при этом не теряется: оно стоит рядом словом.
func (b *quizBox) Bar(i int) int { return (b.Share(i) + 2) / 5 * 5 }

// handleQuizAnswer принимает ответ — и от вошедшего, и от гостя.
//
// CSRF спрашивается ТОЛЬКО у вошедшего, и это не послабление: токен выводится из
// сессии, у гостя её нет, а подделывать у него нечего — своих данных на площадке
// он не имеет вовсе. Происхождение запроса при этом проверяется у обоих
// (postForm), то есть первый рубеж стоит на месте.
func (s *Server) handleQuizAnswer(w http.ResponseWriter, r *http.Request) {
	if !s.postForm(w, r) {
		return
	}
	noteID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || noteID <= 0 {
		s.fail(w, r, http.StatusNotFound, "Такой заметки нет.")
		return
	}
	commentID, _ := strconv.ParseInt(r.FormValue("comment"), 10, 64)
	choice, err := strconv.Atoi(r.FormValue("choice"))
	if err != nil || commentID <= 0 || choice < 0 {
		s.fail(w, r, http.StatusBadRequest, "Такого варианта нет.")
		return
	}

	me, signedIn := s.me(r)
	back := noteURL(noteID, s.threadLinear(w, r), pageParam(r.FormValue("page"))) + anchorOf(commentID)

	// ГОСТЬ: ответ остаётся в его браузере. В базу не идёт ничего — ни строки,
	// ни счётчика, ни следа.
	if !signedIn || s.wr == nil {
		s.setCookie(w, quizCookie, addGuestAnswer(s.guestAnswers(r), commentID, choice), quizCookieTTL)
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if !s.checkCSRF(w, r) {
		return
	}
	err = s.wr.AnswerQuiz(r.Context(), platform.QuizAnswer{
		UserID:    me.ID,
		NoteID:    noteID,
		CommentID: commentID,
		Choice:    choice,
	})
	if err != nil {
		status, problem := writeProblem(err)
		if problem == "" {
			s.oops(w, r, "ответ на вопрос", err)
			return
		}
		// Отказ рисуется страницей заметки: человек нажал кнопку посреди чтения,
		// и уводить его с треда ради одной строки жестоко (как у реакций).
		s.showNote(w, r, noteID, status, compose{Problem: problem})
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// guestQuizOf — ответы гостя для страницы этой заметки.
//
// Куку не разбираем там, где вопросов нет вовсе: страниц треда тысячи, а
// вступительная заметка одна в неделю.
func (s *Server) guestQuizOf(r *http.Request, quiz map[int64]platform.Quiz) map[int64]int {
	if len(quiz) == 0 {
		return nil
	}
	return s.guestAnswers(r)
}

// guestAnswers читает ответы гостя из куки.
//
// Мусор в значении — рабочий случай, а не ошибка: куку правит кто угодно. Всё,
// что не разбирается, просто пропускается; врать это может только самому гостю.
// Имя куки спрашивается у СЕРВЕРА (`cookieName`), а не берётся константой: на
// https он добавляет префикс `__Host-`, и написанное здесь голым именем читало
// бы не то, что пишет setCookie. Поймано на боевой странице 11.09.2026 — кука
// вставала как `__Host-quiz`, а показ её не находил, и ответивший гость видел
// те же три кнопки вместо разгадки. Тесты этого увидеть не могли: тестовый
// сервер живёт на http, где префикса нет вовсе, — поэтому рядом стоит тест на
// https (TestGuestCookieSurvivesTheHostPrefix).
func (s *Server) guestAnswers(r *http.Request) map[int64]int {
	out := map[int64]int{}
	c, err := r.Cookie(s.cookieName(quizCookie))
	if err != nil || c.Value == "" {
		return out
	}
	for _, pair := range strings.Split(c.Value, ",") {
		id, choice, ok := strings.Cut(pair, ":")
		if !ok {
			continue
		}
		cid, err1 := strconv.ParseInt(id, 10, 64)
		ch, err2 := strconv.Atoi(choice)
		if err1 != nil || err2 != nil || cid <= 0 || ch < 0 || ch >= platform.QuizMaxOptions {
			continue
		}
		out[cid] = ch
	}
	return out
}

// addGuestAnswer дописывает ответ в значение куки.
//
// Уже отвеченный вопрос НЕ переписывается — то же правило, что в ядре: первый
// ответ окончателен, иначе после разгадки все оказались бы правы. Старое
// вытесняется с головы, когда значение упирается в потолок: куку возит с собой
// каждый запрос, и расти ей нельзя.
func addGuestAnswer(have map[int64]int, commentID int64, choice int) string {
	if _, ok := have[commentID]; !ok {
		have[commentID] = choice
	}
	var b strings.Builder
	for id, ch := range have {
		pair := strconv.FormatInt(id, 10) + ":" + strconv.Itoa(ch)
		if b.Len()+len(pair)+1 > quizCookieMax && id != commentID {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pair)
	}
	return b.String()
}
