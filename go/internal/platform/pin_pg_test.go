package platform

// Закрепление заметки наверху ленты — против настоящего Postgres.
//
// Проверять тут есть что именно в базе: закреплённая уходит ИЗ ленты (иначе она
// выйдет дважды), потолок считается в той же транзакции, что и вставка, а
// скрытая заметка не всплывает наверх из-за старого закрепления — это условие
// частичного индекса, а не условие в Go.

import (
	"context"
	"errors"
	"testing"
)

func TestЗакреплённаяУходитИзЛентыИВстаётНаверх(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	first := mustNote(t, p, author, "обычная")
	pinned := mustNote(t, p, author, "важная")

	if err := p.SetNotePinned(ctx, mod, pinned, true, "правила"); err != nil {
		t.Fatalf("закрепление: %v", err)
	}

	feed, err := p.Feed(ctx, Viewer{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range feed {
		if n.ID == pinned {
			t.Fatal("закреплённая осталась в ленте: в показе она выйдет дважды")
		}
	}
	if len(feed) != 1 || feed[0].ID != first {
		t.Fatalf("в ленте %d заметок, ожидалась одна обычная", len(feed))
	}

	top, err := p.PinnedNotes(ctx, Viewer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].ID != pinned || !top[0].Pinned {
		t.Fatalf("закреплённые: %+v", top)
	}

	// Открепление возвращает заметку на своё хронологическое место.
	if err := p.SetNotePinned(ctx, mod, pinned, false, ""); err != nil {
		t.Fatalf("открепление: %v", err)
	}
	feed, err = p.Feed(ctx, Viewer{}, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed) != 2 {
		t.Fatalf("после открепления в ленте %d заметок, ожидалось 2", len(feed))
	}
}

// Повтор — не ошибка, но и не действие: два модератора, нажавших одно и то же,
// не должны видеть отказ, а журнал не должен копить записи ни о чём.
func TestПовторноеЗакреплениеНичегоНеДелает(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	note := mustNote(t, p, mustUser(t, p, "Пух"), "важная")

	if err := p.SetNotePinned(ctx, mod, note, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNotePinned(ctx, mod, note, true, ""); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("повтор: %v, ожидалось ErrNothingToDo", err)
	}
	entries, err := p.SubjectAudit(ctx, NoteSubject(note), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Action != ActionPin {
		t.Fatalf("журнал: %+v", entries)
	}
}

// Потолок — правило ленты: закреплённое имеет смысл ровно до тех пор, пока его
// можно окинуть взглядом.
func TestПотолокЗакреплённых(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")

	for i := 0; i < MaxPinned; i++ {
		id := mustNote(t, p, author, "важная")
		if err := p.SetNotePinned(ctx, mod, id, true, ""); err != nil {
			t.Fatalf("закрепление %d: %v", i, err)
		}
	}
	extra := mustNote(t, p, author, "ещё одна")
	if err := p.SetNotePinned(ctx, mod, extra, true, ""); !errors.Is(err, ErrTooManyPinned) {
		t.Fatalf("сверх потолка: %v, ожидалось ErrTooManyPinned", err)
	}
}

// Скрытая заметка наверх не всплывает, даже если её когда-то закрепили: условие
// стоит в индексе и в запросе, а не в памяти того, кто закреплял.
func TestСкрытаяЗакреплённаяНеПоказывается(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	note := mustNote(t, p, mustUser(t, p, "Пух"), "важная")

	if err := p.SetNotePinned(ctx, mod, note, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.HideSubject(ctx, mod, NoteSubject(note), CatSpam, "реклама"); err != nil {
		t.Fatal(err)
	}
	top, err := p.PinnedNotes(ctx, Viewer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 0 {
		t.Fatalf("скрытая заметка осталась наверху ленты: %+v", top)
	}
}

// Закрепление — право модератора: место в общей ленте это решение про чужое
// внимание, а не свойство своей записи.
func TestЗакреплятьМожетТолькоМодератор(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	note := mustNote(t, p, author, "своя")

	err := p.SetNotePinned(ctx, Viewer{UserID: author}, note, true, "")
	if !errors.Is(err, ErrNotModerator) {
		t.Fatalf("закрепление без прав: %v", err)
	}
}
