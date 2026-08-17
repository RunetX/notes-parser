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

// TrimAddressPrefix режет ровно то, что нашёл AddressPrefix, — иначе площадка
// снимала бы обращение в ребро, а в теле оставляла его же.
func TestTrimAddressPrefix(t *testing.T) {
	cases := []struct {
		text, want string
	}{
		{"Ягода, привет", "привет"},
		{"  Noname Noface , ну да", "ну да"},
		{"Ягода, привет\nвторая строка", "привет\nвторая строка"},
		{"Ягода,\nа вот и текст", "а вот и текст"}, // перенос сразу после запятой
		{"Просто текст без обращения", "Просто текст без обращения"},
		{"Когда я разводилась в 35 лет, было тяжело", "Когда я разводилась в 35 лет, было тяжело"},
		{"Ягода,", "Ягода,"}, // одно обращение и больше ничего: пустая реплика хуже
		{"", ""},
	}
	for _, c := range cases {
		if got := TrimAddressPrefix(c.text); got != c.want {
			t.Errorf("TrimAddressPrefix(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
