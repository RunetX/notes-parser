package love

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(srv.URL, "test-agent", time.Millisecond, nil)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFetchNotesRetriesOn500(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if ua := r.Header.Get("User-Agent"); ua != "test-agent" {
			t.Errorf("User-Agent: %q", ua)
		}
		w.Write(fixture(t, "notes_feed.html"))
	}))
	defer srv.Close()

	notes, err := testClient(t, srv).FetchNotes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("ожидалось 3 запроса (2 ретрая), было %d", calls)
	}
	if len(notes) != 5 {
		t.Errorf("заметок: %d", len(notes))
	}
}

func TestFetchNotes403FailsFast(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := testClient(t, srv).FetchNotes(context.Background())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("ожидался ErrForbidden, получено: %v", err)
	}
	if calls != 1 {
		t.Errorf("403 не должен ретраиться, запросов: %d", calls)
	}
}

// loginSite — фейк сайта с флоу входа (паритет с Python): прогрев /notes/
// (ставит куки DDoS-Guard) → POST проверки учётных данных, который при успехе
// ставит сессионную куку и возвращает result:true.
func loginSite(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/notes/": // прогрев
			http.SetCookie(w, &http.Cookie{Name: "__ddg1_", Value: "guard", Path: "/"})
			w.Write([]byte("<html>ok</html>"))
		case r.URL.Path == "/ajax" && r.URL.Query().Get("request") == "login":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.PostForm.Get("login") != "user@mail.ru" || r.PostForm.Get("password") != "secret" {
				w.Write([]byte(`{"login":{"result":false,"errors":"Неверный пароль"}}`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "ngs_ttq", Value: "SESSION", Path: "/"})
			w.Write([]byte(`{"login":{"result":true,"errors":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// Успешный вход возвращает весь набор кук (прогрев + сессионная).
func TestLoginCapturesAllCookies(t *testing.T) {
	srv := loginSite(t)
	defer srv.Close()

	cookies, err := testClient(t, srv).Login(context.Background(), "user@mail.ru", "secret")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(cookies))
	for _, ck := range cookies {
		names = append(names, ck.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "ngs_ttq") || !strings.Contains(joined, "__ddg1_") {
		t.Errorf("ожидались куки прогрева и сессии, получено: %s", joined)
	}
}

func TestLoginFailureReturnsLoginError(t *testing.T) {
	srv := loginSite(t)
	defer srv.Close()

	_, err := testClient(t, srv).Login(context.Background(), "u", "wrong")
	var le *LoginError
	if !errors.As(err, &le) {
		t.Fatalf("ожидалась LoginError, получено: %v", err)
	}
	if le.Errors != "Неверный пароль" {
		t.Errorf("текст ошибки: %q", le.Errors)
	}
}

// Регрессионная фиксация точных полей формы комментария (паритет с poster.py).
func TestPostCommentForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/notes/comments/301109" {
			t.Errorf("путь: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"noteId":   "301109",
			"comId":    "0",
			"comApiId": "98765",
			"reason":   "",
			"content":  "Привет :)",
		}
		for k, v := range want {
			vals, ok := r.PostForm[k]
			if !ok || vals[0] != v {
				t.Errorf("поле %q: got %v, want %q", k, vals, v)
			}
		}
		if c, err := r.Cookie("sid"); err != nil || c.Value != "abc123" {
			t.Errorf("кука сессии не передана: %v, %v", c, err)
		}
	}))
	defer srv.Close()

	cookies := []*http.Cookie{{Name: "sid", Value: "abc123"}}
	err := testClient(t, srv).PostComment(context.Background(), cookies, "301109", "98765", "Привет :)")
	if err != nil {
		t.Fatal(err)
	}
}

// Регрессионная фиксация точных полей формы заметки (паритет с ryumkin.py).
func TestPostNoteForm(t *testing.T) {
	for _, tc := range []struct {
		anonymous bool
		hideMe    string
	}{{false, "0"}, {true, "1"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/notes/add/" {
				t.Errorf("путь: %s", r.URL.Path)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{
				"action_note[lid]":    "0",
				"action_note[href]":   "",
				"action_note[hideme]": tc.hideMe,
				"action_note[nocom]":  "0",
				"action_note[rules]":  "1",
				"id":                  "",
				"category_note":       "0",
				"letter":              "Текст заметки",
			}
			for k, v := range want {
				vals, ok := r.PostForm[k]
				if !ok || vals[0] != v {
					t.Errorf("anonymous=%v, поле %q: got %v, want %q", tc.anonymous, k, vals, v)
				}
			}
		}))
		err := testClient(t, srv).PostNote(context.Background(), nil, "Текст заметки", tc.anonymous)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}
