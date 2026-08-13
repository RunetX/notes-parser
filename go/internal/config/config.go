// Пакет config загружает конфигурацию lovegw из JSON-файла
// с переопределением секретов через переменные окружения.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	// DMToken — токен ЛС-бота команд (РюмкинЪ: /login, заметки, подписки).
	// У telegram это исторический второй бот; у max команды обслуживает сам
	// бот зеркала, поэтому поле здесь устарело и трактуется как TalksToken.
	DMToken string `json:"dm_token,omitempty"`
	// TalksToken — токен бота личной переписки сайта (talks): доставка входящих
	// ЛС, ответ реплаем, /talks и /talk, плюс алерты админу. Пусто — переписку
	// обслуживает бот команд (telegram) или бот зеркала (max), как раньше.
	TalksToken string `json:"talks_token,omitempty"`
	// AdminUserID — id админа в ЭТОМ мессенджере для алертов (у каждого
	// мессенджера своё пространство id). Для telegram по умолчанию берётся
	// admin_tg_user_id.
	AdminUserID int64 `json:"admin_user_id,omitempty"`
	// Signature — подпись под постами в ЭТОМ мессенджере (например, ссылка
	// на свой канал). Пусто — наследуется общий signature.
	Signature string `json:"signature,omitempty"`
}

// Messengers — гейт мессенджеров: какие приёмники включены. При включении
// нескольких зеркалим во все параллельно (fan-out).
type Messengers struct {
	Max      Messenger `json:"max"`
	Telegram Messenger `json:"telegram"`
}

// Talks — гейт личной переписки сайта (talks): один поллер фанит входящие ЛС в
// личку включённых мессенджеров, ответы реплаем/командой уходят на сайт. По
// умолчанию выключен; admin_only и read-only (allow_send=false) — безопасный
// старт. store_text=false — в БД только метаданные, текст живёт в мессенджере.
type Talks struct {
	Enabled           bool `json:"enabled"`
	AdminOnly         bool `json:"admin_only"`
	AllowSend         bool `json:"allow_send"`
	PollIntervalS     int  `json:"poll_interval_s"`
	IdlePollIntervalS int  `json:"idle_poll_interval_s"`
	MaxDialogsPerTick int  `json:"max_dialogs_per_tick"`
	MaxRequestsPerMin int  `json:"max_requests_per_min"`
	StoreText         bool `json:"store_text"`
	RetentionDays     int  `json:"retention_days"`
	// ExcludeUsers — кого поллер не обходит: мессенджер → id владельцев сессий.
	// Отказ от доставки личной переписки, а не выход с сайта: сессия остаётся
	// валидной и продолжает работать в мосте «ответ в чате → комментарий на
	// сайте». Нужен, потому что гасить сессию нельзя, а читать за человека его
	// ЛС (сайт при этом помечает их прочитанными) — можно только с его согласия.
	// Запрет админа: сильнее выбора самого человека (`/delivery`, колонка
	// sessions.talks_delivery) — тот выбирает мессенджер, этот запрещает вовсе.
	ExcludeUsers map[string][]int64 `json:"exclude_users,omitempty"`
}

// LLM — онлайн-доступ к Claude API (Anthropic): автоматическая редактура
// LLM-рубрик дайджеста (дальше — смысловой поиск, C4). Запросы идут через
// telegram_proxy: api.anthropic.com недоступен с российского IP. Пустой
// api_key — LLM выключен, дайджест живёт полуручным циклом.
type LLM struct {
	APIKey    string `json:"api_key"`    // или env LOVEGW_LLM_KEY
	Model     string `json:"model"`      // пусто — дефолт пакета llm (claude-opus-5)
	MaxTokens int    `json:"max_tokens"` // 0 — дефолт пакета llm
}

// Digest — еженедельный дайджест. Планировщик демона в слот выпуска строит
// черновик и материалы (LLM-рубрики заполняет Claude, если настроена секция
// llm) и либо публикует сам (auto_publish), либо зовёт админа в ЛС —
// премодерация через lovegw digest publish. Дефолты слота — суббота 09:00
// Asia/Novosibirsk (почему не вечер пятницы — см. digest.DefaultTZ).
type Digest struct {
	Enabled bool   `json:"enabled"`
	Weekday int    `json:"weekday"` // день слота: 0=воскресенье … 6=суббота
	Hour    int    `json:"hour"`    // час слота (0–23) в поясе tz
	TZ      string `json:"tz"`
	OutDir  string `json:"out_dir"` // каталог черновиков; пусто — digest рядом с БД
	// AutoPublish — публиковать выпуск сразу после генерации. При сбое
	// LLM-редактуры автопубликация не выполняется — черновик с
	// плейсхолдерами уходит админу на полуручной цикл.
	AutoPublish bool `json:"auto_publish"`
}

// ASR — автораспознавание голосовых в треде обсуждения: бот отвечает реплаем
// расшифровкой, дальше человек решает сам. Провайдер российский (Nexara),
// поэтому запросы идут напрямую, мимо telegram_proxy. Ключ — только конфиг или
// env LOVEGW_ASR_API_KEY, в репозиторий не попадает. enabled=false — фича
// выключена целиком, гейт работает как раньше.
type ASR struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider"` // пока только nexara
	BaseURL  string `json:"base_url"` // пусто — боевой адрес провайдера
	APIKey   string `json:"api_key"`  // или env LOVEGW_ASR_API_KEY
	// FFmpegPath — путь к бинарнику конвертера. Пусто — ffmpeg из PATH;
	// в образе задаётся через ENV LOVEGW_ASR_FFMPEG (в distroless нет PATH).
	FFmpegPath        string `json:"ffmpeg_path"`
	MaxDurationSec    int    `json:"max_duration_sec"`     // потолок одного сообщения
	UserDailyLimitSec int    `json:"user_daily_limit_sec"` // суточная квота; 0 — без квоты
	Concurrency       int    `json:"concurrency"`          // воркеров распознавания
	TimeoutSec        int    `json:"timeout_sec"`          // таймаут запроса к провайдеру
}

type Config struct {
	Site          Site        `json:"site"`
	MirrorBot     MirrorBot   `json:"mirror_bot"`
	DMBot         DMBot       `json:"dm_bot"`
	Messengers    *Messengers `json:"messengers"`
	Talks         Talks       `json:"talks"`
	Digest        Digest      `json:"digest"`
	LLM           LLM         `json:"llm"`
	ASR           ASR         `json:"asr"`
	NotesLimit    int         `json:"notes_limit"`
	Signature     string      `json:"signature"`
	AdminTGUserID int64       `json:"admin_tg_user_id"`
	FeedIntervalS int         `json:"feed_interval_s"`
	DBPath        string      `json:"db_path"`
	LogLevel      string      `json:"log_level"`
	// AccountsDBPath — база сервисных аккаунтов сайта (пакет acct). Пусто —
	// accounts.db рядом с боевой БД. Боевых путей не касается: там живут
	// технические сессии для обходов, а не сессии пользователей бота.
	AccountsDBPath string `json:"accounts_db_path,omitempty"`
	// SecretKey / SecretKeyFile — ключ шифрования сессионных кук на диске
	// (base64 32 байт, см. пакет secret; env LOVEGW_SECRET_KEY). Пусто —
	// шифрование выключено, куки лежат открыто, как раньше. Ключ намеренно
	// хранится ВНЕ базы: смысл шифрования в том, что в копии БД его нет.
	SecretKey     string `json:"secret_key,omitempty"`
	SecretKeyFile string `json:"secret_key_file,omitempty"`
	// TelegramProxy — прокси для Bot API (http/https/socks5), если Telegram
	// недоступен напрямую. Сайт и MAX при этом идут мимо прокси. Пусто —
	// напрямую.
	TelegramProxy string `json:"telegram_proxy"`
}

// AccountsDB — путь к базе сервисных аккаунтов сайта. По умолчанию
// accounts.db рядом с боевой БД: обе базы нужны одним и тем же командам.
func (c *Config) AccountsDB() string {
	if c.AccountsDBPath != "" {
		return c.AccountsDBPath
	}
	return filepath.Join(filepath.Dir(c.DBPath), "accounts.db")
}

// Load читает конфиг, накладывает значения по умолчанию и env-переопределения:
// LOVEGW_MIRROR_TOKEN, LOVEGW_DM_TOKEN, LOVEGW_MAX_TOKEN, LOVEGW_MAX_DM_TOKEN,
// LOVEGW_DB_PATH, LOVEGW_SECRET_KEY, LOVEGW_SECRET_KEY_FILE,
// LOVEGW_ACCOUNTS_DB, LOVEGW_ASR_* (см. applyASREnv).
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
		// talks по умолчанию admin-only и read-only (безопасный старт);
		// включение и allow_send — явно в конфиге.
		Talks: Talks{AdminOnly: true},
		// Слот дайджеста: суббота 09:00 Нск — единственное место, где день и
		// час дефолта заданы (обоснование — в комментарии digest.DefaultTZ).
		// Включение — явно в конфиге.
		Digest: Digest{Weekday: 6, Hour: 9, TZ: "Asia/Novosibirsk"},
		// ASR по умолчанию выключен; лимиты — консервативные, чтобы
		// случайное включение не вылилось в счёт от провайдера.
		ASR: ASR{
			Provider:          "nexara",
			MaxDurationSec:    90,
			UserDailyLimitSec: 600,
			Concurrency:       2,
			TimeoutSec:        60,
		},
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
	if v := os.Getenv("LOVEGW_LLM_KEY"); v != "" {
		cfg.LLM.APIKey = v
	}
	envString(&cfg.SecretKey, "LOVEGW_SECRET_KEY")
	envString(&cfg.SecretKeyFile, "LOVEGW_SECRET_KEY_FILE")
	envString(&cfg.AccountsDBPath, "LOVEGW_ACCOUNTS_DB")
	cfg.normalizeMessengers()
	if v := os.Getenv("LOVEGW_MAX_TOKEN"); v != "" {
		cfg.Messengers.Max.Token = v
	}
	if v := os.Getenv("LOVEGW_MAX_DM_TOKEN"); v != "" {
		cfg.Messengers.Max.DMToken = v
	}
	if v := os.Getenv("LOVEGW_MAX_TALKS_TOKEN"); v != "" {
		cfg.Messengers.Max.TalksToken = v
	}
	if v := os.Getenv("LOVEGW_TG_TALKS_TOKEN"); v != "" {
		cfg.Messengers.Telegram.TalksToken = v
	}
	cfg.normalizeTalksTokens()
	if err := cfg.applyASREnv(); err != nil {
		return nil, err
	}

	if cfg.Site.BaseURL == "" {
		return nil, fmt.Errorf("site.base_url не задан")
	}
	return cfg, nil
}

// applyASREnv накладывает env-переопределения секции asr.
func (c *Config) applyASREnv() error {
	if err := envBool(&c.ASR.Enabled, "LOVEGW_ASR_ENABLED"); err != nil {
		return err
	}
	envString(&c.ASR.Provider, "LOVEGW_ASR_PROVIDER")
	envString(&c.ASR.BaseURL, "LOVEGW_ASR_BASE_URL")
	envString(&c.ASR.APIKey, "LOVEGW_ASR_API_KEY")
	envString(&c.ASR.FFmpegPath, "LOVEGW_ASR_FFMPEG")
	if err := envInt(&c.ASR.MaxDurationSec, "LOVEGW_ASR_MAX_DURATION_SEC"); err != nil {
		return err
	}
	if err := envInt(&c.ASR.UserDailyLimitSec, "LOVEGW_ASR_USER_DAILY_LIMIT_SEC"); err != nil {
		return err
	}
	if err := envInt(&c.ASR.Concurrency, "LOVEGW_ASR_CONCURRENCY"); err != nil {
		return err
	}
	return envInt(&c.ASR.TimeoutSec, "LOVEGW_ASR_TIMEOUT_SEC")
}

// envString переопределяет значение, если переменная задана и непуста.
func envString(dst *string, name string) {
	if v := os.Getenv(name); v != "" {
		*dst = v
	}
}

// envInt разбирает целое из переменной окружения; мусор — ошибка конфига,
// а не молчаливый ноль.
func envInt(dst *int, name string) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: ожидалось целое число, получено %q", name, v)
	}
	*dst = n
	return nil
}

// envBool разбирает булево из переменной окружения (1/0, true/false).
func envBool(dst *bool, name string) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s: ожидалось true/false, получено %q", name, v)
	}
	*dst = b
	return nil
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
	if tg.Signature == "" {
		tg.Signature = c.Signature
	}
	if c.Messengers.Max.Signature == "" {
		c.Messengers.Max.Signature = c.Signature
	}
}

// normalizeTalksTokens переносит легаси-настройку MAX: отдельный бот там
// заводился как dm_token и обслуживал всю личку, а теперь это бот переписки —
// конфиг менять руками не нужно. У telegram переноса нет: dm_token там —
// это бот команд (РюмкинЪ), то есть «бот 1».
func (c *Config) normalizeTalksTokens() {
	mx := &c.Messengers.Max
	if mx.TalksToken == "" && mx.DMToken != "" {
		mx.TalksToken, mx.DMToken = mx.DMToken, ""
	}
}
