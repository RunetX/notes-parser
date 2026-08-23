package web

// Раздел администратора. Проверяется здесь то же, что и в модерации: КОМУ видно
// и что происходит по нажатию, — а не SQL и не политика (это platform).

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// adminServer — площадка с вошедшим администратором и тенью зеркала, к которой
// есть что привязывать.
func adminServer(t *testing.T) (http.Handler, *fakeMod, string) {
	t.Helper()
	h, mod, token := modServer(t, platform.RoleAdmin)
	mod.users[1038894] = platform.User{ID: 1038894, Nick: "Пух", Kind: platform.KindShadow}
	mod.users[1493279] = platform.User{ID: 1493279, Nick: "Рио", Kind: platform.KindMember}
	return h, mod, token
}

// Дверь администратора закрыта и от МОДЕРАТОРА: он решает про слова, а
// приглашение — это про людей. И закрыта тем же способом, что очередь от
// постороннего: «нет такой страницы», а не «нужны права».
func TestАдминкаЗакрытаОтМодератора(t *testing.T) {
	for _, role := range []platform.Role{platform.RoleUser, platform.RoleModerator} {
		h, _, token := modServer(t, role)
		if w := do(h, guest(t, "GET", "/mod/admin")); w.Code != http.StatusNotFound {
			t.Errorf("роль %d, гость: код %d, ожидался 404", role, w.Code)
		}
		if w := do(h, as(guest(t, "GET", "/mod/admin"), token)); w.Code != http.StatusNotFound {
			t.Errorf("роль %d: код %d, ожидался 404", role, w.Code)
		}
		w := do(h, postAs(t, "/mod/admin", url.Values{"do": {"issue"}}, token))
		if w.Code != http.StatusNotFound {
			t.Errorf("роль %d, выдача: код %d, ожидался 404", role, w.Code)
		}
	}
}

// Пункт меню виден администратору и только ему: существование закрытой двери —
// само по себе сведения.
func TestПунктМенюТолькоУАдминистратора(t *testing.T) {
	h, _, token := modServer(t, platform.RoleModerator)
	if body := do(h, as(guest(t, "GET", "/mod"), token)).Body.String(); strings.Contains(body, "/mod/admin") {
		t.Error("модератор видит вход в администрирование")
	}
	h, _, token = adminServer(t)
	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	if !strings.Contains(body, `href="/mod/admin"`) || !strings.Contains(body, "Администрирование") {
		t.Error("администратор не видит пункта меню")
	}
}

// Выдача кнопкой: код показывается ОДИН раз, на том же ответе, и рядом сказано,
// кому он выдан, — опечатку в номере надо заметить до того, как код уедет в
// переписку.
func TestКодВыдаётсяКнопкой(t *testing.T) {
	h, mod, token := adminServer(t)

	w := do(h, postAs(t, "/mod/admin", url.Values{
		"do": {"issue"}, "bind": {"1038894"}, "days": {"14"}, "label": {"Пух потеряла анкету"},
	}, token))
	if w.Code != http.StatusOK {
		t.Fatalf("выдача ответила %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		testInviteCode,        // сам код
		"Пух",                 // кому
		"№1038894",            //
		"/login/invite",       // куда его нести
		"Отозвать",            // и как отменить, если не тому
		"Пух потеряла анкету", // пометка попала в список
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на ответе выдачи нет %q", want)
		}
	}
	if len(mod.acts) != 1 || mod.acts[0] != "invite 1038894" {
		t.Fatalf("ядро позвали не так: %v", mod.acts)
	}

	// Второй показ невозможен: в базе лежит только хеш, и страница обязана
	// вести себя так же — иначе «покажите ещё раз» стало бы дырой.
	if again := do(h, as(guest(t, "GET", "/mod/admin"), token)).Body.String(); strings.Contains(again, testInviteCode) {
		t.Error("код остался на странице после перезагрузки")
	}
}

// Привязка к тому, кто уже входил, — штатный способ вернуть доступ, но код в
// этом случае открывает живую учётную запись, и сказано об этом должно быть на
// экране выдачи, а не в справке.
func TestПривязкаКУчастникуПредупреждает(t *testing.T) {
	h, _, token := adminServer(t)

	body := do(h, postAs(t, "/mod/admin", url.Values{
		"do": {"issue"}, "bind": {"1493279"},
	}, token)).Body.String()
	if !strings.Contains(body, "уже входил") {
		t.Error("про доступ к живой учётной записи не сказано")
	}
	body = do(h, postAs(t, "/mod/admin", url.Values{"do": {"issue"}}, token)).Body.String()
	if strings.Contains(body, "уже входил") {
		t.Error("предупреждение показано у приглашения без привязки")
	}
}

// Неизвестный номер — это опечатка администратора, а не поломка: страница
// объясняет её словами и не выдаёт кода.
func TestНеизвестныйУчастникНеПолучаетКода(t *testing.T) {
	h, mod, token := adminServer(t)

	w := do(h, postAs(t, "/mod/admin", url.Values{"do": {"issue"}, "bind": {"999999"}}, token))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ответ %d, ожидался 400", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, testInviteCode) {
		t.Error("код выдан несуществующему участнику")
	} else if !strings.Contains(body, "Такого участника на площадке нет") {
		t.Error("отказ не объяснён словами")
	}
	if len(mod.invites) != 0 {
		t.Error("строка приглашения всё-таки завелась")
	}

	// Не число — та же дорога: форма отвечает объяснением, а не пятисоткой.
	if w := do(h, postAs(t, "/mod/admin",
		url.Values{"do": {"issue"}, "bind": {"Пух"}}, token)); w.Code != http.StatusBadRequest {
		t.Errorf("нечисловой номер: код %d, ожидался 400", w.Code)
	}
}

// Отзыв: строка называется временем выдачи (своего идентификатора у приглашения
// нет, а хеш кода показывать нельзя), и после отзыва кнопки у неё больше нет.
func TestПриглашениеОтзывается(t *testing.T) {
	h, mod, token := adminServer(t)
	do(h, postAs(t, "/mod/admin", url.Values{"do": {"issue"}, "bind": {"1038894"}}, token))
	key := mod.invites[0].CreatedAt.UTC().Format(inviteKeyFormat)

	w := do(h, postAs(t, "/mod/admin", url.Values{"do": {"revoke"}, "issued": {key}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("отзыв ответил %d, ожидался переход", w.Code)
	}
	if mod.invites[0].Live(mod.invites[0].CreatedAt) {
		t.Fatal("приглашение осталось живым")
	}
	body := do(h, as(guest(t, "GET", "/mod/admin"), token)).Body.String()
	if strings.Contains(body, "Отозвать") {
		t.Error("у погасшего приглашения осталась кнопка отзыва")
	}
	if !strings.Contains(body, "истекло") {
		t.Error("погасшее приглашение не помечено на странице")
	}

	// Повторный отзыв — не ошибка: два администратора, нажавших одно и то же,
	// не должны видеть отказ.
	if w := do(h, postAs(t, "/mod/admin",
		url.Values{"do": {"revoke"}, "issued": {key}}, token)); w.Code != http.StatusSeeOther {
		t.Errorf("повторный отзыв: код %d", w.Code)
	}
}

// Формы раздела защищены тем же скрытым полем, что и все пишущие: без него
// чужая страница выдавала бы приглашения от имени вошедшего администратора.
func TestВыдачаТребуетТокенФормы(t *testing.T) {
	h, mod, token := adminServer(t)

	w := do(h, as(post(t, "/mod/admin", url.Values{"do": {"issue"}, "bind": {"1038894"}}), token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("форма без токена: код %d, ожидался 403", w.Code)
	}
	if len(mod.acts) != 0 {
		t.Errorf("ядро всё-таки позвали: %v", mod.acts)
	}
}
