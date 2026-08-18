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

// avatarServer — состояние, в котором кнопка вообще имеет смысл: вошедший
// участник с обоими согласиями, живая запись и доступный НГС.
func avatarServer(t *testing.T, site Site, userID int64) (http.Handler, *fakeWriter, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[userID] = platform.User{ID: userID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), userID, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, k := range []string{platform.ConsentProcessing, platform.ConsentDistribution} {
		if err := auth.GrantConsent(ctx, userID, k, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	wr := &fakeWriter{}
	return newFullServer(t, &fakeStore{}, auth, wr, site, Config{}), wr, token
}

// Нажатие доносит до ядра И ссылку, и байты: ссылка нужна, чтобы в следующий раз
// было видно, что фото в анкете сменилось, а байты — чтобы страница не зависела
// от живости чужого CDN.
func TestAvatarTakenFromNGSProfile(t *testing.T) {
	site := &fakeSite{prof: SiteProfile{Nick: testNick, AvatarURL: testAvatarURL}}
	h, wr, token := avatarServer(t, site, testProfileID)

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
	h, wr, token := avatarServer(t, site, testProfileID)

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
			h, wr, token := avatarServer(t, c.site, testProfileID)
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
			h, _, token := avatarServer(t, c.site, c.userID)
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
	h, wr, token := avatarServer(t, site, platform.NativeIDBase+7)

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
