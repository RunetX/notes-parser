package love

// Присутствие анкеты на сайте: поле last_activity в JSON мобильной страницы
// профиля (m.love.ngs.ru/profile/<id>/, тот же dataFromBlade.layout, что и пол).
//
// Зачем оно. Запрет в «Заметки» ничего не убирает с площадки и потому
// ненаблюдаем со стороны: человек просто перестаёт писать. Отличить запрет от
// «человеку стало не до того» нечем — кроме этого поля: оно показывает, что
// человек ПРОДОЛЖАЕТ ходить на сайт, молча. Замолчал и ходит — закрыли;
// замолчал и не заходит — ушёл сам.
//
// Три проверенных свойства (замеры 13.08.2026):
//   - поле живое: у человека на сайте оно двигается минута в минуту
//     (Актриса 16:46 → 16:50 за три минуты);
//   - наш анонимный просмотр его НЕ двигает — контрольные анкеты, прочитанные
//     трижды подряд, остались со своими старыми отметками (05.08 17:19);
//   - «Приватность» (hide_me) его не прячет: под ней сайт лишь показывает
//     людям другое поле, last_activity_for_hide, а настоящее остаётся в JSON.
//     Поэтому видно и тех, кто скрывает присутствие, — VIP-модераторов в том
//     числе.
//
// Читается анонимно: следа в чужом списке гостей такой заход не оставляет.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Activity — снимок присутствия анкеты.
type Activity struct {
	UserID  int64
	Nick    string
	At      time.Time // last_activity в UTC; нулевое — сайт поля не дал
	Raw     string    // «13.08.2026 16:42» — как отдал сайт
	HideMe  bool      // включена «Приватность»
	VIP     bool
	Missing bool // анкеты нет: 404 или страница без данных профиля
}

// activityLayout — формат last_activity. Время сайта новосибирское.
const activityLayout = "02.01.2006 15:04"

// FetchActivity читает присутствие анкеты. Клиент должен быть создан с базой
// MobileBaseURL и мобильным User-Agent (десктопная версия уводит редиректом).
// cookies не нужны — поле отдаётся и анониму; сессию сюда не носить, чтобы не
// светиться в чужих гостях.
func (c *Client) FetchActivity(ctx context.Context, cookies []*http.Cookie, id int64) (Activity, error) {
	body, err := c.get(ctx, fmt.Sprintf("/profile/%d/", id), cookies...)
	if errors.Is(err, ErrNotFound) {
		return Activity{UserID: id, Missing: true}, nil
	}
	if err != nil {
		return Activity{}, err
	}
	return ParseActivity(body, id)
}

// ParseActivity достаёт присутствие из HTML мобильного профиля. Страница без
// данных профиля — не дрейф вёрстки, а удалённая анкета: Missing. А вот чужая
// анкета в ответе — именно дрейф, и о нём надо знать, иначе наблюдение молча
// пишет чужие отметки.
func ParseActivity(body []byte, id int64) (Activity, error) {
	i := bytes.Index(body, []byte(mobileLayoutMarker))
	if i < 0 {
		return Activity{UserID: id, Missing: true}, nil
	}
	var layout struct {
		HeaderContent struct {
			Profile struct {
				ID           int64  `json:"id"`
				Nick         string `json:"nick"`
				LastActivity string `json:"last_activity"`
				HideMe       bool   `json:"hide_me"`
				VIP          bool   `json:"is_vip"`
			} `json:"profile"`
		} `json:"header_content"`
	}
	dec := json.NewDecoder(bytes.NewReader(body[i+len(mobileLayoutMarker):]))
	if err := dec.Decode(&layout); err != nil {
		return Activity{}, fmt.Errorf("анкета %d: разбор dataFromBlade.layout: %w", id, err)
	}
	p := layout.HeaderContent.Profile
	switch {
	case p.ID == 0:
		return Activity{UserID: id, Missing: true}, nil
	case p.ID != id:
		return Activity{}, &MarkupError{
			Selector: mobileLayoutMarker,
			Context:  fmt.Sprintf("анкета %d: в ответе данные анкеты %d", id, p.ID),
		}
	}
	a := Activity{
		UserID: id,
		Nick:   normalizeSpaces(p.Nick),
		Raw:    p.LastActivity,
		HideMe: p.HideMe,
		VIP:    p.VIP,
	}
	if t, err := time.ParseInLocation(activityLayout, p.LastActivity, nsk); err == nil {
		a.At = t.UTC()
	}
	return a, nil
}
