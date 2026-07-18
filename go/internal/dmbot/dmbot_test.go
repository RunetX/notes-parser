package dmbot

import "testing"

func TestCommand(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/start", "/start"},
		{"/login user pass", "/login"},
		{"/status@ryumkinbot", "/status"},
		{"  /add_note", "/add_note"}, // ведущие пробелы Fields отбрасывает
		{"привет", ""},
		{"", ""},
		{"не команда /login", ""},
	} {
		if got := command(tc.in); got != tc.want {
			t.Errorf("command(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
