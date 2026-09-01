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
	// AuthorProfileID — от чьего имени выходит выпуск на площадке. Подписант
	// нужен потому, что выпуск публикуется НАТИВНОЙ заметкой, а `CreateNote`
	// требует участника: анонимной сводке никто не хозяин, а спросить за неё
	// должно быть с кого. Пусто — берётся анкета владельца из секции pulpit
	// (`owner_profile_id`): это тот же человек, и согласия у него уже есть.
	AuthorProfileID string `json:"author_profile_id"`
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

// Morning — утренняя заметка (пакет morning): одна заметка «доброе утро» в
// сутки, публикуется на love.ngs.ru от анкеты владельца, а на площадку и в
// каналы её приносит зеркало.
//
// Дефолт выключен; рантайм-тумблер живёт не здесь, а в БД
// (settings['morning.enabled']) — по образцу амвона и по той же причине:
// выключает предохранитель, включает админ кнопкой, и переключение обязано
// пережить рестарт. enabled здесь — гейт СБОРКИ службы: false значит «службы
// нет вовсе».
type Morning struct {
	Enabled bool `json:"enabled"`
	// AuthorProfileID — от чьего имени выходит заметка. Пусто — анкета
	// владельца из секции pulpit: это тот же человек и та же сессия.
	AuthorProfileID string `json:"author_profile_id"`
	Hour            int    `json:"hour"` // час слота (0–23) в поясе tz
	TZ              string `json:"tz"`
	// GraceH — сколько часов после слота ещё можно догонять. Трое суток, как у
	// дайджеста, здесь бессмысленны: доброе утро в полдень — уже не доброе
	// утро, а догон перекрыл бы следующий слот.
	GraceH           int      `json:"grace_h"`
	Model            string   `json:"model"`  // пусто — llm.DefaultModel
	Effort           string   `json:"effort"` // low — гасим скрытую задержку размышления
	GenerateTimeoutS int      `json:"generate_timeout_s"`
	MinRunes         int      `json:"min_runes"`
	MaxRunes         int      `json:"max_runes"`
	MaxLines         int      `json:"max_lines"`
	HistorySize      int      `json:"history_size"` // сколько своих прошлых заметок показывать модели
	MaxFacts         int      `json:"max_facts"`    // сколько поводов дня подавать модели
	Sources          []string `json:"sources"`      // календари; пусто — holidays.DefaultNames
	SourceTimeoutS   int      `json:"source_timeout_s"`
	FuseMisses       int      `json:"fuse_misses"` // столько «заметки нет в ленте» подряд = выключаемся
}

// Narod — жители площадки (эпик «народ»): персонажи, реплики которых пишет
// модель.
//
// Дефолт выключен, и режим по умолчанию СУХОЙ. Оба умолчания выбраны так, чтобы
// забытая секция не могла ничего опубликовать: `enabled: false` не поднимает
// службу вовсе, а `dry-run` крутит мир целиком, но на площадку не отправляет ни
// строки. Рантайм-тумблер живёт не здесь, а в БД (settings['narod.enabled']) —
// по образцу амвона и утренней заметки: конфиг монтируется в контейнер файлом и
// правкой не переключается, а выключатель обязан работать всегда.
type Narod struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"` // dry-run | live
	// CardsDir — каталог карточек. Добавить жителя значит положить файл: список
	// нигде в коде не перечислен (DoD брифа).
	CardsDir string `json:"cards_dir"`
	DBPath   string `json:"db_path"` // своя SQLite мира
	Seed     int64  `json:"seed"`    // зерно бросков; 0 — единица
	// Model — модель Claude-ветки. Пусто — умолчание llm.
	Model string `json:"model"`
	// Live — российская модель для того будущего, где песочницу откроют
	// аудитории: с этого дня в промпт попадают тексты живых участников, а
	// согласие обещает не вывозить их за пределы России. Пусто — берётся секция
	// platform.moderation, у которой ключ и каталог уже настроены.
	Live NarodLive `json:"live"`

	ScanEveryS int `json:"scan_every_s"`
	WorkEveryS int `json:"work_every_s"`
	// Потолки СВОИ, консервативнее платформенных: у площадки они держат шторм, а
	// здесь — каскад «житель отвечает жителю», который разгоняется сам и тратит
	// деньги на каждом шаге.
	PerPersonaHour int `json:"per_persona_hour"`
	PerPersonaDay  int `json:"per_persona_day"`
	PerThread      int `json:"per_thread"`
	ThreadCloseH   int `json:"thread_close_h"`
	PlanCapH       int `json:"plan_cap_h"`
	// GeneralizeRate — доля реплик, где случай выносится на весь пол («все бабы
	// такие»). Умолчание — ЗАМЕР по архиву (0,48 % в тредах, где обобщение
	// звучит хоть раз; 0,198 % по всему корпусу). Невод замера ловит заученную
	// формулу, а не поведение, поэтому число это пол, а не потолок: поднимая его,
	// вы принимаете авторское решение, а не уточняете замер.
	GeneralizeRate float64 `json:"generalize_rate"`
	// LatencyScale — множитель замеренной задержки ответа. Единица (умолчание) —
	// человеческий темп из карточки, и его длинный хвост как раз и не даёт
	// принять жителей за ботов. Меньше единицы — сжатое время СТЕНДА: смотреть
	// вживую на разговор, который дозревает до утра, невозможно. Это
	// сознательное отступление от замера, а не его настройка.
	LatencyScale float64 `json:"latency_scale"`
	// DayCalls — потолок обращений к модели за сутки. Прямая единица счёта денег,
	// как daily_requests у модерации.
	DayCalls int `json:"day_calls"`
	// Moves — доли ХОДОВ: чем житель отвечает — шуткой, вопросом автору,
	// придиркой, отмашкой в два слова, своим случаем, оффтопом, уточнением
	// (ключи punch, author, grumble, short, story, offtop, other).
	//
	// В КОНФИГЕ, а не в коде, по прямому требованию владельца 01.09.2026: это
	// единственные доли службы, у которых нет замера, — их подбирают глядя на
	// тред, и подбирать их пересборкой бинарника нельзя. Пусто — умолчания
	// narod.DefaultMoveRates.
	Moves map[string]float64 `json:"moves,omitempty"`
}

// NarodLive — российский провайдер для контекста с живыми текстами.
type NarodLive struct {
	APIKey   string `json:"api_key,omitempty"`
	FolderID string `json:"folder_id,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Platform — собственная площадка «Зазеркалье» (эпик E): Postgres, SSR и API.
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
	// Operator и Contact — реквизиты оператора персональных данных. Попадают в
	// ТЕКСТ согласий до его публикации: доказательством служит финальный
	// документ, а не шаблон, поэтому смена реквизитов требует новой версии
	// текста, и `platform migrate` на попытку подменить выпущенную редакцию
	// честно откажется. Пустые значения дают безличное «Владелец площадки» —
	// на пилоте это правда, но до открытия посторонним (Ш9) их надо заполнить.
	Operator string `json:"operator,omitempty"`
	Contact  string `json:"contact,omitempty"`
	// Contacts — как связаться с площадкой ЖИВЬЁМ, в отличие от Contact выше:
	// тот реквизит оператора персональных данных и вшит в текст согласия, а это
	// обычные ссылки на странице справки. Пустое поле не показывается вовсе —
	// то же правило, что у кнопки аватара: не рисуем то, что заведомо не
	// сработает.
	Contacts Contacts `json:"contacts"`
	// Moderation — автомат модерации (Ш7, пакет platmod).
	Moderation Moderation `json:"moderation"`
	// Bus — шина событий (эпик F, пакет platbus).
	Bus Bus `json:"bus"`
	// Shots — приём картинки к своей заметке (пакет imgconv).
	Shots Shots `json:"shots"`
}

// Shots — картинка, приложенная участником к своей заметке.
//
// Настроек здесь ровно две, и это не скупость. Потолки площадки живут
// КОНСТАНТАМИ с доводом в комментарии (maxFormBytes, previewMaxBytes,
// maxInFlight) именно потому, что число без довода назавтра меняют наугад;
// сторона, качество и число одновременных перекодировок — такие же числа, и
// место им в imgconv, а не в JSON.
//
// Выключатель нужен по другой причине: приём чужих файлов — единственное место,
// где посторонний кладёт что-то на наш диск, и погасить его надо уметь без
// пересборки.
type Shots struct {
	Enabled bool `json:"enabled"`
	// FFmpegPath — путь к бинарнику. Пусто — берётся asr.ffmpeg_path: бинарник
	// в образе один и тот же, и держать для него две настройки значило бы
	// однажды поправить не ту.
	FFmpegPath string `json:"ffmpeg_path,omitempty"`
}

// Bus — шина событий площадки: раздача поводов, разбор реакций, уборка
// состарившегося (подробности — в шапке internal/platbus).
//
// Гейта `enabled` здесь НЕТ намеренно, и это отличие от секции модерации не
// случайно. У автомата модерации выключатель есть потому, что каждая проверка
// стоит денег; шина же не тратит ничего, кроме нескольких запросов в минуту к
// своему же Postgres, — а выключенная шина означает, что факты копятся
// нерозданными, то есть площадка молча перестаёт говорить людям, что им
// ответили. Такое состояние не должно достигаться правкой конфига.
//
// Обе настройки — про темп, а не про поведение: пустая секция даёт рабочие
// умолчания (5 с, сотня фактов за проход).
type Bus struct {
	IntervalS int `json:"interval_s"`
	Batch     int `json:"batch"`
}

// Moderation — автомат модерации площадки: он снимает ОБЪЁМ, решения о людях
// остаются человеку (подробности — в шапке internal/platmod).
//
// Выключен по умолчанию, и это не осторожность ради осторожности: включённый
// автомат тратит деньги на каждую публикацию, а очередь модератора работает и
// без него — просто наполняется жалобами, а не мнениями машины.
//
// Инструменты модератора (скрыть, вернуть, запретить писать, журнал) от этой
// секции НЕ зависят вовсе: они часть ядра и морды.
// Contacts — куда идти человеку, которому нужен живой собеседник, а не форма.
//
// ProfileID — номер участника ПЛОЩАДКИ (он же номер анкеты НГС у переехавших).
// Ссылка ведёт на /u/<id> здесь, а не на love.ngs.ru: ссылок на НГС проект не
// ставит нигде с 27.08.2026. Названный тут человек тем самым открывает свою
// страницу гостям — обычная-то закрыта, — и это его собственное решение про
// свои же данные, ради того и поле.
//
// Telegram и MAX — ПУБЛИЧНЫЕ адреса групп, а не числовые id каналов из секции
// messengers: те служат постингу и годятся боту, а не человеку. Пустое поле не
// показывается вовсе.
type Contacts struct {
	ProfileID int64  `json:"profile_id,omitempty"`
	Telegram  string `json:"telegram,omitempty"`
	MAX       string `json:"max,omitempty"`
	// BotTelegram и BotMAX — публичные адреса РюмкинЪа. Нужны ВХОДУ, а не
	// только контактам: вход по ссылке из бота начинается в боте, и площадке
	// надо уметь показать, куда идти. Пусто и там и там — про этот путь на
	// странице входа не говорится вовсе, ровно как про форму анкеты при мёртвом
	// НГС.
	//
	// Полей ДВА, а не одно, потому что Telegram и MAX — разные сети: адреса у
	// бота там разные, и ссылка одной в другой не работает. Назвав один, мы
	// отправили бы половину людей туда, где их бота нет.
	BotTelegram string `json:"bot_telegram,omitempty"`
	BotMAX      string `json:"bot_max,omitempty"`
}

type Moderation struct {
	Enabled bool `json:"enabled"`
	// Provider — ЧЕЙ API проверяет тексты участников. «yandex» (по умолчанию) —
	// Yandex AI Studio, пакет rullm; «anthropic» — Claude, пакет llm.
	//
	// Это не настройка вкуса и не переключатель качества. Опубликованное
	// согласие обещает людям «не вывозит данные за пределы России», а на
	// проверку уходит написанное ими же, — поэтому «anthropic» здесь и означает
	// расхождение с бумагой, которую участники уже подписали. Значение
	// оставлено ради дайджеста и амвона, живущих на Claude, и ради возможности
	// сравнить модели на стенде (`platform triage`), но боевой выбор один.
	Provider string `json:"provider,omitempty"`
	// APIKey — ключ провайдера. Боевое место ему — env LOVEGW_MODERATION_KEY, а
	// не файл: конфиг монтируется в контейнер файлом и попадает в бэкапы
	// каталога развёртывания, то же правило, что у platform.dsn.
	APIKey string `json:"api_key,omitempty"`
	// FolderID — каталог Yandex Cloud (provider=yandex). Секретом не является:
	// сам по себе он ничего не открывает, ключ лежит отдельно.
	FolderID string `json:"folder_id,omitempty"`
	// BaseURL — переопределение адреса API. Пусто — боевой адрес провайдера;
	// нужен на случай смены хоста, чтобы это не требовало сборки.
	BaseURL string `json:"base_url,omitempty"`
	// Model — чем проверять. Смысл значения зависит от Provider: у yandex это
	// «yandexgpt-lite/latest» (folder приезжает отдельным полем), у anthropic —
	// Haiku 4.5 (`claude-haiku-4-5`, $1/$5 за млн токенов), а не общая модель из
	// секции llm: это ТРИАЖ по закрытому списку, и решение владельца записано в
	// бэклоге прямо — «триаж дешёвой моделью, дорогая только на сомнительной
	// полосе».
	//
	// Отдельно от llm.model ещё и потому, что дайджест с амвоном живут на
	// claude-opus-5: одно поле на всех означало бы либо дорогой триаж, либо
	// дешёвый дайджест.
	Model string `json:"model"`
	// Effort — сколько модели думать. Пусто по умолчанию, и менять это, не
	// сменив модель, НЕЛЬЗЯ: Haiku 4.5 параметра effort не принимает вовсе и
	// отвечает на него ошибкой запроса.
	Effort string `json:"effort"`
	// IntervalS — как часто заглядываем в очередь; BatchSize — сколько
	// публикаций в одном запросе к модели; DailyRequests — потолок ЗАПРОСОВ
	// в сутки, прямая единица счёта денег.
	//
	// Размер пачки — это ДОЛЯ ПРАВИЛ в счёте. Правила весят около тысячи
	// токенов и уходят с каждым запросом, реплика — сотню: при десятке правила
	// съедают половину счёта, при двадцати пяти — пятую часть. Плата за
	// крупную пачку известна: отказ провайдера прилетает на ВСЮ пачку разом
	// (см. escalateExhausted в platmod), то есть крупнее пачка — больше строк
	// уедет к человеку непроверенными.
	IntervalS     int `json:"interval_s"`
	BatchSize     int `json:"batch_size"`
	DailyRequests int `json:"daily_requests"`
	TimeoutS      int `json:"timeout_s"`
	MaxAttempts   int `json:"max_attempts"`
	// FloodWindowS и FloodMax — шторм одинаковых сообщений. Считает его КОД, а
	// не модель: ей показывают одну реплику, а нарушение состоит в повторе.
	FloodWindowS int `json:"flood_window_s"`
	FloodMax     int `json:"flood_max"`
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
	Morning       Morning     `json:"morning"`
	Narod         Narod       `json:"narod"`
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
//	секции       LOVEGW_ASR_* (см. applyASREnv), LOVEGW_PULPIT_* (applyPulpitEnv),
//	             LOVEGW_MORNING_* (applyMorningEnv)
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
		// Утренняя заметка по умолчанию выключена. Слот 05:00 Нск — решение
		// владельца, и оно снято ЗАМЕРОМ, а не вкусом. За год (17.08.2025–
		// 26.08.2026) чужое «доброе утро» появлялось в 145 днях из 374, и по
		// времени ПЕРВОГО приветствия дня слот стоит ровно столько молчания:
		// 07:00 — нас опережают 95 дней в году, 06:00 — 51, 05:00 — 20,
		// 04:00 — 3. Слот ходил 07:00 → 06:00 (24.08.2026) → 05:00
		// (27.08.2026), и каждый шаг втрое сокращает дни, когда правило «чужое
		// утро сильнее нашего» велит боту молчать. Ниже пяти не идём: в четыре
		// утра заметка приходит в пустую ленту, а выигрыш всего 17 дней. До
		// субботнего слота дайджеста (09:00) слот не дотягивается по-прежнему. Длина снята не с потолка:
		// потолок задан ВЛАДЕЛЬЦЕМ и однажды уже двигался: 1200 → 900
		// («лаконичнее», по образцу t3h.ru/n/313059) → 1800 («удвой допустимый
		// объём», 24.08.2026). Цена второго движения названа честно: в ленте
		// площадки текст длиннее 1500 знаков сворачивается показом
		// (`web.longBodyRunes`), то есть длинное утро читатель увидит началом и
		// кнопкой «показать целиком» — на своей странице и в каналах оно
		// по-прежнему целиком. Это потолок, а не цель: второй проход-правка
		// режет заметку, а не добирает до предела. Строк больше, чем можно
		// подумать: поводы идут каждый со своей строки и отбиты пустой, то есть
		// по две строки на повод.
		//
		// ПОЛ поднят до 1000 знаков ЗАМЕРОМ ЖАНРА (25.08.2026, 148 живых
		// заметок-приветствий за год): медиана 812 знаков, у постоянных авторов
		// утра — 999 (KVADRATIг), 1092 (Н.), 1403 (А.С.Сенин), 1418 (Шумелка
		// Мышь); непустых строк — медиана 8, у верхней четверти 15–20. То есть
		// «доброе утро» в этом разделе жанрово ПРОСТЫНЯ, и короткая заметка
		// читается куце — так владелец про наши 600 знаков и сказал. Один
		// потолок объёма не даёт: второй проход режет, и прогон на 1800 дал 391
		// знак. Настоящие рычаги — число поводов (в промпте пять-семь) и
		// рубрики жанра (приметы, «родились», именины), а пол не даёт им
		// схлопнуться обратно.
		Morning: Morning{
			Hour:             5,
			TZ:               "Asia/Novosibirsk",
			GraceH:           3,
			Effort:           "low",
			GenerateTimeoutS: 90,
			MinRunes:         1000,
			MaxRunes:         1800,
			MaxLines:         44,
			HistorySize:      7,
			MaxFacts:         20,
			SourceTimeoutS:   20,
			FuseMisses:       2,
		},
		// Народ по умолчанию выключен и сух. Числа тактов и потолков названы с
		// доводами в narod.Defaults — здесь они повторены, потому что конфиг
		// читает человек, а не служба, и лезть за умолчанием в чужой пакет он не
		// должен.
		Narod: Narod{
			Mode:           "dry-run",
			CardsDir:       "data/narod/cards",
			DBPath:         "data/narod.db",
			Seed:           1,
			ScanEveryS:     30,
			WorkEveryS:     10,
			PerPersonaHour: 2,
			PerPersonaDay:  8,
			PerThread:      6,
			ThreadCloseH:   12,
			PlanCapH:       48,
			GeneralizeRate: 0.0048,
			LatencyScale:   1,
			DayCalls:       100,
		},
		// Площадка по умолчанию выключена. Слушает только петлю контейнера:
		// наружу её выпускает реверс-прокси, у самого приложения портов быть
		// не должно.
		Platform: Platform{
			Listen:   ":8080",
			MediaDir: "data/media",
			// Автомат модерации выключен по умолчанию: он тратит деньги на
			// каждую публикацию, а очередь модератора живёт и без него.
			// Модель — дешёвая: это триаж по закрытому списку, и effort ей не
			// передаётся вовсе (Haiku 4.5 отвечает на него ошибкой).
			Moderation: Moderation{
				// Провайдер по умолчанию РОССИЙСКИЙ: на проверку уходит текст
				// участников, а согласие обещает не вывозить его за пределы
				// России. Имя модели пустое намеренно — оно своё у каждого
				// провайдера и подставляется при сборке клиента.
				Provider:      "yandex",
				IntervalS:     30,
				BatchSize:     25,
				DailyRequests: 500,
				TimeoutS:      90,
				MaxAttempts:   3,
				FloodWindowS:  3600,
				FloodMax:      3,
			},
			// Шина событий: такт в пять секунд. Повод, приехавший позже, читается
			// как поломка — человек уже видел ответ на странице.
			Bus: Bus{IntervalS: 5, Batch: 100},
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
	if err := cfg.applyNarodEnv(); err != nil {
		return nil, err
	}
	if err := cfg.applyMorningEnv(); err != nil {
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
	if c.Morning.Hour < 0 || c.Morning.Hour > 23 {
		return fmt.Errorf("morning.hour: ожидалось 0–23, получено %d", c.Morning.Hour)
	}
	if p := c.Pulpit.ReplyProbability; p < 0 || p > 1 {
		return fmt.Errorf("pulpit.reply_probability: ожидалась вероятность 0–1, получено %v", p)
	}
	if c.Pulpit.MinRunes > c.Pulpit.MaxRunes && c.Pulpit.MaxRunes > 0 {
		return fmt.Errorf("pulpit.min_runes (%d) больше pulpit.max_runes (%d) — ни одна реплика не пройдёт валидатор",
			c.Pulpit.MinRunes, c.Pulpit.MaxRunes)
	}
	// Режим народа проверяется на СБОРКЕ, а не на первом такте: служба, молча
	// не работающая из-за опечатки в слове «dry-run», выглядит выключенной, и
	// разбираться в этом придётся по пустой песочнице.
	if c.Narod.Enabled && c.Narod.Mode != "dry-run" && c.Narod.Mode != "live" {
		return fmt.Errorf("narod.mode: ожидалось dry-run или live, получено %q", c.Narod.Mode)
	}
	if c.Narod.GeneralizeRate < 0 || c.Narod.GeneralizeRate > 1 {
		return fmt.Errorf("narod.generalize_rate: ожидалась доля от 0 до 1, получено %v", c.Narod.GeneralizeRate)
	}
	if c.Narod.LatencyScale < 0 {
		return fmt.Errorf("narod.latency_scale: ожидалось число больше нуля, получено %v", c.Narod.LatencyScale)
	}
	// Полноту секции platform здесь НЕ проверяем, хотя соблазн есть. Конфиг
	// один на все команды одного образа, а площадка нужна троим из семи: у
	// modwatch, guests и activity её `enabled` — просто чужая строка в общем
	// файле, и падать на ней они не должны (проверено собой 17.08.2026: `guests`
	// ушёл в рестарт-петлю из-за отсутствующего DSN, который ему не нужен).
	// Проверяют те, кто открывает базу: `run`, `web` и `platform *`.
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
	if c.Narod.Enabled && !c.Platform.Enabled {
		c.Warnings = append(c.Warnings,
			"narod.enabled=true при platform.enabled=false — жителям негде играть, служба не поднимется")
	}
	if c.Narod.Enabled && c.Narod.Mode == "live" && c.LLM.APIKey == "" {
		c.Warnings = append(c.Warnings,
			"narod.mode=live без llm.api_key — реплики писать нечем, жители будут молчать")
	}
	sort.Strings(c.Warnings) // порядок map'а иначе гуляет от запуска к запуску
}

// applyNarodEnv накладывает env-переопределения секции narod. Их ровно три, и
// список закрыт намеренно: env нужен для того, что различается между стендом и
// боем (включён ли, в каком режиме, какой моделью), а потолки и такты — это
// настройка поведения, и ей место в файле, где рядом с числом стоит довод.
func (c *Config) applyNarodEnv() error {
	if err := envBool(&c.Narod.Enabled, "LOVEGW_NAROD_ENABLED"); err != nil {
		return err
	}
	envString(&c.Narod.Mode, "LOVEGW_NAROD_MODE")
	envString(&c.Narod.Model, "LOVEGW_NAROD_MODEL")
	return nil
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

// applyMorningEnv накладывает env-переопределения секции morning. Их три, как у
// амвона, и по той же причине: выключить фичу на хосте, сменить модель и
// сдвинуть слот. Остальное правится конфигом, а тумблер «работает сейчас»
// живёт в БД.
func (c *Config) applyMorningEnv() error {
	if err := envBool(&c.Morning.Enabled, "LOVEGW_MORNING_ENABLED"); err != nil {
		return err
	}
	envString(&c.Morning.Model, "LOVEGW_MORNING_MODEL")
	return envInt(&c.Morning.Hour, "LOVEGW_MORNING_HOUR")
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
	envString(&c.Platform.Operator, "LOVEGW_PLATFORM_OPERATOR")
	envString(&c.Platform.Contact, "LOVEGW_PLATFORM_CONTACT")
	// Приём картинок гасится с хоста по той же причине, что и автомат
	// модерации: это единственный путь, которым посторонний кладёт файл на наш
	// диск, и закрыть его надо уметь, не трогая смонтированный конфиг.
	if err := envBool(&c.Platform.Shots.Enabled, "LOVEGW_PLATFORM_SHOTS_ENABLED"); err != nil {
		return err
	}
	envString(&c.Platform.Shots.FFmpegPath, "LOVEGW_FFMPEG")
	// Автомат модерации гасится с хоста тем же рычагом, что и амвон: остановить
	// расходы должно быть можно, не пересобирая конфиг, который смонтирован в
	// контейнер файлом.
	if err := envBool(&c.Platform.Moderation.Enabled, "LOVEGW_MODERATION_ENABLED"); err != nil {
		return err
	}
	envString(&c.Platform.Moderation.Model, "LOVEGW_MODERATION_MODEL")
	envString(&c.Platform.Moderation.Provider, "LOVEGW_MODERATION_PROVIDER")
	// Ключ классификатора — только из окружения на боевом хосте: конфиг
	// монтируется файлом и уезжает в бэкапы каталога развёртывания.
	envString(&c.Platform.Moderation.APIKey, "LOVEGW_MODERATION_KEY")
	envString(&c.Platform.Moderation.FolderID, "LOVEGW_MODERATION_FOLDER")
	return envInt(&c.Platform.Moderation.DailyRequests, "LOVEGW_MODERATION_DAILY_REQUESTS")
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
