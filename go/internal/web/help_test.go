package web

// Справка и правила.
//
// Тут проверяется не текст (он живёт и меняется в шаблоне), а четыре свойства,
// без которых страница бессмысленна: она открыта не вошедшему, на неё ведёт
// ссылка с любой страницы, реквизиты оператора на ней те же, что в согласиях, и
// числа в ней взяты из ядра, а не написаны словами.

import (
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func helpBody(t *testing.T, cfg Config) string {
	t.Helper()
	h := newTestServer(t, &fakeStore{}, cfg)
	w := do(h, guest(t, "GET", "/help"))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидался 200", w.Code)
	}
	return w.Body.String()
}

// Правила, которые видно только изнутри, — это не правила, а сюрприз.
func TestHelpIsOpenToGuests(t *testing.T) {
	body := helpBody(t, Config{})
	for _, want := range []string{"Справка и правила", "Правила раздела", "Как войти", "Модерация"} {
		if !strings.Contains(body, want) {
			t.Errorf("в справке нет раздела %q", want)
		}
	}
}

// Ссылка на справку стоит в подвале, то есть доступна с любой страницы: искать
// правила по памяти адреса человек не должен.
func TestHelpIsLinkedFromEveryPage(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := openServer(t, st)
	for _, target := range []string{"/", "/n/312811", "/login"} {
		if body := do(h, guest(t, "GET", target)).Body.String(); !strings.Contains(body, `href="/help"`) {
			t.Errorf("%s: нет ссылки на справку", target)
		}
	}
}

// Реквизиты оператора — те же, что подставлены в тексты согласий. Разойдясь,
// страница стала бы вторым источником правды о том, кто обрабатывает данные.
func TestHelpShowsTheSameOperatorAsConsents(t *testing.T) {
	op := platform.Operator{Name: "ИП Иванов И. И.", Contact: "help@t3h.ru"}
	body := helpBody(t, Config{Operator: op})
	if !strings.Contains(body, op.Name) || !strings.Contains(body, op.Contact) {
		t.Error("в справке нет реквизитов оператора из конфига")
	}

	// Пустые реквизиты дают ту же безличную подстановку, что и в согласиях, а
	// не пустое место: на пилоте это правда, и врать ею не нужно.
	if blank := helpBody(t, Config{}); !strings.Contains(blank, platform.Operator{}.Public().Name) {
		t.Error("без реквизитов справка молчит об операторе")
	}
}

// Числа в справке приезжают из ядра. Справка, разошедшаяся с поведением кнопки,
// хуже отсутствующей: человек поверит написанному и решит, что площадка сломана.
func TestHelpNumbersComeFromTheCore(t *testing.T) {
	body := helpBody(t, Config{})
	if !strings.Contains(body, "10 минут") {
		t.Error("окно правки в справке не совпадает с platform.EditWindow")
	}
	if !strings.Contains(body, "больше 5") {
		t.Error("потолок закреплённых в справке не совпадает с platform.MaxPinned")
	}
}
