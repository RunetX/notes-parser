package love

// JSON/AJAX-транспорт talks: авторизованный GET `/ajax?request=…` (JSON-ответ) и
// JSON-RPC POST `/ajax/` (getMessagesHistory/sendMessage, params позиционные).
// Авторизация — по кукам сессии; `Love.token` в теле не нужен (снято в Ф0).
// Неавторизованный запрос отдаёт HTTP 200 + тело с «Ошибка авторизации», а не
// 401/403 — отсюда ErrUnauthorized по маркеру в теле.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrUnauthorized — сессия сайта недействительна: гостевой ответ talks
// (200 + «Ошибка авторизации»). Наверху → пометить сессию invalid и позвать /login.
var ErrUnauthorized = errors.New("сессия сайта недействительна (гостевой ответ talks)")

// ErrSiteUnavailable — сайт временно недоступен: 5xx фронта (гейтвей DDoS-Guard
// или бэкенд НГС) либо обрыв соединения. В отличие от 403 и дрейфа API это
// проходит само, поэтому наверху — повтор на следующем такте, а не kill-switch.
var ErrSiteUnavailable = errors.New("сайт временно недоступен")

// guestAuthMarker — текст ошибки авторизации в теле гостевого ответа.
const guestAuthMarker = "Ошибка авторизации"

const jsonBodyLimit = 8 << 20

// SchemaError — обязательное поле JSON/HTML-ответа отсутствует или не той формы
// (дрейф API talks; аналог MarkupError для JSON-стороны).
type SchemaError struct {
	Op     string
	Detail string
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("дрейф API talks (%s): %s", e.Op, e.Detail)
}

// getJSONBody выполняет авторизованный GET JSON-эндпоинта `/ajax?request=…`.
func (c *Client) getJSONBody(ctx context.Context, path string, cookies []*http.Cookie) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setJSONHeaders(req, cookies)
	return c.doJSON(req, path)
}

// rpc выполняет JSON-RPC 2.0 вызов POST `/ajax/`. Возвращает сырое поле result.
// Формы result различаются по методу: getMessagesHistory → {html}, sendMessage →
// строка-HTML — разбор оставлен вызывающему (talks.go).
func (c *Client) rpc(ctx context.Context, cookies []*http.Cookie, method string, params ...any) (json.RawMessage, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params, "id": 1,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ajax/", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	c.setJSONHeaders(req, cookies)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", c.baseURL)
	body, err := c.doJSON(req, method)
	if err != nil {
		return nil, err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, &SchemaError{Op: method, Detail: "ответ не JSON-RPC: " + err.Error()}
	}
	if env.Error != nil {
		return nil, fmt.Errorf("RPC %s: %s (код %d)", method, env.Error.Message, env.Error.Code)
	}
	if len(env.Result) == 0 {
		return nil, &SchemaError{Op: method, Detail: "пустой result"}
	}
	return env.Result, nil
}

func (c *Client) setJSONHeaders(req *http.Request, cookies []*http.Cookie) {
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
}

// doJSON выполняет запрос (лимитер уже пройден вызывающим) и классифицирует:
// 403 → ErrForbidden, гостевой ответ (200 + «Ошибка авторизации») → ErrUnauthorized,
// 5xx и обрыв связи → ErrSiteUnavailable (временно, повторяемо),
// прочие не-200 → ошибка со статусом.
func (c *Client) doJSON(req *http.Request, op string) ([]byte, error) {
	resp, err := c.hc.Do(req)
	if err != nil {
		// Обрыв или таймаут — временный отказ. Отмену контекста не маскируем:
		// это штатное завершение демона, а не сбой сайта.
		if req.Context().Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: %w: %v", op, ErrSiteUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrForbidden
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jsonBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("%s: чтение ответа: %w: %v", op, ErrSiteUnavailable, err)
	}
	if resp.StatusCode >= 500 {
		// 502/503/504 — фронт моргнул: ни бан, ни дрейф API. Один такт пропущен,
		// следующий запрос идёт как обычно.
		return nil, fmt.Errorf("%s: статус %d: %w", op, resp.StatusCode, ErrSiteUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: статус %d", op, resp.StatusCode)
	}
	if bytes.Contains(body, []byte(guestAuthMarker)) {
		return nil, ErrUnauthorized
	}
	return body, nil
}
