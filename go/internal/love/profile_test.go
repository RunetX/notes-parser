package love

import "testing"

func TestGenderFromClass(t *testing.T) {
	cases := []struct{ class, want string }{
		{"_male lv-pr-general__nick", GenderMale},
		{"lv-pr-general__nick _female", GenderFemale},
		{"_female", GenderFemale},
		{"lv-pr-general__nick", ""},       // пол не размечен
		{"", ""},                          // нет класса
		{"_maleish other", ""},            // не токен — не путаем с префиксом
		{"lv-hinty__nick _male", GenderMale},
	}
	for _, c := range cases {
		if got := genderFromClass(c.class); got != c.want {
			t.Errorf("genderFromClass(%q) = %q, want %q", c.class, got, c.want)
		}
	}
}
