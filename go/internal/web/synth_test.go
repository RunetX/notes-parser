package web

// СМЕЖНОЕ обсуждение на странице заметки (эпик «народ»).
//
// Двойник — отдельная заметка со своим тредом, в котором о том же материале
// говорят жители. Читателю от страницы нужно ровно две вещи: с оригинала —
// дорога туда и предупреждение, что за ней машинный разговор; с двойника —
// дорога обратно, к самой заметке и к живым людям.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func synthStore() *fakeStore {
	st := noteStore()
	st.synth = platform.NoteSynth{ID: 100000000031, CommentCount: 34}
	return st
}

// С оригинала — ссылка на смежное обсуждение, со значком песочницы: про машину
// читатель обязан узнать ДО перехода, а не после.
func TestСсылкаНаСмежноеОбсуждение(t *testing.T) {
	st := synthStore()
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	for _, want := range []string{
		`href="/n/100000000031"`,
		"Смежное обсуждение",
		"34",
		`class="synth"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице заметки нет %q", want)
		}
	}
	if st.synthOf != 312811 {
		t.Errorf("двойника спросили у заметки %d, а страница про 312811", st.synthOf)
	}
}

// У двойника своего двойника не бывает — и запроса за ним страница не делает:
// правило ядра, и платить за его проверку попаданием в индекс незачем.
func TestУДвойникаСсылкаВедётОбратно(t *testing.T) {
	st := noteStore()
	st.note.SynthOf = 312811
	st.note.Stage = true
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	if !strings.Contains(body, "Сама заметка и её настоящее обсуждение") {
		t.Error("с двойника нет дороги обратно")
	}
	if st.synthOf != 0 {
		t.Errorf("у двойника спросили двойника (заметка %d)", st.synthOf)
	}
}

// Отказ ядра страницу не роняет: без ссылки заметка остаётся заметкой. То же
// правило, что у иллюстраций и мордоленты.
func TestОтказСмежногоОбсужденияНеРоняетСтраницу(t *testing.T) {
	st := synthStore()
	st.synthErr = errors.New("база молчит")
	w := do(openServer(t, st), guest(t, "GET", "/n/312811"))

	if w.Code != http.StatusOK {
		t.Fatalf("страница упала вместе со ссылкой: код %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "Смежное обсуждение") {
		t.Error("ссылка нарисовалась, хотя ядро отказало")
	}
}

// КНОПКА администратора: завести смежное обсуждение там, где читаешь. Ответ на
// нажатие — переход НА ДВОЙНИКА: администратор шёл смотреть, как жители
// заговорят, и оставлять его на прежней странице значило бы прятать сделанное.
func TestКнопкаЗаводитСмежноеОбсуждение(t *testing.T) {
	h, mod, token := modServerOn(t, noteStore(), platform.RoleAdmin) // двойника ещё нет
	mod.synthID = 100000000031

	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if !strings.Contains(body, `value="synth"`) {
		t.Fatal("администратору не показали кнопку смежного обсуждения")
	}

	form := url.Values{"do": {"synth"}, "note": {"312811"}, "back": {"/n/312811"}}
	w := do(h, postAs(t, "/mod/act", form, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("нажатие дало код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/n/100000000031" {
		t.Errorf("после нажатия увели на %q, а не на двойника", got)
	}
}

// А у заметки, где двойник УЖЕ есть, кнопки нет: она обещала бы завести второй,
// которого не бывает. Место ей занимает ссылка «Смежное обсуждение».
func TestУЗаметкиСДвойникомКнопкиНет(t *testing.T) {
	h, _, token := modServerOn(t, synthStore(), platform.RoleAdmin)
	body := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Contains(body, `value="synth"`) {
		t.Error("кнопка обещает завести второго двойника")
	}
	if !strings.Contains(body, "Смежное обсуждение") {
		t.Error("ссылки на уже заведённое обсуждение нет")
	}
}
