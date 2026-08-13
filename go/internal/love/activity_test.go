package love

import (
	"errors"
	"testing"
	"time"
)

// profilePage собирает страницу мобильной анкеты вокруг JSON профиля — так же,
// как её отдаёт сайт: маркер, объект layout, дальше произвольный хвост JS.
func profilePage(profileJSON string) []byte {
	return []byte(`<html><script>dataFromBlade.user = {"id":null};
dataFromBlade.layout = {"user":{"id":null,"last_activity":null},"header_content":{"profile":` +
		profileJSON + `}};
dataFromBlade.other = 1;</script></html>`)
}

func TestParseActivity(t *testing.T) {
	body := profilePage(`{"id":1431505,"nick":"Актриса ","last_activity":"13.08.2026 16:50","hide_me":false,"is_vip":false}`)
	a, err := ParseActivity(body, 1431505)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	want := time.Date(2026, 8, 13, 16, 50, 0, 0, nsk).UTC()
	if !a.At.Equal(want) {
		t.Errorf("время присутствия = %s, ожидалось %s (время сайта новосибирское)", a.At, want)
	}
	if a.Nick != "Актриса" {
		t.Errorf("ник = %q, ожидался %q", a.Nick, "Актриса")
	}
	if a.Missing || a.HideMe || a.VIP {
		t.Errorf("флаги анкеты разобраны неверно: %+v", a)
	}
	if a.Raw != "13.08.2026 16:50" {
		t.Errorf("исходная строка не сохранена: %q", a.Raw)
	}
}

// «Приватность» прячет присутствие от людей, но не из JSON: last_activity
// отдаётся всё равно, а показанное на странице лежит в отдельном поле.
func TestParseActivityHideMe(t *testing.T) {
	body := profilePage(`{"id":606064,"nick":"Хатуль мадан","last_activity":"13.08.2026 16:42",` +
		`"last_activity_for_hide":"12.08.2026 00:51","hide_me":true,"is_vip":true}`)
	a, err := ParseActivity(body, 606064)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !a.HideMe || !a.VIP {
		t.Errorf("не разобраны признаки скрытого присутствия: %+v", a)
	}
	if a.At.IsZero() {
		t.Error("под «Приватностью» отметка потерялась, а сайт её отдаёт")
	}
}

// Страница без данных профиля — удалённая анкета, рабочий случай: обход не
// должен на ней вставать.
func TestParseActivityMissing(t *testing.T) {
	for name, body := range map[string][]byte{
		"без layout":     []byte(`<html><body>Анкета не найдена</body></html>`),
		"пустой профиль": profilePage(`{"id":null}`),
	} {
		a, err := ParseActivity(body, 777)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !a.Missing || a.UserID != 777 {
			t.Errorf("%s: ожидалась отметка «анкеты нет», получено %+v", name, a)
		}
	}
}

// А вот чужая анкета в ответе — дрейф вёрстки: молча писать чужие отметки
// нельзя, наблюдение станет мусором.
func TestParseActivityWrongProfile(t *testing.T) {
	body := profilePage(`{"id":999,"nick":"кто-то","last_activity":"13.08.2026 16:50"}`)
	var me *MarkupError
	if _, err := ParseActivity(body, 777); !errors.As(err, &me) {
		t.Fatalf("ожидалась MarkupError, получено %v", err)
	}
}

// Незнакомый формат времени теряет отметку, но не запись: анкета всё равно
// опрошена, и это надо отличать от ошибки.
func TestParseActivityUnknownTimeFormat(t *testing.T) {
	body := profilePage(`{"id":777,"nick":"ник","last_activity":"только что"}`)
	a, err := ParseActivity(body, 777)
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	if !a.At.IsZero() || a.Raw != "только что" {
		t.Errorf("ожидалась пустая отметка с сохранённой строкой, получено %+v", a)
	}
}

func TestProfileIDFromLink(t *testing.T) {
	cases := map[string]int64{
		"https://love.ngs.ru/profile/981563/": 981563,
		"https://love.ngs.ru/anketa376712/":   376712, // старый формат из питоновского импорта
		"/profile/1431505/":                   1431505,
		"":                                    0,
		"https://love.ngs.ru/notes/":          0,
	}
	for link, want := range cases {
		if got := ProfileIDFromLink(link); got != want {
			t.Errorf("ProfileIDFromLink(%q) = %d, ожидалось %d", link, got, want)
		}
	}
}
