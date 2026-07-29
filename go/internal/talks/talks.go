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
	AlertSend      func(ctx context.Context, text string)
}

// Ключи уведомлений админу.
const (
	keyForbidden = "доступ к сайту talks (403)"
	keyDrift     = "ошибка API talks"
)

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
	// (тогда один лишний дозабор на диалог, дальше по дельте).
	lastUnread map[int64]int
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
		st:         st,
		site:       site,
		transports: transports,
		cfg:        cfg,
		alert:      alerts.New(cfg.AlertSend, cfg.ForbiddenLimit),
		limiter:    rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.MaxReqPerMin)), burst),
		log:        log,
		lastUnread: make(map[int64]int),
	}
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

// pollOnce обходит все транспорты и их владельцев сессий один раз.
func (w *Watcher) pollOnce(ctx context.Context) bool {
	active := false
	for _, tr := range w.transports {
		for _, owner := range w.owners(ctx, tr.Name()) {
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

// owners — владельцы валидных сессий, кого обходим в мессенджере. В admin-only
// оставляем только заданного для этого мессенджера админа (у каждого своё
// пространство id).
func (w *Watcher) owners(ctx context.Context, messenger string) []int64 {
	owners, err := w.st.SessionOwners(ctx, messenger)
	if err != nil {
		w.log.Error("список сессий talks", "messenger", messenger, "err", err)
		return nil
	}
	if !w.cfg.AdminOnly {
		return owners
	}
	admin := w.cfg.AdminIDs[messenger]
	if admin == 0 {
		return nil // admin-only, но админ для мессенджера не задан
	}
	for _, o := range owners {
		if o == admin {
			return []int64{admin}
		}
	}
	return nil
}

// pollOwner опрашивает список диалогов одного владельца и дозабирает новые.
func (w *Watcher) pollOwner(ctx context.Context, tr PMTransport, owner int64) bool {
	messenger := tr.Name()
	cookies, ok := w.cookies(ctx, messenger, owner)
	if !ok {
		return false
	}
	if err := w.limiter.Wait(ctx); err != nil {
		return false
	}
	dialogs, err := w.site.Dialogs(ctx, cookies, w.cfg.MaxDialogs)
	if err != nil {
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
	// Сигнал новой активности: либо last-msg-id диалога сдвинулся (если сайт его
	// отдаёт), либо выросло число непрочитанных (loadBuddiesList отдаёт только
	// его). Без mark-read счётчик залипает, поэтому сравниваем с прошлым виденным.
	byMsgID := d.LastMsgID != "" && d.LastMsgID != peer.CursorMsgID
	byUnread := d.Unread > 0 && d.Unread != w.lastUnread[peerID]
	if !byMsgID && !byUnread {
		return false, false // новой активности нет; недоставленного тоже (курсор идёт лишь по доставленным)
	}

	if err := w.limiter.Wait(ctx); err != nil {
		return false, false
	}
	msgs, err := w.site.History(ctx, cookies, d.PassportID, peer.CursorMsgID, w.cfg.HistoryLimit)
	fetched = true
	if err != nil {
		w.handleSiteError(ctx, err)
		return fetched, false
	}
	w.onSiteOK(ctx)
	w.lastUnread[peerID] = d.Unread // дозабор состоялся — запоминаем счётчик

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

// deliverOne доставляет одно входящее сообщение (идемпотентно по
// message_targets). stop=true — доставка не удалась, курсор двигать нельзя.
func (w *Watcher) deliverOne(ctx context.Context, tr PMTransport, owner int64, peer store.TalkPeer, m love.TalkMessage, rowID int64) (delivered, stop bool) {
	if m.FromSelf || rowID == 0 {
		return false, false // своё исходящее не доставляем; курсор идёт дальше
	}
	if _, _, found, _ := w.st.Target(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID)); found {
		return false, false // уже доставлено ранее
	}
	msgID, err := tr.SendPM(ctx, owner, formatIncoming(w.cfg.BaseURL, peer, m))
	if err != nil {
		w.log.Warn("доставка ЛС talks не удалась", "user", owner, "err", err)
		return false, true
	}
	if err := w.st.SetTarget(ctx, tr.Name(), store.TargetPMMessage, itoa(rowID), msgID, ""); err != nil {
		w.log.Error("привязка доставленного ЛС talks", "err", err)
	}
	return true, false
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
		w.log.Warn("отправка ЛС на сайт не удалась", "user", userID, "err", err)
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
	if err != nil || !valid {
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
