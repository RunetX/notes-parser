package web

// Экраны привязки мессенджера. Проверяются переходы и то, ЧТО человек видит на
// каждом шаге: половина устройства привязки — это порядок «сперва документ,
// потом код», и порядок этот виден только здесь.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// bindServer — вошедший участник и морда со всеми ручками.
func bindServer(t *testing.T) (http.Handler, *fakeAuth, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, testProfileID)
	return newFullServer(t, &fakeStore{}, auth, &fakeWriter{}, nil, nil, Config{}), auth, token
}

// СПЕРВА ДОКУМЕНТ, ПОТОМ КОД. Подпись, данная мимоходом под кнопкой, подписью
// не является — поэтому на первом экране кода нет вовсе.
func TestBindShowsDocumentBeforeCode(t *testing.T) {
	h, auth, token := bindServer(t)

	body := do(h, as(guest(t, "GET", "/me/bind"), token)).Body.String()
	doc, err := platform.ConsentDocOf(platform.Operator{}, platform.ConsentBinding)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, doc.Title) {
		t.Error("на экране привязки нет текста согласия")
	}
	if strings.Contains(body, "MSG-") {
		t.Error("код показан до того, как согласие дано")
	}
	if _, ok := auth.consents[testProfileID][platform.ConsentBinding]; ok {
		t.Error("согласие записано одним показом документа")
	}
}

// Код показывается ПРЯМО В ОТВЕТЕ на POST, а не после перехода: редирект унёс
// бы его в адресную строку, то есть в историю браузера и в лог прокси.
func TestBindCodeComesBackInThePostItself(t *testing.T) {
	h, auth, token := bindServer(t)

	w := do(h, postAs(t, "/me/bind", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("код ответа %d, ожидался 200 с кодом на странице", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "MSG-BIND-") {
		t.Fatal("кода привязки на странице нет")
	}
	// Строка для бота печатается целиком: человек копирует её, и «а как именно
	// написать» отпадает вовсе.
	if !strings.Contains(body, "/bind MSG-BIND-") {
		t.Error("не показано, что именно отправлять боту")
	}
	// Единственное, чем человек может себе навредить, названо на том же экране.
	if !strings.Contains(body, "Никому не пересылайте") {
		t.Error("нет предупреждения о пересылке кода")
	}
	if !auth.consents[testProfileID].Has(platform.ConsentBinding, 1) {
		t.Error("согласие не записано вместе с выдачей кода")
	}
}

// Гостю обеих дверей нет: привязывать нечего, пока неизвестно, к чему.
func TestGuestCannotBind(t *testing.T) {
	h, _, _ := bindServer(t)

	if got := do(h, guest(t, "GET", "/me/bind")).Header().Get("Location"); got != "/login" {
		t.Errorf("гостя с экрана привязки ведёт на %q", got)
	}
	// У POST'а дорога своя и короче: без сессии нет и CSRF-токена, поэтому
	// подделанное нажатие отлетает ещё до вопроса «а кто это». Важно здесь одно
	// — кода в ответе нет.
	w := do(h, post(t, "/me/bind", nil))
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "MSG-") {
		t.Errorf("гостю выдали код привязки: %d", w.Code)
	}
}

// «Моя страница» показывает привязку и даёт её снять. Это единственное место,
// где чужая привязка (подсунутый код) бросится в глаза, поэтому строка обязана
// быть видна, а кнопка — работать.
func TestMeShowsBindingsAndUnbinds(t *testing.T) {
	h, auth, token := bindServer(t)
	auth.bindings[testProfileID] = []platform.Binding{{Messenger: platform.IdentityTelegram}}

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "Telegram") || !strings.Contains(body, "Отвязать") {
		t.Fatal("привязка не показана на «Моей странице»")
	}
	w := do(h, postAs(t, "/me/unbind", url.Values{"messenger": {platform.IdentityTelegram}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("отвязка: код %d", w.Code)
	}
	if len(auth.bindings[testProfileID]) != 0 {
		t.Fatal("ядро не узнало об отвязке")
	}
	// Повторное нажатие (двойной клик, возврат по истории) — не страница ошибки:
	// «уже не привязано» для человека это тот же ответ.
	if w := do(h, postAs(t, "/me/unbind", url.Values{"messenger": {platform.IdentityTelegram}}, token)); w.Code != http.StatusOK {
		t.Errorf("повторная отвязка отвечает %d", w.Code)
	}
}

// «Выйти на всех устройствах» — цена скользящего срока сессии: пока окно было
// абсолютным, украденная кука умирала сама, а теперь живёт, пока ею пользуются.
func TestLogoutAllRevokesEverySession(t *testing.T) {
	h, auth, token := bindServer(t)
	second, _, err := auth.CreateSession(context.Background(), testProfileID, "второе устройство")
	if err != nil {
		t.Fatal(err)
	}
	w := do(h, postAs(t, "/me/logout-all", nil, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код ответа %d", w.Code)
	}
	if len(auth.tokens) != 0 {
		t.Fatalf("живых сессий осталось %d", len(auth.tokens))
	}
	// И СВОЯ кука снимается тоже: оставленный в браузере мёртвый токен показывал
	// бы «вы вошли» до первого же запроса.
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, sessCookie) && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Error("своя кука сессии не снята")
	}
	if _, _, err := auth.SessionUser(context.Background(), second); err == nil {
		t.Error("сессия второго устройства пережила выход")
	}
}

// ОТСУТСТВИЕ подписи и ЖИВАЯ подпись — разные вещи, а Live их не различает: у
// нулевой записи RevokedAt пуст, и она честно отвечает «согласие действует».
// Поэтому строка про отзыв не должна появляться у того, кто никогда ничего не
// привязывал, — а появлялась бы у ВСЕХ.
func TestNoBindingConsentLineForThoseWhoNeverBound(t *testing.T) {
	h, _, token := bindServer(t)

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if strings.Contains(body, "Отозвать согласие") {
		t.Error("не подписывавшему предлагают отозвать согласие на привязку")
	}
	// А тому, кто подписал и не довёл до конца, — предлагают: иначе строка
	// согласия остаётся без данных и снять её нечем.
	if w := do(h, postAs(t, "/me/bind", nil, token)); w.Code != http.StatusOK {
		t.Fatalf("выдача кода: %d", w.Code)
	}
	body = do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, "Отозвать согласие") {
		t.Error("подписавшему и не привязавшему нечем снять согласие")
	}
}

// СКОЛЬЗЯЩИЙ СРОК ОБЯЗАН СКОЛЬЗИТЬ И В БРАУЗЕРЕ.
//
// Срок сессии живёт в ДВУХ местах: строкой в базе и Max-Age у куки. Ядро
// двигало только первый, а кука ставилась один раз при входе — и человек
// оказывался снаружи в тот же день, что и до всей правки: браузер выбрасывал
// куку по своему сроку, и предъявлять становилось нечего. Для владельца
// площадки, у которого анкеты НГС больше нет, это и есть запертая дверь.
//
// Тест смотрит на ЗАГОЛОВОК ОТВЕТА, а не на вызов ядра: дефект жил ровно в том,
// доходит ли продление до браузера.
func TestVisitSlidesTheCookieNotOnlyTheRow(t *testing.T) {
	h, auth, token := bindServer(t)

	// Обычная страница, продления нет — куку трогать незачем.
	if got := sessionCookieOf(do(h, as(guest(t, "GET", "/me"), token))); got != nil {
		t.Errorf("куку переставили без продления: MaxAge=%d", got.MaxAge)
	}

	// Ядро сказало «сессия продлена до…» — кука обязана уехать следом.
	auth.sessionUntil = time.Now().Add(platform.SessionTTL)
	c := sessionCookieOf(do(h, as(guest(t, "GET", "/me"), token)))
	if c == nil {
		t.Fatal("продление не доехало до браузера: Set-Cookie нет вовсе")
	}
	if c.Value != token {
		t.Errorf("переставили не ту куку: %q", c.Value)
	}
	// Срок обязан быть НОВЫМ, то есть почти полным SessionTTL, а не остатком
	// прежнего: иначе «скольжение» сводилось бы к переписыванию того же числа.
	if want := int(platform.SessionTTL / time.Second); c.MaxAge < want-120 || c.MaxAge > want+120 {
		t.Errorf("Max-Age %d, ожидался около %d", c.MaxAge, want)
	}
	if !c.HttpOnly {
		t.Error("переставленная кука потеряла HttpOnly")
	}
	if c.Path != "/" {
		t.Errorf("переставленная кука легла на путь %q", c.Path)
	}
}

// sessionCookieOf — кука сессии из ответа, либо nil. Имя спрашивается у самого
// сервера: на https к нему добавляется префикс __Host-, и написанное словом
// читало бы не то, что пишет setCookie (на этом уже обожглись с кукой рубрики
// 11.09.2026).
func sessionCookieOf(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if strings.HasSuffix(c.Name, sessCookie) {
			return c
		}
	}
	return nil
}
