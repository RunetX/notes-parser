package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInsertNoteDuplicateIgnored(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	n := Note{ID: "1", Text: "текст", Status: StatusSeeded, FirstSeenAt: time.Now()}

	added, err := st.InsertNote(ctx, n)
	if err != nil || !added {
		t.Fatalf("первая вставка: added=%v err=%v", added, err)
	}
	added, err = st.InsertNote(ctx, n)
	if err != nil || added {
		t.Fatalf("дубль должен игнорироваться: added=%v err=%v", added, err)
	}

	ids, err := st.KnownNoteIDs(ctx)
	if err != nil || !ids["1"] || len(ids) != 1 {
		t.Fatalf("KnownNoteIDs: %v, %v", ids, err)
	}
}

func TestUpsertSessionOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if err := st.UpsertSession(ctx, 42, `[{"name":"old"}]`, now); err != nil {
		t.Fatal(err)
	}
	// Повторный /login заменяет куки той же строкой sessions.
	if err := st.UpsertSession(ctx, 42, `[{"name":"new"}]`, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertNote(ctx, Note{ID: "7", Text: "т", Status: StatusPosted,
		FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Повторное открытие: миграция не падает, данные на месте.
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids, err := st.KnownNoteIDs(ctx)
	if err != nil || !ids["7"] {
		t.Fatalf("данные не пережили переоткрытие: %v, %v", ids, err)
	}
}
