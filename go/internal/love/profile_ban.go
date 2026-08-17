package love

// Управление собственной анкетой: блокировка и снятие блокировки. Всё снято
// живьём 12.08.2026 под сессией владельца, включая круг «заблокировал —
// вернул» на боевой анкете; догадок здесь нет.
//
// Обе страницы — это GET /properties/ под куками. В <script> лежит
// `window.dataFromBlade = {…}`, где layout.user несёт:
//   - `isAuth` — гостю сайт отдаёт 200 и isAuth:false, а не 401/403 (та же
//     манера, что в talks) → ErrUnauthorized;
//   - `isActive` — И ЕСТЬ состояние анкеты: true активна, false заблокирована.
//     `profileBlockState` для этого НЕ годится: он пуст в обоих состояниях —
//     это про модераторский бан, а не про свою блокировку.
//
// Кнопка — обычная HTML-форма на /properties/ban/, без всякого AJAX (JS вешает
// на неё только confirm()). Их две, по одной на состояние, и разметка у них
// разная:
//
//	активная анкета:      <input type="submit" class="… js-self-ban" name="ban"
//	                             value="Заблокировать профиль">
//	заблокированная:      <input type="submit" class="custom-btn"
//	                             name="un_ban lv-user-properties__submit-btn"
//	                             value="Разблокировать профиль">
//
// Отсюда два правила, оба оплачены живой проверкой:
//   - опознавать кнопку по ПОЛЮ, а не по классу: на заблокированной странице
//     класса js-self-ban нет вовсе;
//   - слать ПЕРВОЕ СЛОВО name: у кнопки возврата сайт по своей же ошибке
//     склеил в name список классов, и строку целиком сервер игнорирует —
//     анкета осталась закрытой, а чистое `un_ban` вернуло её сразу.
//
// РЯДОМ ЖИВЁТ УДАЛЕНИЕ АНКЕТЫ: форма с тем же action, класс js-self-delete,
// поле delete. Поэтому отбор кнопки — белый список из двух полей (ban/un_ban),
// а не «всё, кроме delete», и Submit проверяет то же ещё раз: цена ошибки
// здесь необратима.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

const (
	propertiesPath = "/properties/"
	// bladeMarker — начало JS-присваивания с данными страницы настроек.
	// Не путать с mobileLayoutMarker: там `dataFromBlade.layout = <layout>`,
	// здесь `window.dataFromBlade = {…, "layout": {…}}`.
	bladeMarker = "window.dataFromBlade = "

	// selSubmit — кнопки-отправки страницы настроек. Опознаём их по полю
	// формы, а не по классу: у заблокированной анкеты класса js-self-ban нет
	// вовсе (проверено живьём 12.08.2026).
	selSubmit = `input[type="submit"]`

	// Поля кнопок формы /properties/ban/. fieldBan и fieldUnban — две стороны
	// одного переключателя, fieldDelete — соседнее УДАЛЕНИЕ анкеты, запрещённое
	// к отправке.
	fieldBan    = "ban"
	fieldUnban  = "un_ban"
	fieldDelete = "delete"
)

// ProfileControl — состояние своей анкеты и ровно та кнопка, которую сайт
// предлагает нажать. Поля формы неэкспортируемые: снаружи такой запрос не
// собрать, его можно только получить из ProfileControl и вернуть в Submit.
type ProfileControl struct {
	Blocked   bool   // анкета заблокирована (layout.user.isActive == false)
	Available bool   // сайт предлагает кнопку — есть что нажать
	Label     string // подпись кнопки на сайте («Заблокировать профиль»)

	action string // action формы: /properties/ban/
	field  string // поле кнопки: ban либо un_ban
	value  string // value кнопки — сайт шлёт подпись как значение
}

// ProfileControl читает страницу настроек под сессией пользователя и возвращает
// состояние своей анкеты вместе с кнопкой, которую сайт сейчас предлагает.
// Гостевой ответ (isAuth:false) — ErrUnauthorized: наверху пометить сессию
// недействительной и позвать /login.
func (c *Client) ProfileControl(ctx context.Context, cookies []*http.Cookie) (ProfileControl, error) {
	body, err := c.get(ctx, propertiesPath, cookies...)
	if err != nil {
		return ProfileControl{}, fmt.Errorf("настройки анкеты: %w", err)
	}
	return parseProfileControl(body)
}

// parseProfileControl разбирает страницу настроек: состояние — из JSON
// страницы, кнопку — из разметки формы.
func parseProfileControl(body []byte) (ProfileControl, error) {
	user, err := bladeUser(body)
	if err != nil {
		return ProfileControl{}, err
	}
	if !user.IsAuth {
		return ProfileControl{}, ErrUnauthorized
	}
	// Состояние анкеты — isActive. profileBlockState на обеих страницах пуст:
	// это про модераторский бан, а не про свою блокировку (проверено живьём).
	ctrl := ProfileControl{Blocked: user.IsActive != nil && !*user.IsActive}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return ProfileControl{}, err
	}
	var found bool
	var parseErr error
	doc.Find(selSubmit).EachWithBreak(func(_ int, btn *goquery.Selection) bool {
		name, _ := btn.Attr("name")
		switch banField(name) {
		case fieldBan, fieldUnban:
		default:
			// Удаление анкеты и любые чужие кнопки — мимо, молча.
			return true
		}
		action, _ := btn.Closest("form").Attr("action")
		if !strings.HasPrefix(action, "/") {
			parseErr = &MarkupError{
				Selector: selSubmit + " → form[action]",
				Context:  "форма блокировки анкеты без внутрисайтового action",
			}
			return false
		}
		// Шлём ПЕРВОЕ СЛОВО name, а не строку целиком: у кнопки возврата сайт
		// склеил в name классы («un_ban lv-user-properties__submit-btn»), и
		// такую строку сервер не принимает (проверено живьём: анкета осталась
		// закрытой), а чистое un_ban принимает сразу.
		ctrl.Available, ctrl.action, ctrl.field = true, action, firstWord(name)
		ctrl.value, _ = btn.Attr("value")
		ctrl.Label = strings.TrimSpace(ctrl.value)
		found = true
		return false
	})
	if parseErr != nil {
		return ProfileControl{}, parseErr
	}
	if !found {
		// Кнопки нет — не дрейф вёрстки сам по себе: состояние известно и без
		// неё, а наверху об этом можно сказать честно.
		return ctrl, nil
	}
	return ctrl, nil
}

// firstWord — первое слово атрибута name: у кнопки возврата сайт склеил в него
// список классов, а серверу нужно одно поле.
func firstWord(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return ""
}

// banField — по какому полю опознаётся кнопка (регистр не важен).
func banField(name string) string { return strings.ToLower(firstWord(name)) }

// SubmitProfileControl нажимает кнопку, прочитанную ProfileControl: шлёт форму
// ровно с тем полем и значением, что стоят на сайте. Без ретраев — повтор
// переключил бы состояние обратно.
func (c *Client) SubmitProfileControl(ctx context.Context, cookies []*http.Cookie, ctrl ProfileControl) error {
	if !ctrl.Available || ctrl.field == "" || !strings.HasPrefix(ctrl.action, "/") {
		return fmt.Errorf("управление анкетой: сайт не предлагает кнопки")
	}
	// Вторая проверка того же, что и в парсере: рядом лежит форма УДАЛЕНИЯ
	// анкеты с тем же action, и отправить её вместо блокировки нельзя никогда.
	// Поэтому не «всё, кроме delete», а белый список из двух полей.
	switch banField(ctrl.field) {
	case fieldBan, fieldUnban:
	default:
		return fmt.Errorf("управление анкетой: поле %q не блокировка и не возврат, не отправляю", ctrl.field)
	}
	resp, err := c.postForm(ctx, ctrl.action, url.Values{ctrl.field: {ctrl.value}}, cookies)
	if err != nil {
		return fmt.Errorf("управление анкетой (%s): %w", ctrl.field, err)
	}
	if err := drainOK(resp); err != nil {
		return fmt.Errorf("управление анкетой (%s): %w", ctrl.field, err)
	}
	return nil
}

// bladeUserData — то немногое из layout.user, что нам нужно. Остальное на этой
// странице — личные данные владельца, и трогать их незачем.
type bladeUserData struct {
	IsAuth bool `json:"isAuth"`
	// IsActive — указатель: false и «поля нет» это разные вещи, а состояние
	// анкеты держится именно на нём.
	IsActive *bool `json:"isActive"`
}

// bladeUser вырезает из страницы ровно одно JSON-значение после маркера
// (json.Decoder сам останавливается на конце объекта, хвост скрипта не мешает)
// и достаёт из него layout.user — тем же приёмом, что parseGenderMobile.
func bladeUser(body []byte) (bladeUserData, error) {
	i := bytes.Index(body, []byte(bladeMarker))
	if i < 0 {
		return bladeUserData{}, &MarkupError{
			Selector: bladeMarker,
			Context:  "страница настроек без данных страницы",
		}
	}
	var blade struct {
		Layout struct {
			User *bladeUserData `json:"user"`
		} `json:"layout"`
	}
	dec := json.NewDecoder(bytes.NewReader(body[i+len(bladeMarker):]))
	if err := dec.Decode(&blade); err != nil {
		return bladeUserData{}, &MarkupError{
			Selector: bladeMarker,
			Context:  "разбор данных страницы настроек: " + err.Error(),
		}
	}
	if blade.Layout.User == nil {
		return bladeUserData{}, &MarkupError{
			Selector: bladeMarker + "layout.user",
			Context:  "страница настроек без пользователя",
		}
	}
	return *blade.Layout.User, nil
}
