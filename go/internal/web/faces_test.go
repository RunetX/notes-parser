package web

// Мордолента: полоса лиц жителей над лентой (faces.go).

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func facesStore() *fakeStore {
	st := profileStore()
	// Больше одной страницы: полоса стоит только на первой, и проверить это
	// можно, только когда вторая существует.
	st.total = 25
	st.faces = []platform.PersonaFace{
		{ID: profUserID, Nick: "Механик Сева",
			AvatarURL: "/media/ab/cdef.webp", Gender: platform.GenderMale},
		{ID: profUserID + 1, Nick: "ГердаИзСемейкиАддамс",
			AvatarURL: "/media/12/3456.webp", Gender: platform.GenderFemale},
	}
	return st
}

// Полоса стоит над лентой, показывает лица и ведёт на страницы жителей — и всё
// это ГОСТЮ: страницы живых участников закрыты, страница жителя открыта всем,
// иначе полоса упиралась бы во «войдите» каждым своим лицом.
func TestМордолентаВедётНаСтраницыЖителей(t *testing.T) {
	st := facesStore()
	h := openServer(t, st)
	body := do(h, guest(t, "GET", "/")).Body.String()

	for _, want := range []string{
		`class="faces"`,
		`href="/u/` + itoa64(profUserID) + `"`,
		`href="/u/` + itoa64(profUserID+1) + `"`,
		"Механик Сева",
		"ГердаИзСемейкиАддамс",
		"/media/ab/cdef.webp",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("в мордоленте нет %q", want)
		}
	}
	if st.facesLimit != facesLimit {
		t.Errorf("полосу спросили с потолком %d, ожидался %d", st.facesLimit, facesLimit)
	}
}

// Полоса обязана СКАЗАТЬ, что перед читателем машина. Значок песочницы стои́т у
// заметки, а полоса висит над лентой, где песочницы может не быть вовсе, — и
// другого места объяснить себя у неё нет (правило Ш5з).
func TestМордолентаНазываетСебя(t *testing.T) {
	h := openServer(t, facesStore())
	body := do(h, guest(t, "GET", "/")).Body.String()

	if !strings.Contains(body, "пишет машина") {
		t.Error("полоса не говорит, что реплики жителей пишет машина")
	}
	if !strings.Contains(body, "/help#narod") {
		t.Error("от полосы нет дороги к объяснению в справке")
	}
}

// Только первая страница. На остальных полоса была бы шапкой, которая едет за
// читателем, а листает он как раз затем, чтобы уйти от начала; заодно на
// страницах 2…5933 нет и лишнего запроса.
func TestМордолентаТолькоНаПервойСтранице(t *testing.T) {
	st := facesStore()
	h := openServer(t, st)

	w := do(h, guest(t, "GET", "/?page=2"))
	if w.Code != http.StatusOK {
		t.Fatalf("вторая страница ленты: код %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="faces"`) {
		t.Error("полоса лиц уехала на вторую страницу ленты")
	}
	if st.facesLimit != 0 {
		t.Error("жителей спросили ради страницы, на которой полосы нет")
	}
}

// Пустую полосу не рисуем вовсе: рамка над лентой, отвечающая на вопрос,
// которого никто не задавал, — это шум. Так же выглядит площадка без жителей и
// площадка, где им ещё не поставили фото.
func TestПустаяМордолентаНеРисуется(t *testing.T) {
	h := openServer(t, profileStore()) // жителей нет вовсе
	if body := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(body, `class="faces"`) {
		t.Error("пустая полоса всё-таки нарисовалась")
	}
}

// Отказ полосы не роняет ленту: без лиц лента остаётся лентой, а без ленты лица
// не нужны. То же правило, что у иллюстраций.
func TestОтказМордолентыНеРоняетЛенту(t *testing.T) {
	st := facesStore()
	st.facesErr = errors.New("база молчит")
	h := openServer(t, st)

	w := do(h, guest(t, "GET", "/"))
	if w.Code != http.StatusOK {
		t.Fatalf("лента упала вместе с полосой: код %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `class="faces"`) {
		t.Error("полоса нарисовалась, хотя ядро отказало")
	}
}
