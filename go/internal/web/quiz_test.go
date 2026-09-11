package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// Вопрос, каким его отдаёт ядро НЕ ответившему: варианты есть, правильный и
// разгадка тоже (ядро их знает), а счёта нет — MyChoice отрицательный.
func quizStore(mine int) *fakeStore {
	q := platform.Quiz{
		CommentID: 1,
		Options: []platform.QuizOption{
			{Text: "2011"}, {Text: "2016"}, {Text: "2021"},
		},
		Right:      1,
		Reveal:     "9 сентября 2016 года, 261 реплика.",
		SourceNote: 270011,
		MyChoice:   mine,
	}
	if mine >= 0 {
		q.Options[0].Votes, q.Options[1].Votes, q.Options[2].Votes = 1, 2, 1
		q.Total = 4
	}
	return &fakeStore{
		note:   sampleNote(),
		thread: sampleThread(),
		quiz:   map[int64]platform.Quiz{1: q},
	}
}

// ГЛАВНОЕ свойство рубрики: до ответа в разметке нет ни разгадки, ни отметки
// правильного варианта. Спрятать их показом мало — страницу читают исходником,
// и рубрика кончилась бы в первый же вечер.
func TestQuizHidesAnswerUntilAnswered(t *testing.T) {
	h := newTestServer(t, quizStore(-1), Config{})
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()

	if !strings.Contains(body, `class="quiz"`) {
		t.Fatal("вопрос не показан вовсе")
	}
	if !strings.Contains(body, ">2016</button>") {
		t.Error("вариантов нет")
	}
	if strings.Contains(body, "261 реплика") {
		t.Error("разгадка уехала в разметку до ответа")
	}
	if strings.Contains(body, "qright") {
		t.Error("правильный вариант отмечен до ответа")
	}
	if strings.Contains(body, "Верно") || strings.Contains(body, "Мимо") {
		t.Error("вердикт показан до ответа")
	}
}

// Гостю варианты показываются КНОПКАМИ — в отличие от реакций, где кнопка
// отвечала бы ему «сначала войдите». Здесь она работает: ответ засчитывается в
// его браузере, и ради этого рубрика и затевалась.
func TestQuizIsClickableForGuest(t *testing.T) {
	h := newTestServer(t, quizStore(-1), Config{})
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()

	if !strings.Contains(body, `action="/n/312811/quiz"`) {
		t.Fatal("гостю не дали ответить")
	}
	// Токена у гостя нет вовсе: он выводится из сессии, а сессии нет.
	if strings.Contains(body, `name="csrf"`) {
		t.Error("в гостевой форме появилось поле токена")
	}
}

// Ответ гостя не идёт в базу ни строкой: пишущего у него нет, а кука — его
// собственная. Проверяется по тому, что ушло Writer'у.
func TestGuestAnswerNeverReachesTheCore(t *testing.T) {
	h, wr, _ := writeServer(t, quizStore(-1))

	rec := do(h, post(t, "/n/312811/quiz", url.Values{"comment": {"1"}, "choice": {"2"}}))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ответ гостя отвергнут: %d", rec.Code)
	}
	if wr.quizAnswer.CommentID != 0 {
		t.Errorf("ответ гостя ушёл в ядро: %+v", wr.quizAnswer)
	}
	var got string
	for _, c := range rec.Result().Cookies() {
		if c.Name == quizCookie {
			got = c.Value
		}
	}
	if got != "1:2" {
		t.Errorf("кука ответа гостя = %q, ждали 1:2", got)
	}
}

// Ответив, гость видит разгадку — но не проценты: его голос в них не входит, и
// показывать ему чужой счёт значило бы обещать, что он там есть.
func TestGuestSeesRevealButNotShares(t *testing.T) {
	h := newTestServer(t, quizStore(-1), Config{})
	req := guest(t, "GET", "/n/312811")
	req.AddCookie(&http.Cookie{Name: quizCookie, Value: "1:0"})
	body := do(h, req).Body.String()

	if !strings.Contains(body, "261 реплика") {
		t.Error("ответившему гостю не показали разгадку")
	}
	if !strings.Contains(body, "qright") {
		t.Error("правильный вариант не отмечен")
	}
	if strings.Contains(body, "qpct") {
		t.Error("гостю показаны проценты, в которых его нет")
	}
	if !strings.Contains(body, "войдите") {
		t.Error("гостю не сказали, что ответ не попал в общий счёт")
	}
}

// Вошедшему, наоборот, счёт показывается — вместе со своим ответом и вердиктом.
func TestMemberSeesSharesAfterAnswering(t *testing.T) {
	h, _, token := writeServer(t, quizStore(1))
	body := do(h, as(httptest.NewRequest("GET", "/n/312811", nil), token)).Body.String()

	if !strings.Contains(body, "qpct") {
		t.Fatal("участнику не показали проценты")
	}
	if !strings.Contains(body, "Верно") {
		t.Error("нет вердикта")
	}
	if !strings.Contains(body, `href="/n/270011"`) {
		t.Error("нет ссылки на разговор-источник")
	}
	// Кнопок больше нет: ответ окончателен, а форма, отвечающая тишиной, — это
	// кнопка, которая ничего не делает.
	if strings.Contains(body, `action="/n/312811/quiz"`) {
		t.Error("после ответа остались формы")
	}
}

// Ответ вошедшего уходит в ядро полностью — включая заметку: без неё голос
// нельзя проверить на принадлежность треду.
func TestMemberAnswerGoesToCore(t *testing.T) {
	h, wr, token := writeServer(t, quizStore(-1))

	rec := do(h, postAs(t, "/n/312811/quiz", url.Values{"comment": {"1"}, "choice": {"2"}}, token))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ответ отвергнут: %d", rec.Code)
	}
	if got := wr.quizAnswer; got.CommentID != 1 || got.Choice != 2 || got.NoteID != 312811 {
		t.Errorf("в ядро ушло %+v", got)
	}
}

// Кука гостя — чужой ввод: её правят руками. Мусор пропускается молча, а не
// роняет страницу.
func TestGuestCookieSurvivesGarbage(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: quizCookie, Value: "junk,7:1,8:,:2,9:99,-3:0,10:0"})
	got := (&Server{}).guestAnswers(req)

	if len(got) != 2 {
		t.Fatalf("разобрано %v, ждали два годных ответа", got)
	}
	if got[7] != 1 || got[10] != 0 {
		t.Errorf("разобрано неверно: %v", got)
	}
}

// Первый ответ окончателен и в куке тоже: иначе гость, увидев разгадку, просто
// нажал бы правильный вариант — и «мимо» перестало бы существовать.
func TestGuestAnswerIsFinal(t *testing.T) {
	have := map[int64]int{5: 0}
	if got := addGuestAnswer(have, 5, 2); got != "5:0" {
		t.Errorf("ответ переписан: %q", got)
	}
}

// Кука гостя обязана ПЕРЕЖИТЬ префикс `__Host-`, и это не педантизм.
//
// Пишет её setCookie именем от cookieName, а на https он добавляет префикс;
// читал же показ голую константу — и на боевой странице 11.09.2026 ответивший
// гость видел те же три кнопки вместо разгадки, при том что кука в браузере
// стояла. Тестовый сервер живёт на http, где префикса нет вовсе, поэтому
// прежние восемь тестов рубрики зеленели на сломанном коде.
//
// Проверка идёт КРУГОМ и через настоящий обработчик: ответ гостя — GET той же
// страницы с тем, что сервер сам положил в Set-Cookie. Так она ловит любое
// расхождение имён, а не только это.
func TestGuestCookieSurvivesTheHostPrefix(t *testing.T) {
	h := newTestServer(t, quizStore(-1), Config{BaseURL: "https://t3h.ru"})

	rec := do(h, post(t, "/n/312811/quiz", url.Values{"comment": {"1"}, "choice": {"2"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("ответ гостя отвергнут: %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("сервер не поставил куку вовсе")
	}
	if name := cookies[0].Name; name != "__Host-quiz" {
		t.Fatalf("кука названа %q — на https ждали префикс", name)
	}

	req := guest(t, "GET", "/n/312811")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	page := do(h, req)
	if page.Code != http.StatusOK {
		t.Fatalf("страница отдала %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), "Ответ сохранён в этом браузере") {
		t.Error("ответившему гостю не показано, что его голос не считается: сервер не нашёл свою же куку")
	}
}
