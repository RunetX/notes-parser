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

const headerUserAgent = "User-Agent"

const (
	loginPath  = "/ajax?request=login"
	warmupPath = "/notes/"
)

// LoginError — сайт принял запрос, но отверг учётные данные.
// Errors — текст ошибки в том виде, как его вернул сайт.
type LoginError struct {
	Errors string
}

func (e *LoginError) Error() string {
	return "вход не выполнен: " + e.Errors
}

// loginResponse — JSON-ответ сайта на попытку входа. result — про пароль,
// errors бывает разной формы (строка/массив), поэтому RawMessage.
type loginResponse struct {
	Login struct {
		Result bool            `json:"result"`
		Errors json.RawMessage `json:"errors"`
	} `json:"login"`
}

// Login выполняет вход на сайт и возвращает куки сессии (весь jar).
// Флоу как в рабочей Python-версии: прогрев (получить куки DDoS-Guard, чтобы
// последующие POST не упирались в челлендж) → POST логина → успех по
// result:true. Сохраняется весь набор кук — именно он потом авторизует
// публикацию заметок и комментариев.
func (c *Client) Login(ctx context.Context, login, password string) ([]*http.Cookie, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: c.hc.Timeout, Jar: jar}

	if err := c.warmup(ctx, hc); err != nil {
		return nil, fmt.Errorf("вход: прогрев: %w", err)
	}

	lr, err := c.postCredentials(ctx, hc, login, password)
	if err != nil {
		return nil, err
	}
	if !lr.Login.Result {
		return nil, &LoginError{Errors: rawToText(lr.Login.Errors)}
	}
	return jar.Cookies(base), nil
}

// warmup имитирует заход браузера: получает DDoS-Guard и предсессионные куки.
func (c *Client) warmup(ctx context.Context, hc *http.Client) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+warmupPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set(headerUserAgent, c.ua)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// postCredentials шлёт логин/пароль на проверку и разбирает JSON-ответ.
func (c *Client) postCredentials(ctx context.Context, hc *http.Client, login, password string) (loginResponse, error) {
	var lr loginResponse
	if err := c.limiter.Wait(ctx); err != nil {
		return lr, err
	}
	form := url.Values{"login": {login}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+loginPath, strings.NewReader(form.Encode()))
	if err != nil {
		return lr, err
	}
	req.Header.Set(headerUserAgent, c.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return lr, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lr, fmt.Errorf("вход: сайт вернул статус %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonBodyLimit))
	if err != nil {
		return lr, err
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		return lr, fmt.Errorf("вход: неожиданный ответ сайта: %w", err)
	}
	return lr, nil
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
