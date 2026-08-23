package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	notes       []platform.NoteView
	pinned      []platform.NoteView
	pinnedCalls int // спрашивают ли закреплённые на страницах, кроме первой
	total       int
	countCalls  int // сколько раз спросили длину ленты (она кэшируется)
	feedOffset  int // что пришло в последний вызов

	note    platform.NoteView
	noteErr error
	images  []platform.Media

	thread []platform.CommentView

	reactions   map[int64][]platform.Reaction
	reactViewer int64

	flat       []platform.CommentView
	flatOffset int
	flatUsed   bool

	// Живой добор: что отдать и с какой границей за ним пришли.
	fresh       []platform.CommentView
	freshAfter  platform.FreshAfter
	freshBound  *platform.FreshAfter
	freshErr    error
	freshNotes  []platform.NoteView
	freshSince  time.Time
	freshNoteID int64

	// Карта сайта: что отдать роботу.
	sitemap []platform.SitemapNote

	// Переезды: что отдать и с какой границей за ними пришли.
	moved      []platform.CommentView
	movedAfter platform.MovedAfter
	movedNext  platform.MovedAfter
	movedErr   error

	pingErr error
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }

func (f *fakeStore) CountNotes(context.Context) (int, error) {
	f.countCalls++
	return f.total, nil
}

func (f *fakeStore) Feed(_ context.Context, _ platform.Viewer, offset, _ int) ([]platform.NoteView, error) {
	f.feedOffset = offset
	return f.notes, nil
}

func (f *fakeStore) PinnedNotes(context.Context, platform.Viewer) ([]platform.NoteView, error) {
	f.pinnedCalls++
	return f.pinned, nil
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

// Одна реплика ищется среди тех же, что отдаёт тред: у формы ответа, которая
// открывается без перезагрузки, адресат обязан быть тем же самым, что и на
// странице.
func (f *fakeStore) CommentViewByID(_ context.Context, _ platform.Viewer, _, id int64) (platform.CommentView, error) {
	for _, c := range append(append([]platform.CommentView{}, f.thread...), f.flat...) {
		if c.ID == id {
			return c, nil
		}
	}
	return platform.CommentView{}, platform.ErrNotFound
}

// Реакции: ключ 0 — сама заметка, остальные — комментарии.
func (f *fakeStore) NoteReactions(_ context.Context, viewerID, _ int64) (map[int64][]platform.Reaction, error) {
	f.reactViewer = viewerID
	return f.reactions, nil
}

func (f *fakeStore) Flat(_ context.Context, _ platform.Viewer, _ int64, offset, _ int) ([]platform.CommentView, error) {
	f.flatUsed, f.flatOffset = true, offset
	return f.flat, nil
}

// Граница добора: по умолчанию фейк считает её по треду — так она совпадает с
// показанным, и большинству тестов до неё дела нет. Настоящее ядро берёт
// максимум по каждой полосе У ВСЕЙ ЗАМЕТКИ, и там, где проверяется именно это
// (линейный вид показывает окно, а не тред), тест ставит freshBound.
func (f *fakeStore) ThreadFreshAfter(_ context.Context, _ int64) (platform.FreshAfter, error) {
	if f.freshErr != nil {
		return platform.FreshAfter{}, f.freshErr
	}
	if f.freshBound != nil {
		return *f.freshBound, nil
	}
	var a platform.FreshAfter
	for _, c := range append(append([]platform.CommentView{}, f.thread...), f.flat...) {
		a.Seen(c.ID)
	}
	return a, nil
}

func (f *fakeStore) CommentsSince(_ context.Context, _ platform.Viewer, noteID int64, after platform.FreshAfter, _ int) ([]platform.CommentView, error) {
	f.freshNoteID, f.freshAfter = noteID, after
	return f.fresh, nil
}

// Переезды. Пустая граница у фейка означает то же, что у ядра: страница
// переездов не носит — и большинству тестов до них дела нет.
func (f *fakeStore) CommentsMoved(_ context.Context, _ platform.Viewer, noteID int64, after platform.MovedAfter, _ int) ([]platform.CommentView, platform.MovedAfter, error) {
	f.freshNoteID, f.movedAfter = noteID, after
	if f.movedErr != nil {
		return nil, after, f.movedErr
	}
	if !after.On() {
		return nil, after, nil // как ядро: пустая граница — переездов не носим
	}
	next := after
	if f.movedNext.On() {
		next = f.movedNext
	}
	return f.moved, next, nil
}

func (f *fakeStore) SitemapNotes(_ context.Context, _, _ int) ([]platform.SitemapNote, error) {
	return f.sitemap, nil
}

func (f *fakeStore) NotesSince(_ context.Context, _ platform.Viewer, after time.Time, _ int64, _ int) ([]platform.NoteView, error) {
	f.freshSince = after
	return f.freshNotes, nil
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestServer(t *testing.T, st Store, cfg Config) http.Handler {
	t.Helper()
	return newFullServer(t, st, newFakeAuth(), nil, nil, nil, cfg)
}

// newFullServer — сервер со всеми зависимостями. nil у site означает «вход по
// анкете недоступен» (рабочее состояние площадки после смерти НГС), nil у mod —
// «модерации нет»: страницы /mod не существует, кнопок под репликами тоже.
func newFullServer(t *testing.T, st Store, auth Auth, wr Writer, mod Moderator, site Site, cfg Config) http.Handler {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://127.0.0.1"
	}
	cfg.Log = quietLog()
	srv := New(cfg, st, auth, wr, mod, site)
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
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, site, Config{})

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
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, site, Config{})

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
	h := newFullServer(t, &fakeStore{}, newFakeAuth(), nil, nil, &fakeSite{}, Config{})
	r := httptest.NewRequest("POST", "/login", strings.NewReader("profile=1493279"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", got)
	}
}

func TestLoginTellsWhenProfileIsMissing(t *testing.T) {
	h := newFullServer(t, &fakeStore{}, newFakeAuth(), nil, nil, &fakeSite{missing: true}, Config{})
	w := do(h, post(t, "/login", url.Values{"profile": {"999999"}}))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "нет") {
		t.Fatalf("несуществующая анкета: код %d", w.Code)
	}
}

// Приглашение — третий путь и единственный, переживающий смерть НГС.
func TestInviteLetsIn(t *testing.T) {
	auth := newFakeAuth()
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, nil, Config{})
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
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, site, Config{})

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
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, site, Config{})

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
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, site, Config{})

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
	return newFullServer(t, &fakeStore{}, auth, nil, nil, nil, Config{}), auth, token
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
		"kind": {platform.ConsentProcessing}, "version": {consentVersion(t, platform.ConsentProcessing)},
	}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("после первого согласия: код %d", w.Code)
	}
	if !strings.Contains(do(h, as(guest(t, "GET", "/consent"), token)).Body.String(), "Шаг 2 из 2") {
		t.Fatal("второй экран согласия не показан")
	}

	w = do(h, postAs(t, "/consent", url.Values{
		"kind": {platform.ConsentDistribution}, "version": {consentVersion(t, platform.ConsentDistribution)},
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
		"kind": {platform.ConsentDistribution}, "version": {consentVersion(t, platform.ConsentDistribution)},
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

func grantBoth(t *testing.T, auth *fakeAuth, _ context.Context) {
	t.Helper()
	grantConsents(t, auth, testProfileID)
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

// «Как в системе» в наборе нет и не будет: это не выбор, а его отсутствие, и
// кнопка, после нажатия которой ничего не выбрано, объясняется дольше, чем
// стоит. Идентификатор при этом не просто ключ — он едет в куку и в класс
// кнопки (.tbtn.t-<id>), поэтому только латиница и дефис.
func TestThemeIDsAreSafeAndNamesUnique(t *testing.T) {
	ids, names := map[string]bool{}, map[string]bool{}
	for _, th := range themes {
		if th.ID == "" {
			t.Errorf("в наборе тема без идентификатора: %+v", th)
		}
		if !regexp.MustCompile(`^[a-z][a-z-]*$`).MatchString(th.ID) {
			t.Errorf("идентификатор %q не годится в класс и куку", th.ID)
		}
		if ids[th.ID] {
			t.Errorf("два раза идентификатор %q", th.ID)
		}
		if names[th.Name] {
			t.Errorf("две темы с именем %q", th.Name)
		}
		ids[th.ID], names[th.Name] = true, true
	}
}

var tokenRe = regexp.MustCompile(`--([a-z-]+)\s*:\s*([^;]+);`)

func tokens(block string) map[string]string {
	out := map[string]string{}
	for _, m := range tokenRe.FindAllStringSubmatch(block, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

// palette — действующие токены темы: базовый :root плюс её собственный блок.
// Склейка обязательна, потому что блок темы объявляет только ОТЛИЧИЯ, а
// «Светлая» и есть базовый :root — своего блока у неё нет вовсе.
func palette(t *testing.T, css, id string) map[string]string {
	t.Helper()
	out := tokens(cssRule(t, css, ":root {"))
	if head := themeBlock(id); strings.Contains(css, head) {
		for k, v := range tokens(cssRule(t, css, head)) {
			out[k] = v
		}
	}
	return out
}

func themeBlock(id string) string { return `:root[data-theme="` + id + `"] {` }

// Кнопка обязана показывать то, чего не показывает другая, и сравнивать для
// этого надо ПАЛИТРЫ, а не имена. Правило оплачено сокращением набора с четырёх
// тем до двух: «Классика» и «Светлая» отдавали одни и те же токены НГС, и
// нажатие ничем не отличалось от нажатия соседней кнопки (экран владельца,
// 18.08.2026 — «не понял, чем Классика отличается от светлой»).
func TestThemePalettesAreDistinct(t *testing.T) {
	css := cssText(t)
	seen := map[string]string{}
	for _, th := range themes {
		if th.ID != "classic" && !strings.Contains(css, themeBlock(th.ID)) {
			t.Errorf("у темы «%s» нет своего блока токенов: кнопка есть, показывать нечего", th.Name)
			continue
		}
		p := palette(t, css, th.ID)
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var fp strings.Builder
		for _, k := range keys {
			fp.WriteString(k + "=" + p[k] + ";")
		}
		if other, ok := seen[fp.String()]; ok {
			t.Errorf("«%s» и «%s» — одна и та же палитра: два имени на одно оформление", other, th.Name)
		}
		seen[fp.String()] = th.Name
	}
}

// hexLum — светлота по WCAG. Нужна ровно для одного вопроса — тёмная палитра
// или светлая, — и потому понимает только #rgb/#rrggbb: словами (red, pink) в
// наборе заданы лишь цвета старой разметки, а карточка всегда шестнадцатеричная.
func hexLum(t *testing.T, v string) float64 {
	t.Helper()
	h := strings.TrimPrefix(v, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	if len(h) != 6 {
		t.Fatalf("цвет %q не шестнадцатеричный — светлоту по нему не посчитать", v)
	}
	var lin [3]float64
	for i := 0; i < 3; i++ {
		var b int
		if _, err := fmt.Sscanf(h[i*2:i*2+2], "%x", &b); err != nil {
			t.Fatalf("цвет %q не разобрался: %v", v, err)
		}
		c := float64(b) / 255
		if c <= 0.03928 {
			lin[i] = c / 12.92
		} else {
			lin[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*lin[0] + 0.7152*lin[1] + 0.0722*lin[2]
}

// В базовом :root цвета старой разметки стоят СЛОВАМИ автора (blue, purple), а
// подсветка анкора почти белая: всё это рассчитано на белый лист. Тема
// наследует их от голого :root, а не от «Тёмной», — значит палитра с тёмной
// карточкой обязана назвать свои, иначе [color=blue] пропадает в фоне, а приход
// по ссылке на комментарий засвечивает реплику целиком. Проверка общая, а не
// «не забыть в графите»: следующую тёмную тему заведут без этого разговора.
func TestDarkPalettesRestateWhatBaseWroteForWhite(t *testing.T) {
	css := cssText(t)
	for _, th := range themes {
		p := palette(t, css, th.ID)
		if hexLum(t, p["card"]) > 0.5 {
			continue
		}
		own := tokens(cssRule(t, css, themeBlock(th.ID)))
		for _, k := range []string{"bb-red", "bb-green", "bb-blue", "bb-purple", "bb-orange", "bb-pink", "hl"} {
			if own[k] == "" {
				t.Errorf("тёмная «%s» не объявила --%s: достанется значение для белого листа", th.Name, k)
			}
		}
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
}

// Индексация ОТКРЫТА с 23.08.2026 (решение владельца, вместе с новыми
// редакциями согласий — прежние обещали обратное), а личное и служебное закрыто
// по-прежнему. Заголовком, а не только robots.txt: robots.txt соблюдают не все,
// а «Моя страница» в чужом кэше — это уже не вопрос вкуса.
func TestRobotsHeaderClosesOnlyPrivatePages(t *testing.T) {
	h := openServer(t, &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()})
	for _, open := range []string{"/", "/n/312811", "/help", "/consents", "/privacy"} {
		if got := do(h, guest(t, "GET", open)).Header().Get("X-Robots-Tag"); got != "" {
			t.Errorf("%s закрыта от поисковиков заголовком %q", open, got)
		}
	}
	for _, closed := range []string{"/me", "/events", "/login", "/mod", "/n/312811/fresh"} {
		if got := do(h, guest(t, "GET", closed)).Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
			t.Errorf("%s открыта поисковикам: заголовок %q", closed, got)
		}
	}
}

// robots.txt собирается из ТОГО ЖЕ списка, что и заголовок: разъехавшись, они
// дали бы страницу, закрытую в одном месте и открытую в другом.
func TestRobotsFileMatchesTheHeader(t *testing.T) {
	h := openServer(t, &fakeStore{})
	body := do(h, guest(t, "GET", "/robots.txt")).Body.String()
	if !strings.Contains(body, "Allow: /") {
		t.Errorf("robots.txt не открывает площадку:\n%s", body)
	}
	for _, root := range privateRoots {
		if !strings.Contains(body, "Disallow: "+root+"\n") {
			t.Errorf("robots.txt не закрывает %s:\n%s", root, body)
		}
	}
	if strings.Contains(body, "Disallow: /\n") {
		t.Errorf("robots.txt по-прежнему закрывает всё:\n%s", body)
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

	// Смайлы лежат подкаталогом, и подкаталог виден в адресе: маршрут обязан
	// принимать путь со слэшем, иначе картинки молча отдают 404.
	sm := assetURL("smile/popcorn.gif")
	if sm == "" || !strings.HasPrefix(sm, "/assets/smile/") {
		t.Fatalf("адрес смайла %q", sm)
	}
	if w := do(h, httptest.NewRequest("GET", sm, nil)); w.Code != http.StatusOK ||
		w.Header().Get("Content-Type") != "image/gif" {
		t.Errorf("смайл: код %d, тип %q", w.Code, w.Header().Get("Content-Type"))
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

// Документ согласия называет площадку ТЕМ ЖЕ именем, что и шапка страницы.
// Имя площадки попадает в текст согласия и (когда дойдёт до Ш9) в уведомление
// РКН, поэтому расхождение здесь — не опечатка: человек подписывает документ о
// площадке, названия которой не видит нигде. Опубликованная редакция при этом
// неизменяема, так что смена имени — это всегда новая версия документа.
func TestConsentDocsCallThePlatformByItsName(t *testing.T) {
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, d := range docs {
		if strings.Contains(d.Body, "«"+SiteName+"»") {
			named = true
		}
		if strings.Contains(d.Body, "площадка «Заметки»") || strings.Contains(d.Body, "площадке «Заметки»") {
			t.Errorf("%s v%d называет площадку старым именем", d.Kind, d.Version)
		}
	}
	if !named {
		t.Errorf("ни один действующий документ не называет площадку «%s»", SiteName)
	}
}

// ---------------------------------------------------------------- карта сайта

// Карта — единственная страница, написанная не для человека. Понадобилась она
// вместе со снятием запрета индексации: своим ходом робот дошёл бы до заметок
// только через постраничку ленты, а это пять с лишним тысяч страниц.
func TestSitemapListsNotesInChunks(t *testing.T) {
	st := &fakeStore{total: platform.SitemapLimit + 1, sitemap: []platform.SitemapNote{
		{ID: 312811, Changed: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)},
	}}
	h := newTestServer(t, st, Config{BaseURL: "https://t3h.ru"})

	index := do(h, guest(t, "GET", "/sitemap.xml"))
	if index.Code != http.StatusOK {
		t.Fatalf("оглавление карты ответило %d", index.Code)
	}
	// Заметок на файл больше, чем помещается, — значит файлов ровно два.
	for _, want := range []string{"https://t3h.ru/sitemap/1.xml", "https://t3h.ru/sitemap/2.xml"} {
		if !strings.Contains(index.Body.String(), want) {
			t.Errorf("в оглавлении нет %s:\n%s", want, index.Body.String())
		}
	}
	if strings.Contains(index.Body.String(), "sitemap/3.xml") {
		t.Error("в оглавлении лишний файл")
	}

	page := do(h, guest(t, "GET", "/sitemap/1.xml"))
	body := page.Body.String()
	if !strings.Contains(body, "<loc>https://t3h.ru/n/312811</loc>") {
		t.Errorf("в карте нет адреса заметки:\n%s", body)
	}
	// «Менялось» — время последней реплики: тред живёт дольше заметки, и робот,
	// судящий по дате публикации, к разговору второй раз не придёт.
	if !strings.Contains(body, "<lastmod>2026-08-23T12:00:00Z</lastmod>") {
		t.Errorf("в карте нет времени последней правки:\n%s", body)
	}
	// Бумаги и справка стоят в первом файле: из ленты на них ведёт только
	// подвал, а найтись они обязаны.
	if !strings.Contains(body, "<loc>https://t3h.ru/help</loc>") {
		t.Errorf("в карте нет справки:\n%s", body)
	}
	if second := do(h, guest(t, "GET", "/sitemap/2.xml")).Body.String(); strings.Contains(second, "/help") {
		t.Error("бумаги повторяются в каждом файле карты")
	}
}

func TestSitemapRejectsBadPage(t *testing.T) {
	h := openServer(t, &fakeStore{})
	for _, q := range []string{"/sitemap/0.xml", "/sitemap/99999.xml", "/sitemap/абв.xml", "/sitemap/1"} {
		if got := do(h, guest(t, "GET", q)).Code; got != http.StatusNotFound {
			t.Errorf("%s принят с кодом %d", q, got)
		}
	}
}

// У одной заметки адресов несколько — дерево, линейный вид, его страницы,
// раскрытая коробка реакций, — и показывают они один и тот же разговор.
// Каноническим объявляется ДЕРЕВО: линейный вид раскладывает те же реплики
// иначе, а дерево среди них единственное полное. Без этого поисковик выбирает
// главный адрес сам и выбирает обычно не тот.
func TestNotePageNamesItsCanonicalAddress(t *testing.T) {
	st := &fakeStore{note: sampleNote(), thread: sampleThread(), flat: sampleThread()}
	h := newTestServer(t, st, Config{BaseURL: "https://t3h.ru"})

	want := `<link rel="canonical" href="https://t3h.ru/n/312811">`
	for _, target := range []string{"/n/312811", "/n/312811?view=linear", "/n/312811?react=3"} {
		if body := do(h, guest(t, "GET", target)).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%s не называет канонический адрес:\n%s", target, body)
		}
	}
	// У ленты страницы РАЗНЫЕ по содержанию, и сводить их к первой значило бы
	// спрятать от поиска весь архив, кроме двадцати свежих записей.
	if feed := do(h, guest(t, "GET", "/?page=2")).Body.String(); strings.Contains(feed, "rel=\"canonical\"") {
		t.Error("страница ленты объявлена копией первой")
	}
}
