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
		`title="Механик Сева"`,
		`alt="ГердаИзСемейкиАддамс"`,
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

// Вид снят с оригинала, а не придуман: в записанной ленте НГС
// (love/testdata/notes_feed.html) полоса — это section.lv-top-tape ПЕРЕД блоком
// содержимого, внутри один ряд фотографий 100×100 и НИ ОДНОЙ подписи: имя живёт
// в alt и всплывает при наведении (там qtip, у нас title — он работает без JS).
func TestМордолентаБезПодписейИНадКарточкой(t *testing.T) {
	body := do(openServer(t, facesStore()), guest(t, "GET", "/")).Body.String()

	// Полоса стои́т НАД карточкой, а не внутри: на сайте она сосед блока
	// содержимого. Проверяем порядком в разметке — из вёрстки это единственное,
	// что видно тесту.
	band, main := strings.Index(body, `class="faceband"`), strings.Index(body, `<main class="main"`)
	if band < 0 || main < 0 || band > main {
		t.Errorf("полоса не над карточкой: faceband на %d, main на %d", band, main)
	}
	// Подписи текстом нет вовсе — имя только в title и alt.
	tape := body[band:main]
	if strings.Contains(tape, "<span") || strings.Contains(tape, "nick") {
		t.Errorf("в полосе появилась подпись текстом:\n%s", tape)
	}
	// И никакого объявления про машину: подпись под полосой ломала сходство с
	// оригиналом (решение владельца 30.08.2026). Сказано об этом значком
	// песочницы у заметки, на странице жителя и в /help#narod.
	if strings.Contains(tape, "пишет машина") {
		t.Error("под полосой снова висит объявление про жителей")
	}
}

// Горизонтальной прокрутки у полосы нет: на НГС ряд ОБРЕЗАН по ширине
// («visible-part»), а не ездит вбок. Правило живёт в стилях, и проверить его
// можно только там.
func TestМордолентаНеЕздитВбок(t *testing.T) {
	rule := cssRule(t, cssText(t), ".faces {")
	if !strings.Contains(rule, "overflow: hidden") {
		t.Errorf("ряд не обрезан по ширине: %s", rule)
	}
	if strings.Contains(rule, "overflow-x: auto") || strings.Contains(rule, "scroll") {
		t.Errorf("у полосы завелась горизонтальная прокрутка: %s", rule)
	}
	if strings.Contains(rule, "flex-wrap") {
		t.Errorf("полоса переносится на второй ряд: %s", rule)
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
