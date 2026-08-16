// Пакет talks — личная переписка сайта (talks) в мессенджеры. Мессенджер-
// агностичное ядро: один поллер сайта (общий rate-лимитер) фанит входящие ЛС в
// личку РюмкинЪ каждого включённого мессенджера, а ответы реплаем/командой
// отправляет на сайт от имени сессии пользователя. Транспорт мессенджера —
// за интерфейсом PMTransport (реализуют tgx и maxx), клиент сайта — за
// SiteTalks (реализует love.Client в Ф4). См. briefs/love-talks-telegram.md.
//
// Файлы пакета: talks.go — типы и конструктор; poll.go — обход сайта;
// incoming.go — доставка входящих в мессенджер; send.go — обратное направление
// (ответ → сайт); delivery.go — выбор единственного получателя ЛС; health.go —
// сессии, алерты и kill-switch; format.go — верстка сообщения; purge.go —
// retention.
package talks

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"lovegw/internal/alerts"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// SiteTalks — операции talks на сайте, нужные поллеру (реализует love.Client).
type SiteTalks interface {
	Dialogs(ctx context.Context, cookies []*http.Cookie, limit int) ([]love.TalkDialog, error)
	History(ctx context.Context, cookies []*http.Cookie, passportID, afterMsgID string, limit int) ([]love.TalkMessage, error)
	Send(ctx context.Context, cookies []*http.Cookie, passportID, text string) (love.TalkMessage, error)
}

// SiteIdentifier (опц.) — снятие site-идентичности со страницы сайта. Поллеру
// она нужна ради паспорта: без него вход одного человека в двух мессенджерах не
// связать, и выбор мессенджера доставки работать не будет. Сессии, заведённые до
// появления talks, паспорта не хранят — поллер дозаполняет его лениво.
// Реализует love.Client.
type SiteIdentifier interface {
	SiteIdentity(ctx context.Context, cookies []*http.Cookie) (profileID, passportID, nick string, err error)
}

// PMTransport — доставка ЛС в конкретный мессенджер. В отличие от dmbot.Transport
// SendPM возвращает id доставленного сообщения — он нужен для message_targets
// (маршрутизация ответа реплаем). Name() = имя мессенджера (store.Messenger*).
type PMTransport interface {
	Name() string
	SendPM(ctx context.Context, userID int64, html string) (msgID string, err error)
	Confirm(ctx context.Context, userID int64, msgID string, ok bool)
}

// Config — параметры поллера talks.
type Config struct {
	BaseURL        string           // база сайта для ссылок на анкеты
	AdminOnly      bool             // MVP: обходить только админов (AdminIDs)
	AdminIDs       map[string]int64 // messenger → id админа (у каждого своё пространство)
	Interval       time.Duration    // активный интервал опроса
	IdleInterval   time.Duration    // холостой интервал (не было новых сообщений)
	MaxDialogs     int              // сколько диалогов дозабирать историей за тик (бюджет)
	HistoryLimit   int              // предел сообщений за один запрос истории
	AllowSend      bool             // false — только читаем, ответы не шлём (обкатка)
	StoreText      bool             // false — текст в БД не пишем (приватность)
	MaxReqPerMin   int              // бюджет запросов к сайту у поллера talks
	ForbiddenLimit int              // подряд ошибок сайта → kill-switch (стоп поллера)
	// ExcludeUsers — messenger → id владельцев сессий, чью переписку не носим в
	// мессенджер (человек отказался от доставки). Сессия при этом живая: она
	// нужна мосту «ответ в чате → комментарий на сайте». Запрет админа: сильнее
	// выбора самого человека (см. delivery.go).
	ExcludeUsers map[string][]int64
	AlertSend    func(ctx context.Context, text string)
	// AskDelivery — спросить человека, куда носить его ЛС: сайт-аккаунт
	// залогинен в двух мессенджерах, а получатель может быть только один.
	// current — сессия, куда носим до его ответа (целиком, а не одно имя
	// мессенджера: вторым входом бывает и другой аккаунт в том же мессенджере).
	// Сообщение с кнопками умеет собрать только диалоговое ядро, поэтому это
	// замыкание из runDaemon; nil — не спрашиваем (CLI-прогоны).
	AskDelivery func(ctx context.Context, messenger string, userID int64, current store.TalksOwner)
}

// Watcher — поллер talks и роутер ответов. Поллинг идёт в одной горутине
// (Run); HandleReply зовётся из обработчиков апдейтов мессенджеров. Общее
// изменяемое состояние (errStreak/stopped) трогает только Run; store и лимитер
// потокобезопасны.
type Watcher struct {
	st         *store.Store
	site       SiteTalks
	transports []PMTransport
	cfg        Config
	alert      *alerts.Alerter
	limiter    *rate.Limiter
	log        *slog.Logger

	errStreak int
	stopped   bool
	// lastUnread — последнее виденное число непрочитанных по peer.id: сигнал
	// «есть новое» без mark-read (loadBuddiesList не отдаёт last-msg-id).
	// Трогает только горутина Run — без мьютекса. Сбрасывается при рестарте
	// (тогда один лишний дозабор на диалог, дальше по дельте). Запоминаем счётчик
	// и когда дозабора не было: чтение истории гасит непрочитанное на самом
	// сайте, и без этой фиксации следующее сообщение вернуло бы unread к уже
	// виденному значению — то есть новым бы не считалось.
	lastUnread map[int64]int
	// lastFetchAt — когда последний раз дозабирали историю диалога (страховка от
	// залипшего счётчика, см. staleUnreadAfter). Тоже только из Run.
	lastFetchAt map[int64]time.Time
	// unreachable — владельцы сессий, кому доставлять некуда: заблокировали бота
	// или ни разу не открывали с ним диалог (бот не пишет первым — а бот
	// переписки для многих новый, они его и не запускали). Их обход прекращаем:
	// читать чужую переписку, зная, что она никуда не уедет, — значит впустую
	// гасить человеку непрочитанное на сайте. Живёт в памяти и снимается
	// рестартом: после «/start» у бота человек чинится сам, ценой одной попытки.
	unreachable map[ownerKey]bool
	// excluded — отказавшиеся от доставки (Config.ExcludeUsers), развёрнутые в
	// множество. В отличие от unreachable это решение человека, а не сбой:
	// снимается только правкой конфига.
	excluded map[ownerKey]bool
	// identityTried — у кого уже пробовали снять паспорт в этом запуске (см.
	// captureIdentity): страница отдаёт его не всегда, а тратить на это по
	// запросу к сайту каждый такт незачем.
	identityTried map[ownerKey]bool
}

// ownerKey — владелец сессии в конкретном мессенджере (пространства id разные).
type ownerKey struct {
	messenger string
	user      int64
}

// New создаёт поллер. transports — по одному на включённый мессенджер.
func New(st *store.Store, site SiteTalks, transports []PMTransport, cfg Config, log *slog.Logger) *Watcher {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.IdleInterval <= 0 {
		cfg.IdleInterval = 5 * time.Minute
	}
	if cfg.MaxDialogs <= 0 {
		cfg.MaxDialogs = 5
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 50
	}
	if cfg.MaxReqPerMin <= 0 {
		cfg.MaxReqPerMin = 6
	}
	if cfg.ForbiddenLimit <= 0 {
		cfg.ForbiddenLimit = 3
	}
	burst := 3
	if cfg.MaxReqPerMin < burst {
		burst = cfg.MaxReqPerMin
	}
	return &Watcher{
		st:            st,
		site:          site,
		transports:    transports,
		cfg:           cfg,
		alert:         alerts.New(cfg.AlertSend, cfg.ForbiddenLimit),
		limiter:       rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.MaxReqPerMin)), burst),
		log:           log,
		lastUnread:    make(map[int64]int),
		lastFetchAt:   make(map[int64]time.Time),
		unreachable:   make(map[ownerKey]bool),
		excluded:      excludedSet(cfg.ExcludeUsers),
		identityTried: make(map[ownerKey]bool),
	}
}

// excludedSet разворачивает конфигурационные списки в множество владельцев.
func excludedSet(byMessenger map[string][]int64) map[ownerKey]bool {
	set := make(map[ownerKey]bool)
	for messenger, users := range byMessenger {
		for _, u := range users {
			set[ownerKey{messenger, u}] = true
		}
	}
	return set
}

func toStoreMsg(peerID int64, m love.TalkMessage, storeText bool) store.TalkMessage {
	dir := store.TalkIn
	if m.FromSelf {
		dir = store.TalkOut
	}
	text := m.Text
	if !storeText {
		text = ""
	}
	return store.TalkMessage{
		PeerID: peerID, SiteMsgID: m.SiteMsgID, Direction: dir,
		Text: text, MediaURL: m.MediaURL, SentAt: m.SentAt,
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
