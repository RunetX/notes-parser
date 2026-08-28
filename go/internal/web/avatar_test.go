package web

// Кнопка «Обновить аватар» на «моей странице»: взять фото из анкеты НГС ещё раз.
//
// Проверяется здесь не картинка, а ПРАВИЛА, каждое из которых оплачено:
// фото не теряется там, где его не просили менять; кнопки нет там, где она
// заведомо ответит отказом; запрос, идущий на чужой сайт, стоит как вход.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

const testAvatarURL = "https://n1s1.hsmedia.ru/preview/love/avatars/abc_100_100_c.jpg"

// testPhotoURL — фото, которое УЖЕ стоит у человека здесь: адрес нашего
// хранилища, а не анкеты НГС. Отличать их важно: одно решает, есть ли что
// обновлять, другое — есть ли что снимать.
const testPhotoURL = "/media/ab/abcdef.jpg"

// avatarServer — состояние, в котором кнопка вообще имеет смысл: вошедший
// участник с обоими согласиями, живая запись и доступный НГС. photo — фото,
// которое у человека уже стоит («Убрать фото» показывается там, где снимать
// есть что); пустая строка означает силуэт.
func avatarServer(t *testing.T, site Site, userID int64, photo string) (http.Handler, *fakeWriter, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[userID] = platform.User{ID: userID, Nick: testNick, Kind: platform.KindMember}
	auth.avatars[userID] = photo
	token, _, err := auth.CreateSession(context.Background(), userID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, userID)
	wr := &fakeWriter{}
	return newFullServer(t, &fakeStore{}, auth, wr, nil, site, Config{}), wr, token
}

// Нажатие доносит до ядра И ссылку, и байты: ссылка нужна, чтобы в следующий раз
// было видно, что фото в анкете сменилось, а байты — чтобы страница не зависела
// от живости чужого CDN.
func TestAvatarTakenFromNGSProfile(t *testing.T) {
	site := &fakeSite{prof: SiteProfile{Nick: testNick, AvatarURL: testAvatarURL}}
	h, wr, token := avatarServer(t, site, testProfileID, "")

	w := do(h, postAs(t, "/me/avatar", nil, token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if wr.avatar.url != testAvatarURL {
		t.Errorf("до ядра дошла ссылка %q, ожидалась %q", wr.avatar.url, testAvatarURL)
	}
	if len(wr.avatar.data) == 0 {
		t.Error("байты фото до ядра не дошли")
	}
}

// В анкете нет фото — прежнее ОСТАЁТСЯ. Снять его было бы потерей по нажатию
// кнопки: своего файла площадка не принимает, и вернуть фото человеку неоткуда.
func TestAvatarKeepsOldPhotoWhenProfileHasNone(t *testing.T) {
	site := &fakeSite{prof: SiteProfile{Nick: testNick}} // силуэт клиент НГС не пропускает
	h, wr, token := avatarServer(t, site, testProfileID, testPhotoURL)

	w := do(h, postAs(t, "/me/avatar", nil, token))
	if w.Code != http.StatusOK {
		t.Fatalf("код %d, ожидалась «моя страница» с объяснением", w.Code)
	}
	if !strings.Contains(w.Body.String(), "нет фото") {
		t.Error("человеку не сказано, почему ничего не изменилось")
	}
	if wr.avatar.url != "" || wr.avatar.data != nil {
		t.Errorf("ядро всё-таки трогали: %+v", wr.avatar)
	}
}

// Отказ ЧУЖОГО сайта — не наша поломка: человек остаётся на своей странице с
// объяснением, а не получает 500, и фото при этом не трогается.
func TestAvatarSurvivesSiteFailure(t *testing.T) {
	cases := []struct {
		name string
		site *fakeSite
	}{
		{"анкета не читается", &fakeSite{err: errors.New("500")}},
		{"анкеты нет", &fakeSite{missing: true}},
		{"файл не забрался", &fakeSite{
			prof:      SiteProfile{Nick: testNick, AvatarURL: testAvatarURL},
			avatarErr: errors.New("таймаут"),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, wr, token := avatarServer(t, c.site, testProfileID, testPhotoURL)
			w := do(h, postAs(t, "/me/avatar", nil, token))
			if w.Code != http.StatusOK {
				t.Fatalf("код %d, ожидалась «моя страница»", w.Code)
			}
			if wr.avatar.url != "" {
				t.Error("фото поменяли, хотя НГС ничего не отдал")
			}
		})
	}
}

// Кнопки нет там, где она заведомо ответит отказом: без клиента НГС (сайт
// закрылся) и у вошедшего по приглашению — анкеты НГС у него нет вовсе.
func TestAvatarButtonOnlyWhereItWorks(t *testing.T) {
	live := &fakeSite{prof: SiteProfile{Nick: testNick, AvatarURL: testAvatarURL}}
	cases := []struct {
		name   string
		site   Site
		userID int64
		want   bool
	}{
		{"анкета НГС и живой сайт", live, testProfileID, true},
		{"сайт недоступен", nil, testProfileID, false},
		{"вход по приглашению", live, platform.NativeIDBase + 7, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := avatarServer(t, c.site, c.userID, "")
			body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
			if got := strings.Contains(body, `action="/me/avatar"`); got != c.want {
				t.Errorf("кнопка показана: %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// Нажатие у вошедшего по приглашению отвечает внятно, а не молча падает: id вне
// полосы НГС означает, что анкеты, из которой брать фото, не существует.
func TestAvatarRefusedForNonNGSMember(t *testing.T) {
	site := &fakeSite{prof: SiteProfile{Nick: testNick, AvatarURL: testAvatarURL}}
	h, wr, token := avatarServer(t, site, platform.NativeIDBase+7, "")

	w := do(h, postAs(t, "/me/avatar", nil, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("код %d, ожидался 400", w.Code)
	}
	if wr.avatar.url != "" {
		t.Error("фото всё-таки поменяли")
	}
}

// Обновление аватара ходит на НГС дважды (анкета плюс файл), поэтому стоит оно
// как вход, а не как обычная запись: наш слот занят всё время чужого ответа.
func TestAvatarCostsAsMuchAsLogin(t *testing.T) {
	r := httptest.NewRequest("POST", "/me/avatar", nil)
	if got := costOf(r); got != costLogin {
		t.Errorf("цена %v, ожидалась %v", got, costLogin)
	}
	if got := budgetOf(r); got != loginBudget {
		t.Errorf("срок %v, ожидался %v", got, loginBudget)
	}
}


// ---------------------------------------------------------------- «Убрать фото»

// Своя рука фото снимает — в отличие от чужой. Это вторая дорога рядом с
// «Обновить аватар», и появилась она из тупика: пустая анкета НГС причиной снять
// фото не считается, поэтому стёрший фото ТАМ оставался здесь с прежним навсегда.
func TestAvatarClearedByOwnHand(t *testing.T) {
	site := &fakeSite{prof: SiteProfile{Nick: testNick}} // в анкете НГС фото уже нет
	h, wr, token := avatarServer(t, site, testProfileID, testPhotoURL)

	w := do(h, postAs(t, "/me/avatar/clear", nil, token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me" {
		t.Fatalf("код %d, Location %q", w.Code, w.Header().Get("Location"))
	}
	if wr.cleared != testProfileID {
		t.Errorf("ядру велено снять фото у %d, ожидался %d", wr.cleared, testProfileID)
	}
}

// Снятие не зависит от НГС ВООБЩЕ: ни от живости сайта, ни от полосы id. Иначе
// закрытие love.ngs.ru заперло бы людей с чужим прошлогодним фото навсегда — а
// закроется он однажды наверняка.
func TestAvatarClearWorksWithoutNGS(t *testing.T) {
	id := platform.NativeIDBase + 7 // вход по приглашению, анкеты НГС нет вовсе
	h, wr, token := avatarServer(t, nil, id, testPhotoURL)

	w := do(h, postAs(t, "/me/avatar/clear", nil, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if wr.cleared != id {
		t.Errorf("фото не сняли: cleared = %d", wr.cleared)
	}
}

// Кнопки нет там, где снимать нечего, — и есть там, где НГС уже недоступен.
func TestAvatarClearButtonWhereThereIsPhoto(t *testing.T) {
	live := &fakeSite{prof: SiteProfile{Nick: testNick, AvatarURL: testAvatarURL}}
	cases := []struct {
		name  string
		site  Site
		photo string
		want  bool
	}{
		{"фото стоит", live, testPhotoURL, true},
		{"фото нет — снимать нечего", live, "", false},
		{"сайт закрылся, фото осталось", nil, testPhotoURL, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := avatarServer(t, c.site, testProfileID, c.photo)
			body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
			if got := strings.Contains(body, `action="/me/avatar/clear"`); got != c.want {
				t.Errorf("кнопка показана: %v, ожидалось %v", got, c.want)
			}
		})
	}
}

// Снятие никуда не ходит и стоит как обычная запись. Цена соседней кнопки — цена
// ВХОДА, потому что там мы ждём чужой сайт; платить столько же за UPDATE одной
// строки значило бы наказывать человека за устройство соседнего маршрута.
func TestAvatarClearCostsAsPlainWrite(t *testing.T) {
	r := httptest.NewRequest("POST", "/me/avatar/clear", nil)
	if got := costOf(r); got != costWrite {
		t.Errorf("цена %v, ожидалась %v", got, costWrite)
	}
}
