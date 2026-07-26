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

// MirrorBot — бот, постящий в канал и слушающий группу обсуждения
// (легаси-формат конфига; новый формат — секция messengers).
type MirrorBot struct {
	Token            string `json:"token"`
	ChannelID        int64  `json:"channel_id"`
	DiscussionChatID int64  `json:"discussion_chat_id"`
}

// DMBot — бот личных сообщений (РюмкинЪ; легаси-формат конфига).
type DMBot struct {
	Token string `json:"token"`
}

// Messenger — один мессенджер-приёмник зеркала.
type Messenger struct {
	Enabled          bool   `json:"enabled"`
	Token            string `json:"token"`
	ChannelID        int64  `json:"channel_id"`
	DiscussionChatID int64  `json:"discussion_chat_id"`
	// DMToken — токен ЛС-бота (РюмкинЪ); пока только у telegram.
	DMToken string `json:"dm_token,omitempty"`
	// AdminUserID — id админа в ЭТОМ мессенджере для алертов (у каждого
	// мессенджера своё пространство id). Для telegram по умолчанию берётся
	// admin_tg_user_id.
	AdminUserID int64 `json:"admin_user_id,omitempty"`
}

// Messengers — гейт мессенджеров: какие приёмники включены. При включении
// нескольких зеркалим во все параллельно (fan-out).
type Messengers struct {
	Max      Messenger `json:"max"`
	Telegram Messenger `json:"telegram"`
}

type Config struct {
	Site          Site        `json:"site"`
	MirrorBot     MirrorBot   `json:"mirror_bot"`
	DMBot         DMBot       `json:"dm_bot"`
	Messengers    *Messengers `json:"messengers"`
	NotesLimit    int         `json:"notes_limit"`
	Signature     string      `json:"signature"`
	AdminTGUserID int64       `json:"admin_tg_user_id"`
	FeedIntervalS int         `json:"feed_interval_s"`
	DBPath        string      `json:"db_path"`
	LogLevel      string      `json:"log_level"`
	// TelegramProxy — прокси для Bot API (http/https/socks5), если Telegram
	// недоступен напрямую. Сайт и MAX при этом идут мимо прокси. Пусто —
	// напрямую.
	TelegramProxy string `json:"telegram_proxy"`
}

// Load читает конфиг, накладывает значения по умолчанию и env-переопределения:
// LOVEGW_MIRROR_TOKEN, LOVEGW_DM_TOKEN, LOVEGW_MAX_TOKEN, LOVEGW_DB_PATH.
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
	cfg.normalizeMessengers()
	if v := os.Getenv("LOVEGW_MAX_TOKEN"); v != "" {
		cfg.Messengers.Max.Token = v
	}

	if cfg.Site.BaseURL == "" {
		return nil, fmt.Errorf("site.base_url не задан")
	}
	return cfg, nil
}

// normalizeMessengers приводит конфиг к секции messengers. Без секции —
// обратная совместимость: плоские mirror_bot/dm_bot читаются как telegram
// (включён по факту наличия токена). Telegram-поля, не заданные в секции,
// добираются из плоских (env LOVEGW_MIRROR_TOKEN попадает в оба формата).
func (c *Config) normalizeMessengers() {
	if c.Messengers == nil {
		c.Messengers = &Messengers{
			Telegram: Messenger{
				Enabled:          c.MirrorBot.Token != "",
				Token:            c.MirrorBot.Token,
				ChannelID:        c.MirrorBot.ChannelID,
				DiscussionChatID: c.MirrorBot.DiscussionChatID,
				DMToken:          c.DMBot.Token,
			},
		}
	}
	tg := &c.Messengers.Telegram
	if tg.Token == "" {
		tg.Token = c.MirrorBot.Token
	}
	if tg.ChannelID == 0 {
		tg.ChannelID = c.MirrorBot.ChannelID
	}
	if tg.DiscussionChatID == 0 {
		tg.DiscussionChatID = c.MirrorBot.DiscussionChatID
	}
	if tg.DMToken == "" {
		tg.DMToken = c.DMBot.Token
	}
	if tg.AdminUserID == 0 {
		tg.AdminUserID = c.AdminTGUserID
	}
}
