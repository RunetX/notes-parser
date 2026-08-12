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

// Один сайт-аккаунт в двух мессенджерах: ЛС уезжают только в один (в тот, где
// вход свежее), а человека один раз спрашивают, куда носить.
func TestDuplicateAccountDeliversOnceAndAsks(t *testing.T) {
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

	type ask struct {
		messenger string
		user      int64
		current   string
	}
	var asks []ask
	cfg := bothMessengers()
	cfg.AskDelivery = func(_ context.Context, messenger string, userID int64, current store.TalksOwner) {
		asks = append(asks, ask{messenger, userID, current.Messenger})
	}
	w := New(st, site, []PMTransport{tg, mx}, cfg, nil)

	w.pollOnce(ctx)
	if len(tg.sent) != 0 {
		t.Errorf("вход в telegram старее — ЛС туда не носим: %+v", tg.sent)
	}
	if len(mx.sent) != 1 {
		t.Fatalf("ожидалась одна доставка в MAX, got %d", len(mx.sent))
	}
	if len(asks) != 2 {
		t.Fatalf("спросить надо в обоих мессенджерах: %+v", asks)
	}
	for _, a := range asks {
		if a.current != store.MessengerMax {
			t.Errorf("в вопросе называем нынешнего получателя: %+v", a)
		}
	}

	// Второй такт: вопрос не повторяется (отметка легла в БД).
	w.pollOnce(ctx)
	if len(asks) != 2 {
		t.Errorf("вопрос задаётся один раз: %+v", asks)
	}
}

// Выбор человека сильнее свежести входа: после /delivery в telegram ЛС едут
// туда, а MAX перестаёт выгребать историю (и гасить непрочитанное на сайте).
func TestChoiceOverridesFreshestLogin(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	old := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", old)
	seedOwner(t, st, store.MessengerMax, 200, "777", old.Add(24*time.Hour))
	if _, err := st.SetTalksDelivery(ctx, store.MessengerTelegram, 100, store.DeliveryOn, time.Now()); err != nil {
		t.Fatal(err)
	}

	site := &fakeSite{
		dialogs: []love.TalkDialog{{PassportID: "555", Nick: "Мария", LastMsgID: "m1"}},
		history: map[string][]love.TalkMessage{"555": {{SiteMsgID: "m1", Text: "привет"}}},
	}
	tg := &fakeTransport{name: store.MessengerTelegram}
	mx := &fakeTransport{name: store.MessengerMax}
	asked := 0
	cfg := bothMessengers()
	cfg.AskDelivery = func(context.Context, string, int64, store.TalksOwner) { asked++ }
	w := New(st, site, []PMTransport{tg, mx}, cfg, nil)

	w.pollOnce(ctx)
	if len(tg.sent) != 1 || len(mx.sent) != 0 {
		t.Fatalf("ЛС должны уйти в telegram: tg=%d max=%d", len(tg.sent), len(mx.sent))
	}
	if asked != 0 {
		t.Errorf("выбор сделан — спрашивать не о чем, спросили %d раз(а)", asked)
	}
	if site.historyCalls != 1 {
		t.Errorf("историю читает только выбранный мессенджер: %d запросов", site.historyCalls)
	}
}

// Отказ от доставки: сессия живая (мост «ответ в чате → комментарий» работает),
// но диалоги не опрашиваются вовсе — читать чужие ЛС, гася их на сайте, незачем.
func TestDeliveryOffStopsPolling(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "777", time.Now())
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

// Сессия без паспорта (заведена до появления talks): поллер снимает его сам,
// иначе два входа одного человека не связать. Попытка одна на запуск.
func TestCaptureIdentityFillsPassport(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedOwner(t, st, store.MessengerTelegram, 100, "", time.Now())
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
