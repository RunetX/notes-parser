package web

// Проверки живого канала (эпик F, F3).
//
// Главная из них — TestLiveBypassesSemaphore: живой канал безопасен ровно
// постольку, поскольку он НЕ занимает общий слот морды. Ошибись здесь — и
// десяток открытых вкладок остановит площадку целиком, а заметить это можно
// будет только в бою.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// fakeLive — шина, умеющая поток. Отдельно от fakeEvents намеренно: так
// проверяется и обратное — без этой способности канала просто нет.
type fakeLive struct {
	fakeEvents
	events []platform.LiveEvent
	pokes  []platform.Poke
	last   int64
}

func (f *fakeLive) LiveSince(_ context.Context, after int64, _ int) ([]platform.LiveEvent, error) {
	var out []platform.LiveEvent
	for _, e := range f.events {
		if e.ID > after {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeLive) PokesSince(_ context.Context, after int64, _ int) ([]platform.Poke, error) {
	var out []platform.Poke
	for _, p := range f.pokes {
		if p.EventID > after {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeLive) LastEventID(context.Context) (int64, error) { return f.last, nil }

// liveRequest — запрос к потоку, который сам закончится: обработчик живёт до
// отмены контекста, и без неё тест повис бы.
func liveRequest(t *testing.T, token, query string, life time.Duration) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), life)
	t.Cleanup(cancel)
	return as(guest(t, "GET", "/live"+query).WithContext(ctx), token)
}

// Гостю потока нет: не из-за прав (читать может каждый), а из-за арифметики —
// так число долгоживущих соединений ограничено числом участников.
func TestLiveRefusesGuest(t *testing.T) {
	h, _ := busServer(t, &fakeLive{})
	if w := do(h, guest(t, "GET", "/live")); w.Code != http.StatusNotFound {
		t.Fatalf("гостю поток ответил %d, ожидалось 404", w.Code)
	}
}

// Шина без способности отдавать поток — рабочее состояние, а не поломка:
// страница просто не дописывается сама.
func TestLiveAbsentWithoutCapability(t *testing.T) {
	h, token := busServer(t, &fakeEvents{})
	if w := do(h, as(guest(t, "GET", "/live"), token)); w.Code != http.StatusNotFound {
		t.Fatalf("без способности поток ответил %d, ожидалось 404", w.Code)
	}
}

func TestLiveStreamHeaders(t *testing.T) {
	h, token := busServer(t, &fakeLive{})
	w := do(h, liveRequest(t, token, "?note=312811", 30*time.Millisecond))

	if w.Code != http.StatusOK {
		t.Fatalf("поток ответил %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type %q — это не поток событий", ct)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("поток разрешено кэшировать")
	}
	// Заголовок для прокси, буферизующих по умолчанию: без него сигналы
	// приезжают пачкой, и «живой» страница не становится.
	if w.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("буферизация прокси не отключена")
	}
}

// Сигнал доходит до подписчика темы и несёт ТОЛЬКО ссылку: ни текста, ни имени.
// Второе проверяется здесь же — тем, что в потоке нет ничего, кроме двух полей.
func TestLiveSignalCarriesNoText(t *testing.T) {
	src := &fakeLive{events: []platform.LiveEvent{
		{ID: 7, Kind: platform.EventComment, NoteID: 312811, CommentID: 99},
	}}
	srv := serverOf(t, src)
	token := tokenOf(t, srv)

	// Хаб в тестах не крутится (его запускает Run), поэтому такт делаем руками —
	// проверяется разводка по темам, а не тикер.
	go func() {
		time.Sleep(10 * time.Millisecond)
		srv.hub.tick(context.Background())
	}()

	w := do(srvRoutes(srv), liveRequest(t, token, "?note=312811", 120*time.Millisecond))
	body := w.Body.String()
	if !strings.Contains(body, `"kind":"comment"`) || !strings.Contains(body, `"note":312811`) {
		t.Fatalf("сигнал не дошёл:\n%s", body)
	}
	if strings.Contains(body, "99") && !strings.Contains(body, "312811") {
		t.Error("в сигнале оказался id реплики")
	}
}

// Потолок на человека: две вкладки живут, третья получает честный отказ и
// обходится обычной страницей.
func TestLiveCapsPerUser(t *testing.T) {
	srv := serverOf(t, &fakeLive{})
	h := srvRoutes(srv)
	token := tokenOf(t, srv)

	for range maxLivePerUser {
		r := liveRequest(t, token, "", 2*time.Second)
		go do(h, r)
	}
	waitFor(t, func() bool { return srv.hub.count() == maxLivePerUser })

	w := do(h, liveRequest(t, token, "", 50*time.Millisecond))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("третий поток принят с кодом %d, ожидалось 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("отказ без Retry-After: клиенту не сказано, когда возвращаться")
	}
}

// САМАЯ ВАЖНАЯ проверка эпика. Живой канал идёт мимо семафора морды — иначе
// десяток открытых вкладок занял бы все двенадцать слотов, и площадка
// перестала бы отдавать страницы вовсе.
func TestLiveBypassesSemaphore(t *testing.T) {
	srv := serverOf(t, &fakeLive{})
	// Занимаем ВСЕ слоты: обычная страница с этого момента отвечает «занято».
	for range maxInFlight {
		srv.guard.sem <- struct{}{}
	}
	reached := false
	h := srv.withGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	if w := do(h, guest(t, "GET", "/n/312811")); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("обычная страница при занятом семафоре ответила %d, ожидалось 503", w.Code)
	}
	if reached {
		t.Fatal("обычная страница прошла сквозь занятый семафор")
	}
	if w := do(h, guest(t, "GET", "/live?note=1")); !reached || w.Code == http.StatusServiceUnavailable {
		t.Errorf("живой канал не прошёл мимо семафора: код %d, обработчик достигнут %v", w.Code, reached)
	}
}

// Частоту при этом канал платит наравне со всеми: мимо семафора — не значит
// мимо потолков вообще.
func TestLiveStillPaysRate(t *testing.T) {
	if costOf(httptest.NewRequest("GET", "/live?note=1", nil)) != costPage {
		t.Error("живой канал не списывает цену запроса — потолок частоты его не считает")
	}
}

// ---------------------------------------------------------------- помощники

func serverOf(t *testing.T, ev Events) *Server {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()},
		&fakeStore{}, auth, nil, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetEvents(ev)
	return srv
}

func srvRoutes(s *Server) http.Handler { return s.routes() }

func tokenOf(t *testing.T, s *Server) string {
	t.Helper()
	token, _, err := s.auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// count — сколько слушателей держит хаб. Только для тестов: в бою это число
// никого не интересует, пока не упирается в потолок.
func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("не дождались состояния")
}
