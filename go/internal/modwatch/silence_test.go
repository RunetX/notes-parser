package modwatch

import (
	"testing"
	"time"
)

// Живой замер 13.08.2026, на котором метод и родился: из топ-40 июля выпали
// шестеро. Пятеро перестали заходить в тот же час, что и писать, — это уход.
// Актриса при 682 репликах молчит с 31 июля и при этом на сайте прямо сейчас —
// так выглядит запрет.
func TestClassifySilenceRealCase(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 50, 0, 0, time.UTC) // 16:50 Нск
	people := []Commenter{
		{UserID: 1431505, Nick: "Актриса", Comments: 682,
			LastComment: time.Date(2026, 7, 31, 15, 7, 0, 0, time.UTC)},
		{UserID: 1515257, Nick: "УЖасный", Comments: 520,
			LastComment: time.Date(2026, 8, 5, 10, 18, 0, 0, time.UTC)},
		{UserID: 1500827, Nick: "Nadin", Comments: 498,
			LastComment: time.Date(2026, 8, 4, 17, 48, 0, 0, time.UTC)},
		{UserID: 999, Nick: "Не смотрели", Comments: 300,
			LastComment: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{UserID: 1, Nick: "Пишет сейчас", Comments: 900,
			LastComment: now.Add(-2 * time.Hour)},
	}
	profiles := map[int64]ProfileRow{
		1431505: {UserID: 1431505, LastAt: now, CheckedAt: now},
		1515257: {UserID: 1515257, LastAt: time.Date(2026, 8, 5, 10, 19, 0, 0, time.UTC), CheckedAt: now},
		1500827: {UserID: 1500827, LastAt: time.Date(2026, 8, 4, 17, 46, 0, 0, time.UTC), CheckedAt: now},
		1:       {UserID: 1, LastAt: now, CheckedAt: now},
	}
	rows := ClassifySilence(people, profiles, SilenceOptions{Now: now})

	if len(rows) != 4 {
		t.Fatalf("строк %d, ожидалось 4: молчащие без того, кто пишет", len(rows))
	}
	first := rows[0]
	if first.UserID != 1431505 || first.Verdict != VerdictBan {
		t.Fatalf("первой строкой ожидалась Актриса с вердиктом %q, получено %+v", VerdictBan, first)
	}
	if first.After < 12*24*time.Hour {
		t.Errorf("«ходил после» = %s, ожидались двенадцать с лишним суток", first.After)
	}
	want := map[int64]string{
		1515257: VerdictLeft,    // заход замер через минуту после последней реплики
		1500827: VerdictLeft,    // заход даже раньше последней реплики
		999:     VerdictUnknown, // анкету ещё не опрашивали — вердикта нет
	}
	for _, r := range rows[1:] {
		if got := want[r.UserID]; got != r.Verdict {
			t.Errorf("u%d: вердикт %q, ожидался %q", r.UserID, r.Verdict, got)
		}
	}
}

// Живой прогон 13.08.2026 показал, чего правилу не хватало: Игорь u1514601 —
// 460 реплик, молчит восемь суток, после последней реплики ходил ещё 12 часов.
// По «ходил после» он кандидат на запрет, но последний его заход был тогда же,
// восемь суток назад: человек ушёл, просто на полдня позже, чем замолчал.
// Решает поэтому не хвост, а свежесть последнего захода.
func TestClassifySilenceLeftLaterIsNotABan(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 50, 0, 0, time.UTC)
	rows := ClassifySilence(
		[]Commenter{{UserID: 1514601, Nick: "Игорь", Comments: 460,
			LastComment: time.Date(2026, 8, 4, 22, 44, 0, 0, time.UTC)}},
		map[int64]ProfileRow{1514601: {UserID: 1514601,
			LastAt: time.Date(2026, 8, 5, 11, 13, 0, 0, time.UTC), CheckedAt: now}},
		SilenceOptions{Now: now})
	if len(rows) != 1 {
		t.Fatalf("строк %d, ожидалась одна", len(rows))
	}
	if rows[0].Verdict != VerdictLeftLater {
		t.Fatalf("вердикт %q, ожидался %q: ходил после молчания, но уже неделю не заходит",
			rows[0].Verdict, VerdictLeftLater)
	}
}

// Порог «недописал» отсекает редких комментаторов: четверо суток молчания у
// пишущего раз в неделю — не событие, и без порога такие забивают список.
func TestClassifySilenceMinMissed(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 50, 0, 0, time.UTC)
	people := []Commenter{
		{UserID: 1, Nick: "Редкий", Comments: 21, LastComment: now.Add(-4 * 24 * time.Hour)},
		{UserID: 2, Nick: "Частый", Comments: 600, LastComment: now.Add(-4 * 24 * time.Hour)},
	}
	profiles := map[int64]ProfileRow{
		1: {UserID: 1, LastAt: now, CheckedAt: now},
		2: {UserID: 2, LastAt: now, CheckedAt: now},
	}
	rows := ClassifySilence(people, profiles, SilenceOptions{Now: now, MinMissed: DefaultMinMissed})
	if len(rows) != 1 || rows[0].UserID != 2 {
		t.Fatalf("строки = %+v, ожидался только частый комментатор", rows)
	}
	if got := rows[0].Missed; got < 79 || got > 81 {
		t.Errorf("недописал = %.1f, ожидалось около 80 (600 реплик за 30 суток × 4 суток молчания)", got)
	}
}

// Удалённая анкета — отдельный вердикт: молчание объясняется, но не запретом в
// секции, а закрытием аккаунта.
func TestClassifySilenceMissingProfile(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	rows := ClassifySilence(
		[]Commenter{{UserID: 7, Comments: 100, LastComment: now.Add(-10 * 24 * time.Hour)}},
		map[int64]ProfileRow{7: {UserID: 7, Missing: true, CheckedAt: now}},
		SilenceOptions{Now: now})
	if len(rows) != 1 || rows[0].Verdict != VerdictMissing {
		t.Fatalf("ожидался вердикт %q, получено %+v", VerdictMissing, rows)
	}
}

// Порог «ходил после» отсекает обычный хвост вечера: человек дописал последнюю
// реплику и ещё побродил по сайту час — это не запрет.
func TestClassifySilenceMarginCutsEveningTail(t *testing.T) {
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	last := now.Add(-5 * 24 * time.Hour)
	rows := ClassifySilence(
		[]Commenter{{UserID: 8, Comments: 100, LastComment: last}},
		map[int64]ProfileRow{8: {UserID: 8, LastAt: last.Add(time.Hour), CheckedAt: now}},
		SilenceOptions{Now: now})
	if len(rows) != 1 || rows[0].Verdict != VerdictLeft {
		t.Fatalf("ожидался вердикт %q, получено %+v", VerdictLeft, rows)
	}
}
