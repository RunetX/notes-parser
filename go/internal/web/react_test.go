package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func reactStore() *fakeStore {
	return &fakeStore{
		note:   sampleNote(),
		thread: sampleThread(),
		reactions: map[int64][]platform.Reaction{
			0: {{Code: "agree", Count: 3}},
			1: {{Code: "flowers", Count: 2, Mine: true}},
		},
	}
}

// Реакции читаются ОДНИМ запросом на страницу — и заметки, и всего треда.
// Запрос на реплику положил бы страницу с девятью сотнями комментариев.
func TestNoteReactionsReadOnce(t *testing.T) {
	st := reactStore()
	h := newTestServer(t, st, Config{})
	body := do(h, guest(t, "GET", "/n/312811")).Body.String()
	if !strings.Contains(body, `class="rxb"`) {
		t.Fatal("реакции не показаны")
	}
	// Гостю — счётчики, но не кнопки: кнопка, отвечающая «сначала войдите», это
	// обещание, которого страница не держит.
	if strings.Contains(body, `action="/n/312811/react"`) {
		t.Error("гостю показана форма реакции")
	}
	if st.reactViewer != 0 {
		t.Errorf("читателем реакций числится %d, а не гость", st.reactViewer)
	}
}

// Участнику те же реакции показываются кнопками — своя отмечена.
func TestReactionsAreButtonsForMember(t *testing.T) {
	h, _, token := writeServer(t, reactStore())

	body := do(h, as(httptest.NewRequest("GET", "/n/312811", nil), token)).Body.String()
	if !strings.Contains(body, `action="/n/312811/react"`) {
		t.Fatal("формы реакции нет")
	}
	if !strings.Contains(body, `class="rxb mine"`) {
		t.Error("своя реакция не отмечена")
	}
	// Выбор шести кнопок не рисуется под каждой репликой — только «+».
	if n := strings.Count(body, `class="rxpick"`); n != 0 {
		t.Errorf("выбиралка раскрыта без спроса (%d)", n)
	}
	if !strings.Contains(body, `class="rxadd"`) {
		t.Error("нет кнопки «поставить реакцию»")
	}
}

// Раскрывается выбор ровно в одном месте — по ?react=<id>, тем же приёмом, что
// и форма ответа. Так страница остаётся лёгкой без всякого скрипта.
func TestReactionPickerOpensForOneTargetOnly(t *testing.T) {
	h, _, token := writeServer(t, reactStore())

	body := do(h, as(httptest.NewRequest("GET", "/n/312811?view=tree&react=2", nil), token)).Body.String()
	if n := strings.Count(body, `class="rxpick"`); n != 1 {
		t.Fatalf("выбиралок на странице %d, ожидалась одна", n)
	}
	for _, code := range platform.ReactionCodes {
		if !strings.Contains(body, `value="`+code+`"`) {
			t.Errorf("в выбиралке нет кнопки %s", code)
		}
	}
}

// Нажатие уходит в ядро как есть и возвращает человека к той же реплике.
func TestReactPostsAndReturnsToComment(t *testing.T) {
	h, wr, token := writeServer(t, reactStore())

	form := url.Values{"comment": {"2"}, "code": {"agree"}}
	w := do(h, postAs(t, "/n/312811/react", form, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if got := w.Header().Get("Location"); !strings.HasSuffix(got, "#c2") {
		t.Errorf("вернули на %q, а не к нажатой реплике", got)
	}
	if wr.reaction.CommentID != 2 || wr.reaction.Code != "agree" || wr.reaction.NoteID != 312811 {
		t.Errorf("в ядро ушло %+v", wr.reaction)
	}
	if wr.reaction.UserID == 0 {
		t.Error("реакция ушла без автора")
	}
}

// Отказ ядра показывается страницей заметки, а не отдельной: человек нажал
// кнопку посреди чтения, уводить его с треда незачем.
func TestReactRefusalKeepsNotePage(t *testing.T) {
	h, wr, token := writeServer(t, reactStore())
	wr.fail = platform.ErrBadReaction

	w := do(h, postAs(t, "/n/312811/react", url.Values{"code": {"нету"}}, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Комментарии") {
		t.Error("вместо страницы заметки показано что-то другое")
	}
}

// Гость нажать не может: путь записи у реакции ровно тот же, что у реплики.
func TestReactRequiresMember(t *testing.T) {
	h := newFullServer(t, reactStore(), newFakeAuth(), &fakeWriter{}, nil, nil, Config{})
	w := do(h, post(t, "/n/312811/react", url.Values{"code": {"agree"}}))
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Errorf("гостю ответили %d", w.Code)
	}
}
