package love

import "testing"

func TestAddressPrefix(t *testing.T) {
	cases := []struct {
		text, want string
	}{
		{"Ягода, привет", "ягода"},
		{"ЯГОДА, привет", "ягода"},                        // регистр кириллицы
		{"  Noname Noface , ну да", "noname noface"},      // пробелы вокруг ника
		{"Ягода, привет\nвторая строка", "ягода"},         // обращение только в первой строке
		{"Просто текст без обращения", ""},                //
		{"а, ну да", ""},                                  // слишком короткий префикс
		{"Когда я разводилась в 35 лет, было тяжело", ""}, // придаточное, а не ник
		{",", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := AddressPrefix(c.text); got != c.want {
			t.Errorf("AddressPrefix(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
