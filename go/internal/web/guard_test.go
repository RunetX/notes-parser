package web

// Проверки потолков наплыва. Каждая называет один факт: что именно площадка
// обязана сделать с потоком запросов, чтобы на том же ядре не встало зеркало.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// from — запрос от названного адреса. Адрес приходит заголовком, как из-за
// Caddy: своего сокета у морды в бою нет вовсе.
func from(t *testing.T, method, target, ip string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("X-Forwarded-For", ip)
	return r
}

// hit — запрос к серверу, ответ кодом.
func hit(h http.Handler, r *http.Request) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// Поток с одного адреса упирается в потолок частоты, а сосед этого не замечает:
// корзина считается по клиенту, иначе один робот закрывал бы площадку всем.
func TestПотолокЧастотыПерсонален(t *testing.T) {
	srv := openServer(t, &fakeStore{total: 1})

	limited := false
	for i := 0; i < 60 && !limited; i++ {
		if hit(srv, from(t, "GET", "/n/312696", "203.0.113.7")) == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("поток одинаковых запросов с одного адреса не упёрся в потолок")
	}
	if code := hit(srv, from(t, "GET", "/n/312696", "198.51.100.4")); code != http.StatusOK {
		t.Fatalf("соседний адрес получил %d, а потолок не его", code)
	}
}

// Отказ по частоте называет срок: и человеку («подождите»), и роботу
// (Retry-After) нужно знать, когда возвращаться.
func TestОтказПоЧастотеНазываетСрок(t *testing.T) {
	srv := openServer(t, &fakeStore{total: 1})

	var w *httptest.ResponseRecorder
	for i := 0; i < 60; i++ {
		w = httptest.NewRecorder()
		srv.ServeHTTP(w, from(t, "GET", "/n/312696", "203.0.113.9"))
		if w.Code == http.StatusTooManyRequests {
			break
		}
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("потолок не сработал: %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("в отказе нет Retry-After")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("отказ отдан как %q, а он обязан быть дешевле страницы", ct)
	}
}

// Статика и проба здоровья корзину не тратят: они отдаются из памяти, и
// считать их значило бы выгонять человека за то, что его браузер забрал стили.
func TestСтатикаНеТратитКорзину(t *testing.T) {
	srv := openServer(t, &fakeStore{total: 1})

	for i := 0; i < 200; i++ {
		if code := hit(srv, from(t, "GET", "/healthz", "203.0.113.11")); code == http.StatusTooManyRequests {
			t.Fatalf("проба здоровья упёрлась в потолок на %d-м запросе", i+1)
		}
	}
	if code := hit(srv, from(t, "GET", "/", "203.0.113.11")); code != http.StatusOK {
		t.Fatalf("лента после двух сотен проб здоровья ответила %d", code)
	}
}

// blockingStore — хранилище, застревающее на чтении ленты: так проверяется
// одновременность, не выдумывая медленный Postgres.
type blockingStore struct {
	fakeStore
	enter   chan struct{}
	release chan struct{}
}

func (b *blockingStore) Feed(_ context.Context, _ platform.Viewer, _, _ int) ([]platform.NoteView, error) {
	b.enter <- struct{}{}
	<-b.release
	return nil, nil
}

// Больше maxInFlight страниц разом площадка не рисует: следующему сразу
// отвечают «занято», а не ставят его в очередь занимать память.
func TestСверхПотолкаОдновременностиОтказ(t *testing.T) {
	st := &blockingStore{
		fakeStore: fakeStore{total: 1},
		enter:     make(chan struct{}),
		release:   make(chan struct{}),
	}
	srv := openServer(t, st)
	defer close(st.release)

	// Каждый со своего адреса: проверяем именно одновременность, а не частоту.
	for i := 0; i < maxInFlight; i++ {
		go func(i int) {
			hit(srv, from(t, "GET", "/", "203.0.113."+strconv.Itoa(100+i)))
		}(i)
	}
	for i := 0; i < maxInFlight; i++ {
		select {
		case <-st.enter:
		case <-time.After(5 * time.Second):
			t.Fatalf("до хранилища дошло только %d запросов из %d", i, maxInFlight)
		}
	}
	if code := hit(srv, from(t, "GET", "/", "198.51.100.77")); code != http.StatusServiceUnavailable {
		t.Fatalf("сверх потолка одновременности ответ %d, ожидался 503", code)
	}
}

// Тело формы ограничено: заметка это текст, а без потолка net/http принял бы
// десять мегабайт на каждый POST.
func TestТелоФормыОграничено(t *testing.T) {
	srv := openServer(t, &fakeStore{total: 1})

	form := url.Values{"theme": {"dark"}, "back": {"/"}, "мусор": {strings.Repeat("ы", maxFormBytes)}}
	r := post(t, "/theme", form)
	r.Header.Set("X-Forwarded-For", "203.0.113.21")
	if code := hit(srv, r); code != http.StatusBadRequest {
		t.Fatalf("форма на %d байт принята с ответом %d", len(form.Encode()), code)
	}
}

// Длина ленты считается по всем заметкам, поэтому спрашивается не чаще раза в
// countTTL: после раскатки архива это 117 тысяч строк на каждый заход.
func TestДлинаЛентыСчитаетсяНеЧаще(t *testing.T) {
	st := &fakeStore{total: 40}
	srv := openServer(t, st)

	for i := 0; i < 3; i++ {
		if code := hit(srv, from(t, "GET", "/", "203.0.113.31")); code != http.StatusOK {
			t.Fatalf("лента ответила %d", code)
		}
	}
	if st.countCalls != 1 {
		t.Errorf("счётчик ленты спрошен %d раза за три захода, ожидался один", st.countCalls)
	}
}

// Корзина наполняется временем: перебравший ждёт и продолжает, а не остаётся
// закрытым до перезапуска.
func TestКорзинаНаполняетсяВременем(t *testing.T) {
	g := newGuard(quietLog())
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return at }
	var key [8]byte

	for i := 0; i < int(bucketBurst/costThread); i++ {
		if !g.allow(key, costThread) {
			t.Fatalf("полная корзина не пустила %d-й запрос", i+1)
		}
	}
	if g.allow(key, costThread) {
		t.Fatal("пустая корзина пустила запрос")
	}

	at = at.Add(2 * time.Second) // +4 токена
	if !g.allow(key, costThread) {
		t.Fatal("через две секунды корзина не наполнилась")
	}
}

// Адрес берётся из ПОСЛЕДНЕЙ записи X-Forwarded-For: первую пишет сам клиент, и
// доверять ей — значит отдать потолок тому, от кого он защищает.
func TestАдресБерётсяИзПоследнейЗаписи(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "127.0.0.1, 203.0.113.55")
	if got := clientIP(r); got != "203.0.113.55" {
		t.Errorf("адрес клиента %q, ожидался 203.0.113.55", got)
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:41234"
	if got := clientIP(r); got != "10.0.0.5" {
		t.Errorf("без заголовка адрес %q, ожидался 10.0.0.5", got)
	}
}

// Дорогое стоит дороже: страница треда отдаёт дерево целиком, вход тянет за
// собой чужой сайт и личное сообщение человеку.
func TestЦенаМаршрутаРастётСРаботой(t *testing.T) {
	cases := []struct {
		method, path string
		want         float64
	}{
		{"GET", "/assets/style.css", 0},
		{"GET", "/", costPage},
		{"GET", "/n/312696", costThread},
		{"POST", "/n/312696/reply", costWrite},
		{"POST", "/login", costLogin},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := costOf(r); got != c.want {
			t.Errorf("%s %s стоит %v, ожидалось %v", c.method, c.path, got, c.want)
		}
	}
}

// Оборванный запрос ошибкой не пишется. Под наплывом человек уходит со страницы
// раньше, чем она дорисуется, и строка ERROR на каждый такой запрос превратила
// бы лог в ту же нагрузку, ради которой стоят потолки.
func TestОборванныйЗапросНеПишетОшибку(t *testing.T) {
	var buf bytes.Buffer
	auth := newFakeAuth()
	auth.fail = context.Canceled
	srv := New(Config{
		BaseURL: "http://127.0.0.1",
		Log:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, &fakeStore{total: 1}, auth, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })

	r := from(t, "GET", "/", "203.0.113.51")
	r.AddCookie(&http.Cookie{Name: sessCookie, Value: "оборванная"})
	if code := hit(srv.routes(), r); code != http.StatusOK {
		t.Fatalf("страница ответила %d", code)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("прерванный запрос записан ошибкой: %s", buf.String())
	}
}
