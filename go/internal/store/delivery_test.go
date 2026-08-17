package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// seedOwner заводит сессию с паспортом и заданным временем входа.
func seedOwner(t *testing.T, st *Store, messenger string, userID int64, passport string, loginAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertSession(ctx, messenger, userID, "[]", loginAt); err != nil {
		t.Fatal(err)
	}
	if passport == "" {
		return
	}
	if err := st.SetSessionIdentity(ctx, messenger, userID, "p"+passport, passport, "ник"); err != nil {
		t.Fatal(err)
	}
}

func newDeliveryStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Два входа одного сайт-аккаунта: пока выбора нет — получает свежий, а выбор
// человека выключает доставку второму (иначе он гасил бы непрочитанное на сайте).
func TestSetTalksDeliveryTurnsOffTwin(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	old := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	seedOwner(t, st, MessengerTelegram, 100, "777", old)
	seedOwner(t, st, MessengerMax, 200, "777", old.Add(24*time.Hour))

	owners, err := st.TalksOwners(ctx)
	if err != nil || len(owners) != 2 {
		t.Fatalf("сессии: %v %v", owners, err)
	}
	groups := GroupByAccount(owners)
	if len(groups) != 1 {
		t.Fatalf("один паспорт — одна группа, got %d", len(groups))
	}
	win, ok := PickDelivery(groups[0])
	if !ok || win.Messenger != MessengerMax {
		t.Fatalf("без выбора получает свежий вход: %+v %v", win, ok)
	}

	// Человек выбрал Telegram — доставка в MAX выключается.
	off, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOn, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 1 || off[0].Messenger != MessengerMax {
		t.Fatalf("выключить должны MAX: %+v", off)
	}
	owners, _ = st.TalksOwners(ctx)
	win, ok = PickDelivery(GroupByAccount(owners)[0])
	if !ok || win.Messenger != MessengerTelegram {
		t.Fatalf("явный выбор сильнее свежести: %+v %v", win, ok)
	}

	// Отказ в оставшемся мессенджере — не носим никуда.
	if _, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOff, time.Now()); err != nil {
		t.Fatal(err)
	}
	owners, _ = st.TalksOwners(ctx)
	if _, ok := PickDelivery(GroupByAccount(owners)[0]); ok {
		t.Error("оба выключены — доставлять некуда")
	}
}

// Без паспорта сессии не сливаются в один аккаунт: пустое значение — «не знаю»,
// а не «тот же человек».
func TestGroupByAccountKeepsUnknownPassportsApart(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "", now)
	seedOwner(t, st, MessengerMax, 200, "", now)

	owners, err := st.TalksOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(GroupByAccount(owners)); got != 2 {
		t.Fatalf("две неопознанные сессии — два аккаунта, got %d", got)
	}
	// И выбор одного из них второго не трогает.
	off, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOn, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 0 {
		t.Errorf("связать не с чем — выключать нечего: %+v", off)
	}
}

// TalksAccount собирает сессии одного человека и отвечает ErrNotFound, когда
// он не входил вовсе.
func TestTalksAccount(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "777", now)
	seedOwner(t, st, MessengerMax, 200, "777", now)
	seedOwner(t, st, MessengerMax, 201, "888", now) // чужой аккаунт

	acc, err := st.TalksAccount(ctx, MessengerTelegram, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acc) != 2 || acc[0].Messenger != MessengerTelegram {
		t.Fatalf("свой мессенджер первым, чужой аккаунт не берём: %+v", acc)
	}
	if _, err := st.TalksAccount(ctx, MessengerTelegram, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("без сессии ожидался ErrNotFound, got %v", err)
	}
}

// Истёкшая сессия из обхода выпадает, но выбор на ней остаётся: человек войдёт
// снова, и настройка должна пережить это.
func TestTalksOwnersSkipsInvalidButKeepsChoice(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "777", now)
	seedOwner(t, st, MessengerMax, 200, "777", now.Add(time.Hour))
	if _, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOn, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionValid(ctx, MessengerTelegram, 100, false, now); err != nil {
		t.Fatal(err)
	}

	owners, err := st.TalksOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0].Messenger != MessengerMax {
		t.Fatalf("истёкшая сессия в обход не идёт: %+v %v", owners, err)
	}
	if owners[0].Delivery != DeliveryOff {
		t.Errorf("MAX остаётся выключенным: %q", owners[0].Delivery)
	}
	acc, err := st.TalksAccount(ctx, MessengerTelegram, 100)
	if err != nil {
		t.Fatal(err)
	}
	if acc[0].Delivery != DeliveryOn {
		t.Errorf("выбор пережил протухшую сессию: %q", acc[0].Delivery)
	}
}

// Свежая сессия переписку читать не разрешает: молчание — не согласие.
// Согласие даёт кнопка «читать и присылать сюда», то есть DeliveryOn, и оно
// общее на весь сайт-аккаунт — иначе второй мессенджер продолжил бы вычитывать
// ту же переписку.
func TestScanConsentComesWithDeliveryAndCoversAccount(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "777", now)
	seedOwner(t, st, MessengerMax, 200, "777", now.Add(time.Hour))

	owners, err := st.TalksOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ScanAllowed(GroupByAccount(owners)[0]) {
		t.Fatal("без согласия переписку не читаем")
	}

	if _, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOn, now); err != nil {
		t.Fatal(err)
	}
	owners, _ = st.TalksOwners(ctx)
	for _, o := range owners {
		if o.Scan != ScanOn {
			t.Errorf("согласие — на весь аккаунт: %+v", o)
		}
	}

	// Отказ от мессенджера согласия не снимает: человек сказал «не сюда», а не
	// «не читай» (носить при этом станет некуда — это решает PickDelivery).
	if _, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOff, now); err != nil {
		t.Fatal(err)
	}
	owners, _ = st.TalksOwners(ctx)
	if !ScanAllowed(GroupByAccount(owners)[0]) {
		t.Error("«не присылать сюда» — не отказ от чтения")
	}
}

// Отказ от чтения гасит весь сайт-аккаунт и не задевает чужой.
func TestSetTalksScanOffCoversAccountOnly(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "777", now)
	seedOwner(t, st, MessengerMax, 200, "777", now)
	seedOwner(t, st, MessengerMax, 201, "888", now) // чужой аккаунт
	if _, err := st.SetTalksDelivery(ctx, MessengerMax, 201, DeliveryOn, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetTalksDelivery(ctx, MessengerTelegram, 100, DeliveryOn, now); err != nil {
		t.Fatal(err)
	}

	if err := st.SetTalksScan(ctx, MessengerMax, 200, ScanOff, now); err != nil {
		t.Fatal(err)
	}
	owners, _ := st.TalksOwners(ctx)
	for _, group := range GroupByAccount(owners) {
		want := group[0].PassportID == "888"
		if got := ScanAllowed(group); got != want {
			t.Errorf("паспорт %s: чтение %v, ожидалось %v", group[0].PassportID, got, want)
		}
	}

	if err := st.SetTalksScan(ctx, MessengerTelegram, 999, ScanOff, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("без сессии ожидался ErrNotFound, got %v", err)
	}
	if err := st.SetTalksScan(ctx, MessengerTelegram, 100, "может быть", now); err == nil {
		t.Error("третьего значения у согласия нет")
	}
}

// Спросили один раз — отметка легла в БД (рестарт демона не переспрашивает).
func TestMarkTalksAsked(t *testing.T) {
	ctx := context.Background()
	st := newDeliveryStore(t)
	now := time.Now()
	seedOwner(t, st, MessengerTelegram, 100, "777", now)

	owners, _ := st.TalksOwners(ctx)
	if owners[0].Asked {
		t.Fatal("свежая сессия — ещё не спрашивали")
	}
	if err := st.MarkTalksAsked(ctx, MessengerTelegram, 100, now); err != nil {
		t.Fatal(err)
	}
	owners, _ = st.TalksOwners(ctx)
	if !owners[0].Asked {
		t.Error("отметка о вопросе не сохранилась")
	}
}
