package love

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

const loginPath = "/ajax?request=login"

// LoginError — сайт принял запрос, но отверг учётные данные.
// Errors — текст ошибки в том виде, как его вернул сайт.
type LoginError struct {
	Errors string
}

func (e *LoginError) Error() string {
	return "вход не выполнен: " + e.Errors
}

// loginResponse — JSON-ответ сайта на попытку входа.
// errors бывает разной формы, поэтому RawMessage.
type loginResponse struct {
	Login struct {
		Result bool            `json:"result"`
		Errors json.RawMessage `json:"errors"`
	} `json:"login"`
}

// Login выполняет вход на сайт и возвращает куки установленной сессии.
// Куки собираются через отдельный cookie jar: сайт может ставить их
// и в редиректах.
func (c *Client) Login(ctx context.Context, login, password string) ([]*http.Cookie, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: c.hc.Timeout, Jar: jar}

	form := url.Values{
		"login":    {login},
		"password": {password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+loginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("вход: сайт вернул статус %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var lr loginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return nil, fmt.Errorf("вход: неожиданный ответ сайта: %w", err)
	}
	if !lr.Login.Result {
		return nil, &LoginError{Errors: rawToText(lr.Login.Errors)}
	}

	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}
	return jar.Cookies(base), nil
}

// rawToText приводит поле errors к человекочитаемой строке.
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "причина не указана"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
