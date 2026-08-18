package love

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mobileProfileHTML — минимальный слепок страницы мобильного профиля: JSON
// dataFromBlade.layout с залогиненным пользователем (layout.user, свой sex)
// и просматриваемой анкетой (header_content.profile), после JSON — хвост JS.
func mobileProfileHTML(profileJSON string) []byte {
	return []byte(`<html><script>
 dataFromBlade.layout = {"user":{"id":42,"nick":"viewer","sex":1},` +
		`"header_content":{"profile":` + profileJSON + `},"content":"..."};
 dataFromBlade.layout.menu = [];
</script></html>`)
}

func TestParseGenderMobile(t *testing.T) {
	cases := []struct {
		name, id, profile, want string
	}{
		{"мужчина", "18471", `{"id":18471,"nick":"Т","sex":1}`, GenderMale},
		{"женщина", "515996", `{"id":515996,"nick":"Я","sex":0}`, GenderFemale},
		{"sex отсутствует", "10", `{"id":10,"nick":"n"}`, ""},
		{"чужой id в профиле", "10", `{"id":11,"nick":"n","sex":1}`, ""},
		{"неизвестное значение", "10", `{"id":10,"sex":7}`, ""},
	}
	for _, c := range cases {
		got, err := parseGenderMobile(mobileProfileHTML(c.profile), c.id)
		if err != nil {
			t.Errorf("%s: неожиданная ошибка %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: пол %q, ожидался %q", c.name, got, c.want)
		}
	}
}

func TestParseGenderMobileNoLayout(t *testing.T) {
	got, err := parseGenderMobile([]byte(`<html>Профиль удалён</html>`), "10")
	if err != nil || got != "" {
		t.Errorf("страница без layout: (%q, %v), ожидалось (\"\", nil)", got, err)
	}
}

func TestMobileBaseURL(t *testing.T) {
	got, err := MobileBaseURL("https://love.ngs.ru")
	if err != nil || got != "https://m.love.ngs.ru" {
		t.Errorf("MobileBaseURL = (%q, %v), ожидалось https://m.love.ngs.ru", got, err)
	}
}

// 404 на анкете — это «такого номера нет», а не сбой сайта, и разница видна
// человеку: на опечатке он должен услышать «проверьте номер», а не «НГС не
// отвечает» и ждать у моря погоды (жалоба 18.08.2026 — вход по номеру «6»).
func TestFetchProfileTreats404AsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, srv).FetchProfile(context.Background(), "6")
	if !errors.Is(err, ErrProfileMissing) {
		t.Fatalf("404 отдан как %v, ожидался ErrProfileMissing", err)
	}
}
