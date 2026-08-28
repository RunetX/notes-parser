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
	"log/slog"
	"net/http"
	"strings"
	"sync"
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
	// CacheSystem — ставить кэш-точку на системный блок: следующий запрос с ТЕМ
	// ЖЕ системным промптом читает его из кэша по 0,1 цены вместо полной.
	//
	// Выключено по умолчанию, и это не осторожность, а арифметика: запись в кэш
	// стоит 1,25 цены входа, чтение — 0,1, то есть окупается со ВТОРОГО запроса
	// (1,25 + 0,1 против 2,0), а до него это наценка. Одиночному вызову —
	// рубрике дайджеста, утренней заметке — второго запроса не будет никогда,
	// поэтому наценка досталась бы ему целиком. Включать там, где повтор
	// ДОКАЗАН, а не вероятен: раунды voice идут встык с одним и тем же
	// системным промптом, а вот амвон пишет под разные заметки с промежутком в
	// минуты и часы, и пятиминутный кэш до второго вызова обычно не доживает.
	//
	// Короткий префикс (у claude-opus-5 граница 512 токенов) не кэшируется
	// вовсе — молча, без ошибки и без наценки: сама пометка безопасна, просто
	// бесполезна.
	CacheSystem bool
	// Log — куда писать расход по каждому запросу (уровень Debug: это
	// подробность на разбор, а не событие). nil — slog.Default().
	Log *slog.Logger
}

// Usage — израсходованное клиентом с момента создания. Нужен потому, что счёт
// приходит помесячно и общий на всё, а вопрос «во что обошёлся ЭТОТ прогон»
// задаётся сразу после него: калибровочный прогон эмуляции стоит десятки
// долларов, и решать по нему, брать ли модель дешевле, можно только зная цифру.
type Usage struct {
	Calls               int64
	InputTokens         int64 // вход, посчитанный заново (мимо кэша)
	OutputTokens        int64
	CacheCreationTokens int64 // записано в кэш (1,25 цены входа)
	CacheReadTokens     int64 // прочитано из кэша (0,1 цены входа)
}

// String — строка для отчёта прогона. Чтения кэша названы отдельно: по ним
// видно, сработала ли кэш-точка, а это не вопрос вкуса — нулевые чтения при
// включённом CacheSystem означают, что префикс кто-то ломает.
func (u Usage) String() string {
	return fmt.Sprintf("вызовов %d, вход %d (записано в кэш %d, прочитано из кэша %d), выход %d",
		u.Calls, u.InputTokens, u.CacheCreationTokens, u.CacheReadTokens, u.OutputTokens)
}

// Client — тонкая обёртка над официальным SDK.
type Client struct {
	api         anthropic.Client
	model       string
	maxTokens   int64
	effort      anthropic.OutputConfigEffort
	cacheSystem bool
	log         *slog.Logger

	mu    sync.Mutex
	usage Usage
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
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		api:         anthropic.NewClient(opts...),
		model:       model,
		maxTokens:   maxTokens,
		effort:      anthropic.OutputConfigEffort(cfg.Effort),
		cacheSystem: cfg.CacheSystem,
		log:         log,
	}
}

// Model — имя модели (для логов: результат осмыслен против конкретной версии).
func (c *Client) Model() string { return c.model }

// Usage — сколько израсходовано с момента создания клиента.
func (c *Client) Usage() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

// GenerateJSON выполняет один запрос и требует ответ строго по JSON-схеме
// (structured outputs). Возвращает сырой JSON ответа.
func (c *Client) GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error) {
	sys := anthropic.TextBlockParam{Text: system}
	if c.cacheSystem && system != "" {
		sys.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: c.maxTokens,
		System:    []anthropic.TextBlockParam{sys},
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
	// Расход считается ДО разбора ответа: отказ классификатора и обрыв по
	// бюджету оплачены так же, как удача, и потерянный на них счёт — ровно та
	// строка, которую ищут, когда прогон вышел дороже ожидаемого.
	c.record(resp.Usage)
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

// record — накопить расход одного ответа и записать его в лог.
func (c *Client) record(u anthropic.Usage) {
	c.mu.Lock()
	c.usage.Calls++
	c.usage.InputTokens += u.InputTokens
	c.usage.OutputTokens += u.OutputTokens
	c.usage.CacheCreationTokens += u.CacheCreationInputTokens
	c.usage.CacheReadTokens += u.CacheReadInputTokens
	c.mu.Unlock()

	c.log.Debug("расход модели",
		"model", c.model,
		"input", u.InputTokens,
		"cache_write", u.CacheCreationInputTokens,
		"cache_read", u.CacheReadInputTokens,
		"output", u.OutputTokens)
}
