package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://127.0.0.1"
	}
	cfg.Log = quietLog()
	srv := New(cfg, st)
	t.Cleanup(func() { _ = srv.Close() })
	return srv.routes()
}

func openServer(t *testing.T, st Store) http.Handler {
	t.Helper()
	return newTestServer(t, st, Config{PreviewKey: testKey})
}

// guest — обычный посетитель: не вошёл и входить не собирался. Чтение открыто
// всем, поэтому таким запросом проверяется почти всё.
func guest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, target, nil)
}

// signedReq — запрос человека, вошедшего по общему ключу.
func signedReq(t *testing.T, method, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	sum := sha256.Sum256([]byte(testKey))
	r.AddCookie(&http.Cookie{Name: authCookie, Value: hex.EncodeToString(sum[:])})
	return r
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

// Чтение открыто всем — это решение владельца от 18.08.2026, и оно отменило
// прежнее правило «пустой ключ закрывает всё». Незаданный ключ теперь означает
// ровно одно: войти пока некуда, а читать можно.
func TestReadingIsOpenWithoutKey(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := newTestServer(t, st, Config{})
	for _, target := range []string{"/", "/n/312811", "/login", "/healthz", "/robots.txt"} {
		if got := do(h, httptest.NewRequest("GET", target, nil)).Code; got != http.StatusOK {
			t.Errorf("%s: код %d, ожидался 200", target, got)
		}
	}
}

// Без ключа страница входа честно говорит, что войти некуда, и не показывает
// поле, к которому не подходит ни одно значение.
func TestLoginWithoutKeyOffersNoForm(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, Config{})
	body := do(h, httptest.NewRequest("GET", "/login", nil)).Body.String()
	if strings.Contains(body, `action="/login"`) {
		t.Error("форма входа показана, хотя ключ не задан")
	}
}

// Кнопка «Вход» стоит в правом верхнем углу шапки — там же, где на НГС, — и
// ведёт на страницу входа, запомнив, откуда человек пришёл.
func TestHeaderOffersEnterAndRemembersPlace(t *testing.T) {
	h := openServer(t, &fakeStore{note: sampleNote()})
	body := do(h, guest(t, "GET", "/n/312811?view=linear")).Body.String()
	if !strings.Contains(body, `class="acct"`) || !strings.Contains(body, ">Вход<") {
		t.Error("в шапке нет кнопки «Вход»")
	}
	if !strings.Contains(body, `href="/login?to=%2fn%2f312811%3fview%3dlinear"`) {
		t.Errorf("кнопка входа не помнит страницу, с которой нажали:\n%s", body)
	}
}

// У вошедшего на том же месте «Выход», и это форма с POST: выход меняет
// состояние, а GET-ссылку браузер нажимает сам в префетче.
func TestHeaderOffersExitWhenSignedIn(t *testing.T) {
	h := openServer(t, &fakeStore{})
	body := do(h, signedReq(t, "GET", "/")).Body.String()
	if !strings.Contains(body, `action="/logout"`) || !strings.Contains(body, ">Выход<") {
		t.Error("вошедшему не предложен выход")
	}
	if strings.Contains(body, ">Вход<") {
		t.Error("вошедшему всё ещё предлагают войти")
	}
}

func TestLoginAcceptsKeyAndSetsCookie(t *testing.T) {
	h := openServer(t, &fakeStore{})
	form := url.Values{"key": {testKey}, "to": {"/n/312811"}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := do(h, r)

	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/n/312811" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	var got *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == authCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("кука входа не поставлена")
	}
	if !got.HttpOnly {
		t.Error("кука входа должна быть HttpOnly")
	}
	// В куке лежит хеш ключа, а не сам ключ: утечка куки не должна отдавать
	// строку, которую владелец диктует голосом и, скорее всего, где-то повторит.
	if strings.Contains(got.Value, testKey) {
		t.Error("в куке лежит сам ключ")
	}
}

func TestLoginRejectsWrongKey(t *testing.T) {
	h := openServer(t, &fakeStore{})
	form := url.Values{"key": {"не тот"}, "to": {"/"}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := do(h, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("код %d, ожидался 401", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("при неверном ключе кука не ставится")
	}
}

func TestLoginRejectsCrossSitePost(t *testing.T) {
	h := openServer(t, &fakeStore{})
	form := url.Values{"key": {testKey}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Fatalf("код %d, ожидался 403", got)
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	h := openServer(t, &fakeStore{})
	form := url.Values{"back": {"/n/312811"}}
	r := httptest.NewRequest("POST", "/logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	sum := sha256.Sum256([]byte(testKey))
	r.AddCookie(&http.Cookie{Name: authCookie, Value: hex.EncodeToString(sum[:])})
	w := do(h, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/n/312811" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == authCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("кука входа не снята")
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
		form := url.Values{"theme": {"light"}, "back": {back}}
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
	h := newTestServer(t, &fakeStore{}, Config{PreviewKey: testKey, MediaDir: dir})

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
