package platform

// Реакции против настоящего Postgres: половина их правил живёт в SQL —
// переключение одним DELETE, «одна на человека на объект» через UNIQUE NULLS
// NOT DISTINCT (у реакции на заметку comment_id пуст, и по умолчанию Postgres
// считал бы такие строки разными) и счёт всего треда одним GROUP BY.

import (
	"context"
	"errors"
	"testing"
)

func TestReactionToggleAndReplace(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID := mustNote(t, p, author, "заметка")

	react := func(code string) error {
		return p.React(ctx, NewReaction{UserID: author, NoteID: noteID, Code: code})
	}
	mine := func() []Reaction {
		all, err := p.NoteReactions(ctx, author, noteID)
		if err != nil {
			t.Fatalf("чтение реакций: %v", err)
		}
		return all[0]
	}

	if err := react("agree"); err != nil {
		t.Fatalf("первое нажатие: %v", err)
	}
	got := mine()
	if len(got) != 1 || got[0].Code != "agree" || got[0].Count != 1 || !got[0].Mine {
		t.Fatalf("после нажатия %+v", got)
	}

	// Другая кнопка МЕНЯЕТ реакцию, а не добавляет вторую: иначе под репликой
	// копится частокол значков от одного человека.
	if err := react("flowers"); err != nil {
		t.Fatalf("смена реакции: %v", err)
	}
	if got := mine(); len(got) != 1 || got[0].Code != "flowers" {
		t.Fatalf("после смены %+v", got)
	}

	// Та же кнопка второй раз — снимает.
	if err := react("flowers"); err != nil {
		t.Fatalf("снятие: %v", err)
	}
	if got := mine(); len(got) != 0 {
		t.Fatalf("после снятия %+v", got)
	}
}

// Реакции заметки и её треда приезжают ОДНИМ запросом, разложенные по объектам:
// ключ 0 — сама заметка, остальные — комментарии.
func TestNoteReactionsCoverWholeThread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	one := mustUser(t, p, "Рио")
	two := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, one, "заметка")
	commentID, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: two, Body: "реплика"})
	if err != nil {
		t.Fatalf("комментарий: %v", err)
	}

	for _, r := range []NewReaction{
		{UserID: one, NoteID: noteID, CommentID: commentID, Code: "agree"},
		{UserID: two, NoteID: noteID, CommentID: commentID, Code: "agree"},
		{UserID: two, NoteID: noteID, Code: "popcorn"},
	} {
		if err := p.React(ctx, r); err != nil {
			t.Fatalf("реакция %+v: %v", r, err)
		}
	}

	all, err := p.NoteReactions(ctx, one, noteID)
	if err != nil {
		t.Fatalf("чтение реакций: %v", err)
	}
	if got := all[commentID]; len(got) != 1 || got[0].Count != 2 || !got[0].Mine {
		t.Errorf("реакции комментария %+v", got)
	}
	// Чужая реакция «моей» не становится — это и есть весь смысл user_id в базе.
	if got := all[0]; len(got) != 1 || got[0].Code != "popcorn" || got[0].Mine {
		t.Errorf("реакции заметки %+v", got)
	}
}

// Чужой код в базу не попадает: набор кнопок задаём мы, и код, которого нет,
// означал бы кнопку, которую нечем нарисовать. Ворота записи — те же, что у
// публикации: закрытый тред не принимает и реакцию.
func TestReactionRefusals(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID := mustNote(t, p, author, "заметка")

	if err := p.React(ctx, NewReaction{UserID: author, NoteID: noteID, Code: "лайк"}); !errors.Is(err, ErrBadReaction) {
		t.Errorf("чужой код: %v", err)
	}
	if err := p.React(ctx, NewReaction{UserID: author, NoteID: 999999999, Code: "agree"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("несуществующая заметка: %v", err)
	}
	// Комментарий из ЧУЖОГО треда не принимается: нажатие приходит формой, а в
	// форме можно поменять что угодно.
	other := mustNote(t, p, author, "другая заметка")
	alien, err := p.CreateComment(ctx, NewComment{NoteID: other, AuthorID: author, Body: "реплика"})
	if err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	if err := p.React(ctx, NewReaction{UserID: author, NoteID: noteID, CommentID: alien, Code: "agree"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("комментарий чужого треда: %v", err)
	}

	mod := Viewer{UserID: mustUser(t, p, "модератор"), Role: RoleModerator}
	if err := p.SetThreadLocked(ctx, mod, noteID, true, ""); err != nil {
		t.Fatalf("замок: %v", err)
	}
	if err := p.React(ctx, NewReaction{UserID: author, NoteID: noteID, Code: "agree"}); !errors.Is(err, ErrThreadLocked) {
		t.Errorf("закрытый тред: %v", err)
	}
}
