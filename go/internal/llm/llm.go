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

// defaultMaxTokens — потолок ответа. Это ПОТОЛОК, а не расход, поэтому берём с
// запасом: рубрики дайджеста короткие, но voice просит по три черновика заметки
// сразу, а размышление модели идёт в тот же бюджет — на 8192 генерация длинных
// заметок обрывалась так, что текста не оставалось вовсе.
const defaultMaxTokens = 16384

// requestTimeout — таймаут одного запроса: модель на высоком effort может
// думать минуты.
const requestTimeout = 5 * time.Minute

// Config — параметры клиента.
type Config struct {
	APIKey    string
	Model     string          // пусто — DefaultModel
	MaxTokens int             // 0 — defaultMaxTokens
	Transport *http.Transport // nil — прямое соединение (tgx.ProxyTransport для прокси)
	// Effort — сколько модели думать (low|medium|high|xhigh|max). Пусто —
	// как решит модель (adaptive), то есть сегодняшнее поведение дайджеста.
	// Это единственный рычаг задержки: claude-opus-5 думает по умолчанию, а
	// выключать размышление нельзя — с thinking: disabled модель роняет
	// <thinking>-теги в видимый ответ. Амвону нужен «low»: он гонится за
	// секундами, чтобы успеть первым под свежую заметку.
	Effort string
	// Timeout — потолок одного запроса. 0 — requestTimeout пакета (5 мин).
	Timeout time.Duration
}

// Client — тонкая обёртка над официальным SDK.
type Client struct {
	api       anthropic.Client
	model     string
	maxTokens int64
	effort    anthropic.OutputConfigEffort
}

// ErrRefusal — классификаторы модели отклонили запрос (stop_reason refusal).
var ErrRefusal = errors.New("модель отклонила запрос (refusal)")

// New создаёт клиента. baseURL переопределяет адрес API (тесты против
// httptest); пусто — боевой api.anthropic.com.
func New(cfg Config, baseURL string) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	opts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithRequestTimeout(timeout),
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
	return &Client{
		api:       anthropic.NewClient(opts...),
		model:     model,
		maxTokens: maxTokens,
		effort:    anthropic.OutputConfigEffort(cfg.Effort),
	}
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
			// Пустой effort SDK не сериализует (omitzero) — запрос остаётся
			// ровно таким, каким был до появления рычага.
			Effort: c.effort,
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
	// Обрыв по бюджету проверяется ДО пустоты: при adaptive thinking весь бюджет
	// может уйти в размышление, текста не остаётся вовсе, и «пустой ответ» уводит
	// от настоящей причины.
	if resp.StopReason == anthropic.StopReasonMaxTokens {
		return nil, fmt.Errorf("ответ оборван по max_tokens=%d — поднимите llm.max_tokens", c.maxTokens)
	}
	if b.Len() == 0 {
		return nil, fmt.Errorf("пустой ответ модели (stop_reason %s)", resp.StopReason)
	}
	return []byte(b.String()), nil
}
