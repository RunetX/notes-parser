// Пакет config загружает конфигурацию lovegw из JSON-файла
// с переопределением секретов через переменные окружения.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// сайте».
	//
	// Согласие человека спрашивают и без этого списка: без него переписку не
	// читают вовсе (`sessions.talks_scan`, кнопка в `/delivery`). Здесь —
	// запрет админа поверх согласия: у него нет ни вопроса, ни кнопки отмены, и
	// человека, попавшего в список, поллер даже не спрашивает.
	// Единственная причина, по которой список остаётся: сам админ может знать
	// то, чего не знает человек за кнопкой.
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

// Pulpit — амвон: собственный комментарий владельца под каждой новой заметкой
// (пакет pulpit). Дефолт выключен; рантайм-тумблер живёт не здесь, а в БД
// (settings['pulpit.enabled']) — выключает предохранитель, включает админ
// кнопкой, и переключение обязано пережить рестарт. enabled здесь — гейт
// сборки службы: false значит «службы нет вовсе».
type Pulpit struct {
	Enabled bool `json:"enabled"`
	// OwnerProfileID — id анкеты владельца на сайте: по нему берётся сессия
	// (SessionForProfile) и опознаётся своя реплика в треде.
	OwnerProfileID string `json:"owner_profile_id"`
	// FeedIntervalS — свой обход ленты. Он отдельный от зеркального: общий
	// лимитер love.Client (0,5 rps) делит очередь с опросом комментариев всех
	// живых заметок, и заметка добиралась до нас за p90 = 619 с, тогда как
	// первый чужой комментарий приходит за медианные 164 с.
	FeedIntervalS    int    `json:"feed_interval_s"`
	FreshnessMin     int    `json:"freshness_min"`      // старше — не пишем (заметка уже обжита)
	MaxLatencyS      int    `json:"max_latency_s"`      // после этого от момента находки реплику не шлём вовсе
	GenerateTimeoutS int    `json:"generate_timeout_s"` // потолок генерации со всеми переспросами
	MaxPerDay        int    `json:"max_per_day"`        // предохранитель от «сайт выкатил архив в ленту»
	Model            string `json:"model"`              // пусто — llm.DefaultModel
	Effort           string `json:"effort"`             // low — гасим скрытую задержку размышления
	MinRunes         int    `json:"min_runes"`
	MaxRunes         int    `json:"max_runes"`
	MaxLines         int    `json:"max_lines"`
	AllowEmoji       bool   `json:"allow_emoji"`
	HistorySize      int    `json:"history_size"`  // сколько своих последних реплик показывать модели
	FormCooldown     int    `json:"form_cooldown"` // формы последних N реплик не предлагаются
	// ReplyProbability — вероятность ответить тому, кто ответил нам. 0 —
	// в переписку не вступаем вовсе.
	ReplyProbability float64 `json:"reply_probability"`
	RepliesPerNote   int     `json:"replies_per_note"`
	RepliesPerDay    int     `json:"replies_per_day"`
	ReplyWindowH     int     `json:"reply_window_h"`
	FuseMisses       int     `json:"fuse_misses"` // столько «страница есть, реплики нет» подряд = выключаемся
}

// Platform — собственная площадка «Заметки» (эпик E): Postgres, SSR и API.
// Секция гейтит саму службу; «работает ли она сейчас» решается не здесь.
//
// DSN держит пароль, поэтому боевое место ему — env LOVEGW_PLATFORM_DSN, а не
// config.json: конфиг монтируется в контейнер файлом и попадает в бэкапы вместе
// с каталогом развёртывания.
type Platform struct {
	Enabled bool   `json:"enabled"`
	DSN     string `json:"dsn"`      // postgres://user:pass@host:5432/db?sslmode=disable
	Listen  string `json:"listen"`   // адрес HTTP-сервера, за реверс-прокси — :8080
	BaseURL string `json:"base_url"` // https://t3h.ru — для абсолютных ссылок и кук
	// MediaDir — каталог CAS: /data/media/<2 hex>/<sha256>. Отдаёт эти файлы
	// Caddy напрямую, мимо Go: на одном ядре самый жирный по трафику путь не
	// должен проходить через приложение.
	MediaDir string `json:"media_dir"`
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
	Pulpit        Pulpit      `json:"pulpit"`
	Platform      Platform    `json:"platform"`
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
	// Warnings — настройки, которые заданы, но ни на что не влияют. Не ошибка:
	// демон обязан подняться и с ними. Но и не молчать: токен, выключенный
	// забытым `enabled: false`, ищут часами — зеркало просто никуда не постит.
	// Печатают их демон на старте и doctor.
	Warnings []string `json:"-"`
}

// AccountsDB — путь к базе сервисных аккаунтов сайта. По умолчанию
// accounts.db рядом с боевой БД: обе базы нужны одним и тем же командам.
func (c *Config) AccountsDB() string {
	if c.AccountsDBPath != "" {
		return c.AccountsDBPath
	}
	return filepath.Join(filepath.Dir(c.DBPath), "accounts.db")
}

// Load читает конфиг, накладывает значения по умолчанию и env-переопределения,
// после чего проверяет диапазоны (validate) и собирает предупреждения о
// настройках, которые ни на что не влияют (Warnings).
//
// Переменные окружения — все, что понимает конфиг:
//
//	токены       LOVEGW_MIRROR_TOKEN, LOVEGW_DM_TOKEN, LOVEGW_TG_TALKS_TOKEN,
//	             LOVEGW_MAX_TOKEN, LOVEGW_MAX_DM_TOKEN, LOVEGW_MAX_TALKS_TOKEN
//	пути и сеть  LOVEGW_DB_PATH, LOVEGW_ACCOUNTS_DB, LOVEGW_TG_PROXY
//	ключи        LOVEGW_LLM_KEY, LOVEGW_SECRET_KEY, LOVEGW_SECRET_KEY_FILE
//	секции       LOVEGW_ASR_* (см. applyASREnv), LOVEGW_PULPIT_* (applyPulpitEnv)
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
		// Амвон по умолчанию выключен. Пороги длины сняты с самого владельца
		// (p25 = 42 руны, p50 = 79), а не выбраны по вкусу. Нижний 25: у шутки
		// панч бывает в четыре слова. Верхний 180 и четыре строки — это потолок
		// сетапа с панчем; на 300 и 12 строках модель писала три абзаца, а их в
		// ленте никто не читает. max_per_day = 25 при медиане 10 заметок в
		// сутки — это runaway-guard, а не троттлинг.
		Pulpit: Pulpit{
			FeedIntervalS:    20,
			FreshnessMin:     15,
			MaxLatencyS:      180,
			GenerateTimeoutS: 90,
			MaxPerDay:        25,
			// effort=low не ради скорости (запас по времени есть: первый чужой
			// комментарий приходит через медианные 164 с). Замер 16.08.2026 на
			// живых черновиках: на medium ход мысли протекает в само поле text
			// — «Wait, no.» посреди реплики, склейка «uправила», обломки вроде
			// «}» и приписка-резюме в скобках. Три случая из семи против нуля
			// из четырёх на low. Валидатор такое ловит, но переспрос стоит
			// секунд, а брак всё равно иногда доезжает целым. Потолок генерации
			// 90 с (а не прежние 45) накрывает ВСЕ переспросы разом.
			Effort:           "low",
			MinRunes:         25,
			MaxRunes:         180,
			MaxLines:         4,
			AllowEmoji:       true,
			HistorySize:      20,
			FormCooldown:     5,
			ReplyProbability: 0.15,
			RepliesPerNote:   1,
			RepliesPerDay:    3,
			ReplyWindowH:     24,
			FuseMisses:       3,
		},
		// Площадка по умолчанию выключена. Слушает только петлю контейнера:
		// наружу её выпускает реверс-прокси, у самого приложения портов быть
		// не должно.
		Platform: Platform{
			Listen:   ":8080",
			MediaDir: "data/media",
		},
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение конфига: %w", err)
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("разбор конфига %s: %w", path, err)
	}

	envString(&cfg.MirrorBot.Token, "LOVEGW_MIRROR_TOKEN")
	envString(&cfg.DMBot.Token, "LOVEGW_DM_TOKEN")
	envString(&cfg.DBPath, "LOVEGW_DB_PATH")
	envString(&cfg.TelegramProxy, "LOVEGW_TG_PROXY")
	envString(&cfg.LLM.APIKey, "LOVEGW_LLM_KEY")
	envString(&cfg.SecretKey, "LOVEGW_SECRET_KEY")
	envString(&cfg.SecretKeyFile, "LOVEGW_SECRET_KEY_FILE")
	envString(&cfg.AccountsDBPath, "LOVEGW_ACCOUNTS_DB")
	cfg.normalizeMessengers()
	envString(&cfg.Messengers.Max.Token, "LOVEGW_MAX_TOKEN")
	envString(&cfg.Messengers.Max.DMToken, "LOVEGW_MAX_DM_TOKEN")
	envString(&cfg.Messengers.Max.TalksToken, "LOVEGW_MAX_TALKS_TOKEN")
	envString(&cfg.Messengers.Telegram.TalksToken, "LOVEGW_TG_TALKS_TOKEN")
	cfg.normalizeTalksTokens()
	if err := cfg.applyASREnv(); err != nil {
		return nil, err
	}
	if err := cfg.applyPulpitEnv(); err != nil {
		return nil, err
	}
	if err := cfg.applyPlatformEnv(); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.collectWarnings()
	return cfg, nil
}

// validate ловит значения, при которых демон либо не поднимется вовсе, либо
// поведёт себя необъяснимо. Здесь только то, что нельзя починить дефолтом:
// «0 значит по умолчанию» уже разобрано в конструкторах служб, а вот 0 в
// интервале ленты — это паника time.NewTicker на старте, и объяснять её потом
// по стектрейсу незачем.
func (c *Config) validate() error {
	if c.Site.BaseURL == "" {
		return fmt.Errorf("site.base_url не задан")
	}
	if c.Site.RequestIntervalMS < 0 {
		return fmt.Errorf("site.request_interval_ms: ожидалось неотрицательное число, получено %d", c.Site.RequestIntervalMS)
	}
	if c.FeedIntervalS <= 0 {
		return fmt.Errorf("feed_interval_s: ожидалось положительное число, получено %d", c.FeedIntervalS)
	}
	if c.NotesLimit <= 0 {
		return fmt.Errorf("notes_limit: ожидалось положительное число, получено %d", c.NotesLimit)
	}
	if c.Digest.Weekday < 0 || c.Digest.Weekday > 6 {
		return fmt.Errorf("digest.weekday: ожидалось 0–6 (0 — воскресенье), получено %d", c.Digest.Weekday)
	}
	if c.Digest.Hour < 0 || c.Digest.Hour > 23 {
		return fmt.Errorf("digest.hour: ожидалось 0–23, получено %d", c.Digest.Hour)
	}
	if p := c.Pulpit.ReplyProbability; p < 0 || p > 1 {
		return fmt.Errorf("pulpit.reply_probability: ожидалась вероятность 0–1, получено %v", p)
	}
	if c.Pulpit.MinRunes > c.Pulpit.MaxRunes && c.Pulpit.MaxRunes > 0 {
		return fmt.Errorf("pulpit.min_runes (%d) больше pulpit.max_runes (%d) — ни одна реплика не пройдёт валидатор",
			c.Pulpit.MinRunes, c.Pulpit.MaxRunes)
	}
	// Площадку без DSN поднимать нечем, а падать на первом же запросе к базе
	// хуже, чем не стартовать вовсе: контейнер уйдёт в рестарт-петлю молча.
	if c.Platform.Enabled && c.Platform.DSN == "" {
		return fmt.Errorf("platform.enabled, но platform.dsn пуст (задайте LOVEGW_PLATFORM_DSN)")
	}
	return nil
}

// collectWarnings собирает настройки, заданные вхолостую. Главный случай —
// токен при выключенном мессенджере: гейт messengers молча отключает приёмник,
// а снаружи это выглядит как «зеркало не работает без причины».
func (c *Config) collectWarnings() {
	c.Warnings = nil
	for name, m := range map[string]Messenger{"max": c.Messengers.Max, "telegram": c.Messengers.Telegram} {
		if m.Token != "" && !m.Enabled {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"messengers.%s: токен задан, но enabled=false — этот мессенджер выключен целиком", name))
		}
	}
	if c.Digest.AutoPublish && !c.Digest.Enabled {
		c.Warnings = append(c.Warnings,
			"digest.auto_publish=true при digest.enabled=false — планировщик выпуска не запускается")
	}
	if c.LLM.APIKey == "" && c.Digest.AutoPublish {
		c.Warnings = append(c.Warnings,
			"digest.auto_publish=true без llm.api_key — LLM-рубрики заполнить нечем, выпуск уйдёт админу на полуручной цикл")
	}
	sort.Strings(c.Warnings) // порядок map'а иначе гуляет от запуска к запуску
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

// applyPulpitEnv накладывает env-переопределения секции pulpit. Их три:
// выключить фичу на хосте, сменить модель и придержать ответы на ответы —
// остальное правится конфигом, а тумблер «включено сейчас» живёт в БД.
func (c *Config) applyPulpitEnv() error {
	if err := envBool(&c.Pulpit.Enabled, "LOVEGW_PULPIT_ENABLED"); err != nil {
		return err
	}
	envString(&c.Pulpit.Model, "LOVEGW_PULPIT_MODEL")
	return envFloat(&c.Pulpit.ReplyProbability, "LOVEGW_PULPIT_REPLY_PROBABILITY")
}

// applyPlatformEnv — переопределения секции platform. DSN держит пароль к
// Postgres, поэтому его боевое место — именно env, рядом с токенами ботов.
func (c *Config) applyPlatformEnv() error {
	if err := envBool(&c.Platform.Enabled, "LOVEGW_PLATFORM_ENABLED"); err != nil {
		return err
	}
	envString(&c.Platform.DSN, "LOVEGW_PLATFORM_DSN")
	envString(&c.Platform.Listen, "LOVEGW_PLATFORM_LISTEN")
	envString(&c.Platform.BaseURL, "LOVEGW_PLATFORM_BASE_URL")
	envString(&c.Platform.MediaDir, "LOVEGW_PLATFORM_MEDIA_DIR")
	return nil
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

// envFloat разбирает дробное из переменной окружения (вероятность ответа).
func envFloat(dst *float64, name string) error {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%s: ожидалось число, получено %q", name, v)
	}
	*dst = f
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
