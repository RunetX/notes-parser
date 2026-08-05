package main

// Общий конструктор клиента Claude API для всех команд, которым нужна онлайн-LLM
// (дайджест, генерация в манере автора). Живёт отдельно от команд намеренно:
// иначе каждая новая команда тянула бы к себе импорт tgx ради транспорта, и
// проверить «эта команда никуда не публикует» по списку импортов стало бы нельзя.

import (
	"errors"

	"lovegw/internal/config"
	"lovegw/internal/llm"
	"lovegw/internal/tgx"
)

// llmClient строит LLM-клиента по конфигу; запросы идут через telegram_proxy —
// api.anthropic.com, как и Bot API, недоступен с российского IP.
func llmClient(cfg *config.Config) (*llm.Client, error) {
	if cfg.LLM.APIKey == "" {
		return nil, errors.New("llm.api_key не задан (секция llm конфига или env LOVEGW_LLM_KEY)")
	}
	transport, err := tgx.ProxyTransport(cfg.TelegramProxy)
	if err != nil {
		return nil, err
	}
	return llm.New(llm.Config{
		APIKey:    cfg.LLM.APIKey,
		Model:     cfg.LLM.Model,
		MaxTokens: cfg.LLM.MaxTokens,
		Transport: transport,
	}, ""), nil
}
