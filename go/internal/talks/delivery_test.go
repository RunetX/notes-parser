package talks

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// siteWithIdentity — сайт, умеющий отдать паспорт владельца сессии
// (talks.SiteIdentifier): так поллер дозаполняет старые сессии.
type siteWithIdentity struct {
	*fakeSite
	passport string
	calls    int
	err      error
}

func (s *siteWithIdentity) SiteIdentity(context.Context, []*http.Cookie) (string, string, string, error) {
	s.calls++
	if s.err != nil {
		return "", "", "", s.err
	}
	return "p" + s.passport, s.passport, "ник", nil
}

// seedOwner заводит сессию с паспортом и заданным временем входа.
func seedOwner(t *testing.T, st *store.Store, messenger string, userID int64, passport string, loginAt time.Time) {
	t.Helper()
	ctx := context.Background()
	ck := []*http.Cookie{{Name: "sess", Value: "abc", Domain: "love.ngs.ru", Path: "/", Expires: time.Now().Add(24 * time.Hour)}}
	js, err := love.CookiesToJSON(ck, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, messenger, userID, js, loginAt); err != nil {
		t.Fatal(err)
	}
	if passport != "" {
		if err := st.SetSessionIdentity(ctx, messenger, userID, "p"+passport, passport, "ник"); err != nil {
			t.Fatal(err)
		}
	}
}

// bothMessengers — конфиг с двумя мессенджерами и без admin-only (речь про
// обычных пользователей, вошедших дважды).
func bothMessengers() Config {
	cfg := testConfig()
	cfg.AdminOnly = false
	return cfg
}

// askRecorder — конфиг, запоминающий, у кого спросили согласия.
type askRecorder struct {
	messenger string
	user      int64
	elsewhere bool
}

// Пока согласия нет, сайт не трогаем ВОВСЕ: ни списка диалогов, ни истории —
// иначе сообщения молча пометились бы прочитанными, а человек всё это время
// числился бы в сети. Вместо обхода — один вопрос в каждый мессенджер.
func TestNoConsentAsksAndLeavesSiteAlone(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	old := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", old)
	seedOwner(t, st, store.MessengerMax, 200, "777", old.Add(24*time.Hour))

	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "555", Nick: "Мария", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"555": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tg := &fakeTransport{name: store.MessengerTelegram}
	mx := &fakeTransport{name: store.MessengerMax}

	var asks []askRecorder
	cfg := bothMessengers()
	cfg.AskScan = func(_ context.Context, messenger string, userID int64, alsoElsewhere bool) {
		asks = append(asks, askRecorder{messenger, userID, alsoElsewhere})
	}
	w := New(st, site, []PMTransport{tg, mx}, cfg, nil)

	w.pollOnce(ctx)
	if site.dialogCalls != 0 || site.historyCalls != 0 {
		t.Errorf("без согласия сайт не трогаем: диалогов %d, историй %d",
			site.dialogCalls, site.historyCalls)
	}
	if len(tg.sent) != 0 || len(mx.sent) != 0 {
		t.Errorf("без согласия ЛС не доставляем: tg=%d max=%d", len(tg.sent), len(mx.sent))
	}
	if len(asks) != 2 {
		t.Fatalf("спросить надо в обоих мессенджерах: %+v", asks)
	}
	for _, a := range asks {
		if !a.elsewhere {
			t.Errorf("аккаунт залогинен дважды — это должно быть видно в вопросе: %+v", a)
		}
	}

	// Второй такт: вопрос не повторяется (отметка легла в БД).
	w.pollOnce(ctx)
	if len(asks) != 2 {
		t.Errorf("вопрос задаётся один раз: %+v", asks)
	}
}

// Согласие даётся кнопкой «читать и присылать сюда»: она же выбирает мессенджер.
// После неё ЛС едут в telegram, хотя вход в MAX свежее, а MAX истории не читает
// (и не гасит непрочитанное на сайте).
func TestConsentChoosesMessengerAndStartsPolling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	old := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", old)
	seedOwner(t, st, store.MessengerMax, 200, "777", old.Add(24*time.Hour))
	allowScan(t, st, store.MessengerTelegram, 100)

	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "555", Nick: "Мария", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"555": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tg := &fakeTransport{name: store.MessengerTelegram}
	mx := &fakeTransport{name: store.MessengerMax}
	asked := 0
	cfg := bothMessengers()
	cfg.AskScan = func(context.Context, string, int64, bool) { asked++ }
	w := New(st, site, []PMTransport{tg, mx}, cfg, nil)

	w.pollOnce(ctx)
	if len(tg.sent) != 1 || len(mx.sent) != 0 {
		t.Fatalf("ЛС должны уйти в telegram: tg=%d max=%d", len(tg.sent), len(mx.sent))
	}
	if asked != 0 {
		t.Errorf("согласие дано — спрашивать не о чем, спросили %d раз(а)", asked)
	}
	if site.historyCalls != 1 {
		t.Errorf("историю читает только выбранный мессенджер: %d запросов", site.historyCalls)
	}
}

// Отказ от чтения: сессия живая (мост «ответ в чате → комментарий» работает),
// но обход к сайту под ней больше не ходит — ни разу и ни за чем.
func TestScanOffStopsPolling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", time.Now())
	allowScan(t, st, store.MessengerTelegram, 100)
	if err := st.SetTalksScan(ctx, store.MessengerTelegram, 100, store.ScanOff, time.Now()); err != nil {
		t.Fatal(err)
	}
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "555", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"555": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tg := &fakeTransport{name: store.MessengerTelegram}
	asked := 0
	cfg := bothMessengers()
	cfg.AskScan = func(context.Context, string, int64, bool) { asked++ }
	w := New(st, site, []PMTransport{tg}, cfg, nil)

	w.pollOnce(ctx)
	if len(tg.sent) != 0 || site.dialogCalls != 0 || site.historyCalls != 0 {
		t.Errorf("отказавшегося не обходим: доставок %d, диалогов %d, историй %d",
			len(tg.sent), site.dialogCalls, site.historyCalls)
	}
	if asked != 0 {
		t.Errorf("отказ — это ответ, переспрашивать нечего: спросили %d раз(а)", asked)
	}
}

// Отказ от доставки при одной сессии: носить ЛС становится некуда, и обход
// прекращается сам — читать чужие ЛС, гася их на сайте, незачем.
func TestDeliveryOffStopsPolling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", time.Now())
	allowScan(t, st, store.MessengerTelegram, 100)
	if _, err := st.SetTalksDelivery(ctx, store.MessengerTelegram, 100, store.DeliveryOff, time.Now()); err != nil {
		t.Fatal(err)
	}
	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "555", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"555": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tg := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tg}, bothMessengers(), nil)

	w.pollOnce(ctx)
	if len(tg.sent) != 0 || site.historyCalls != 0 {
		t.Errorf("отказавшегося не обходим: доставок %d, запросов истории %d",
			len(tg.sent), site.historyCalls)
	}
}

// Запрет админа (talks.exclude_users) сильнее согласия человека — и вопроса ему
// тоже не задают: за него уже решили.
func TestExcludedIsNotAsked(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", time.Now())
	site := &fakeSite{dialogs: []love.TalkDialog{{PassportID: "555", LastMsgID: "m1"}}}
	tg := &fakeTransport{name: store.MessengerTelegram}
	asked := 0
	cfg := bothMessengers()
	cfg.ExcludeUsers = map[string][]int64{store.MessengerTelegram: {100}}
	cfg.AskScan = func(context.Context, string, int64, bool) { asked++ }
	w := New(st, site, []PMTransport{tg}, cfg, nil)

	w.pollOnce(ctx)
	if asked != 0 || site.dialogCalls != 0 {
		t.Errorf("запрещённого админом не спрашиваем и не обходим: спросили %d, диалогов %d",
			asked, site.dialogCalls)
	}
}

// Сессия без паспорта (заведена до появления talks): поллер снимает его сам,
// иначе два входа одного человека не связать. Попытка одна на запуск.
func TestCaptureIdentityFillsPassport(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "", time.Now())
	allowScan(t, st, store.MessengerTelegram, 100)
	site := &siteWithIdentity{fakeSite: &fakeSite{}, passport: "777"}
	tg := &fakeTransport{name: store.MessengerTelegram}
	w := New(st, site, []PMTransport{tg}, bothMessengers(), nil)

	w.pollOnce(ctx)
	owners, err := st.TalksOwners(ctx)
	if err != nil || len(owners) != 1 {
		t.Fatalf("сессии: %+v %v", owners, err)
	}
	if owners[0].PassportID != "777" {
		t.Fatalf("паспорт должен дозаполниться: %q", owners[0].PassportID)
	}
	w.pollOnce(ctx)
	if site.calls != 1 {
		t.Errorf("паспорт снимаем один раз: %d запросов", site.calls)
	}
}
