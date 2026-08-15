package main

// Общий конструктор клиента Claude API для всех команд, которым нужна онлайн-LLM
// (дайджест, генерация в манере автора). Живёт отдельно от команд намеренно:
// иначе каждая новая команда тянула бы к себе импорт tgx ради транспорта, и
// проверить «эта команда никуда не публикует» по списку импортов стало бы нельзя.

import (
	"errors"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/llm"
	"lovegw/internal/tgx"
)

// llmClient строит LLM-клиента по конфигу; запросы идут через telegram_proxy —
// api.anthropic.com, как и Bot API, недоступен с российского IP.
func llmClient(cfg *config.Config) (*llm.Client, error) {
	return llmClientFor(cfg, cfg.LLM.Model, "", 0)
}

// llmClientFor — клиент под конкретную задачу: ключ и прокси общие, а модель,
// усердие и таймаут свои. Нужен амвону: ему важны секунды (модель по умолчанию
// думает, и это скрытая задержка), а дайджесту — качество, и менять его
// поведение заодно нельзя. Пустые model/effort и нулевой timeout означают
// «как в секции llm».
func llmClientFor(cfg *config.Config, model, effort string, timeout time.Duration) (*llm.Client, error) {
	if cfg.LLM.APIKey == "" {
		return nil, errors.New("llm.api_key не задан (секция llm конфига или env LOVEGW_LLM_KEY)")
	}
	transport, err := tgx.ProxyTransport(cfg.TelegramProxy)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = cfg.LLM.Model
	}
	return llm.New(llm.Config{
		APIKey:    cfg.LLM.APIKey,
		Model:     model,
		MaxTokens: cfg.LLM.MaxTokens,
		Transport: transport,
		Effort:    effort,
		Timeout:   timeout,
	}, ""), nil
}
