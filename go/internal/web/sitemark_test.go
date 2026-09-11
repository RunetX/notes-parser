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

// РЕПЛИКА ПЛОЩАДКИ подписана так же, как её заметка: знак вместо лица и серое
// имя без ссылки.
//
// Разошлись эти два места молча и по понятной причине: до пятничной рубрики
// (11.09.2026) площадка в тредах не говорила вовсе — под её анкетой выходила
// только недельная сводка. С рубрикой она заговорила комментариями, и в колонке
// аватара у неё встал силуэт по умолчанию, то есть картинка «человек, у которого
// нет фото» (жалоба владельца в первый же боевой вечер).
func TestРепликаПлощадкиПодписанаЗнаком(t *testing.T) {
	st := noteStore()
	st.thread = []platform.CommentView{{
		ID: 7, Author: platform.Author{ID: 100000000098, Nick: SiteName, System: true},
		Body: "В каком году это написано?", Depth: 1,
		PublishedAt: st.note.PublishedAt,
	}}
	body := do(openServer(t, st), guest(t, "GET", "/n/312811")).Body.String()

	cava := between(t, body, `<div class="cava">`, "</div>")
	if !strings.Contains(cava, `class="ava sitemark"`) {
		t.Errorf("у реплики площадки нет её знака:\n%s", cava)
	}
	if strings.Contains(cava, `class="ava sil"`) {
		t.Error("под репликой площадки нарисован силуэт человека")
	}
	// Имя стои́т в строке даты, а не в колонке аватара, — печатать его дважды
	// незачем.
	if strings.Contains(cava, SiteName) {
		t.Error("имя площадки напечатано ещё и в колонке аватара")
	}
	// Серое и без ссылки — ровно как в заметке (решение владельца 11.09.2026:
	// «цвет ника у Зазеркалья как в заметке тоже будет серый»).
	if !strings.Contains(body, `class="nick _site">`+SiteName) {
		t.Error("имя площадки в треде подписано не как в заметке")
	}
	if strings.Contains(body, `href="/u/100000000098"`) {
		t.Error("имя площадки в треде сделано ссылкой на анкету")
	}
}

// Охранный к предыдущему: обычный собеседник остаётся с лицом и со ссылкой.
// Без него тест зеленел бы и на шаблоне, рисующем знак площадки всем подряд.
func TestОбычнаяРепликаОстаётсяСЛицом(t *testing.T) {
	body := do(openServer(t, noteStore()), guest(t, "GET", "/n/312811")).Body.String()
	if strings.Contains(body, `class="ava sitemark"`) {
		t.Error("под обычной репликой встал знак площадки")
	}
	if strings.Contains(body, `class="nick _site"`) {
		t.Error("обычный собеседник подписан как площадка")
	}
}

// between — кусок между первым вхождением from и следующим to.
func between(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("на странице нет %q", from)
	}
	rest := s[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("кусок после %q не закрыт", from)
	}
	return rest[:j]
}
