package love

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SessionCookie — нормализованный формат куки пользовательской сессии,
// в котором она хранится в БД (и в sessions_export.json импортёра).
type SessionCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"` // unix; 0 — сессионная
	Secure  bool    `json:"secure"`
}

// CookiesFromJSON разворачивает сохранённые куки в http.Cookie, отбрасывая
// протухшие. Пустой результат при непустом входе — признак истёкшей сессии.
func CookiesFromJSON(data []byte, now time.Time) ([]*http.Cookie, error) {
	var stored []SessionCookie
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("разбор кук сессии: %w", err)
	}
	var cookies []*http.Cookie
	for _, c := range stored {
		if c.Expires > 0 && time.Unix(int64(c.Expires), 0).Before(now) {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value})
	}
	return cookies, nil
}

// CookiesToJSON сериализует куки для хранения в БД.
func CookiesToJSON(cookies []*http.Cookie, now time.Time) (string, error) {
	stored := make([]SessionCookie, 0, len(cookies))
	for _, c := range cookies {
		sc := SessionCookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
			Path:   c.Path,
			Secure: c.Secure,
		}
		if !c.Expires.IsZero() {
			sc.Expires = float64(c.Expires.Unix())
		}
		stored = append(stored, sc)
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
