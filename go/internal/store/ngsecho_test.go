package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func echoStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Память зеркала об опознанном эхе: заметки и реплики лежат в одной таблице, но
// спрашивают их по-разному — заметки все разом на обход ленты, реплики по одной
// заметке на такт треда.
func TestNGSEchoRemembersNotesAndComments(t *testing.T) {
	ctx := context.Background()
	st := echoStore(t)
	now := time.Now()

	if err := st.MarkNGSEcho(ctx, EchoNote, "313200", "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNGSEcho(ctx, EchoComment, "777", "313100", now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNGSEcho(ctx, EchoComment, "778", "313101", now); err != nil {
		t.Fatal(err)
	}

	notes := map[string]bool{}
	if err := st.NGSEchoNotes(ctx, notes); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !notes["313200"] {
		t.Fatalf("эхо заметок: %v", notes)
	}

	comments := map[int64]bool{}
	if err := st.NGSEchoComments(ctx, "313100", comments); err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || !comments[777] {
		t.Fatalf("эхо реплик заметки 313100: %v", comments)
	}
}

// Такт зеркала видит одну и ту же реплику страницы много раз подряд: отметка
// обязана быть идемпотентной, иначе первый же повтор уронил бы обход.
func TestNGSEchoMarkIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := echoStore(t)
	for range 3 {
		if err := st.MarkNGSEcho(ctx, EchoComment, "42", "n1", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	got := map[int64]bool{}
	if err := st.NGSEchoComments(ctx, "n1", got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("повтор отметки размножил строки: %v", got)
	}
}

// Карта ДОПОЛНЯЕТСЯ, а не подменяется: зеркало кладёт в неё сначала известные
// реплики, потом эхо, и один ответ на вопрос «про эту реплику уже всё решено».
func TestNGSEchoAddsToExistingSet(t *testing.T) {
	ctx := context.Background()
	st := echoStore(t)
	if err := st.MarkNGSEcho(ctx, EchoComment, "2", "n1", time.Now()); err != nil {
		t.Fatal(err)
	}
	known := map[int64]bool{1: true}
	if err := st.NGSEchoComments(ctx, "n1", known); err != nil {
		t.Fatal(err)
	}
	if !known[1] || !known[2] {
		t.Fatalf("набор известных потерян: %v", known)
	}
}
