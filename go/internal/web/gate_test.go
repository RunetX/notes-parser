package web

// Ворота: при MembersOnly площадку читают только вошедшие (gate.go).
//
// Тесты стоят НА ПУТИ ДАННЫХ — через настоящий роутер, а не через openToGuests:
// формула тут очевидная, а дефект такого слоя живёт ровно в том, доходит ли
// запрос до обработчика. Ровно так же закрыт состав окна у жителей
// (narod/window_test.go) и пол собеседника (narod/wiring_test.go), и по той же
// причине: величина, посчитанная верно и не доехавшая до места, где ею
// пользуются, — это дефект, который тест на формулу не видит.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// closedServer — площадка с закрытыми воротами и вошедшим участником.
func closedServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{notes: []platform.NoteView{sampleNote()}}
	return newFullServer(t, st, auth, nil, nil, nil, Config{MembersOnly: true}), token
}

// Посторонний не читает НИЧЕГО из площадки — ни ленту, ни заметку, ни профиль.
// Карта сайта в этом списке не для полноты: она печатает адрес и время
// последней реплики каждой заметки, то есть отдала бы ровно то, чего человека
// лишили страницами.
func TestЗакрытаяПлощадкаОтворачиваетГостя(t *testing.T) {
	h, _ := closedServer(t)

	for _, path := range []string{"/", "/n/1", "/n/1?view=linear", "/u/1", "/me", "/events", "/sitemap.xml", "/fresh", "/n/1/fresh", "/new"} {
		w := do(h, guest(t, "GET", path))
		if w.Code != http.StatusSeeOther {
			t.Errorf("%s: код %d, ждали 303", path, w.Code)
			continue
		}
		if got := w.Header().Get("Location"); got != "/login" {
			t.Errorf("%s: увели на %q, а не на /login", path, got)
		}
	}
}

// Гостю остаётся ровно то, без чего закрытая дверь становится глухой стеной:
// сам вход, объяснение и бумаги. Политику обработки оператор обязан
// опубликовать так, чтобы её прочёл ЛЮБОЙ (ч. 2 ст. 18.1), — это не щедрость, а
// закон, и потому у него свой тест, а не строка в общем списке.
func TestЗакрытаяПлощадкаОставляетВходСправкуИБумаги(t *testing.T) {
	h, _ := closedServer(t)

	for _, path := range []string{"/login", "/help", "/help/login", "/help/data", "/privacy", "/disclaimer", "/consents", "/robots.txt", "/healthz"} {
		if w := do(h, guest(t, "GET", path)); w.Code != http.StatusOK {
			t.Errorf("%s: код %d, ждали 200 — гостю это открыто", path, w.Code)
		}
	}
}

// Вошедшему закрытые ворота не мешают ничем.
func TestВошедшийЧитаетЗакрытуюПлощадку(t *testing.T) {
	h, token := closedServer(t)

	w := do(h, as(guest(t, "GET", "/"), token))
	if w.Code != http.StatusOK {
		t.Fatalf("лента вошедшему: код %d", w.Code)
	}
	// Тело рисуется абзацами, поэтому ищется первый из них, а не строка целиком.
	if body := w.Body.String(); !strings.Contains(body, "Первый абзац.") {
		t.Errorf("в ленте нет самой заметки:\n%s", body)
	}
}

// Выключенный признак означает ровно прежнее поведение (18.08.2026 — 12.09.2026),
// и это половина довода, по которому закрытие сделано настройкой, а не
// вычеркнутым кодом: у признака два настоящих значения, и оба проверяемы.
func TestОткрытаяПлощадкаПускаетГостяКакПрежде(t *testing.T) {
	st := &fakeStore{notes: []platform.NoteView{sampleNote()}}
	h := newTestServer(t, st, Config{})

	if w := do(h, guest(t, "GET", "/")); w.Code != http.StatusOK {
		t.Fatalf("гость не читает открытую площадку: код %d", w.Code)
	}
}

// robots.txt обязан говорить роботу то же, что ворота говорят человеку: иначе
// робот и человек видят разные площадки. Карта сайта при закрытых воротах не
// объявляется вовсе — ссылка на неё вела бы в редирект.
func TestRobotsПриЗакрытойПлощадке(t *testing.T) {
	h, _ := closedServer(t)

	body := do(h, guest(t, "GET", "/robots.txt")).Body.String()
	if !strings.Contains(body, "Disallow: /\n") {
		t.Errorf("робот не отвёрнут от площадки:\n%s", body)
	}
	if !strings.Contains(body, "Allow: /help") {
		t.Errorf("справка роботу закрыта, хотя человеку открыта:\n%s", body)
	}
	if strings.Contains(body, "Sitemap:") {
		t.Errorf("карта сайта объявлена, хотя лежит за воротами:\n%s", body)
	}
}

// Экран входа — единственное, что видит пришедший по ссылке из канала, поэтому
// обещать ему открытую ленту он не вправе. Проверяется не наличие новой фразы, а
// ОТСУТСТВИЕ прежнего обещания: текст ещё перепишут, а врать он не должен и
// после правки.
func TestЭкранВходаНеОбещаетОткрытуюЛенту(t *testing.T) {
	h, _ := closedServer(t)

	body := do(h, guest(t, "GET", "/login")).Body.String()
	if strings.Contains(body, "можно и без входа") {
		t.Errorf("вход обещает гостю чтение без входа:\n%s", body)
	}
	if !strings.Contains(body, `href="/help"`) {
		t.Errorf("с экрана входа некуда пойти за объяснением:\n%s", body)
	}
}

// А при открытых воротах он это обещание возвращает: тексты обязаны совпадать с
// поведением в ОБОИХ состояниях, иначе справка врёт в одном из них.
func TestЭкранВходаПриОткрытойПлощадкеОбещаетЧтение(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, Config{})

	if body := do(h, guest(t, "GET", "/login")).Body.String(); !strings.Contains(body, "можно и без входа") {
		t.Errorf("открытая площадка не говорит, что читать можно без входа:\n%s", body)
	}
}

// Контакт владельца: ссылка на его страницу за воротами ведёт в редирект, а
// «ссылка, ведущая к отказу, хуже её отсутствия» — правило площадки, записанное
// в web/user.go. Строка при этом обязана остаться: живой собеседник у площадки
// есть, и знать об этом гостю надо.
func TestКонтактВладельцаНеВедётВРедирект(t *testing.T) {
	cfg := Config{MembersOnly: true, Contacts: Contacts{ProfileID: 1493279}}
	h := newTestServer(t, &fakeStore{}, cfg)

	body := do(h, guest(t, "GET", "/help")).Body.String()
	if strings.Contains(body, `href="/u/1493279"`) {
		t.Errorf("гостю дана ссылка на закрытую страницу:\n%s", body)
	}
	if !strings.Contains(body, "Владелец площадки") {
		t.Errorf("контакт владельца пропал совсем:\n%s", body)
	}
}

// А вошедшему — ровно прежняя ссылка: для него страница открыта.
func TestВошедшемуКонтактВладельцаОстаётсяСсылкой(t *testing.T) {
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{MembersOnly: true, Contacts: Contacts{ProfileID: 1493279}}
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, nil, cfg)

	body := do(h, as(guest(t, "GET", "/help"), token)).Body.String()
	if !strings.Contains(body, `href="/u/1493279"`) {
		t.Errorf("вошедший лишился ссылки на страницу владельца:\n%s", body)
	}
}
