package digest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeSite — площадка на время теста: помнит, что публиковали и что закрепляли.
type fakeSite struct {
	bodies []string
	pins   map[int64]bool
	nextID int64
	fail   error
}

func newFakeSite() *fakeSite { return &fakeSite{pins: map[int64]bool{}, nextID: 100000000000} }

func (s *fakeSite) PublishNote(_ context.Context, body string) (int64, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	s.bodies = append(s.bodies, body)
	s.nextID++
	return s.nextID, nil
}

func (s *fakeSite) PinNote(_ context.Context, noteID int64, pinned bool) error {
	s.pins[noteID] = pinned
	return nil
}

func draft(sections ...[]string) Draft { return Draft{Sections: sections} }

// Тело выпуска — плоский текст со знаками НГС: ссылки с подписью в заметке не
// существует, поэтому подпись и адрес печатаются рядом, а маркер ведёт на
// страницу ПЛОЩАДКИ (у зеркальной заметки id строки равен id на НГС).
func TestRenderPlatform(t *testing.T) {
	d := draft(
		[]string{"<b>📌 Заметка недели</b>", "{note:312811|Про кота} — 44 комментария"},
		[]string{"<i>курсив</i> и &amp;"},
	)
	got := RenderPlatform(d, "https://t3h.ru/")
	want := strings.Join([]string{
		"[b]📌 Заметка недели[/b]",
		"Про кота — https://t3h.ru/n/312811 — 44 комментария",
		"[i]курсив[/i] и &",
	}, "\n\n")
	if got != want {
		t.Errorf("тело выпуска:\n%q\nожидалось:\n%q", got, want)
	}
}

// Выпуск выходит РОВНО ОДИН раз: повторный запуск (докат после сбоя, ручной
// publish поверх автопубликации) обязан вернуть уже опубликованную заметку, а
// не завести вторую.
func TestPublishPlatformIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	site := newFakeSite()
	d := draft([]string{"выпуск"})

	id, created, err := PublishPlatform(ctx, st, site, d, "2026-W34", "https://t3h.ru")
	if err != nil || !created || id == 0 {
		t.Fatalf("первая публикация: id=%d created=%v err=%v", id, created, err)
	}
	again, created, err := PublishPlatform(ctx, st, site, d, "2026-W34", "https://t3h.ru")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("выпуск опубликован повторно")
	}
	if again != id {
		t.Errorf("вернулся чужой id: %d вместо %d", again, id)
	}
	if len(site.bodies) != 1 {
		t.Errorf("заметок на площадке: %d", len(site.bodies))
	}
}

// Отказ площадки не оставляет отметки: неделя обязана остаться неопубликованной,
// иначе планировщик сочтёт её закрытой и выпуск не выйдет уже никогда.
func TestPublishPlatformFailureLeavesNoMark(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	site := newFakeSite()
	site.fail = errors.New("база занята")

	if _, _, err := PublishPlatform(ctx, st, site, draft([]string{"выпуск"}), "2026-W34", "https://t3h.ru"); err == nil {
		t.Fatal("ожидалась ошибка")
	}
	site.fail = nil
	if _, created, err := PublishPlatform(ctx, st, site, draft([]string{"выпуск"}), "2026-W34", "https://t3h.ru"); err != nil || !created {
		t.Fatalf("повтор после отказа: created=%v err=%v", created, err)
	}
}

// Новый выпуск снимает закреп с прошлого: мест наверху всего пять, и за месяц
// они кончились бы одними дайджестами.
func TestPinIssueRepins(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	site := newFakeSite()

	first, _, err := PublishPlatform(ctx, st, site, draft([]string{"первый"}), "2026-W34", "https://t3h.ru")
	if err != nil {
		t.Fatal(err)
	}
	if err := PinIssue(ctx, st, site, first); err != nil {
		t.Fatal(err)
	}
	second, _, err := PublishPlatform(ctx, st, site, draft([]string{"второй"}), "2026-W35", "https://t3h.ru")
	if err != nil {
		t.Fatal(err)
	}
	if err := PinIssue(ctx, st, site, second); err != nil {
		t.Fatal(err)
	}
	if site.pins[first] {
		t.Error("прошлый выпуск остался закреплённым")
	}
	if !site.pins[second] {
		t.Error("свежий выпуск не закреплён")
	}
}
