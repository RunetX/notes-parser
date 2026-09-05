package web

// ЗАПИСЬ САМОЙ ПЛОЩАДКИ в колонке автора: знак вместо лица.
//
// С 05.09.2026 недельный выпуск выходит от служебной анкеты площадки, а не от
// человека. У анкеты нет и не может быть фотографии, поэтому без правки на её
// месте вставал силуэт по умолчанию — картинка, которая говорит «человек, у
// которого нет фото». Человека здесь нет вовсе, и вид у такой записи уже был
// придуман для двойника: знак площадки и её имя, оба некликаемые.

import (
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func TestЗаписьПлощадкиПодписанаЗнаком(t *testing.T) {
	st := noteStore()
	st.note.Author = platform.Author{ID: 100000000098, Nick: SiteName, System: true}
	st.notes[0] = st.note

	for _, page := range []string{"/n/312811", "/"} {
		// Смотрим ИМЕННО колонку автора: силуэты есть и у собеседников в треде,
		// и в мордоленте, и по всей странице их искать бессмысленно.
		col := authorColumn(t, do(openServer(t, st), guest(t, "GET", page)).Body.String())
		if !strings.Contains(col, `class="ava sitemark"`) {
			t.Errorf("%s: у записи площадки нет её знака в колонке автора", page)
		}
		if !strings.Contains(col, `class="nick _site">`+SiteName) {
			t.Errorf("%s: в колонке автора не стои́т имя площадки", page)
		}
		// Силуэт — ровно то, ради чего правка: он говорит «человек без фото».
		if strings.Contains(col, `class="ava sil"`) {
			t.Errorf("%s: под записью площадки нарисован силуэт человека", page)
		}
		// Ссылки на страницу участника у неё нет: служебная анкета — не тот, с
		// кем идут знакомиться.
		if strings.Contains(col, `href="/u/100000000098"`) {
			t.Errorf("%s: имя площадки сделано ссылкой на анкету", page)
		}
	}
}

// authorColumn — первая колонка автора страницы.
func authorColumn(t *testing.T, page string) string {
	t.Helper()
	i := strings.Index(page, `<div class="author">`)
	if i < 0 {
		t.Fatal("на странице нет колонки автора вовсе")
	}
	rest := page[i:]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		t.Fatal("колонка автора не закрыта")
	}
	return rest[:j]
}

// Охранный: обычная заметка не должна поехать вместе с ней. Без него тест
// зеленел бы и на шаблоне, который рисует знак площадки всем подряд.
func TestОбычнаяЗаметкаОстаётсяСЛицом(t *testing.T) {
	body := do(openServer(t, noteStore()), guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, `class="ava sitemark"`) {
		t.Error("под обычной заметкой встал знак площадки вместо автора")
	}
}
