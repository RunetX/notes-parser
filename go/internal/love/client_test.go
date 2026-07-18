package love

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestLoginSuccessCapturesCookies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("login") != "user@mail.ru" || r.PostForm.Get("password") != "secret" {
			t.Errorf("форма логина: %v", r.PostForm)
		}
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc123", Path: "/"})
		w.Write([]byte(`{"login":{"result":true,"errors":""}}`))
	}))
	defer srv.Close()

	cookies, err := testClient(t, srv).Login(context.Background(), "user@mail.ru", "secret")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range cookies {
		if c.Name == "sid" && c.Value == "abc123" {
			found = true
		}
	}
	if !found {
		t.Errorf("кука сессии не захвачена: %v", cookies)
	}
}

func TestLoginFailureReturnsLoginError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":{"result":false,"errors":"Неверный пароль"}}`))
	}))
	defer srv.Close()

	_, err := testClient(t, srv).Login(context.Background(), "u", "p")
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
