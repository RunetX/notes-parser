// Пакет talks — личная переписка сайта (talks) в мессенджеры. Мессенджер-
// агностичное ядро: один поллер сайта (общий rate-лимитер) фанит входящие ЛС в
// личку РюмкинЪ каждого включённого мессенджера, а ответы реплаем/командой
// отправляет на сайт от имени сессии пользователя. Транспорт мессенджера —
// за интерфейсом PMTransport (реализуют tgx и maxx), клиент сайта — за
// SiteTalks (реализует love.Client в Ф4). См. briefs/love-talks-telegram.md.
package talks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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

// Ключи уведомлений админу.
const (
	keyForbidden   = "доступ к сайту talks (403)"
	keyDrift       = "ошибка API talks"
	keyUnavailable = "сайт talks недоступен"
)

// undeliveredWindow — сколько ещё пытаться дослать входящее, застрявшее из-за
// сбоя мессенджера. Граница нужна: история сайта отдаёт только первую страницу
// (20 сообщений), и уехавшее за неё живым дозабором уже не достать — без окна
// такой хвост заставлял бы перезапрашивать диалог каждый такт вечно. Что старше
// — только руками: `lovegw talks watch -backfill N`.
const undeliveredWindow = 48 * time.Hour

// staleUnreadAfter — как долго верить залипшему счётчику непрочитанных, прежде
// чем перепроверить диалог живьём. Дельта счётчика слепа к случаю «сайт погасил
// непрочитанное нашим же чтением истории, а потом пришло одно новое»: счётчик
// возвращается к тому же значению, при котором мы читали, и новым не считается.
// Один лишний запрос в четверть часа на диалог с непрочитанным — дешевле
// потерянного сообщения; доставка дедуплицирована по message_targets.
const staleUnreadAfter = 15 * time.Minute

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

// Run крутит поллер до отмены контекста (или срабатывания kill-switch).
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.transports) == 0 {
		return nil
	}
	w.log.Info("поллер talks запущен",
		"admin_only", w.cfg.AdminOnly, "allow_send", w.cfg.AllowSend,
		"store_text", w.cfg.StoreText, "interval", w.cfg.Interval)
	for ctx.Err() == nil {
		active := w.pollOnce(ctx)
		if w.stopped {
			return nil // kill-switch: зеркало продолжает работать
		}
		wait := w.cfg.IdleInterval
		if active {
			wait = w.cfg.Interval
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// PollOnce делает один проход поллера и возвращает, была ли активность.
// Для отладочного/безопасного прогона `lovegw talks watch -once`.
func (w *Watcher) PollOnce(ctx context.Context) bool { return w.pollOnce(ctx) }

// pollOnce обходит все транспорты и их владельцев сессий один раз. План
// доставки считается разом на весь такт: у сайт-аккаунта, залогиненного в двух
// мессенджерах, получатель ровно один (см. delivery.go).
func (w *Watcher) pollOnce(ctx context.Context) bool {
	active := false
	plan := w.plan(ctx)
	for _, tr := range w.transports {
		for _, owner := range plan[tr.Name()] {
			if w.pollOwner(ctx, tr, owner) {
				active = true
			}
			if w.stopped {
				return active
			}
		}
	}
	return active
}

// pollOwner опрашивает список диалогов одного владельца и дозабирает новые.
func (w *Watcher) pollOwner(ctx context.Context, tr PMTransport, o store.TalksOwner) bool {
	messenger, owner := tr.Name(), o.UserID
	cookies, ok := w.cookies(ctx, messenger, owner)
	if !ok {
		return false
	}
	w.captureIdentity(ctx, o, cookies)
	if err := w.limiter.Wait(ctx); err != nil {
		return false
	}
	dialogs, err := w.site.Dialogs(ctx, cookies, w.cfg.MaxDialogs)
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, owner) // сессия этого юзера истекла — не роняем поллер
			return false
		}
		w.handleSiteError(ctx, err)
		return false
	}
	w.onSiteOK(ctx)

	active, fetches := false, 0
	for _, d := range dialogs {
		if fetches >= w.cfg.MaxDialogs {
			break
		}
		fetched, act := w.processDialog(ctx, tr, owner, cookies, d)
		if fetched {
			fetches++
		}
		if act {
			active = true
		}
		if w.stopped {
			break
		}
	}
	return active
}

// processDialog заводит/обновляет собеседника и, если есть новая активность,
// дозабирает историю после курсора и доставляет входящие. Курсор двигается
// только по успешно доставленным сообщениям — недоставленные переотправятся
// повторным History на следующем тике (важно при store_text=false: текст
// берётся живым из истории, а не из БД).
func (w *Watcher) processDialog(ctx context.Context, tr PMTransport, owner int64, cookies []*http.Cookie, d love.TalkDialog) (fetched, active bool) {
	messenger := tr.Name()
	peerID, err := w.st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: messenger, OwnerUserID: owner, PassportID: d.PassportID,
		ProfileID: d.ProfileID, Nick: d.Nick, AvatarURL: d.AvatarURL,
	})
	if err != nil {
		w.log.Error("upsert собеседника talks", "err", err)
		return false, false
	}
	peer, err := w.st.TalkPeerByID(ctx, peerID)
	if err != nil {
		return false, false
	}
	if !w.needsFetch(ctx, messenger, peerID, peer, d) {
		w.lastUnread[peerID] = d.Unread // фиксируем и без дозабора — см. коммент к полю
		return false, false
	}

	if err := w.limiter.Wait(ctx); err != nil {
		return false, false
	}
	msgs, err := w.site.History(ctx, cookies, d.PassportID, peer.CursorMsgID, w.cfg.HistoryLimit)
	fetched = true
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, owner)
			return fetched, false
		}
		w.handleSiteError(ctx, err)
		return fetched, false
	}
	w.onSiteOK(ctx)
	w.lastUnread[peerID] = d.Unread // дозабор состоялся — запоминаем счётчик
	w.lastFetchAt[peerID] = time.Now()

	newCursor, cursorTime := peer.CursorMsgID, peer.LastEventAt
	for _, m := range msgs {
		rowID, _, err := w.st.InsertTalkMessage(ctx, toStoreMsg(peerID, m, w.cfg.StoreText))
		if err != nil {
			w.log.Error("запись сообщения talks", "err", err)
			break
		}
		delivered, stop := w.deliverOne(ctx, tr, owner, peer, m, rowID)
		if delivered {
			active = true
		}
		if stop {
			break // доставка не удалась — не двигаем курсор дальше этого сообщения
		}
		newCursor, cursorTime = m.SiteMsgID, m.SentAt
	}
	if newCursor != peer.CursorMsgID {
		if err := w.st.SetPeerCursor(ctx, peerID, newCursor, cursorTime); err != nil {
			w.log.Error("сдвиг курсора talks", "err", err)
		}
	}
	return fetched, active
}

// needsFetch решает, дозабирать ли историю диалога. Сигнал новой активности:
// либо last-msg-id диалога сдвинулся (если сайт его отдаёт — loadBuddiesList
// его НЕ отдаёт, поэтому в бою работает только вторая ветка), либо число
// непрочитанных отличается от виденного в прошлый раз.
//
// Третья ветка — недоставленное: сбой мессенджера не должен съедать сообщение.
// Курсор на нём стоит, но одного этого мало — дозабор запускают счётчики, а они
// после чтения истории уже погашены (сайт помечает сообщения прочитанными), и
// повторной попытки не наступало бы никогда. Поэтому спрашиваем БД: есть ли
// входящее без записи в message_targets. Признак переживает и рестарт демона,
// в отличие от любого флага в памяти.
func (w *Watcher) needsFetch(ctx context.Context, messenger string, peerID int64, peer store.TalkPeer, d love.TalkDialog) bool {
	if d.LastMsgID != "" && d.LastMsgID != peer.CursorMsgID {
		return true
	}
	if d.Unread > 0 && d.Unread != w.lastUnread[peerID] {
		return true
	}
	if d.Unread > 0 && time.Since(w.lastFetchAt[peerID]) >= staleUnreadAfter {
		return true // счётчик залип на прежнем значении — см. staleUnreadAfter
	}
	pending, err := w.st.HasUndeliveredIncoming(ctx, messenger, peerID, time.Now().Add(-undeliveredWindow))
	if err != nil {
		w.log.Error("проверка недоставленных ЛС talks", "peer", peerID, "err", err)
		return false
	}
	if pending {
		w.log.Debug("в диалоге есть недоставленное ЛС — переспрашиваю историю", "peer", peerID)
	}
	return pending
}

// deliverOne доставляет одно входящее сообщение (идемпотентно по
// message_targets). stop=true — доставка не удалась, курсор двигать нельзя.
func (w *Watcher) deliverOne(ctx context.Context, tr PMTransport, owner int64, peer store.TalkPeer, m love.TalkMessage, rowID int64) (delivered, stop bool) {
	if m.FromSelf || rowID == 0 {
		return false, false // своё исходящее не доставляем; курсор идёт дальше
	}
	_, _, found, err := w.st.Target(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID))
	if err != nil {
		// Эта проверка — единственный дедуп доставки: ошибка БД здесь не
		// «не доставлено», а «неизвестно». Останавливаемся, не двигая курсор:
		// задержка лучше дубля в ЛС.
		w.log.Error("проверка доставки ЛС talks", "peer", peer.ID, "row", rowID, "err", err)
		return false, true
	}
	if found {
		return false, false // уже доставлено ранее
	}
	msgID, err := tr.SendPM(ctx, owner, formatIncoming(w.cfg.BaseURL, peer, m))
	if err != nil {
		w.log.Warn("доставка ЛС talks не удалась", "user", owner, "err", err)
		if isUnreachable(err) {
			w.markUnreachable(ctx, tr.Name(), owner, err)
		}
		return false, true
	}
	if err := w.st.SetTarget(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID), msgID, ""); err != nil {
		w.log.Error("привязка доставленного ЛС talks", "err", err)
	}
	return true, false
}

// isUnreachable — отказ мессенджера, который повтором не лечится: пользователь
// заблокировал бота или ни разу не открывал с ним диалог (первым бот писать не
// может). Сравниваем строки, а не типизированные ошибки: SDK у мессенджеров
// разные, общего кода отказа нет — в Telegram это «Forbidden: bot was blocked by
// the user» и «Bad Request: chat not found», в MAX — код `dialog.not.found`
// («sending message failed: dialog.not.found : Dialog not found», снято с боя
// 12.08.2026). Цена промаха здесь не «одно потерянное сообщение», а поток:
// нераспознанный отказ заставляет поллер читать переписку человека каждый такт,
// сайт при каждом чтении помечает её прочитанной, а доставки нет — ЛС пропадают
// молча. Неопознанный отказ всё же считаем временным: снять с обхода живого
// человека хуже, чем лишний раз попробовать.
func isUnreachable(err error) bool {
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"blocked by the user",
		"chat not found",
		"dialog.not.found", // MAX: человек не открывал диалог с ботом
		"user is deactivated",
		"bot can't initiate conversation",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// markUnreachable снимает владельца сессии с обхода и один раз сообщает об этом
// админу: сам пользователь уведомления не получит — доставлять ему как раз и
// нечем.
func (w *Watcher) markUnreachable(ctx context.Context, messenger string, owner int64, cause error) {
	k := ownerKey{messenger, owner}
	if w.unreachable[k] {
		return
	}
	w.unreachable[k] = true
	w.log.Warn("ЛС talks доставлять некуда — диалоги этого пользователя больше не опрашиваю",
		"messenger", messenger, "user", owner, "err", cause)
	if w.cfg.AlertSend != nil {
		w.cfg.AlertSend(ctx, fmt.Sprintf(
			"личная переписка не доставляется пользователю %d (%s): %v. Он заблокировал бота или не открывал с ним диалог; опрос его диалогов остановлен до рестарта демона.",
			owner, messenger, cause))
	}
}

// DeliverExisting принудительно доставляет последние perDialog ВХОДЯЩИХ из
// первых maxDialogs диалогов каждого владельца, игнорируя триггер непрочитанного
// — для обкатки/бэкфилла: показать уже существующую переписку в мессенджере,
// когда всё на сайте прочитано. Читаем под сессией владельца, а доставляем в
// deliverTo (0 — самому владельцу): удобно, когда демонстрируешь в другой
// аккаунт. Идемпотентно по message_targets (повтор не задваивает); на сайт
// ничего не пишет. Возвращает число доставленных сообщений.
func (w *Watcher) DeliverExisting(ctx context.Context, maxDialogs, perDialog int, deliverTo int64) (int, error) {
	total := 0
	plan := w.plan(ctx)
	for _, tr := range w.transports {
		for _, o := range plan[tr.Name()] {
			owner := o.UserID
			cookies, ok := w.cookies(ctx, tr.Name(), owner)
			if !ok {
				continue
			}
			target := deliverTo
			if target == 0 {
				target = owner
			}
			if err := w.limiter.Wait(ctx); err != nil {
				return total, err
			}
			dialogs, err := w.site.Dialogs(ctx, cookies, maxDialogs)
			if err != nil {
				return total, err
			}
			for i, d := range dialogs {
				if i >= maxDialogs {
					break
				}
				n, err := w.deliverDialogTail(ctx, tr, owner, target, cookies, d, perDialog)
				if err != nil {
					return total, err
				}
				total += n
			}
		}
	}
	return total, nil
}

// deliverDialogTail доставляет последние perDialog входящих одного диалога
// (perDialog ≤ 0 — все со страницы истории) в чат target, не двигая
// курсор/триггеры. owner — владелец сессии (ключ собеседника), target — куда слать.
func (w *Watcher) deliverDialogTail(ctx context.Context, tr PMTransport, owner, target int64, cookies []*http.Cookie, d love.TalkDialog, perDialog int) (int, error) {
	peerID, err := w.st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: tr.Name(), OwnerUserID: owner, PassportID: d.PassportID,
		ProfileID: d.ProfileID, Nick: d.Nick, AvatarURL: d.AvatarURL,
	})
	if err != nil {
		return 0, err
	}
	peer, err := w.st.TalkPeerByID(ctx, peerID)
	if err != nil {
		return 0, err
	}
	if err := w.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	msgs, err := w.site.History(ctx, cookies, d.PassportID, "", w.cfg.HistoryLimit)
	if err != nil {
		return 0, err
	}
	var incoming []love.TalkMessage
	for _, m := range msgs {
		if !m.FromSelf {
			incoming = append(incoming, m)
		}
	}
	if perDialog > 0 && len(incoming) > perDialog {
		incoming = incoming[len(incoming)-perDialog:]
	}
	n := 0
	for _, m := range incoming {
		rowID, _, err := w.st.InsertTalkMessage(ctx, toStoreMsg(peerID, m, w.cfg.StoreText))
		if err != nil {
			return n, err
		}
		if delivered, _ := w.deliverOne(ctx, tr, target, peer, m, rowID); delivered {
			n++
		}
	}
	return n, nil
}

// HandleReply маршрутизирует ответ пользователя (реплай на доставленное ЛС) в
// диалог сайта. replyMsgID — id самого ответа (для at-most-once), replyToID —
// id сообщения, на которое ответили. Возвращает true, если сообщение относится
// к talks (обработано — дальше его никто не трогает).
func (w *Watcher) HandleReply(ctx context.Context, messenger, replyMsgID string, userID int64, replyToID, text string) bool {
	if text == "" {
		return false
	}
	peer, err := w.st.PeerByDeliveredPM(ctx, messenger, replyToID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			w.log.Error("поиск диалога по реплаю", "err", err)
		}
		return false // ответ не на talks-сообщение — не наш
	}
	if peer.OwnerUserID != userID {
		w.log.Warn("реплай на чужой диалог talks — игнор", "owner", peer.OwnerUserID, "from", userID)
		return true
	}
	tr := w.transportFor(messenger)
	if tr == nil {
		return false
	}
	if !w.cfg.AllowSend {
		tr.Confirm(ctx, userID, replyMsgID, false)
		return true
	}
	// At-most-once ДО отправки: потерянный ответ лучше задвоенного (как в bridge).
	fresh, err := w.st.TryMarkReplyProcessed(ctx, messenger, "dm:"+replyMsgID, time.Now())
	if err != nil || !fresh {
		return true
	}
	w.SendToPeer(ctx, tr, userID, peer, replyMsgID, text)
	return true
}

// SendToPeer отправляет текст в диалог сайта от имени сессии пользователя и
// подтверждает реакцией. Используется и реплаем (HandleReply), и командой /talk
// (Ф3, залипание на диалоге).
func (w *Watcher) SendToPeer(ctx context.Context, tr PMTransport, userID int64, peer store.TalkPeer, ackID, text string) {
	messenger := tr.Name()
	cookies, ok := w.cookies(ctx, messenger, userID)
	if !ok {
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	if err := w.limiter.Wait(ctx); err != nil {
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	sent, err := w.site.Send(ctx, cookies, peer.PassportID, text)
	if err != nil {
		if errors.Is(err, love.ErrUnauthorized) {
			w.invalidateOwner(ctx, tr, userID)
		} else {
			w.log.Warn("отправка ЛС на сайт не удалась", "user", userID, "err", err)
		}
		tr.Confirm(ctx, userID, ackID, false)
		return
	}
	_ = w.st.SetSessionValid(ctx, messenger, userID, true, time.Now())
	text2 := text
	if !w.cfg.StoreText {
		text2 = ""
	}
	if _, _, err := w.st.InsertTalkMessage(ctx, store.TalkMessage{
		PeerID: peer.ID, SiteMsgID: sent.SiteMsgID, Direction: store.TalkOut,
		Text: text2, MediaURL: sent.MediaURL, SentAt: sent.SentAt,
	}); err != nil {
		w.log.Error("запись исходящего talks", "err", err)
	}
	tr.Confirm(ctx, userID, ackID, true)
}

// SendToDialog отправляет текст в диалог по его id — команда /talk (залипание
// на собеседнике). Проверяет, что диалог принадлежит этому пользователю в этом
// мессенджере. false — диалог чужой/не найден. ackID — id сообщения-исходника
// для реакции-подтверждения.
func (w *Watcher) SendToDialog(ctx context.Context, messenger string, userID, peerID int64, ackID, text string) bool {
	if text == "" {
		return false
	}
	peer, err := w.st.TalkPeerByID(ctx, peerID)
	if err != nil || peer.Messenger != messenger || peer.OwnerUserID != userID {
		return false
	}
	tr := w.transportFor(messenger)
	if tr == nil {
		return false
	}
	if !w.cfg.AllowSend {
		tr.Confirm(ctx, userID, ackID, false)
		return true
	}
	w.SendToPeer(ctx, tr, userID, peer, ackID, text)
	return true
}

// TransportFor возвращает транспорт мессенджера (для проводки команд в Ф3).
func (w *Watcher) TransportFor(messenger string) PMTransport { return w.transportFor(messenger) }

func (w *Watcher) transportFor(messenger string) PMTransport {
	for _, tr := range w.transports {
		if tr.Name() == messenger {
			return tr
		}
	}
	return nil
}

// cookies читает и разбирает куки сессии; невалидную/битую помечает invalid.
func (w *Watcher) cookies(ctx context.Context, messenger string, owner int64) ([]*http.Cookie, bool) {
	cookiesJSON, valid, err := w.st.SessionCookies(ctx, messenger, owner)
	if err != nil {
		// Чаще всего это «нет ключа шифрования» или чужой ключ: владелец молча
		// выпадает из обхода, и без записи в лог причину не найти.
		w.log.Error("не прочитать сессию владельца", "messenger", messenger,
			"owner", owner, "err", err)
		return nil, false
	}
	if !valid {
		return nil, false
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil || len(cookies) == 0 {
		_ = w.st.SetSessionValid(ctx, messenger, owner, false, time.Now())
		return nil, false
	}
	return cookies, true
}

// handleSiteError троттлит алерт и после ForbiddenLimit подряд ошибок сайта
// глушит поллер (kill-switch), не трогая зеркало.
func (w *Watcher) handleSiteError(ctx context.Context, err error) {
	// Временный отказ (5xx фронта, обрыв связи) в kill-switch не идёт: он
	// проходит сам, а остановленный до рестарта поллер теряет входящие ЛС —
	// история сайта отдаёт только последнюю страницу, и уехавшее за неё живым
	// дозабором уже не достать. Такой сбой копится в алертере: три подряд —
	// одно сообщение админу, первый успех — «восстановилось». Поллинг при этом
	// продолжается на холостом интервале. Боевой случай: 502 на
	// loadBuddiesList 12.08.2026 — поллер лёг до ручного рестарта.
	if errors.Is(err, love.ErrSiteUnavailable) {
		w.alert.Fail(ctx, keyUnavailable, err.Error())
		return
	}
	w.errStreak++
	key, detail := keyDrift, err.Error()
	if errors.Is(err, love.ErrForbidden) {
		key, detail = keyForbidden, "сайт вернул 403 (геоблок или бан IP)"
	}
	if w.errStreak >= w.cfg.ForbiddenLimit {
		w.stop(ctx, key+": "+detail)
		return
	}
	w.alert.Fail(ctx, key, detail)
}

func (w *Watcher) onSiteOK(ctx context.Context) {
	w.errStreak = 0
	w.alert.OK(ctx, keyForbidden)
	w.alert.OK(ctx, keyDrift)
	w.alert.OK(ctx, keyUnavailable)
}

const sessionExpiredMsg = "🔒 Сессия НГС.Лав истекла — личные сообщения на паузе. Войдите снова: /login"

// invalidateOwner помечает сессию владельца невалидной и уведомляет его о
// повторном входе. В мультисессии истёкшая сессия ОДНОГО пользователя (гостевой
// ответ talks, ErrUnauthorized) не должна ронять поллер для остальных — в отличие
// от 403/дрейфа, которые глобальны и ведут к kill-switch. Невалидная сессия
// выпадает из плана доставки (TalksOwners берёт только valid=1), поэтому
// уведомление уходит один раз.
func (w *Watcher) invalidateOwner(ctx context.Context, tr PMTransport, owner int64) {
	if err := w.st.SetSessionValid(ctx, tr.Name(), owner, false, time.Now()); err != nil {
		w.log.Error("сброс истёкшей сессии talks", "messenger", tr.Name(), "user", owner, "err", err)
	}
	if _, err := tr.SendPM(ctx, owner, sessionExpiredMsg); err != nil {
		w.log.Debug("уведомление о протухшей сессии не отправлено", "user", owner, "err", err)
	}
	w.log.Info("сессия talks истекла — на паузе до /login", "messenger", tr.Name(), "user", owner)
}

func (w *Watcher) stop(ctx context.Context, reason string) {
	if w.stopped {
		return
	}
	w.stopped = true
	w.log.Error("поллер talks остановлен (kill-switch)", "reason", reason)
	if w.cfg.AlertSend != nil {
		w.cfg.AlertSend(ctx, "поллер talks остановлен: "+reason+". Зеркало работает; включить снова — рестарт.")
	}
}

// PurgeLoop периодически удаляет сообщения talks старше retentionDays
// (приватность: в БД не копится переписка). Метаданные собеседников остаются —
// они лёгкие. Прогон раз в 12ч, первый — сразу на старте. retentionDays ≤ 0 —
// очистка выключена. Блокируется до отмены контекста.
func PurgeLoop(ctx context.Context, st *store.Store, retentionDays int, log *slog.Logger) error {
	if retentionDays <= 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	purge := func() {
		cutoff := time.Now().AddDate(0, 0, -retentionDays)
		n, err := st.PurgeTalksOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("retention talks", "err", err)
			return
		}
		if n > 0 {
			log.Info("retention talks: старые сообщения удалены", "n", n, "older_than_days", retentionDays)
		}
	}
	purge()
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			purge()
		}
	}
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
