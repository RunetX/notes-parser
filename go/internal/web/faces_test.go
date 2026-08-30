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

// Полоса ЛИСТАЕТСЯ, а полосы прокрутки не видно (уточнение владельца
// 30.08.2026: «явно горизонтальный скролл не должен отображаться, но сама
// возможность пролистывания на десктопе и в мобильной версии должна остаться»).
// Одно без другого не работает: убери прокрутку — и тридцать лиц превратятся в
// восемь, покажи полосу — и она разрежет ряд пополам.
func TestМордолентаЛистаетсяБезПолосыПрокрутки(t *testing.T) {
	css := cssText(t)
	rule := cssRule(t, css, ".faces {")
	if !strings.Contains(rule, "overflow-x: auto") {
		t.Errorf("ряд не листается вовсе: %s", rule)
	}
	if strings.Contains(rule, "flex-wrap") {
		t.Errorf("полоса переносится на второй ряд: %s", rule)
	}
	// Полосу прокрутки прячут ТРИ разных свойства: договориться о ней браузеры
	// так и не смогли, и забытое означает видимую полосу у части читателей.
	for _, want := range []string{"scrollbar-width: none", "-ms-overflow-style: none"} {
		if !strings.Contains(rule, want) {
			t.Errorf("в правиле .faces нет %q: %s", want, rule)
		}
	}
	if !strings.Contains(css, ".faces::-webkit-scrollbar") {
		t.Error("в стилях нет правила для вебкитовской полосы прокрутки")
	}
}

// Стрелки листания рисует СКРИПТ, а не сервер: без JS ряд листается пальцем и
// тачпадом, а мёртвых кнопок на странице не бывает. Отсюда две проверки —
// разметка их не печатает, а в стилях у них есть [hidden]: display: flex
// сильнее правила из таблицы браузера, и без явного правила спрятанная стрелка
// осталась бы на виду (та же грабля, что со свёрнутой веткой в треде).
func TestСтрелкиМордолентыРисуетСкрипт(t *testing.T) {
	body := do(openServer(t, facesStore()), guest(t, "GET", "/")).Body.String()
	if strings.Contains(body, "tnav") {
		t.Error("сервер напечатал стрелки листания: без JS они окажутся мёртвыми")
	}
	if !strings.Contains(cssText(t), ".tnav[hidden]") {
		t.Error("у стрелок нет правила [hidden]: спрятанная останется на виду")
	}
}

// Полоса стоит на ВСЕХ страницах чтения (просьба владельца 30.08.2026: «на
// странице заметки, профиля и справки тоже должна отображаться»). На НГС
// мордолента — часть раздела, а не украшение начала ленты, и вторая страница
// ленты здесь ничем не отличается от первой.
//
// А страницы, где человек ДЕЛАЕТ, а не читает, её не получают: там лица соседей
// не помощь, а помеха, и лишний поход в базу заодно.
func TestМордолентаНаВсехСтраницахЧтения(t *testing.T) {
	st := facesStore()
	st.profile.Persona = true // страница жителя открыта гостю
	h := openServer(t, st)

	for _, page := range []string{"/", "/?page=2", "/n/312811", profilePath(), "/help"} {
		w := do(h, guest(t, "GET", page))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: код %d", page, w.Code)
		}
		if !strings.Contains(w.Body.String(), `class="faces"`) {
			t.Errorf("на %s нет мордоленты", page)
		}
	}
	for _, page := range []string{"/login", "/nosuchpage"} {
		if strings.Contains(do(h, guest(t, "GET", page)).Body.String(), `class="faces"`) {
			t.Errorf("полоса заехала на %s — это страница действия, а не чтения", page)
		}
	}
}

// Полоса читается из кэша: она едет теперь в каждую страницу чтения, а меняется
// от силы раз в минуту. Тот же приём, что у счётчика длины ленты.
func TestМордолентаЧитаетсяИзКэша(t *testing.T) {
	st := facesStore()
	h := openServer(t, st)

	for i := 0; i < 3; i++ {
		do(h, guest(t, "GET", "/"))
	}
	if st.facesCalls != 1 {
		t.Errorf("жителей спросили %d раза вместо одного: кэш не работает", st.facesCalls)
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
