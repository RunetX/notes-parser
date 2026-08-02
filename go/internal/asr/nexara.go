package asr

// Клиент Nexara — российского облачного ASR с API, совместимым с OpenAI
// Whisper (POST /audio/transcriptions, multipart, Bearer-ключ). Сервис
// российский, поэтому ходим напрямую, мимо telegram_proxy.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// nexaraBaseURL — боевой адрес API (клиент дописывает /audio/transcriptions).
const nexaraBaseURL = "https://api.nexara.ru/api/v1"

// defaultModel — единственная модель Nexara на момент подключения.
const defaultModel = "whisper-1"

// language — язык треда. Явное указание надёжнее автодетекта Whisper:
// на коротких репликах он путает русский с соседними языками.
const language = "ru"

// defaultTimeout — таймаут одного запроса к провайдеру.
const defaultTimeout = 60 * time.Second

// transcribeRetries — число попыток при временных сбоях (сеть, 429, 5xx).
const transcribeRetries = 3

// retryBase — база экспоненциального backoff. Переменная, а не константа,
// чтобы тесты ужимали задержку (идиома maxx.retryAfter).
var retryBase = time.Second

// ErrAuth — провайдер отверг ключ или на балансе нет денег (401/402).
// Ретраить бессмысленно: нужен человек.
var ErrAuth = errors.New("провайдер ASR отверг ключ или баланс исчерпан")

// NexaraConfig — параметры клиента.
type NexaraConfig struct {
	APIKey  string
	Model   string        // пусто — defaultModel
	Timeout time.Duration // 0 — defaultTimeout
}

// Nexara — HTTP-клиент провайдера.
type Nexara struct {
	hc      *http.Client
	baseURL string
	apiKey  string
	model   string
	log     *slog.Logger
}

// NewNexara создаёт клиента. baseURL переопределяет адрес API (тесты против
// httptest, другой совместимый провайдер); пусто — боевой Nexara.
func NewNexara(cfg NexaraConfig, baseURL string, log *slog.Logger) *Nexara {
	if baseURL == "" {
		baseURL = nexaraBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Nexara{
		hc:      &http.Client{Timeout: timeout},
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   model,
		log:     log,
	}
}

// Transcribe распознаёт аудио с ретраями и экспоненциальным backoff.
// Тело читаем целиком: повторная попытка должна отправить те же байты заново.
func (n *Nexara) Transcribe(ctx context.Context, wav io.Reader) (string, error) {
	audio, err := io.ReadAll(wav)
	if err != nil {
		return "", fmt.Errorf("чтение аудио: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < transcribeRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * retryBase
			backoff += time.Duration(rand.Int64N(int64(backoff/2) + 1))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		text, retriable, err := n.transcribeOnce(ctx, audio)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if !retriable {
			return "", err
		}
		n.log.Warn("повтор запроса к ASR", "attempt", attempt+1, "err", err)
	}
	return "", fmt.Errorf("распознавание: попытки исчерпаны: %w", lastErr)
}

// transcribeOnce — одна попытка. retriable отделяет временные сбои (сеть,
// 429, 5xx) от окончательных (ключ, баланс, кривой запрос).
func (n *Nexara) transcribeOnce(ctx context.Context, audio []byte) (text string, retriable bool, err error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", false, err
	}
	if _, err := part.Write(audio); err != nil {
		return "", false, err
	}
	for field, value := range map[string]string{
		"model":           n.model,
		"language":        language,
		"response_format": "json",
	} {
		if err := mw.WriteField(field, value); err != nil {
			return "", false, err
		}
	}
	if err := mw.Close(); err != nil {
		return "", false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.baseURL+"/audio/transcriptions", &body)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+n.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := n.hc.Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", false, fmt.Errorf("разбор ответа ASR: %w", err)
		}
		return strings.TrimSpace(out.Text), false, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusPaymentRequired:
		return "", false, fmt.Errorf("%w (статус %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return "", true, fmt.Errorf("ASR: статус %d: %s", resp.StatusCode, errBody(resp.Body))
	default:
		return "", false, fmt.Errorf("ASR: статус %d: %s", resp.StatusCode, errBody(resp.Body))
	}
}

// errBody вытаскивает начало тела ошибки для лога.
func errBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
