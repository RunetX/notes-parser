package main

import "testing"

// Силуэт по умолчанию — не фото, и вход не вправе записать его в
// users.ngs_avatar_url: разовый добор медиа однажды сходил бы по этой ссылке за
// всех сразу, и «аватар есть у всех» превратилось бы в фон под каждой репликой.
// Правило про сайт живёт в love.IsRealAvatar, здесь проверяется, что вход его
// спрашивает.
func TestRealAvatarDropsPlaceholder(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://n1s1.hsmedia.ru/cache/love/avatars/abc_100_100_c.jpg",
			"https://n1s1.hsmedia.ru/cache/love/avatars/abc_100_100_c.jpg"},
		{"https://m.love.ngs.ru/static/i/new/profile/female300px.png", ""},
		{"/static/i/new/profile/anonymous300px.png", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := realAvatar(c.in); got != c.want {
			t.Errorf("realAvatar(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}
