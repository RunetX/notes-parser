// Пакет llm — онлайн-клиент Claude API (Anthropic) для редакторских задач
// проекта (LLM-рубрики дайджеста; далее — смысловой поиск, эпик C4 бэклога).
//
// api.anthropic.com, как и Telegram Bot API, недоступен с российского IP —
// запросы идут через тот же прокси, что и Bot API (telegram_proxy), сайт
// love.ngs.ru при этом остаётся на прямом соединении.
package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel — модель по умолчанию.
const DefaultModel = "claude-opus-5"

// defaultMaxTokens — потолок ответа: рубрики дайджеста короткие, но модель
// думает в тот же бюджет токенов.
const defaultMaxTokens = 8192

// requestTimeout — таймаут одного запроса: модель на высоком effort может
// думать минуты.
const requestTimeout = 5 * time.Minute

// Config — параметры клиента.
type Config struct {
	APIKey    string
	Model     string          // пусто — DefaultModel
	MaxTokens int             // 0 — defaultMaxTokens
	Transport *http.Transport // nil — прямое соединение (tgx.ProxyTransport для прокси)
}

// Client — тонкая обёртка над официальным SDK.
type Client struct {
	api       anthropic.Client
	model     string
	maxTokens int64
}

// ErrRefusal — классификаторы модели отклонили запрос (stop_reason refusal).
var ErrRefusal = errors.New("модель отклонила запрос (refusal)")

// New создаёт клиента. baseURL переопределяет адрес API (тесты против
// httptest); пусто — боевой api.anthropic.com.
func New(cfg Config, baseURL string) *Client {
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(requestTimeout),
	}
	if cfg.Transport != nil {
		opts = append(opts, option.WithHTTPClient(&http.Client{Transport: cfg.Transport}))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := int64(cfg.MaxTokens)
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	return &Client{api: anthropic.NewClient(opts...), model: model, maxTokens: maxTokens}
}

// Model — имя модели (для логов: результат осмыслен против конкретной версии).
func (c *Client) Model() string { return c.model }

// GenerateJSON выполняет один запрос и требует ответ строго по JSON-схеме
// (structured outputs). Возвращает сырой JSON ответа.
func (c *Client) GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error) {
	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: c.maxTokens,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("запрос к %s: %w", c.model, err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return nil, fmt.Errorf("%w: %s", ErrRefusal, resp.StopDetails.Explanation)
	}
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return nil, fmt.Errorf("пустой ответ модели (stop_reason %s)", resp.StopReason)
	}
	if resp.StopReason == anthropic.StopReasonMaxTokens {
		return nil, fmt.Errorf("ответ оборван по max_tokens=%d — поднимите llm.max_tokens", c.maxTokens)
	}
	return []byte(b.String()), nil
}
