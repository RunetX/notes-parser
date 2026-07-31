package digest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"lovegw/internal/store"
)

func TestDecideSlotPrecedence(t *testing.T) {
	w := testWindow()
	fresh := w.End.Add(time.Hour)
	stale := w.End.Add(72 * time.Hour)
	cases := []struct {
		name               string
		now                time.Time
		published, drafted bool
		want               slotAction
	}{
		{"свежий слот — строить", fresh, false, false, slotDraft},
		{"опубликован — пропуск даже свежего", fresh, true, false, slotSkipPublished},
		{"черновик лежит — не перетирать", fresh, false, true, slotSkipDrafted},
		{"протух — задним числом не строим", stale, false, false, slotSkipOld},
		{"протух, но опубликован — это не «пропущенный»", stale, true, false, slotSkipPublished},
		{"протух, но черновик есть — правится", stale, false, true, slotSkipDrafted},
	}
	for _, tc := range cases {
		if got := decideSlot(tc.now, w, defaultGrace, tc.published, tc.drafted); got != tc.want {
			t.Errorf("%s: %v, ожидалось %v", tc.name, got, tc.want)
		}
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func scheduleCfg(t *testing.T, notify func(context.Context, string)) ScheduleConfig {
	t.Helper()
	return ScheduleConfig{
		Loc:      nsk,
		Weekday:  time.Friday,
		Hour:     19,
		OutDir:   t.TempDir(),
		SiteBase: siteBase,
		Grace:    defaultGrace,
		Notify:   notify,
	}
}

func TestProcessSlotDraftsOnceAndNotifies(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, func(_ context.Context, text string) { notes = append(notes, text) })
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk) // полчаса после слота W31

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("уведомлений: %d (%v)", len(notes), notes)
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err != nil {
		t.Fatalf("черновик не создан: %v", err)
	}
	if _, err := os.Stat(MaterialsPath(cfg.OutDir, "2026-W31")); err != nil {
		t.Fatalf("материалы не созданы: %v", err)
	}

	// Повторный тик того же слота: черновик лежит — не трогаем, не спамим.
	if err := processSlot(ctx, st, cfg, now.Add(time.Hour), quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("повторное уведомление: %v", notes)
	}
}

func TestProcessSlotSkipsPublished(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// Выпуск W31 уже опубликован в один из мессенджеров.
	if err := st.SetTarget(ctx, store.MessengerMax, store.TargetDigest, "2026-W31", "m1", ""); err != nil {
		t.Fatal(err)
	}
	called := false
	cfg := scheduleCfg(t, func(context.Context, string) { called = true })
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("опубликованный выпуск не должен перегенерироваться")
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err == nil {
		t.Error("черновик не должен создаваться для опубликованного выпуска")
	}
}

func TestProcessSlotSkipsStale(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	called := false
	cfg := scheduleCfg(t, func(context.Context, string) { called = true })
	// Вторник: последний слот (пятница W31) старше 48 часов.
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("протухший слот не догоняется")
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err == nil {
		t.Error("черновик протухшего слота не должен создаваться")
	}
}
