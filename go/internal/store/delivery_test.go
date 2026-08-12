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
