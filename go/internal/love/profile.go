package love

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// selProfileNick — заголовок ника на странице профиля /profile/<id>/. Его класс
// несёт пол анкеты: "_male"/"_female" (например
// `<h2 class="_male lv-pr-general__nick">`). Единственный надёжный признак пола
// в серверном HTML профиля.
const selProfileNick = ".lv-pr-general__nick"

// Значения пола (как в классе профиля, без ведущего подчёркивания).
const (
	GenderMale   = "male"
	GenderFemale = "female"
)

// FetchGender загружает профиль /profile/<id>/ под куками пользователя и
// извлекает пол из класса заголовка ника. Пустая строка — пол не размечен
// (скрытая/удалённая анкета либо дрейф вёрстки). id — числовой id анкеты.
// Ходит под сессией: часть анкет сайт отдаёт по-разному гостю и залогиненному.
func (c *Client) FetchGender(ctx context.Context, cookies []*http.Cookie, id string) (string, error) {
	body, err := c.get(ctx, "/profile/"+id+"/", cookies...)
	if err != nil {
		return "", err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	class, _ := doc.Find(selProfileNick).First().Attr("class")
	return genderFromClass(class), nil
}

// genderFromClass достаёт пол из списка классов заголовка ника: токен
// "_male"/"_female". Возвращает "" — явного пола нет.
func genderFromClass(class string) string {
	for _, f := range strings.Fields(class) {
		switch f {
		case "_male":
			return GenderMale
		case "_female":
			return GenderFemale
		}
	}
	return ""
}
