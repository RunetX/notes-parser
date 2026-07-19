// Пакет config загружает конфигурацию lovegw из JSON-файла
// с переопределением секретов через переменные окружения.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Site — параметры доступа к сайту love.ngs.ru.
type Site struct {
	BaseURL           string `json:"base_url"`
	UserAgent         string `json:"user_agent"`
	RequestIntervalMS int    `json:"request_interval_ms"`
}

// MirrorBot — бот, постящий в канал и слушающий группу обсуждения.
type MirrorBot struct {
	Token            string `json:"token"`
	ChannelID        int64  `json:"channel_id"`
	DiscussionChatID int64  `json:"discussion_chat_id"`
}

// DMBot — бот личных сообщений (РюмкинЪ).
type DMBot struct {
	Token string `json:"token"`
}

type Config struct {
	Site          Site      `json:"site"`
	MirrorBot     MirrorBot `json:"mirror_bot"`
	DMBot         DMBot     `json:"dm_bot"`
	NotesLimit    int       `json:"notes_limit"`
	Signature     string    `json:"signature"`
	AdminTGUserID int64     `json:"admin_tg_user_id"`
	FeedIntervalS int       `json:"feed_interval_s"`
	DBPath        string    `json:"db_path"`
	LogLevel      string    `json:"log_level"`
	// TelegramProxy — прокси для Bot API (http/https/socks5), если Telegram
	// недоступен напрямую. Сайт при этом идёт мимо прокси. Пусто — напрямую.
	TelegramProxy string `json:"telegram_proxy"`
}

// Load читает конфиг, накладывает значения по умолчанию и env-переопределения:
// LOVEGW_MIRROR_TOKEN, LOVEGW_DM_TOKEN, LOVEGW_DB_PATH.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Site: Site{
			BaseURL:           "https://love.ngs.ru",
			UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
			RequestIntervalMS: 2000,
		},
		NotesLimit:    5,
		FeedIntervalS: 60,
		DBPath:        "data/lovegw.db",
		LogLevel:      "info",
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение конфига: %w", err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	if v := os.Getenv("LOVEGW_MIRROR_TOKEN"); v != "" {
		cfg.MirrorBot.Token = v
	}
	if v := os.Getenv("LOVEGW_DM_TOKEN"); v != "" {
		cfg.DMBot.Token = v
	}
	if v := os.Getenv("LOVEGW_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("LOVEGW_TG_PROXY"); v != "" {
		cfg.TelegramProxy = v
	}

	if cfg.Site.BaseURL == "" {
		return nil, fmt.Errorf("site.base_url не задан")
	}
	return cfg, nil
}
