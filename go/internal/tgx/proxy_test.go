package tgx

import (
	"strings"
	"testing"
)

// Пароль прокси не должен попадать ни в вывод doctor, ни в тексты ошибок:
// строку читают через плечо и пересылают в переписку.
func TestMaskProxy(t *testing.T) {
	const secret = "865a71273c5e527a0c66"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"с логином и паролем", "socks5://tgproxy:" + secret + "@45.151.183.247:39080",
			"socks5://tgproxy:***@45.151.183.247:39080"},
		{"http-прокси", "http://user:" + secret + "@proxy.example:3128",
			"http://user:***@proxy.example:3128"},
		{"логин без пароля", "socks5://tgproxy@45.151.183.247:39080",
			"socks5://tgproxy@45.151.183.247:39080"},
		{"без логина", "socks5://45.151.183.247:39080", "socks5://45.151.183.247:39080"},
		{"пусто", "", ""},
		{"не разбирается", "socks5://user:" + secret + "@host:поррт", "<строка прокси не разбирается>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MaskProxy(c.in)
			if got != c.want {
				t.Errorf("MaskProxy(%q) = %q, ждали %q", c.in, got, c.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("пароль утёк в вывод: %q", got)
			}
		})
	}
}

// Ошибка разбора тоже не должна нести пароль — она уходит и в лог, и в doctor.
func TestProxyTransportErrorHidesPassword(t *testing.T) {
	const secret = "865a71273c5e527a0c66"
	_, err := ProxyTransport("socks5://user:" + secret + "@host:поррт")
	if err == nil {
		t.Fatal("битая строка прокси должна давать ошибку")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("пароль утёк в текст ошибки: %v", err)
	}
}
