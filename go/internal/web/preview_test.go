//go:build preview

package web

// Стенд «посмотреть глазами».
//
// Поднять морду как команду нельзя: `lovegw web` требует живого Postgres и
// совпадения версии схемы, а на рабочей машине её нет. Заводить ради этого
// второй путь сборки морды (`--fake`) тем более нельзя — он однажды разойдётся
// с настоящим, и разойдётся молча. Зато настоящий routes() уже собирается в
// тестах поверх тест-дублей, и этого довольно: отдаются и страницы, и стили, и
// шрифты, то есть тему видно целиком.
//
// Метка сборки, а не обычный тест: висящий сервер в общем прогоне — это
// прогон, который никогда не кончается.
//
//	cd go && go test ./internal/web -tags preview -run Предпросмотр -timeout 0 -v
//
// Правило: всё, что здесь показано, — тест-дубли. Ни одной боевой строки, ни
// одного адреса боевой базы.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// previewAddr — порт постоянный намеренно: адрес со случайным номером
// приходится вычитывать из лога при каждом запуске, а смотрят сюда по многу
// раз подряд и с открытой вкладкой.
const previewAddr = "127.0.0.1:8791"

func TestПредпросмотр(t *testing.T) {
	st := profileStore()
	st.profile.Comments = 412
	st.profile.Bio = "Слесарь-ремонтник на заводе, руки в мазуте, гараж вместо кабинета."
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: platform.RoleAdmin,
	})
	h := newFullServer(t, st, auth, &fakeWriter{}, newFakeMod(), nil, Config{})

	// Вошедшим считается всякий, кто не попросил обратного: кука сессии
	// HttpOnly, скриптом её не поставить, а вводить руками в отладчике при
	// каждом заходе — это и есть та возня, из-за которой глазами смотрят реже,
	// чем надо. Гостя показывает ?guest=1 — вид у него другой, и посмотреть на
	// него тоже надо.
	signed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.Query().Has("guest") {
			r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
		}
		// ?theme=grimoire — посмотреть другую палитру, не нажимая кнопку:
		// снимок экрана делается одним запуском браузера, и нажать в нём нечем.
		if th := r.URL.Query().Get("theme"); th != "" {
			r.AddCookie(&http.Cookie{Name: themeCookie, Value: th})
		}
		h.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", previewAddr)
	if err != nil {
		t.Fatalf("порт %s занят: %v", previewAddr, err)
	}
	srv := httptest.NewUnstartedServer(signed)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	t.Logf("вошедшим показывается всё; гость — добавить ?guest=1")
	for _, p := range []string{"/", "/n/312811", profilePath(), "/help", "/help/read", "/login", "/new"} {
		t.Logf("%s%s", srv.URL, p)
	}
	t.Log("Ctrl+C, когда насмотритесь")
	for {
		time.Sleep(time.Hour)
	}
}
