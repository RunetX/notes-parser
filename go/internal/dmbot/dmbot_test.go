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

func TestCommandArg(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/subscribe Граф", "Граф"},
		{"/subscribe Граф M.N.", "Граф M.N."}, // ключевое слово может быть с пробелами
		{"/subscribe   слово  ", "слово"},
		{"/mysubs", ""},
		{"/subscribe", ""},
		{"", ""},
	} {
		if got := commandArg(tc.in); got != tc.want {
			t.Errorf("commandArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
