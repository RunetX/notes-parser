package love

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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

// mobileLayoutMarker — начало JS-присваивания на страницах мобильной версии
// m.love.ngs.ru, в JSON которого лежат данные просматриваемой анкеты.
const mobileLayoutMarker = "dataFromBlade.layout = "

// MobileBaseURL превращает базовый URL сайта в базовый URL мобильной версии:
// https://love.ngs.ru -> https://m.love.ngs.ru. Мобильная версия — отдельный
// vhost с заметно более мягким порогом DDoS-Guard, чем десктопный.
func MobileBaseURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("базовый URL сайта: %w", err)
	}
	u.Host = "m." + u.Host
	return u.String(), nil
}

// FetchGenderMobile загружает профиль мобильной версии /profile/<id>/ и достаёт
// пол из JSON dataFromBlade.layout (header_content.profile.sex: 1 — мужчина,
// 0 — женщина). Клиент должен быть создан с базой MobileBaseURL и мобильным
// User-Agent. Пустая строка — пол не размечен (удалённая/скрытая анкета или
// дрейф вёрстки). Куки сессии несёт jar клиента либо параметр cookies.
func (c *Client) FetchGenderMobile(ctx context.Context, cookies []*http.Cookie, id string) (string, error) {
	body, err := c.get(ctx, "/profile/"+id+"/", cookies...)
	if err != nil {
		return "", err
	}
	return parseGenderMobile(body, id)
}

// parseGenderMobile — извлечение пола из HTML мобильного профиля. Декодируется
// ровно одно JSON-значение после маркера (json.Decoder сам останавливается на
// конце объекта, хвост JS не мешает). id сверяется с профилем в JSON, чтобы не
// принять чужие данные (например, layout.user — это залогиненный пользователь).
func parseGenderMobile(body []byte, id string) (string, error) {
	i := bytes.Index(body, []byte(mobileLayoutMarker))
	if i < 0 {
		return "", nil // страница без layout — удалённая анкета или заглушка
	}
	var layout struct {
		HeaderContent struct {
			Profile struct {
				ID  int64 `json:"id"`
				Sex *int  `json:"sex"` // указатель: 0 — женщина, отличаем от отсутствия
			} `json:"profile"`
		} `json:"header_content"`
	}
	dec := json.NewDecoder(bytes.NewReader(body[i+len(mobileLayoutMarker):]))
	if err := dec.Decode(&layout); err != nil {
		return "", fmt.Errorf("профиль %s: разбор dataFromBlade.layout: %w", id, err)
	}
	p := layout.HeaderContent.Profile
	if strconv.FormatInt(p.ID, 10) != id || p.Sex == nil {
		return "", nil
	}
	switch *p.Sex {
	case 1:
		return GenderMale, nil
	case 0:
		return GenderFemale, nil
	}
	return "", nil
}

// profileIDRe — id анкеты в ссылке на автора. Сегодня сайт пишет
// /profile/<id>/, в старых записях (импорт из питоновского прототипа) —
// /anketa<id>/; обе формы встречаются в зеркале рядом.
var profileIDRe = regexp.MustCompile(`/(?:profile/|anketa)(\d+)`)

// ProfileIDFromLink достаёт числовой id анкеты из ссылки на автора.
// 0 — ссылки нет либо она не про анкету (так выглядят безанкетные авторы).
func ProfileIDFromLink(link string) int64 {
	m := profileIDRe.FindStringSubmatch(link)
	if m == nil {
		return 0
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return id
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
