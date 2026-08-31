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
	"time"

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

// ---------------------------------------------------- как выглядит двойник

// Карточка двойника — жалоба владельца 31.08.2026: «длинная фраза-заглушка и
// единый автор Паноптикум выглядят не очень». Собран стенд, на котором видно
// обе половины правки сразу.
func twinStore() *fakeStore {
	st := noteStore()
	st.note.ID = 100000000030
	st.note.SynthOf = 312811
	st.note.Stage = true
	st.note.Author = platform.Author{ID: 1472546, Nick: "Паноптикум"}
	// Тело двойника нарочно НЕ похоже на настоящее: подпись карточки и заголовок
	// значка песочницы говорят про машину теми же словами, и проверка «служебная
	// фраза не показана» ловила бы их вместо тела.
	st.note.Body = "СЛУЖЕБНОЕ-ТЕЛО-ДВОЙНИКА"
	st.origins = map[int64]platform.SynthOrigin{
		100000000030: {
			ID:             312811,
			Nick:           "Рио",
			PublishedAt:    time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
			PublishedExact: true,
			Body:           "Мне 43, и я впервые задумалась про очередь.",
		},
	}
	st.notes = []platform.NoteView{st.note}
	return st
}

// ГЛАВНОЕ: на странице двойника видно, О ЧЁМ говорят. До правки читатель
// открывал девяносто реплик и служебную фразу, а предмет разговора узнавал
// только переходом на оригинал.
func TestКарточкаДвойникаЦитируетОригинал(t *testing.T) {
	st := twinStore()
	body := do(openServer(t, st), guest(t, "GET", "/n/100000000030")).Body.String()

	for _, want := range []string{
		"Мне 43, и я впервые задумалась про очередь.",
		`class="synthq"`,
		"Рио",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице двойника нет %q", want)
		}
	}
	// Своё тело двойника не показывается вовсе: это одна и та же служебная
	// фраза у всех двойников, и предмета она не называет.
	if strings.Contains(body, "СЛУЖЕБНОЕ-ТЕЛО-ДВОЙНИКА") {
		t.Error("служебное тело двойника всё ещё показано вместо цитаты")
	}
	if len(st.originsAsked) != 1 || st.originsAsked[0] != 100000000030 {
		t.Errorf("оригинал спросили у %v, а страница про двойника 100000000030", st.originsAsked)
	}
}

// ВТОРАЯ половина жалобы: автором двойника стоял администратор, нажавший
// кнопку, — выходило, будто человек написал заметку из служебной фразы.
// Двойник же запись САМОЙ ПЛОЩАДКИ, и в колонке автора стоит её знак.
func TestУДвойникаНетАвтораЧеловека(t *testing.T) {
	body := do(openServer(t, twinStore()), guest(t, "GET", "/n/100000000030")).Body.String()

	if strings.Contains(body, "Паноптикум") {
		t.Error("администратор, нажавший кнопку, показан автором двойника")
	}
	if !strings.Contains(body, `class="ava sitemark"`) {
		t.Error("в колонке автора нет знака площадки")
	}
}

// Та же карточка в ЛЕНТЕ — двойник стои́т в ней наравне с заметками (решение
// владельца 31.08.2026), значит и выглядеть обязан так же, как на своей
// странице: один шаблон, одна цитата, тот же знак вместо лица.
func TestВЛентеДвойникТожеСЦитатой(t *testing.T) {
	body := do(openServer(t, twinStore()), guest(t, "GET", "/")).Body.String()

	for _, want := range []string{"Мне 43, и я впервые задумалась про очередь.", `class="ava sitemark"`} {
		if !strings.Contains(body, want) {
			t.Errorf("в ленте у двойника нет %q", want)
		}
	}
	if strings.Contains(body, "Паноптикум") {
		t.Error("в ленте двойник подписан администратором")
	}
}

// Скрытый оригинал цитаты не даёт (ядро его не отдаёт), и карточка обязана
// пережить это молча: подпись и дорога обратно на месте, цитаты нет.
func TestБезОригиналаКарточкаНеЛомается(t *testing.T) {
	st := twinStore()
	st.origins = nil
	body := do(openServer(t, st), guest(t, "GET", "/n/100000000030")).Body.String()

	if strings.Contains(body, `class="synthq"`) {
		t.Error("нарисована пустая цитата")
	}
	for _, want := range []string{"Смежное обсуждение", "Сама заметка и её настоящее обсуждение"} {
		if !strings.Contains(body, want) {
			t.Errorf("карточка без цитаты потеряла %q", want)
		}
	}
}

// Отказ базы карточку тоже не роняет — то же правило, что у иллюстраций и
// мордоленты: без цитаты двойник остаётся собой, а без страницы цитата не нужна.
func TestОтказЗаОригиналомНеРоняетСтраницу(t *testing.T) {
	st := twinStore()
	st.originsErr = errors.New("база молчит")
	if got := do(openServer(t, st), guest(t, "GET", "/n/100000000030")).Code; got != http.StatusOK {
		t.Errorf("страница двойника при отказе за оригиналом: код %d, ожидался 200", got)
	}
}
