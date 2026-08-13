package modwatch

import (
	"testing"
	"time"
)

// dense набивает реплики с шагом step на протяжении span, начиная с from.
func dense(from time.Time, span, step time.Duration) []time.Time {
	var out []time.Time
	for t := from; t.Before(from.Add(span)); t = t.Add(step) {
		out = append(out, t)
	}
	return out
}

// Настоящий случай: ГердаИзСемейкиАддамс, бан «Мат» 05.08.2026 22:52 Нск на
// сутки. Последняя реплика в 22:16 — за 36 минут до запрета, возврат в 23:01
// следующего дня, через 9 минут после снятия. Пауза 24,75 ч.
func TestDetectBanOnRealCase(t *testing.T) {
	base := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	last := time.Date(2026, 8, 5, 15, 16, 0, 0, time.UTC)   // 22:16 Нск
	back := time.Date(2026, 8, 6, 16, 1, 0, 0, time.UTC)    // 23:01 Нск
	banned := time.Date(2026, 8, 5, 15, 52, 0, 0, time.UTC) // 22:52 Нск — правда

	var times []time.Time
	times = append(times, dense(base, 53*time.Hour, 90*time.Minute)...) // до паузы
	times = append(times, last)
	times = append(times, dense(back, 48*time.Hour, 90*time.Minute)...) // после

	bans := DetectBans(1357482, times, BanOptions{})
	if len(bans) != 1 {
		t.Fatalf("ожидался ровно один запрет, найдено %d: %+v", len(bans), bans)
	}
	b := bans[0]
	if b.Tier != 24*time.Hour {
		t.Fatalf("срок %v, ожидались сутки", b.Tier)
	}
	if b.Delay() < 0 || b.Delay() > time.Hour {
		t.Fatalf("опоздание возврата %v — не похоже на снятие запрета", b.Delay())
	}
	// Главное: окно должно накрывать настоящее время бана.
	if banned.Before(b.From) || banned.After(b.To) {
		t.Fatalf("окно [%v … %v] не накрывает настоящий бан %v", b.From, b.To, banned)
	}
	if w := b.To.Sub(b.From); w > time.Hour {
		t.Fatalf("окно шире часа (%v) — для сверки присутствия бесполезно", w)
	}
}

// Кто заходит через день, суточных «банов» давать не должен: подпись не в самой
// паузе, а в том, что она выбивается из ритма человека.
func TestEveryOtherDayWriterIsNotBanned(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	var times []time.Time
	for d := 0; d < 20; d++ {
		day := base.AddDate(0, 0, 2*d)
		times = append(times, day, day.Add(20*time.Minute))
	}
	if bans := DetectBans(1, times, BanOptions{}); len(bans) != 0 {
		t.Fatalf("ритм «через день» принят за запреты: %+v", bans)
	}
}

// Уход с площадки — не запрет: после паузы человек больше не пишет.
func TestLeavingIsNotBan(t *testing.T) {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	times := dense(base, 48*time.Hour, time.Hour)
	times = append(times, base.Add(48*time.Hour+25*time.Hour)) // одна реплика и тишина
	if bans := DetectBans(1, times, BanOptions{}); len(bans) != 0 {
		t.Fatalf("уход принят за запрет: %+v", bans)
	}
}

// Недельный и месячный сроки ловятся тем же правилом.
func TestLongerTiers(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tier := range []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		span := tier * banContextFactor / 2
		var times []time.Time
		times = append(times, dense(base, span, 6*time.Hour)...)
		last := times[len(times)-1]
		times = append(times, dense(last.Add(tier+90*time.Minute), span, 6*time.Hour)...)
		bans := DetectBans(1, times, BanOptions{Tiers: []time.Duration{tier}})
		if len(bans) != 1 || bans[0].Tier != tier {
			t.Fatalf("срок %v не пойман: %+v", tier, bans)
		}
	}
}
