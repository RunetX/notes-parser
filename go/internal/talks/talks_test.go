package talks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// --- фейки ---

type fakeSite struct {
	dialogs    []love.TalkDialog
	history    map[string][]love.TalkMessage
	dialogsErr error
	historyErr error
	// markRead повторяет поведение боевого сайта: чтение истории гасит
	// непрочитанное (собеседник видит «просмотрено»). В боевой разметке
	// loadBuddiesList при этом НЕ отдаёт last-msg-id, поэтому в тестах с
	// markRead поле LastMsgID оставляем пустым — как в жизни.
	markRead     bool
	historyCalls int // сколько раз дозабирали историю (бюджет запросов к сайту)

	sent       []sentToSite
	sendReturn love.TalkMessage
	sendErr    error
}

type sentToSite struct {
	passportID string
	text       string
}

func (f *fakeSite) Dialogs(_ context.Context, _ []*http.Cookie, _ int) ([]love.TalkDialog, error) {
	if f.dialogsErr != nil {
		return nil, f.dialogsErr
	}
	return f.dialogs, nil
}

func (f *fakeSite) History(_ context.Context, _ []*http.Cookie, passportID, _ string, _ int) ([]love.TalkMessage, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	f.historyCalls++
	if f.markRead {
		for i := range f.dialogs {
			if f.dialogs[i].PassportID == passportID {
				f.dialogs[i].Unread = 0
			}
		}
	}
	return f.history[passportID], nil
}

func (f *fakeSite) Send(_ context.Context, _ []*http.Cookie, passportID, text string) (love.TalkMessage, error) {
	if f.sendErr != nil {
		return love.TalkMessage{}, f.sendErr
	}
	f.sent = append(f.sent, sentToSite{passportID, text})
	return f.sendReturn, nil
}

type fakeTransport struct {
	name     string
	sendErr  error
	nextID   int
	sent     []sentPM
	confirms []confirmCall
}

type sentPM struct {
	userID int64
	html   string
	msgID  string
}

type confirmCall struct {
	userID int64
	msgID  string
	ok     bool
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) SendPM(_ context.Context, userID int64, html string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.nextID++
	id := "d" + itoa(int64(f.nextID))
	f.sent = append(f.sent, sentPM{userID, html, id})
	return id, nil
}

func (f *fakeTransport) Confirm(_ context.Context, userID int64, msgID string, ok bool) {
	f.confirms = append(f.confirms, confirmCall{userID, msgID, ok})
}

// --- хелперы ---

const testOwner = 42

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedSession(t *testing.T, st *store.Store, owner int64) {
	t.Helper()
	ctx := context.Background()
	ck := []*http.Cookie{{Name: "sess", Value: "abc", Domain: "love.ngs.ru", Path: "/", Expires: time.Now().Add(24 * time.Hour)}}
	js, err := love.CookiesToJSON(ck, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, store.MessengerTelegram, owner, js, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func testConfig() Config {
	return Config{
		AdminOnly: true, AdminIDs: map[string]int64{store.MessengerTelegram: testOwner},
		AllowSend: true, StoreText: false, MaxReqPerMin: 6000, ForbiddenLimit: 3,
		BaseURL: "https://love.ngs.ru",
	}
}

// --- тесты ---

func TestPollDeliversOnceAndAdvancesCursor(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Мария", ProfileID: "555", LastMsgID: "m2"}},
		history: map[string][]love.TalkMessage{
			"777": {{SiteMsgID: "m1", Text: "привет"}, {SiteMsgID: "m2", Text: "как дела?"}},
		},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("ожидалось 2 доставки, got %d", len(tr.sent))
	}
	// HTML содержит ник и ссылку на анкету.
	if got := tr.sent[0].html; !strings.Contains(got, "Мария") || !strings.Contains(got, "/profile/555/") {
		t.Errorf("HTML без ника/анкеты: %q", got)
	}

	// Повторный опрос: активности нет (LastMsgID == курсор) — доставок не прибавилось.
	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("повторная доставка (задвоение): %d", len(tr.sent))
	}

	// Новое сообщение: fake History отдаёт всё, дедуп по target не даёт задвоить.
	site.dialogs[0].LastMsgID = "m3"
	site.history["777"] = append(site.history["777"], love.TalkMessage{SiteMsgID: "m3", Text: "ещё"})
	w.pollOnce(ctx)
	if len(tr.sent) != 3 {
		t.Fatalf("ожидалась одна новая доставка, всего %d", len(tr.sent))
	}
}

func TestStoreTextFalseKeepsDBEmptyButDeliversText(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Ева", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "секрет"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)
	w.pollOnce(ctx)

	if len(tr.sent) != 1 || !strings.Contains(tr.sent[0].html, "секрет") {
		t.Fatalf("живой текст должен уйти в мессенджер: %+v", tr.sent)
	}
	// В БД текст пуст (store_text=false).
	peers, _ := st.TalkPeers(ctx, store.MessengerTelegram, testOwner)
	if len(peers) != 1 {
		t.Fatalf("ожидался 1 диалог, got %d", len(peers))
	}
}

// Затык в мессенджере не должен съедать сообщение: сайт уже пометил его
// прочитанным нашим же чтением истории, счётчик непрочитанных обнулился, и
// прежний триггер (только дельта счётчика) не переспрашивал диалог никогда —
// сообщение оставалось «просмотрено на сайте, но не доставлено».
func TestDeliveryFailureIsRetried(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		markRead: true,
		dialogs:  []love.TalkDialog{{PassportID: "777", Nick: "Ника", Unread: 2}},
		history: map[string][]love.TalkMessage{
			"777": {{SiteMsgID: "m1", Text: "раз"}, {SiteMsgID: "m2", Text: "два"}},
		},
	}
	tr := &fakeTransport{name: store.MessengerTelegram, sendErr: errors.New("мессенджер недоступен")}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx) // доставка падает, курсор не двигается; сайт погасил непрочитанное
	if len(tr.sent) != 0 {
		t.Fatalf("при ошибке доставка не должна учитываться: %d", len(tr.sent))
	}
	if site.dialogs[0].Unread != 0 {
		t.Fatal("предпосылка теста: чтение истории гасит непрочитанное на сайте")
	}
	tr.sendErr = nil
	w.pollOnce(ctx) // нового на сайте нет — дозабор держится на недоставленном в БД
	if len(tr.sent) != 2 {
		t.Fatalf("после восстановления ожидалось 2 доставки, got %d", len(tr.sent))
	}
	// Всё доставлено — лишних дозаборов больше нет.
	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("доставленное не должно слаться повторно: %d", len(tr.sent))
	}
}

// Сайт гасит непрочитанное нашим чтением, поэтому следующее сообщение возвращает
// счётчик к тому же значению, при котором мы читали в прошлый раз. Дельта его не
// видит — спасает фиксация счётчика на каждом наблюдении.
func TestNewMessageAfterSiteMarkedReadIsDelivered(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		markRead: true,
		dialogs:  []love.TalkDialog{{PassportID: "777", Nick: "Ника", Unread: 1}},
		history:  map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "раз"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx)
	w.pollOnce(ctx) // такт без новых: наблюдаем обнулённый счётчик

	site.dialogs[0].Unread = 1 // пришло одно новое — счётчик снова «1», как и был при чтении
	site.history["777"] = append(site.history["777"], love.TalkMessage{SiteMsgID: "m2", Text: "два"})
	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("новое сообщение при том же значении счётчика должно доставляться, got %d", len(tr.sent))
	}
}

// Тот же случай, но новое сообщение приходит ДО такта с обнулённым счётчиком —
// фиксировать нечего, и диалог спасает только перепроверка залипшего счётчика.
func TestStaleUnreadForcesRefetch(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		markRead: true,
		dialogs:  []love.TalkDialog{{PassportID: "777", Nick: "Ника", Unread: 1}},
		history:  map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "раз"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx)
	site.dialogs[0].Unread = 1 // новое пришло сразу после чтения
	site.history["777"] = append(site.history["777"], love.TalkMessage{SiteMsgID: "m2", Text: "два"})
	w.pollOnce(ctx)
	if len(tr.sent) != 1 {
		t.Fatalf("до истечения staleUnreadAfter лишних дозаборов нет: %d", len(tr.sent))
	}
	// Прошло больше staleUnreadAfter — счётчик перепроверяем живьём.
	for id := range w.lastFetchAt {
		w.lastFetchAt[id] = time.Now().Add(-staleUnreadAfter - time.Minute)
	}
	w.pollOnce(ctx)
	if len(tr.sent) != 2 {
		t.Fatalf("залипший счётчик должен приводить к дозабору, got %d", len(tr.sent))
	}
}

// Постоянный отказ мессенджера (бота заблокировали, диалога с ним нет) повтором
// не лечится: обход такого владельца прекращаем, иначе поллер каждый такт читает
// его переписку на сайте и гасит ему непрочитанное впустую. Админу — одно
// сообщение, не поток.
func TestUnreachableUserStopsPolling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		markRead: true,
		dialogs:  []love.TalkDialog{{PassportID: "777", Nick: "Ника", Unread: 1}},
		history:  map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "раз"}}},
	}
	tr := &fakeTransport{
		name:    store.MessengerTelegram,
		sendErr: errors.New("forbidden, Forbidden: bot was blocked by the user"),
	}
	var alerts []string
	cfg := testConfig()
	cfg.AlertSend = func(_ context.Context, text string) { alerts = append(alerts, text) }
	w := New(st, site, []PMTransport{tr}, cfg, nil)

	w.pollOnce(ctx)
	historyCalls := site.historyCalls
	w.pollOnce(ctx)
	w.pollOnce(ctx)
	if site.historyCalls != historyCalls {
		t.Fatalf("после постоянного отказа сайт опрашивать не должны: %d → %d", historyCalls, site.historyCalls)
	}
	if len(alerts) != 1 {
		t.Fatalf("админу ожидалось ровно одно сообщение, got %d: %v", len(alerts), alerts)
	}
	// Временный отказ — наоборот, повторяется (проверено в TestDeliveryFailureIsRetried).
	if w.stopped {
		t.Error("недоступность одного пользователя не должна ронять поллер целиком")
	}
}

// Отказ от доставки: сессия живая (она нужна мосту «ответ в чате → комментарий
// на сайте»), но переписку этого человека не читаем вовсе — иначе сайт пометит
// её прочитанной, а в мессенджер ничего не уедет.
func TestExcludedOwnerIsNotPolled(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Ника", Unread: 1}},
		history: map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "раз"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	cfg := testConfig()
	cfg.AdminOnly = false
	cfg.ExcludeUsers = map[string][]int64{store.MessengerTelegram: {testOwner}}
	w := New(st, site, []PMTransport{tr}, cfg, nil)

	w.pollOnce(ctx)
	if site.historyCalls != 0 || len(tr.sent) != 0 {
		t.Fatalf("исключённого владельца не опрашиваем: history=%d sent=%d", site.historyCalls, len(tr.sent))
	}
	// Сессия при этом остаётся валидной — её использует мост.
	owners, err := st.SessionOwners(ctx, store.MessengerTelegram)
	if err != nil || len(owners) != 1 {
		t.Fatalf("сессия должна остаться живой: owners=%v err=%v", owners, err)
	}
}

func TestDeliverExistingBackfill(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	// unread=0 и LastMsgID пуст — обычный поллинг не триггерится.
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", Nick: "Оля", ProfileID: "555"}},
		history: map[string][]love.TalkMessage{
			"777": {
				{SiteMsgID: "m1", Text: "раз"},
				{SiteMsgID: "m2", Text: "два", FromSelf: true},
				{SiteMsgID: "m3", Text: "три"},
			},
		},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	w.pollOnce(ctx)
	if len(tr.sent) != 0 {
		t.Fatalf("без непрочитанного поллинг не доставляет: %d", len(tr.sent))
	}

	// Бэкфилл последнего 1 входящего: m3 (исходящее m2 пропущено).
	n, err := w.DeliverExisting(ctx, 5, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(tr.sent) != 1 || !strings.Contains(tr.sent[0].html, "три") {
		t.Fatalf("бэкфилл: n=%d sent=%+v", n, tr.sent)
	}
	// deliverTo=0 → доставка владельцу сессии.
	if tr.sent[0].userID != testOwner {
		t.Errorf("ожидалась доставка владельцу %d, got %d", testOwner, tr.sent[0].userID)
	}
	// Идемпотентность: повтор не задваивает.
	n2, err := w.DeliverExisting(ctx, 5, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 || len(tr.sent) != 1 {
		t.Fatalf("повторный бэкфилл задвоил: n2=%d sent=%d", n2, len(tr.sent))
	}

	// Явный deliverTo → доставка в другой чат (демо в другой аккаунт).
	const other = 99
	n3, err := w.DeliverExisting(ctx, 5, 2, other)
	if err != nil {
		t.Fatal(err)
	}
	if n3 == 0 || tr.sent[len(tr.sent)-1].userID != other {
		t.Fatalf("deliverTo=%d не сработал: n3=%d last=%+v", other, n3, tr.sent[len(tr.sent)-1])
	}
}

func TestHandleReplyAtMostOnceAndJurisdiction(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs:    []love.TalkDialog{{PassportID: "777", Nick: "Юля", LastMsgID: "m1"}},
		history:    map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "привет"}}},
		sendReturn: love.TalkMessage{SiteMsgID: "out1", FromSelf: true},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)
	w.pollOnce(ctx)
	delivered := tr.sent[0].msgID // id доставленного ЛС, на него отвечаем реплаем

	// Реплай от владельца — одна отправка на сайт.
	if !w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "мой ответ") {
		t.Fatal("реплай на talks-сообщение должен быть обработан")
	}
	if len(site.sent) != 1 || site.sent[0].passportID != "777" || site.sent[0].text != "мой ответ" {
		t.Fatalf("ответ на сайт: %+v", site.sent)
	}
	// Повторный тот же реплай (переигранный апдейт) — at-most-once, второй раз не шлём.
	w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "мой ответ")
	if len(site.sent) != 1 {
		t.Fatalf("at-most-once нарушен: %d", len(site.sent))
	}

	// Реплай от чужого пользователя — обработан, но на сайт не уходит.
	w.HandleReply(ctx, store.MessengerTelegram, "r2", 99, delivered, "чужой")
	if len(site.sent) != 1 {
		t.Fatalf("чужой реплай не должен слаться на сайт: %d", len(site.sent))
	}

	// Реплай не на talks-сообщение — не наш.
	if w.HandleReply(ctx, store.MessengerTelegram, "r3", testOwner, "неизвестное", "x") {
		t.Fatal("реплай на не-talks сообщение не должен считаться нашим")
	}
}

func TestHandleReplyReadOnly(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "777", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"777": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tr := &fakeTransport{name: store.MessengerTelegram}
	cfg := testConfig()
	cfg.AllowSend = false
	w := New(st, site, []PMTransport{tr}, cfg, nil)
	w.pollOnce(ctx)
	delivered := tr.sent[0].msgID

	w.HandleReply(ctx, store.MessengerTelegram, "r1", testOwner, delivered, "ответ")
	if len(site.sent) != 0 {
		t.Fatalf("в read-only ответ на сайт не уходит: %d", len(site.sent))
	}
	if len(tr.confirms) != 1 || tr.confirms[0].ok {
		t.Fatalf("должен быть один отрицательный Confirm: %+v", tr.confirms)
	}
}

func TestExpiredSessionIsolatedNotKillSwitch(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	// Гостевой ответ talks на список диалогов = сессия юзера истекла.
	site := &fakeSite{dialogsErr: love.ErrUnauthorized}
	tr := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tr}, testConfig(), nil)

	for i := 0; i < 5; i++ {
		w.pollOnce(ctx)
	}
	if w.stopped {
		t.Fatal("истёкшая сессия одного юзера не должна ронять поллер (kill-switch — только для 403/дрейфа)")
	}
	// Сессия помечена невалидной → выпала из owners.
	owners, err := st.SessionOwners(ctx, store.MessengerTelegram)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 0 {
		t.Fatalf("истёкшая сессия должна стать невалидной, owners=%v", owners)
	}
	// Уведомление про /login — ровно одно (сессия выпала, повторов нет).
	if len(tr.sent) != 1 || !strings.Contains(tr.sent[0].html, "/login") {
		t.Fatalf("ожидалось одно уведомление про /login: %+v", tr.sent)
	}
}

func TestPurgeTalksOlderThan(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	peerID, err := st.UpsertTalkPeer(ctx, store.TalkPeer{
		Messenger: store.MessengerTelegram, OwnerUserID: testOwner, PassportID: "777"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.InsertTalkMessage(ctx, store.TalkMessage{
		PeerID: peerID, SiteMsgID: "m1", Direction: store.TalkIn, Text: "x"}); err != nil {
		t.Fatal(err)
	}
	// cutoff в будущем → удаляются все сообщения; собеседник остаётся.
	n, err := st.PurgeTalksOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ожидалось удаление 1 сообщения, got %d", n)
	}
	peers, _ := st.TalkPeers(ctx, store.MessengerTelegram, testOwner)
	if len(peers) != 1 {
		t.Fatalf("собеседник не должен удаляться ретеншеном, got %d", len(peers))
	}
}

// Временный отказ сайта (502 гейтвея) поллер переживает: kill-switch тут
// вреден — до ручного рестарта входящие ЛС не доедут, а история сайта отдаёт
// только последнюю страницу. Админ получает одно сообщение и «восстановилось».
func TestTransientSiteErrorDoesNotKillPoller(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	var alerts []string
	site := &fakeSite{dialogsErr: fmt.Errorf("loadBuddiesList: статус 502: %w", love.ErrSiteUnavailable)}
	cfg := testConfig()
	cfg.AlertSend = func(_ context.Context, text string) { alerts = append(alerts, text) }
	w := New(st, site, []PMTransport{&fakeTransport{name: store.MessengerTelegram}}, cfg, nil)

	for i := 0; i < 5; i++ {
		w.pollOnce(ctx)
	}
	if w.stopped {
		t.Fatal("временный отказ сайта не должен ронять поллер (kill-switch — только для 403/дрейфа)")
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], keyUnavailable) {
		t.Fatalf("ожидалось одно уведомление о недоступности сайта, got %v", alerts)
	}

	// Сайт ожил — поллер продолжает работать и сообщает о восстановлении.
	site.dialogsErr = nil
	w.pollOnce(ctx)
	if len(alerts) != 2 || !strings.Contains(alerts[1], "восстановилось") {
		t.Fatalf("после восстановления ожидалось уведомление, got %v", alerts)
	}
}

func TestKillSwitchOnForbidden(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedSession(t, st, testOwner)
	var alerted int
	site := &fakeSite{dialogsErr: love.ErrForbidden}
	cfg := testConfig()
	cfg.AlertSend = func(context.Context, string) { alerted++ }
	w := New(st, site, []PMTransport{&fakeTransport{name: store.MessengerTelegram}}, cfg, nil)

	for i := 0; i < 3; i++ {
		w.pollOnce(ctx)
	}
	if !w.stopped {
		t.Fatal("после ForbiddenLimit подряд 403 поллер должен остановиться")
	}
	if alerted == 0 {
		t.Error("kill-switch должен уведомить админа")
	}
}
