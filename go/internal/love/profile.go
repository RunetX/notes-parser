package love

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// Значения пола (как в классе профиля, без ведущего подчёркивания).
const (
	GenderMale   = "male"
	GenderFemale = "female"
)

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

// parseGenderMobile — пол из мобильной анкеты. «Анкеты нет» здесь не сбой, а
// «пол не размечен»: обход пола идёт пачками, и снесённая анкета не повод его
// рвать. Для входа та же ситуация значит другое, поэтому решает вызывающий.
func parseGenderMobile(body []byte, id string) (string, error) {
	p, err := parseMobileProfile(body, id)
	if errors.Is(err, ErrProfileMissing) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return p.Gender, nil
}

// ErrProfileMissing — анкеты по этому номеру сайт не отдал: снесена, скрыта
// целиком или номера не существует. Отдельная ошибка, потому что при входе на
// площадку это не сбой, а нормальный ответ человеку («такой анкеты нет»).
var ErrProfileMissing = errors.New("анкета не найдена")

// Profile — публичные поля анкеты, прочитанные АНОНИМНО с мобильного vhost.
//
// Анонимность здесь — не аккуратность, а свойство, на котором стоит вход в свою
// площадку: чтение под чужой сессией двигало бы `last_activity` человека и
// светило бы его в «гостях» анкеты, то есть сообщало бы сайту, кто именно
// собрался переезжать. Наш анонимный просмотр не оставляет следа вовсе
// (проверено контрольными чтениями, см. память last-activity-detects-section-ban).
type Profile struct {
	ID        int64
	Nick      string
	AvatarURL string // на hsmedia.ru; наружу такие ссылки не отдаём
	AboutMe   string // поле «о себе» — сюда человек вставляет код входа
	Gender    string // GenderMale / GenderFemale / "" — не размечен
	Active    bool   // is_active: анкета не заблокирована владельцем
	Blocked   bool   // block: заблокирована (владельцем или администрацией)
	// Hidden — включена «Приватность». На «о себе» она НЕ распространяется:
	// замер 18.08.2026 по 24 анкетам нашёл анкету с hide_me = true и непустым
	// about_me, читаемым анонимно (1281493). Прячет «Приватность» только
	// активность — вместо last_activity сайт показывает last_activity_for_hide.
	// Поэтому вход по коду работает и под ней, и объяснять человеку нечего.
	Hidden bool
}

// FetchProfile читает анкету мобильной версии. Клиент должен быть создан с
// базой MobileBaseURL и мобильным User-Agent: с десктопным сайт уводит
// редиректом, а десктопный vhost банит серию запросов почти сразу.
func (c *Client) FetchProfile(ctx context.Context, id string) (Profile, error) {
	body, err := c.get(ctx, "/profile/"+id+"/")
	if err != nil {
		return Profile{}, err
	}
	return parseMobileProfile(body, id)
}

// parseMobileProfile — разбор HTML мобильной анкеты. Декодируется ровно одно
// JSON-значение после маркера (json.Decoder сам останавливается на конце
// объекта, хвост JS не мешает). id сверяется с профилем в JSON, чтобы не
// принять чужие данные: рядом лежит layout.user — это ЗАЛОГИНЕННЫЙ
// пользователь, и при анонимном чтении он пуст, а под сессией был бы наш.
func parseMobileProfile(body []byte, id string) (Profile, error) {
	i := bytes.Index(body, []byte(mobileLayoutMarker))
	if i < 0 {
		// Страница без layout — удалённая анкета или заглушка. Для пола это
		// «не размечен», для входа — «нет такой анкеты»; различает вызывающий.
		return Profile{}, ErrProfileMissing
	}
	var layout struct {
		HeaderContent struct {
			Profile struct {
				ID       int64           `json:"id"`
				Nick     string          `json:"nick"`
				Avatar   string          `json:"avatar"`
				AboutMe  string          `json:"about_me"`
				Sex      *int            `json:"sex"` // указатель: 0 — женщина, отличаем от отсутствия
				IsActive *bool           `json:"is_active"`
				Block    json.RawMessage `json:"block"` // null у живой; вид значения сайт меняет
				HideMe   bool            `json:"hide_me"`
			} `json:"profile"`
		} `json:"header_content"`
	}
	dec := json.NewDecoder(bytes.NewReader(body[i+len(mobileLayoutMarker):]))
	if err := dec.Decode(&layout); err != nil {
		return Profile{}, fmt.Errorf("профиль %s: разбор dataFromBlade.layout: %w", id, err)
	}
	p := layout.HeaderContent.Profile
	if strconv.FormatInt(p.ID, 10) != id {
		return Profile{}, ErrProfileMissing
	}
	out := Profile{
		ID:        p.ID,
		Nick:      p.Nick,
		AvatarURL: p.Avatar,
		AboutMe:   p.AboutMe,
		Active:    p.IsActive == nil || *p.IsActive,
		Blocked:   len(p.Block) > 0 && !bytes.Equal(p.Block, []byte("null")),
		Hidden:    p.HideMe,
	}
	if p.Sex != nil {
		switch *p.Sex {
		case 1:
			out.Gender = GenderMale
		case 0:
			out.Gender = GenderFemale
		}
	}
	return out, nil
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
