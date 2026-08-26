package web

// «Проматывать к новым» (jump.go): настройка на /me, атрибут на треде, прыжок в
// скрипте. Тесты держат все три конца — порознь любой из них выглядит рабочим.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// [Ж] Настройка стоит на «Моей странице» и НАЗЫВАЕТ своё состояние словом.
//
// Подпись «Проматывать к новым» на выключенной настройке читается и как «сейчас
// так», и как «сделать так» — различить их нечем, а ошибка тут стоит того, что
// человек уверен в обратном тому, что происходит. Поэтому состояние словом,
// кнопка — действием.
func TestJumpPrefIsOnMyPageAndNamesItsState(t *testing.T) {
	h, auth, token := signedInServer(t)
	grantBoth(t, auth, nil)

	off := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(off, `action="/me/jump"`) {
		t.Fatal("на «Моей странице» нет настройки «Проматывать к новым»")
	}
	if !strings.Contains(off, "сейчас выключено") || !strings.Contains(off, ">Включить<") {
		t.Error("выключенная настройка не называет своё состояние либо не предлагает включить")
	}

	r := as(post(t, "/me/jump", url.Values{"on": {"1"}}), token)
	w := do(h, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался переход на /me", w.Code)
	}
	c := cookieOf(w, jumpCookie)
	if c == nil || c.Value != "1" {
		t.Fatalf("кука настройки не выставлена: %+v", c)
	}

	on := do(h, as(withCookie(guest(t, "GET", "/me"), jumpCookie, "1"), token)).Body.String()
	if !strings.Contains(on, "сейчас включено") || !strings.Contains(on, ">Выключить<") {
		t.Error("включённая настройка не называет своё состояние либо не предлагает выключить")
	}

	// Снятие гасит куку, а не пишет в неё второе значение для «нет»: отсутствие
	// и есть «нет», и разбирать два способа сказать одно и то же не нужно.
	w = do(h, as(post(t, "/me/jump", url.Values{"on": {"0"}}), token))
	if c := cookieOf(w, jumpCookie); c == nil || c.Value != "" || c.MaxAge >= 0 {
		t.Errorf("выключение не погасило куку: %+v", c)
	}
}

// [Ж] Чужая страница переключить настройку не может: та же проверка источника,
// что у смены темы. CSRF тут нет намеренно — цена подделки равна прокрутке у
// самого пострадавшего, а поле в форме потребовало бы сессии, которой у формы
// темы тоже нет.
func TestJumpPrefRefusesForeignPage(t *testing.T) {
	h, _, token := signedInServer(t)
	r := as(post(t, "/me/jump", url.Values{"on": {"1"}}), token)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if got := do(h, r).Code; got != http.StatusForbidden {
		t.Errorf("код %d, ожидался отказ чужой странице", got)
	}
}

// [Ж] Атрибут стоит на треде и ТОЛЬКО вместе с живым добором: прыгать некуда
// там, где ничего не дописывается. Читателю без настройки его нет вовсе —
// скрипт узнаёт о ней из разметки, и «выключено» обязано выглядеть как
// отсутствие, а не как data-jump="0".
func TestThreadCarriesJumpOnlyWithLiveAppend(t *testing.T) {
	note := sampleNote()
	note.CommentCount = 100 // хватит на несколько страниц линейного вида
	st := &fakeStore{note: note, thread: sampleThread(), flat: sampleThread()}
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	h := newFullServer(t, st, auth, nil, nil, nil, Config{})
	grantBoth(t, auth, nil)

	plain := do(h, as(guest(t, "GET", "/n/312811"), token)).Body.String()
	if strings.Contains(plain, "data-jump") {
		t.Error("атрибут стоит у того, кто настройку не включал")
	}
	if !strings.Contains(plain, "data-fresh") {
		t.Fatal("живого добора нет вовсе — проверять нечего")
	}

	on := do(h, as(withCookie(guest(t, "GET", "/n/312811"), jumpCookie, "1"), token)).Body.String()
	if !strings.Contains(on, `data-jump="1"`) {
		t.Error("настройка включена, а тред об этом не говорит")
	}

	// Дальняя страница линейного вида — исторический срез: добора там нет, и
	// атрибута не должно быть тоже, иначе он повис бы обещанием без механизма.
	w := do(h, as(withCookie(guest(t, "GET", "/n/312811?view=linear&page=2"), jumpCookie, "1"), token))
	if w.Code != http.StatusOK {
		t.Fatalf("вторая страница линейного вида отдала %d — проверять нечего", w.Code)
	}
	if strings.Contains(w.Body.String(), "data-jump") {
		t.Error("атрибут остался на странице, которая не дописывается")
	}
}

// [Ж] Скрипт не уводит страницу из-под рук. Проверить это исполнением нечем —
// браузера в тестах нет, — но правило обязано быть НАЗВАНО в коде, а не жить
// договорённостью: прокрутка в момент набора ответа стоит человеку набранного.
//
// Тут же второе правило того же рода: цель прыжка выбирается по порядку
// СТРАНИЦЫ, а не по номеру реплики. Сравнение по номеру выглядит очевидным и
// неверно по устройству — полосы идентификаторов между собой не упорядочены.
func TestJumpScriptYieldsToTheReader(t *testing.T) {
	js := jsText(t)
	for _, want := range []string{
		"activeElement",           // курсор в поле — человек набирает
		"getSelection",            // выделение — человек читает или копирует
		"prefers-reduced-motion",  // просили не двигать картинку — не двигаем
		"compareDocumentPosition", // цель — первая по странице, а не по номеру
	} {
		if !strings.Contains(js, want) {
			t.Errorf("в живом доборе нет %q — прыжок к новому уведёт страницу из-под рук", want)
		}
	}
}

func jsText(t *testing.T) string {
	t.Helper()
	a, ok := assets[strings.TrimPrefix(assetURL("app.js"), "/assets/")]
	if !ok {
		t.Fatal("app.js не найден")
	}
	return string(a.data)
}

func withCookie(r *http.Request, name, value string) *http.Request {
	r.AddCookie(&http.Cookie{Name: name, Value: value})
	return r
}
