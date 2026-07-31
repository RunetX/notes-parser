package digest

import (
	"testing"
	"time"
)

// Фиксированный пояс вместо tzdata: Новосибирск — постоянный +07.
var nsk = time.FixedZone("+07", 7*3600)

func TestSlotForAroundSlotMoment(t *testing.T) {
	// 2026-07-31 — пятница.
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"за минуту до слота — прошлая пятница",
			time.Date(2026, 7, 31, 18, 59, 0, 0, nsk),
			time.Date(2026, 7, 24, 19, 0, 0, 0, nsk)},
		{"ровно слот — эта пятница",
			time.Date(2026, 7, 31, 19, 0, 0, 0, nsk),
			time.Date(2026, 7, 31, 19, 0, 0, 0, nsk)},
		{"минутой позже — эта пятница",
			time.Date(2026, 7, 31, 19, 1, 0, 0, nsk),
			time.Date(2026, 7, 31, 19, 0, 0, 0, nsk)},
		{"среда — прошлая пятница",
			time.Date(2026, 7, 29, 12, 0, 0, 0, nsk),
			time.Date(2026, 7, 24, 19, 0, 0, 0, nsk)},
	}
	for _, tc := range cases {
		w := SlotFor(tc.now, nsk, time.Friday, 19, 0)
		if !w.End.Equal(tc.want) {
			t.Errorf("%s: End = %v, ожидалось %v", tc.name, w.End, tc.want)
		}
		if !w.Start.Equal(tc.want.AddDate(0, 0, -7)) {
			t.Errorf("%s: Start = %v, ожидалось %v", tc.name, w.Start, tc.want.AddDate(0, 0, -7))
		}
	}
}

func TestSlotForWeeksShiftAndID(t *testing.T) {
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, nsk)
	w := SlotFor(now, nsk, time.Friday, 19, 0)
	if w.ID != "2026-W31" {
		t.Errorf("ID = %q, ожидалось 2026-W31", w.ID)
	}
	prev := SlotFor(now, nsk, time.Friday, 19, -1)
	if !prev.End.Equal(time.Date(2026, 7, 24, 19, 0, 0, 0, nsk)) || prev.ID != "2026-W30" {
		t.Errorf("weeks=-1: End=%v ID=%q", prev.End, prev.ID)
	}
}

func TestSlotIDAcrossYearBoundary(t *testing.T) {
	// 2027-01-01 — пятница, по ISO принадлежит 53-й неделе 2026 года.
	now := time.Date(2027, 1, 1, 20, 0, 0, 0, nsk)
	w := SlotFor(now, nsk, time.Friday, 19, 0)
	if w.ID != "2026-W53" {
		t.Errorf("новогодний стык: ID = %q, ожидалось 2026-W53", w.ID)
	}
}

func TestNextSlot(t *testing.T) {
	slot := time.Date(2026, 7, 31, 19, 0, 0, 0, nsk)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"до слота — ближайшая пятница", slot.Add(-time.Hour), slot},
		{"ровно в слот — следующая неделя", slot, slot.AddDate(0, 0, 7)},
		{"после слота — следующая неделя", slot.Add(time.Minute), slot.AddDate(0, 0, 7)},
	}
	for _, tc := range cases {
		if got := NextSlot(tc.now, nsk, time.Friday, 19); !got.Equal(tc.want) {
			t.Errorf("%s: %v, ожидалось %v", tc.name, got, tc.want)
		}
	}
}
