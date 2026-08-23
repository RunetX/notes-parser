// Пакет rullm — онлайн-LLM российского провайдера (Yandex AI Studio) для задач,
// в которых в модель уходит текст участников площадки.
//
// Зачем он есть отдельно от `llm`. Не ради цены и не ради удобства: в
// опубликованном согласии (`platform/consents/processing.v2.txt`) площадка
// обещает людям «не вывозит данные за пределы России», а классификатор
// модерации отправляет им же написанное. С Claude это расхождение с бумагой,
// которую участники уже подписали, — и починить его правкой текста нельзя:
// опубликованная редакция неизменяема, а новая обесценивает все прежние
// согласия (`ConsentState.Has` требует `Version >= version`). Поэтому переезд
// на российский сервис — не оптимизация, а условие того, чтобы автомат
// модерации вообще можно было включить.
//
// Контракт ровно тот же, что у `llm.Client`: один метод `GenerateJSON`, потому
// что `platmod.JSONGenerator` именно им и описан. Пакет `platmod` о провайдере
// не знает ничего, и это не должно измениться — иначе политика решений начнёт
// зависеть от того, чья модель отвечает.
//
// **Прокси здесь нет намеренно.** `llm` ходит через `telegram_proxy`, потому
// что api.anthropic.com недоступен с российского IP; у российского сервиса всё
// наоборот — он доступен прямо, как love.ngs.ru. Это заодно снимает с
// модерации зависимость от прокси, который держится ради Telegram.
package rullm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL — OpenAI-совместимый эндпоинт AI Studio.
const DefaultBaseURL = "https://llm.api.cloud.yandex.net/v1"

// DefaultModel — модель триажа, и выбрана она НЕ по цене.
//
// Первым дефолтом была `yandexgpt-lite/latest`: замер на смешанной пачке из
// десятка строк дал те же вердикты, что gpt-oss-120b, вдвое дешевле. Замер был
// слишком мал. На живом треде (115 реплик, 23.08.2026) `lite` придирается вдвое
// чаще — четыре автоскрытия против двух, — а идёт вчетверо дольше.
//
// Главное же — ВОСПРОИЗВОДИМОСТЬ. Модели с размышлениями (`gpt-oss-120b`)
// плавают даже с зерном: три прогона по одним и тем же репликам дают три разных
// набора скрытого, и «уверена» перестаёт что-либо значить. `yandexgpt-5.1`
// размышлений не ведёт, и с зерном три прогона совпали ЗНАК В ЗНАК; она же
// оказалась самой быстрой из десяти проверенных (17 с против 38–85).
//
// Правило на будущее: модель классификатора выбирается прогоном стенда
// (`platform triage`) НЕСКОЛЬКО РАЗ подряд — одиночный прогон не отличает
// придирчивость от разброса.
const DefaultModel = "yandexgpt-5.1/latest"

// defaultMaxTokens — потолок ответа. Меньше, чем у `llm`: здесь не сочиняют
// тексты, а отвечают по схеме, и пачка из десяти вердиктов укладывается в
// ~600 токенов (замер там же).
const defaultMaxTokens = 8192

// defaultTimeout — потолок одного запроса.
const defaultTimeout = 90 * time.Second

// retries — сколько раз повторяем при 429 и 5xx. Немного, и вот почему: у
// вызывающего свой счётчик попыток (`platmod` даёт строке три захода), и
// мгновенная сетевая икота не должна съедать один из них целиком.
const retries = 2

// seed — зерно выборки, одно и то же у каждого запроса.
//
// Одной температуры 0 НЕ ХВАТАЕТ, и это замер, а не предположение: 23.08.2026
// пять прогонов классификатора по одним и тем же 115 репликам дали пять разных
// наборов скрытого — одна и та же реплика в трёх прогонах звалась угрозой, в
// двух чистой. Отсюда и цена: право автомата убрать чужие слова держится на
// поле «уверена», а уверенность, меняющаяся от прогона к прогону, не значит
// ничего — ни объяснить решение человеку, ни повторить его на стенде нельзя.
//
// Зерно вернуло повторяемость (проба на живом API: температура 1 с зерном —
// три одинаковых ответа подряд). Оно НАМЕРЕННО не в конфиге: это не настройка
// вкуса, а условие того, что вердикт вообще что-то значит.
const seed = 1

// Config — параметры клиента.
type Config struct {
	// APIKey — ключ сервисного аккаунта (заголовок «Authorization: Api-Key»).
	APIKey string
	// FolderID — каталог Yandex Cloud. Нужен дважды: заголовком и внутри имени
	// модели, поэтому в конфиге он лежит отдельным полем, а URI собирается
	// здесь: иначе folder_id пришлось бы вписывать в `moderation.model`, и он
	// же уезжал бы в карточку проверки, где не нужен вовсе.
	FolderID string
	// Model — имя модели без folder, вида «yandexgpt-lite/latest». Пусто —
	// DefaultModel.
	Model string
	// MaxTokens — 0 означает defaultMaxTokens.
	MaxTokens int
	// Timeout — потолок одного запроса; 0 — defaultTimeout.
	Timeout time.Duration
}

// Client — клиент AI Studio.
type Client struct {
	http      *http.Client
	baseURL   string
	key       string
	folder    string
	model     string
	maxTokens int
}

// ErrFiltered — провайдер отказался обрабатывать сам ВХОДНОЙ текст.
//
// Для классификатора это рабочий случай, а не диковина: ему по устройству дают
// ровно тот текст, на который жалуются. Отдельной обработки в `platmod` нет и
// не нужно — исчерпав попытки, строка остаётся в очереди непроверенной, то есть
// достаётся живому модератору. Это и есть верный исход; ошибка здесь нужна
// лишь затем, чтобы в логе он не выглядел поломкой сети.
var ErrFiltered = errors.New("провайдер отклонил входной текст")

// New создаёт клиента. baseURL пусто — боевой DefaultBaseURL (переопределяется
// в тестах и на случай смены адреса).
func New(cfg Config, baseURL string) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("ключ не задан")
	}
	if cfg.FolderID == "" {
		return nil, errors.New("folder_id не задан")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	c := &Client{
		http:      &http.Client{Timeout: timeout},
		baseURL:   strings.TrimSuffix(cmp.Or(baseURL, DefaultBaseURL), "/"),
		key:       cfg.APIKey,
		folder:    cfg.FolderID,
		model:     cmp.Or(cfg.Model, DefaultModel),
		maxTokens: cfg.MaxTokens,
	}
	if c.maxTokens == 0 {
		c.maxTokens = defaultMaxTokens
	}
	return c, nil
}

// Model — имя модели для логов и карточки проверки. Без folder_id: в карточке
// он шум, а решение осмыслено против модели, а не против каталога.
func (c *Client) Model() string { return c.model }

// request — тело запроса. Поля названы по OpenAI-совместимой схеме AI Studio.
type request struct {
	Model          string    `json:"model"`
	Messages       []message `json:"messages"`
	ResponseFormat *format   `json:"response_format,omitempty"`
	MaxTokens      int       `json:"max_tokens"`
	Temperature    float64   `json:"temperature"`
	Seed           int       `json:"seed"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type format struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name string `json:"name"`
	// Strict — «соблюдай схему буквально». Проба 23.08.2026 показала, что
	// AI Studio его принимает и держит: enum категорий и
	// additionalProperties: false соблюдены, номер приходит на каждую строку
	// пачки. Без этого поля гарантии нет, и разбор ответа пришлось бы
	// обвешивать проверками на стороне вызывающего.
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type response struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// GenerateJSON выполняет один запрос и требует ответ строго по JSON-схеме.
// Возвращает сырой JSON ответа — как и `llm.Client`, разбирать его дело
// вызывающего.
func (c *Client) GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error) {
	body, err := json.Marshal(request{
		Model: "gpt://" + c.folder + "/" + c.model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: prompt},
		},
		ResponseFormat: &format{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: "response", Strict: true, Schema: schema},
		},
		MaxTokens: c.maxTokens,
		// Триаж, а не сочинение: разброс здесь только мешает воспроизводимости
		// решения, которое потом придётся объяснять человеку.
		Temperature: 0,
		Seed:        seed,
	})
	if err != nil {
		return nil, fmt.Errorf("сборка запроса: %w", err)
	}

	var resp *response
	for attempt := 0; ; attempt++ {
		resp, err = c.once(ctx, body)
		if err == nil || attempt >= retries || !retryable(err) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	if err != nil {
		return nil, err
	}

	if len(resp.Choices) == 0 {
		return nil, errors.New("пустой ответ модели: нет choices")
	}
	ch := resp.Choices[0]
	// Обрыв по бюджету проверяется ДО пустоты: оборванный ответ приходит с
	// текстом, но невалидным JSON, и «не разобрался» увело бы от настоящей
	// причины (тот же порядок, что в пакете llm).
	if ch.FinishReason == "length" {
		return nil, fmt.Errorf("ответ оборван по max_tokens=%d", c.maxTokens)
	}
	if ch.FinishReason == "content_filter" {
		return nil, ErrFiltered
	}
	if strings.TrimSpace(ch.Message.Content) == "" {
		return nil, fmt.Errorf("пустой ответ модели (finish_reason %q)", ch.FinishReason)
	}
	return []byte(ch.Message.Content), nil
}

// httpError — отказ провайдера с кодом; отдельным типом, чтобы решать про
// повтор по коду, а не по тексту сообщения.
type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("AI Studio ответила %d: %s", e.code, e.body)
}

// retryable — стоит ли повторять: перегрузка и внутренние ошибки да, отказ по
// ключу или схеме нет (повтор их не вылечит, а попытку сожжёт).
func retryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.code == http.StatusTooManyRequests || he.code >= 500
	}
	// Сетевая ошибка: соединение оборвалось, DNS не ответил.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// once — одна попытка.
func (c *Client) once(ctx context.Context, body []byte) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Api-Key "+c.key)
	// x-folder-id нужен помимо folder внутри имени модели: по нему считается
	// расход каталога.
	req.Header.Set("x-folder-id", c.folder)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("запрос к AI Studio: %w", err)
	}
	defer res.Body.Close() //nolint:errcheck // тело только читаем

	// Потолок на чтение: отвечает чужой сервис, и «ответ на гигабайт» не должен
	// становиться нашей проблемой.
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("чтение ответа: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, &httpError{code: res.StatusCode, body: excerpt(raw)}
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("разбор ответа AI Studio: %w (%s)", err, excerpt(raw))
	}
	return &out, nil
}

// excerpt — кусок чужого ответа для сообщения об ошибке. Целиком его в лог
// класть незачем, а без него отказ читается как «что-то пошло не так».
func excerpt(b []byte) string {
	const max = 400
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
