package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// Проверки идут против настоящего роутера, но с поддельным хранилищем: пакет
// web отвечает за страницы, а не за SQL, и гонять ради вёрстки интеграционные
// тесты ядра (они ходят к боевому Postgres по ssh-туннелю и стоят двух минут)
// было бы платой без покупки.

const testKey = "секретный-ключ-стройки"

type fakeStore struct {
	notes      []platform.NoteView
	total      int
	feedOffset int // что пришло в последний вызов

	note    platform.NoteView
	noteErr error
	images  []platform.Media

	thread []platform.CommentView

	flat       []platform.CommentView
	flatOffset int
	flatUsed   bool

	pingErr error
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) CountNotes(context.Context) (int, error) { return f.total, nil }

func (f *fakeStore) Feed(_ context.Context, _ platform.Viewer, offset, _ int) ([]platform.NoteView, error) {
	f.feedOffset = offset
	return f.notes, nil
}

func (f *fakeStore) NoteViewByID(_ context.Context, _ platform.Viewer, id int64) (platform.NoteView, error) {
	if f.noteErr != nil {
		return platform.NoteView{}, f.noteErr
	}
	n := f.note
	n.ID = id
	return n, nil
}

func (f *fakeStore) NoteImages(context.Context, int64) ([]platform.Media, error) {
	return f.images, nil
}

func (f *fakeStore) Thread(context.Context, platform.Viewer, int64) ([]platform.CommentView, error) {
	return f.thread, nil
}

func (f *fakeStore) Flat(_ context.Context, _ platform.Viewer, _ int64, offset, _ int) ([]platform.CommentView, error) {
	f.flatUsed, f.flatOffset = true, offset
	return f.flat, nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestServer(t *testing.T, st Store, cfg Config) http.Handler {
	t.Helper()
	return newFullServer(t, st, newFakeAuth(), nil, nil, cfg)
}

// newFullServer — сервер со всеми зависимостями. site == nil означает «вход по
// анкете недоступен»: это рабочее состояние площадки после смерти НГС.
func newFullServer(t *testing.T, st Store, auth Auth, wr Writer, site Site, cfg Config) http.Handler {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://127.0.0.1"
	}
	cfg.Log = quietLog()
	srv := New(cfg, st, auth, wr, site)
	t.Cleanup(func() { _ = srv.Close() })
	return srv.routes()
}

func openServer(t *testing.T, st Store) http.Handler {
	t.Helper()
	return newTestServer(t, st, Config{})
}

// guest — обычный посетитель: не вошёл и входить не собирался. Чтение открыто
// всем, поэтому таким запросом проверяется почти всё.
func guest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, target, nil)
}

// post — форма, отправленная с нашей же страницы.
func post(t *testing.T, target string, form url.Values) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

// as — тот же запрос, но от вошедшего.
func as(r *http.Request, token string) *http.Request {
	r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
	return r
}

// postAs — форма от вошедшего: и кука сессии, и скрытое поле CSRF. Обе половины
// сразу, потому что в браузере они тоже приходят вместе — а отдельно проверять
// вторую есть отдельный тест.
func postAs(t *testing.T, target string, form url.Values, token string) *http.Request {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set(csrfField, csrfToken(token))
	return as(post(t, target, form), token)
}

// cookieOf достаёт куку из ответа.
func cookieOf(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// sampleThread — три реплики: корень, ответ на него и ответ на ответ.
func sampleThread() []platform.CommentView {
	at := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	return []platform.CommentView{
		{ID: 1, Author: platform.Author{ID: 1, Nick: "Пух"}, Body: "Согласна.", Depth: 1, PublishedAt: at},
		{ID: 2, Author: platform.Author{ID: 2, Nick: "Мавр"}, Body: "и правда", Depth: 2,
			ReplyTo: &platform.ReplyRef{CommentID: 1, Nick: "Пух"}, PublishedAt: at},
		{ID: 3, Author: platform.Author{ID: 1, Nick: "Пух"}, Body: "вот именно", Depth: 3,
			ReplyTo: &platform.ReplyRef{CommentID: 2, Nick: "Мавр"}, PublishedAt: at},
	}
}

func sampleNote() platform.NoteView {
	return platform.NoteView{
		ID:             312811,
		Author:         platform.Author{ID: 1493279, Nick: "Рио", AvatarURL: "/media/ab/abcd.jpg"},
		Body:           "Первый абзац.\n\nВторой абзац.",
		CommentCount:   3,
		PublishedAt:    time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		PublishedExact: true,
	}
}

// ---------------------------------------------------------------- вход

// Чтение открыто всем — решение владельца от 18.08.2026. Ни одна страница не
// требует входа; вход нужен, чтобы писать и чтобы прежние реплики стали своими.
func TestReadingIsOpenToEveryone(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := newTestServer(t, st, Config{})
	for _, target := range []string{"/", "/n/312811", "/login", "/login/invite", "/healthz", "/robots.txt"} {
		if got := do(h, guest(t, "GET", target)).Code; got != http.StatusOK {
			t.Errorf("%s: код %d, ожидался 200", target, got)
		}
	}
}

// Нет клиента НГС — форма ввода анкеты не показывается вовсе: показать её и
// получить отказ хуже, чем сразу сказать про приглашения.
func TestLoginWithoutSiteOffersInviteOnly(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, Config{})
	body := do(h, guest(t, "GET", "/login")).Body.String()
	if strings.Contains(body, `name="profile"`) {
		t.Error("форма входа по анкете показана, хотя НГС недоступен")
	}
	if !strings.Contains(body, "/login/invite") {
		t.Error("не предложен вход по приглашению")
	}
}

// Полный путь основного входа: номер анкеты → код → код в «о себе» → согласия.
func TestLoginByProfileCode(t *testing.T) {
	auth := newFakeAuth()
	site := &fakeSite{prof: SiteProfile{Nick: testNick}}
	h := newFullServer(t, &fakeStore{}, auth, nil, site, Config{})

	w := do(h, post(t, "/login", url.Values{"profile": {"https://love.ngs.ru/profile/1493279/"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("шаг «это вы?»: код %d", w.Code)
	}
	code := auth.codes[testProfileID]
	if code == "" || !strings.Contains(w.Body.String(), code) {
		t.Fatal("код не показан на странице")
	}
	if !strings.Contains(w.Body.String(), testNick) {
		t.Error("на экране подтверждения нет ника анкеты")
	}
	jar := cookieOf(w, codeCookie)
	if jar == nil || !jar.HttpOnly {
		t.Fatal("кука проверки не поставлена или не HttpOnly")
	}

	// Кода в анкете ещё нет — вход не проходит.
	r := post(t, "/login/check", nil)
	r.AddCookie(jar)
	if got := do(h, r).Code; got != http.StatusUnauthorized {
		t.Fatalf("проверка без кода в анкете: код %d, ожидался 401", got)
	}

	// Человек вставил код в «о себе».
	site.prof.AboutMe = "слеп, глуп, туп\n" + code
	r = post(t, "/login/check", nil)
	r.AddCookie(jar)
	w = do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/consent" {
		t.Fatalf("успешная проверка: код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	sess := cookieOf(w, sessCookie)
	if sess == nil || sess.Value == "" || !sess.HttpOnly {
		t.Fatal("сессия не выдана")
	}
}

// Код в чужом «о себе» видит кто угодно, поэтому одной анкеты мало: проверку
// засчитывают только вместе с кукой того, кто эту проверку начал.
func TestVerificationNeedsBothHalves(t *testing.T) {
	auth := newFakeAuth()
	site := &fakeSite{prof: SiteProfile{Nick: testNick}}
	h := newFullServer(t, &fakeStore{}, auth, nil, site, Config{})

	do(h, post(t, "/login", url.Values{"profile": {"1493279"}}))
	site.prof.AboutMe = auth.codes[testProfileID] // код в анкете есть

	// ...а куки нет: это посторонний, подсмотревший код.
	w := do(h, post(t, "/login/check", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("проверка без куки: код %d, ожидался 401", w.Code)
	}
	if cookieOf(w, sessCookie) != nil {
		t.Fatal("постороннему выдали сессию по чужому коду из анкеты")
	}
}

func TestLoginRejectsCrossSitePost(t *testing.T) {
	h := newFullServer(t, &fakeStore{}, newFakeAuth(), nil, &fakeSite{}, Config{})
	r := httptest.NewRequest("POST", "/login", strings.NewReader("profile=1493279"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", got)
	}
}

func TestLoginTellsWhenProfileIsMissing(t *testing.T) {
	h := newFullServer(t, &fakeStore{}, newFakeAuth(), nil, &fakeSite{missing: true}, Config{})
	w := do(h, post(t, "/login", url.Values{"profile": {"999999"}}))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "нет") {
		t.Fatalf("несуществующая анкета: код %d", w.Code)
	}
}

// Приглашение — третий путь и единственный, переживающий смерть НГС.
func TestInviteLetsIn(t *testing.T) {
	auth := newFakeAuth()
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, Config{})
	w := do(h, post(t, "/login/invite", url.Values{"code": {testInvite}, "nick": {"Новенький"}}))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/consent" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if cookieOf(w, sessCookie) == nil {
		t.Fatal("сессия по приглашению не выдана")
	}
	// Второй раз тот же код не работает.
	if got := do(h, post(t, "/login/invite", url.Values{"code": {testInvite}})).Code; got != http.StatusUnauthorized {
		t.Errorf("повторное использование приглашения: код %d, ожидался 401", got)
	}
}

// ---------------------------------------------------------------- вход в личку

// Основной канал: код уходит личным сообщением на НГС, человек переписывает его
// в форму. Код при этом НЕ показывается на экране — в этом вся разница с
// запасным путём.
func TestLoginByTalksCode(t *testing.T) {
	auth := newFakeAuth()
	var sent []string
	site := talksSite{&fakeSite{prof: SiteProfile{Nick: testNick, PassportID: 280703879}, sent: &sent}}
	h := newFullServer(t, &fakeStore{}, auth, nil, site, Config{})

	w := do(h, post(t, "/login", url.Values{"profile": {"1493279"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("шаг «это вы?»: код %d", w.Code)
	}
	if len(sent) != 1 {
		t.Fatalf("сообщений отправлено %d, ожидалось 1", len(sent))
	}
	code := auth.talks[testProfileID]
	if code == "" || !strings.Contains(sent[0], code) {
		t.Fatalf("в сообщение попал не тот код: %q", sent[0])
	}
	if !strings.Contains(sent[0], "280703879:") {
		t.Errorf("сообщение ушло не на паспорт анкеты: %q", sent[0])
	}
	body := w.Body.String()
	if strings.Contains(body, code) {
		t.Fatal("код показан на экране — так вход под чужой анкетой стоит одного нажатия")
	}
	if !strings.Contains(body, `name="code"`) {
		t.Fatal("нет поля для ввода кода")
	}

	jar := cookieOf(w, codeCookie)
	if jar == nil {
		t.Fatal("кука проверки не поставлена")
	}
	if strings.Contains(jar.Value, code) {
		t.Fatal("код положили в куку — у канала лички его знает только получатель")
	}

	// Не тот код — не пускаем.
	r := post(t, "/login/check", url.Values{"code": {"T3H-ZZZZ-ZZZZ"}})
	r.AddCookie(jar)
	if got := do(h, r).Code; got != http.StatusUnauthorized {
		t.Fatalf("чужой код: код %d, ожидался 401", got)
	}

	// Тот самый — пускаем.
	r = post(t, "/login/check", url.Values{"code": {strings.ToLower(code)}})
	r.AddCookie(jar)
	w = do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/consent" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if cookieOf(w, sessCookie) == nil {
		t.Fatal("сессия не выдана")
	}
}

// Служебный аккаунт мёртв или сообщение не ушло — вход обязан уйти на запасной
// путь, а не встать. Человек в этом не виноват и починить это не может.
func TestFailedTalksSendFallsBackToProfileField(t *testing.T) {
	auth := newFakeAuth()
	var sent []string
	site := talksSite{&fakeSite{
		prof:    SiteProfile{Nick: testNick, PassportID: 280703879},
		sent:    &sent,
		sendErr: errors.New("НГС не принял сообщение"),
	}}
	h := newFullServer(t, &fakeStore{}, auth, nil, site, Config{})

	w := do(h, post(t, "/login", url.Values{"profile": {"1493279"}}))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	code := auth.codes[testProfileID]
	if code == "" || !strings.Contains(w.Body.String(), code) {
		t.Fatal("запасной путь не показал код для поля «о себе»")
	}
	if jar := cookieOf(w, codeCookie); jar == nil || !strings.Contains(jar.Value, code) {
		t.Fatal("у запасного пути код обязан быть в куке: проверка там двусторонняя")
	}
}

// Код, показанный на экране, нельзя принимать введённым обратно: иначе войти под
// чужой анкетой можно в одно нажатие — запросил код на чужой номер, увидел его у
// себя, переписал в поле. Каналы разведены именно поэтому, и тест стережёт это.
func TestShownCodeIsNotAcceptedBackAsInput(t *testing.T) {
	auth := newFakeAuth()
	site := &fakeSite{prof: SiteProfile{Nick: testNick}} // слать нечем — запасной путь
	h := newFullServer(t, &fakeStore{}, auth, nil, site, Config{})

	w := do(h, post(t, "/login", url.Values{"profile": {"1493279"}}))
	code := auth.codes[testProfileID]
	jar := cookieOf(w, codeCookie)
	if code == "" || jar == nil {
		t.Fatal("запасной путь не начался")
	}
	// В анкете кода нет, зато он введён в форму — это не должно помочь.
	r := post(t, "/login/check", url.Values{"code": {code}})
	r.AddCookie(jar)
	if got := do(h, r).Code; got != http.StatusUnauthorized {
		t.Fatalf("показанный код приняли вводом: код %d, ожидался 401", got)
	}
}

// ---------------------------------------------------------------- согласия

func signedInServer(t *testing.T) (http.Handler, *fakeAuth, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	return newFullServer(t, &fakeStore{}, auth, nil, nil, Config{}), auth, token
}

// Два согласия спрашиваются ПО ОДНОМУ: экран с двумя галочками — ровно то, что
// запрещает ч. 1 ст. 10.1, и стеречь это должен тест, а не память.
func TestConsentAsksOneDocumentAtATime(t *testing.T) {
	h, auth, token := signedInServer(t)

	w := do(h, as(guest(t, "GET", "/consent"), token))
	body := w.Body.String()
	if !strings.Contains(body, "Шаг 1 из 2") {
		t.Fatalf("первый экран согласия не показан:\n%s", body)
	}
	if strings.Contains(body, "Согласие на распространение") {
		t.Error("оба документа на одном экране — это и запрещено")
	}

	w = do(h, postAs(t, "/consent", url.Values{
		"kind": {platform.ConsentProcessing}, "version": {"1"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("после первого согласия: код %d", w.Code)
	}
	if !strings.Contains(do(h, as(guest(t, "GET", "/consent"), token)).Body.String(), "Шаг 2 из 2") {
		t.Fatal("второй экран согласия не показан")
	}

	w = do(h, postAs(t, "/consent", url.Values{
		"kind": {platform.ConsentDistribution}, "version": {"1"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("после второго согласия: код %d", w.Code)
	}
	if got := do(h, as(guest(t, "GET", "/consent"), token)).Header().Get("Location"); got != "/me" {
		t.Errorf("после обоих согласий ведёт на %q, ожидалось /me", got)
	}
	if len(auth.consents[testProfileID]) != 2 {
		t.Errorf("записано согласий: %d, ожидалось 2", len(auth.consents[testProfileID]))
	}
}

// Подделанное поле формы не должно записать согласие на документ, которого
// человек не видел: что именно подписано, решает сервер.
func TestConsentIgnoresFormClaim(t *testing.T) {
	h, auth, token := signedInServer(t)
	do(h, postAs(t, "/consent", url.Values{
		"kind": {platform.ConsentDistribution}, "version": {"1"},
	}, token))
	if _, ok := auth.consents[testProfileID][platform.ConsentDistribution]; ok {
		t.Error("записано согласие на документ, который не показывался")
	}
}

// Отказ откатывает вход целиком, а не оставляет участника без согласий.
func TestConsentRefusalRollsBackLogin(t *testing.T) {
	h, auth, token := signedInServer(t)
	w := do(h, postAs(t, "/consent/refuse", nil, token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if len(auth.aborted) != 1 || auth.aborted[0] != testProfileID {
		t.Error("вход не откачен")
	}
	if c := cookieOf(w, sessCookie); c == nil || c.MaxAge >= 0 {
		t.Error("кука сессии не снята")
	}
}

// Незавершённый вход ведёт на согласия, а не показывает половину «Моей страницы».
func TestMeRedirectsToConsentWhenIncomplete(t *testing.T) {
	h, _, token := signedInServer(t)
	if got := do(h, as(guest(t, "GET", "/me"), token)).Header().Get("Location"); got != "/consent" {
		t.Errorf("ведёт на %q, ожидалось /consent", got)
	}
}

// Отзыв согласия на распространение доходит до ядра: обещание «исчезает
// немедленно» обязано быть исполнимым, а не написанным в документе.
func TestMeRevokesDistribution(t *testing.T) {
	h, auth, token := signedInServer(t)
	ctx := context.Background()
	grantBoth(t, auth, ctx)

	w := do(h, postAs(t, "/me/consent", url.Values{
		"kind": {platform.ConsentDistribution}, "action": {"revoke"},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d", w.Code)
	}
	if len(auth.revoked[testProfileID]) != 1 {
		t.Fatal("отзыв не дошёл до ядра")
	}
}

// В шапке у вошедшего — свой ник и выход, у гостя — «Вход».
func TestHeaderShowsWhoYouAre(t *testing.T) {
	h, auth, token := signedInServer(t)
	grantBoth(t, auth, context.Background())

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, `class="who"`) || !strings.Contains(body, testNick) {
		t.Error("в шапке нет ника вошедшего")
	}
	if !strings.Contains(body, `action="/logout"`) {
		t.Error("вошедшему не предложен выход")
	}
	if strings.Contains(body, ">Вход<") {
		t.Error("вошедшему всё ещё предлагают войти")
	}
}

func grantBoth(t *testing.T, auth *fakeAuth, ctx context.Context) {
	t.Helper()
	for _, k := range []string{platform.ConsentProcessing, platform.ConsentDistribution} {
		if err := auth.GrantConsent(ctx, testProfileID, k, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogoutClearsSession(t *testing.T) {
	h, auth, token := signedInServer(t)
	w := do(h, postAs(t, "/logout", url.Values{"back": {"/n/312811"}}, token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/n/312811" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if len(auth.tokens) != 0 {
		t.Error("сессия не погашена в ядре")
	}
	if c := cookieOf(w, sessCookie); c == nil || c.MaxAge >= 0 {
		t.Error("кука сессии не снята")
	}
}

func TestParseProfileID(t *testing.T) {
	cases := map[string]int64{
		"1493279":                              1493279,
		" 1493279 ":                            1493279,
		"https://love.ngs.ru/profile/1493279/": 1493279,
		"m.love.ngs.ru/profile/175869/":        175869,
		"мой профиль":                          0,
		"0":                                    0,
		"999999999999":                         0, // нативная полоса — это не анкета НГС
		// Хвост в адресе не должен уводить на чужой номер: 18.08.2026 человек
		// так попал на /profile/6/ и получил «НГС не отвечает» вместо
		// «проверьте номер».
		"https://love.ngs.ru/profile/1516080/photos/6/": 1516080,
		"love.ngs.ru/profile/1493279/?page=2":           1493279,
		"https://love.ngs.ru/anketa306551/":             306551,
		// Ссылки нет — берём самое длинное число, а не последнее.
		"анкета 1409563, мне 41": 1409563,
	}
	for in, want := range cases {
		if got := parseProfileID(in); got != want {
			t.Errorf("parseProfileID(%q) = %d, ожидалось %d", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- лента

func TestFeedRendersNotes(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}}
	h := openServer(t, st)
	w := do(h, guest(t, "GET", "/"))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Рио", "Первый абзац.", `href="/n/312811"`, "Комментарии"} {
		if !strings.Contains(body, want) {
			t.Errorf("в ленте нет %q", want)
		}
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q: страницы не кэшируются", got)
	}
}

func TestFeedAnonymousNoteShowsNoAuthor(t *testing.T) {
	n := sampleNote()
	n.Anonymous, n.Author = true, platform.Author{}
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{n}})
	body := do(h, guest(t, "GET", "/")).Body.String()
	if !strings.Contains(body, "Аноним") {
		t.Error("анонимная заметка не подписана «Аноним»")
	}
	if strings.Contains(body, "Рио") {
		t.Error("в разметке анонимной заметки оказался автор")
	}
}

// ---------------------------------------------------------------- заметка

func TestNotePageShowsTreeByDefault(t *testing.T) {
	st := &fakeStore{note: sampleNote(), thread: sampleThread()}
	h := openServer(t, st)
	w := do(h, guest(t, "GET", "/n/312811"))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	body := w.Body.String()
	if st.flatUsed {
		t.Error("по умолчанию тред обязан быть деревом")
	}
	for _, want := range []string{`id="c1"`, `class="c d2"`, `href="#c1"`, "Пух</a>, и правда"} {
		if !strings.Contains(body, want) {
			t.Errorf("в треде нет %q", want)
		}
	}
	// Обращение дорисовано из ребра, а не взято из тела: в базе тело хранится
	// без префикса, и «Пух, Пух, …» на странице означало бы, что мы его удвоили.
	if strings.Contains(body, "Пух, Пух") {
		t.Error("обращение нарисовано дважды")
	}
}

func TestNotePageLinearViewRemembered(t *testing.T) {
	st := &fakeStore{note: sampleNote()}
	h := openServer(t, st)
	w := do(h, guest(t, "GET", "/n/312811?view=linear"))
	if !st.flatUsed {
		t.Fatal("линейный вид не дошёл до хранилища")
	}
	// Выбор вида запоминается: переключатель на сайте живой, и выбирать заново
	// на каждой заметке — раздражение на ровном месте.
	var remembered bool
	for _, c := range w.Result().Cookies() {
		if c.Name == viewCookie && c.Value == "linear" {
			remembered = true
		}
	}
	if !remembered {
		t.Error("вид треда не запомнен кукой")
	}
}

func TestNotePageRemembersViewFromCookie(t *testing.T) {
	st := &fakeStore{note: sampleNote()}
	h := openServer(t, st)
	r := guest(t, "GET", "/n/312811")
	r.AddCookie(&http.Cookie{Name: viewCookie, Value: "linear"})
	do(h, r)
	if !st.flatUsed {
		t.Error("запомненный вид не применился")
	}
}

func TestNoteNotFound(t *testing.T) {
	h := openServer(t, &fakeStore{noteErr: platform.ErrNotFound})
	if got := do(h, guest(t, "GET", "/n/999")).Code; got != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404", got)
	}
}

// Скрытая заметка для читателя просто отсутствует: «есть, но спрятана» —
// это показ работы модерации посторонним.
func TestHiddenNoteLooksMissing(t *testing.T) {
	n := sampleNote()
	n.Status = platform.StatusHiddenMod
	h := openServer(t, &fakeStore{note: n})
	w := do(h, guest(t, "GET", "/n/312811"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "Первый абзац") {
		t.Error("скрытая заметка показала текст")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	h := openServer(t, &fakeStore{})
	if got := do(h, guest(t, "GET", "/такого-нет")).Code; got != http.StatusNotFound {
		t.Fatalf("код %d, ожидался 404", got)
	}
}

// ---------------------------------------------------------------- темы

func TestThemeSetsCookieAndReturns(t *testing.T) {
	h := openServer(t, &fakeStore{})
	form := url.Values{"theme": {"dark"}, "back": {"/n/312811"}}
	r := httptest.NewRequest("POST", "/theme", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/n/312811" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	var ok bool
	for _, c := range w.Result().Cookies() {
		if c.Name == themeCookie && c.Value == "dark" {
			ok = true
		}
	}
	if !ok {
		t.Error("тема не запомнена")
	}
}

// Адрес возврата приходит из формы, то есть от кого угодно: без проверки это
// открытый редирект на чужой сайт с нашего домена.
func TestThemeRefusesForeignReturn(t *testing.T) {
	h := openServer(t, &fakeStore{})
	for _, back := range []string{"//evil.example", `/\evil.example`, "https://evil.example/x"} {
		form := url.Values{"theme": {"classic"}, "back": {back}}
		r := httptest.NewRequest("POST", "/theme", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if got := do(h, r).Header().Get("Location"); got != "/" {
			t.Errorf("back=%q увёл на %q", back, got)
		}
	}
}

func TestThemeCookieRendersAttribute(t *testing.T) {
	h := openServer(t, &fakeStore{})
	r := guest(t, "GET", "/")
	r.AddCookie(&http.Cookie{Name: themeCookie, Value: "classic"})
	if !strings.Contains(do(h, r).Body.String(), `data-theme="classic"`) {
		t.Error("тема из куки не доехала до разметки")
	}
	// Мусор в куке читается как «как в системе»: кука приходит от человека.
	r2 := guest(t, "GET", "/")
	r2.AddCookie(&http.Cookie{Name: themeCookie, Value: `<script>alert(1)</script>`})
	if strings.Contains(do(h, r2).Body.String(), "<script>alert") {
		t.Error("значение куки попало в разметку")
	}
}

// Тем ровно две, и «как в системе» среди них нет. Это не выбор, а его
// отсутствие: кнопка, после нажатия которой ничего не выбрано, объясняется
// дольше, чем стоит. Светлая при этом одна — прежние «Классика» и «Светлая»
// показывали одну и ту же палитру.
func TestThemeListIsTwoDistinctThemes(t *testing.T) {
	if len(themes) != 2 {
		t.Fatalf("тем %d: %+v", len(themes), themes)
	}
	seen := map[string]bool{}
	for _, th := range themes {
		if th.ID == "" {
			t.Errorf("в наборе тема без идентификатора: %+v", th)
		}
		if seen[th.Name] {
			t.Errorf("две темы с именем %q", th.Name)
		}
		seen[th.Name] = true
	}
}

// Выбор, сделанный до сокращения набора, не пропадает: кука light — та же
// светлая палитра. Уронить её в «решает браузер» значило бы у человека с тёмной
// системой самовольно погасить страницу.
func TestLegacyLightCookieStaysLight(t *testing.T) {
	h := openServer(t, &fakeStore{})
	r := guest(t, "GET", "/")
	r.AddCookie(&http.Cookie{Name: themeCookie, Value: "light"})
	if !strings.Contains(do(h, r).Body.String(), `data-theme="classic"`) {
		t.Error("прежняя светлая тема потерялась")
	}
}

// ---------------------------------------------------------------- заголовки и статика

func TestSecurityHeaders(t *testing.T) {
	h := openServer(t, &fakeStore{})
	head := do(h, guest(t, "GET", "/")).Header()
	csp := head.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("в CSP нет %q", want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP ослаблен unsafe-inline — тогда и глубина ветки поехала бы в style")
	}
	if !strings.Contains(head.Get("X-Robots-Tag"), "noindex") {
		t.Error("нет запрета индексации заголовком")
	}
}

func TestAssetsAreHashedAndCacheable(t *testing.T) {
	h := openServer(t, &fakeStore{})
	u := assetURL("style.css")
	if u == "" || !strings.Contains(u, ".css") || u == "/assets/style.css" {
		t.Fatalf("адрес статики без хеша: %q", u)
	}
	w := do(h, httptest.NewRequest("GET", u, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("код %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	etag := w.Header().Get("ETag")
	r := httptest.NewRequest("GET", u, nil)
	r.Header.Set("If-None-Match", etag)
	if got := do(h, r).Code; got != http.StatusNotModified {
		t.Errorf("повторный запрос: код %d, ожидался 304", got)
	}
	// Статика доступна и не вошедшему: страница входа тоже должна быть одета.
	if do(h, guest(t, "GET", u)).Code != http.StatusOK {
		t.Error("статика требует входа")
	}
}

func TestMediaServesFilesAndHidesListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ab", "abcd.jpg"), []byte("не-картинка-но-байты"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newTestServer(t, &fakeStore{}, Config{MediaDir: dir})

	if got := do(h, httptest.NewRequest("GET", "/media/ab/abcd.jpg", nil)).Code; got != http.StatusOK {
		t.Errorf("файл не отдался: код %d", got)
	}
	// Каталог — 404, а не список: перечня хранилища наружу быть не должно.
	if got := do(h, httptest.NewRequest("GET", "/media/ab/", nil)).Code; got != http.StatusNotFound {
		t.Errorf("каталог отдался: код %d", got)
	}
	if got := do(h, httptest.NewRequest("GET", "/media/../config.json", nil)).Code; got == http.StatusOK {
		t.Error("выход за каталог хранилища отдал файл")
	}
}

func TestHealthFollowsDatabase(t *testing.T) {
	st := &fakeStore{}
	h := openServer(t, st)
	if got := do(h, httptest.NewRequest("GET", "/healthz", nil)).Code; got != http.StatusOK {
		t.Fatalf("код %d", got)
	}
	st.pingErr = errors.New("нет связи")
	if got := do(h, httptest.NewRequest("GET", "/healthz", nil)).Code; got != http.StatusServiceUnavailable {
		t.Fatalf("код %d, ожидался 503: живой контейнер с мёртвой базой обслужить ничего не может", got)
	}
}

// [Ф] Угол участника устроен как на НГС: значок в правом верхнем углу, под ним
// выпадающее меню. Пунктов там три, у нас два — отдельной страницы настроек
// площадка не заводит, ник и согласия живут на /me.
func TestAccountMenuHasProfileAndExit(t *testing.T) {
	h, auth, token := signedInServer(t)
	grantBoth(t, auth, context.Background())

	head, _, ok := strings.Cut(do(h, as(guest(t, "GET", "/"), token)).Body.String(), "</header>")
	if !ok {
		t.Fatal("на странице нет шапки")
	}
	for _, want := range []string{`<details class="acct">`, `<summary`, `href="/me"`,
		"Мой профиль", `action="/logout"`, "Выход"} {
		if !strings.Contains(head, want) {
			t.Errorf("в меню участника нет %q", want)
		}
	}
}

// Меню обязано открываться БЕЗ скрипта: details/summary раскрывает сам браузер.
// Строгий CSP запрещает inline-скрипты, внешних зависимостей у площадки нет, и
// пункт «Выход», доступный только при работающем JS, однажды перестал бы быть
// доступным вовсе.
func TestAccountMenuWorksWithoutScript(t *testing.T) {
	h, auth, token := signedInServer(t)
	grantBoth(t, auth, context.Background())
	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()

	menu := body[strings.Index(body, `<details class="acct">`):]
	menu = menu[:strings.Index(menu, "</details>")]
	if !strings.Contains(menu, "<summary") {
		t.Fatal("меню не раскрывается разметкой: нет summary")
	}
	if !strings.Contains(menu, "Мой профиль") || !strings.Contains(menu, "Выход") {
		t.Error("пункты меню приходят не с сервера — без JS их не будет")
	}
}

// У гостя меню нет вовсе: ему нечего в нём открывать, а «Вход» остаётся.
func TestGuestHasNoAccountMenu(t *testing.T) {
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}})
	head, _, _ := strings.Cut(do(h, guest(t, "GET", "/")).Body.String(), "</header>")
	if strings.Contains(head, `<details class="acct">`) {
		t.Error("гостю показали меню участника")
	}
	if !strings.Contains(head, ">Вход<") {
		t.Error("гостю не показали вход")
	}
}
