package web

// Форма ответа, открывающаяся на месте.
//
// Проверяется здесь ровно то, ради чего заведён отдельный обработчик: строка
// приходит ОДНА и та же самая, что нарисовала бы страница. Совпадение проверено
// сравнением, а не глазами, — потому что вся затея держится на том, что разметку
// собирает один шаблон, и разойтись эти две дороги не имеют права.

import (
	"net/http"
	"strings"
	"testing"
)

// Кусок, а не страница: заголовки и «база» здесь были бы мусором, который
// клиент выбросит.
func TestReplyFormArrivesAsOneRow(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	w := do(h, as(guest(t, "GET", "/n/312811/reply?view=tree&to=2"), token))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", w.Code)
	}
	body := strings.TrimSpace(w.Body.String())
	if !strings.HasPrefix(body, `<li class="replyrow`) || !strings.HasSuffix(body, "</li>") {
		t.Fatalf("это не строка треда:\n%s", body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "<header") {
		t.Error("вместо куска приехала целая страница")
	}
	if !strings.Contains(body, `name="reply_to" value="2"`) {
		t.Error("адресат не подставлен в форму")
	}
	if !strings.Contains(body, `name="csrf"`) {
		t.Error("в форме нет поля csrf — отправить её будет нечем")
	}
}

// Главное свойство: обе дороги — обновлением и скриптом — дают ОДНУ И ТУ ЖЕ
// строку. Разойдясь, они дали бы вторую разметку, а с ней и второе место, где
// однажды забудут про «кому отвечаете», про тень и про экранирование.
func TestReplyFormRowMatchesThePage(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	page := do(h, as(guest(t, "GET", "/n/312811?view=tree&reply=2"), token)).Body.String()
	fragment := strings.TrimSpace(do(h, as(guest(t, "GET", "/n/312811/reply?view=tree&to=2"), token)).Body.String())

	// Сравнивается вхождение целиком, а не кусок до первого «</li>»: внутри
	// формы есть свой список — справочник разметки, — и по нему строку не
	// обрезать. Совпало вхождение — значит скрипт вставит ровно то, что
	// нарисовала бы перезагрузка.
	if !strings.Contains(page, fragment) {
		at := strings.Index(page, `<li class="replyrow`)
		if at < 0 {
			t.Fatal("на странице нет строки с формой")
		}
		n := 0
		for n < len(fragment) && at+n < len(page) && fragment[n] == page[at+n] {
			n++
		}
		t.Errorf("строка скриптом разошлась со страницей на %d-м байте:\n— страница: %.100q\n— скрипт:   %.100q",
			n, page[at+n:], fragment[n:])
	}
}

// Ради этой строки обработчик и ходит в базу за адресатом: тень он или участник,
// в разметке реплики не написано, а вопрос у отвечающего ровно один — дойдёт ли
// ответ до того, кому он пишет.
func TestReplyFormToShadowWarnsItWontReachNGS(t *testing.T) {
	st := noteStore()
	st.thread[1].Author.Shadow = true
	h, _, token := writeServer(t, st)

	body := do(h, as(guest(t, "GET", "/n/312811/reply?to=2"), token)).Body.String()
	if !strings.Contains(body, "не переехал") {
		t.Error("не сказано, что адресат ещё не на площадке")
	}

	st.thread[1].Author.Shadow = false
	body = do(h, as(guest(t, "GET", "/n/312811/reply?to=2"), token)).Body.String()
	if strings.Contains(body, "не переехал") {
		t.Error("участника объявили тенью")
	}
}

// Отказ здесь честный, а не пустой кусок, как у живого добора: на нём скрипт
// уходит по той же ссылке обычным переходом, и человек видит настоящую причину —
// снесённую реплику или чужую заметку в адресе.
func TestReplyFormRefusesWhatItCannotAnswer(t *testing.T) {
	h, _, token := writeServer(t, noteStore())

	cases := []struct {
		target string
		want   int
	}{
		{"/n/312811/reply", http.StatusBadRequest},      // не сказано, кому
		{"/n/312811/reply?to=0", http.StatusBadRequest}, // то же самое числом
		{"/n/312811/reply?to=999", http.StatusNotFound}, // такой реплики нет
	}
	for _, c := range cases {
		if got := do(h, as(guest(t, "GET", c.target), token)).Code; got != c.want {
			t.Errorf("%s: код %d, ожидался %d", c.target, got, c.want)
		}
	}
}

// Гостю форма не показывается и здесь: «читать можно всем» не значит «писать
// могут все», и ответ обязан быть тем же, что на странице.
func TestReplyFormShowsGuestTheDoorNotTheForm(t *testing.T) {
	h := openServer(t, noteStore())

	body := do(h, guest(t, "GET", "/n/312811/reply?to=2")).Body.String()
	if strings.Contains(body, "<textarea") {
		t.Error("гостю показали форму ответа")
	}
	if !strings.Contains(body, `href="/login"`) {
		t.Error("гостю не предложили войти")
	}
}
