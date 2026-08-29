package web

// Песочница (эпик «народ») глазами читателя: значок, отсутствие формы и
// объяснение вместо неё.

import (
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// stageStore — та же заметка, но помеченная песочницей.
func stageStore() *fakeStore {
	n := sampleNote()
	n.ID = platform.NativeIDBase + 5
	n.Stage = true
	return &fakeStore{total: 1, notes: []platform.NoteView{n}, note: n, thread: sampleThread()}
}

func stagePaths(st *fakeStore) []string {
	return []string{"/", "/n/" + itoa64(st.note.ID)}
}

// Читать песочницу может кто угодно: она в ленте, у неё своя страница и свой
// значок. Закрыта в ней только запись.
func TestStageIsReadableByEveryone(t *testing.T) {
	st := stageStore()
	h := newTestServer(t, st, Config{})
	for _, path := range stagePaths(st) {
		w := do(h, guest(t, "GET", path))
		if w.Code != 200 {
			t.Fatalf("%s: код %d", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), `class="orig"`) {
			t.Errorf("%s: у песочницы нет метки происхождения", path)
		}
	}
}

// Формы ответа в песочнице нет ни у гостя, ни у участника — и на её месте стоит
// ОБЪЯСНЕНИЕ. Молчащее место под тредом читается поломкой, а гостю «войдите,
// чтобы ответить» было бы неправдой: войдя, он всё равно не сможет.
func TestStageExplainsWhyThereIsNoForm(t *testing.T) {
	st := stageStore()
	path := "/n/" + itoa64(st.note.ID)

	h := newTestServer(t, st, Config{})
	body := do(h, guest(t, "GET", path)).Body.String()
	if strings.Contains(body, `class="wform"`) {
		t.Error("гостю в песочнице показали форму ответа")
	}
	if !strings.Contains(body, "жители") || !strings.Contains(body, "/help#narod") {
		t.Errorf("гостю не объяснили, почему формы нет:\n%s", tailOf(body))
	}

	hm, _, token := writeServer(t, st)
	member := do(hm, as(guest(t, "GET", path), token)).Body.String()
	if strings.Contains(member, `class="wform"`) {
		t.Error("участнику в песочнице показали форму ответа")
	}
	if !strings.Contains(member, "/help#narod") {
		t.Error("участнику не объяснили, почему формы нет")
	}
}

// В ленте у песочницы нет и ссылки «Добавить комментарий»: она вела бы к
// странице, на которой ответить нельзя.
func TestStageHasNoAddCommentInFeed(t *testing.T) {
	h, _, token := writeServer(t, stageStore())
	feed := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	if strings.Contains(feed, "Добавить комментарий") {
		t.Error("в ленте у песочницы стоит «Добавить комментарий»")
	}
}

// А обычная заметка от этого не пострадала: у участника форма на месте.
func TestOrdinaryNoteStillHasTheForm(t *testing.T) {
	h, _, token := writeServer(t, noteStore())
	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if !strings.Contains(body, `action="/n/312811/reply"`) {
		t.Error("у обычной заметки пропала форма ответа")
	}
	if strings.Contains(body, "/help#narod") {
		t.Error("обычная заметка объявила себя песочницей")
	}
}

// tailOf — хвост страницы для сообщения об ошибке: целиком она слишком велика.
func tailOf(s string) string {
	if len(s) > 1200 {
		return s[len(s)-1200:]
	}
	return s
}
