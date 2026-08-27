package main

import (
	"testing"

	"lovegw/internal/love"
	"lovegw/internal/platform"
)

// Пол сайта переводится в наш, а НЕРАЗМЕЧЕННЫЙ обязан стать GenderUnknown — и
// это не мелочь перевода, а несущее правило команды `platform gender`: по
// Unknown она оставляет строку в покое. Иначе анкета, скрытая целиком, или
// дрейф вёрстки снимали бы уже известный пол — то есть чужая беда портила бы
// наши данные, и на странице человек молча терял бы силуэт.
func TestPlatformGenderKeepsUnmarkedUnknown(t *testing.T) {
	cases := []struct {
		in   string
		want platform.Gender
	}{
		{love.GenderMale, platform.GenderMale},
		{love.GenderFemale, platform.GenderFemale},
		{"", platform.GenderUnknown},
		{"unknown", platform.GenderUnknown},
	}
	for _, c := range cases {
		if got := platformGender(c.in); got != c.want {
			t.Errorf("platformGender(%q) = %d, ожидалось %d", c.in, got, c.want)
		}
	}
}

// Отчёт команды человек читает глазами, поэтому пол в нём слово, а не число.
func TestGenderWord(t *testing.T) {
	if genderWord(platform.GenderFemale) != "женский" || genderWord(platform.GenderMale) != "мужской" {
		t.Error("пол назван не по-русски")
	}
	if genderWord(platform.GenderUnknown) != "неизвестен" {
		t.Error("неизвестный пол обязан называть себя неизвестным, а не пустотой")
	}
}
