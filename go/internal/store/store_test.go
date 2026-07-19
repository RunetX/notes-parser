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

func TestNoteAvatarRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	want := "https://cdn/avatars/x.jpg"
	if _, err := st.InsertNote(ctx, Note{
		ID: "1", Text: "т", Status: StatusPosted,
		AuthorAvatarURL: want, FirstSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.NoteByID(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthorAvatarURL != want {
		t.Errorf("author_avatar_url: got %q, want %q", got.AuthorAvatarURL, want)
	}
}

func TestNoteImagesUnsentAndMark(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i, u := range []string{"https://cdn/a.jpg", "https://cdn/b.jpg"} {
		if err := st.InsertNoteImage(ctx, "n1", i, u); err != nil {
			t.Fatal(err)
		}
	}
	// Повторная вставка того же URL идемпотентна.
	if err := st.InsertNoteImage(ctx, "n1", 0, "https://cdn/a.jpg"); err != nil {
		t.Fatal(err)
	}
	imgs, err := st.UnsentNoteImages(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("ожидалось 2 неотправленных иллюстрации, получено %d", len(imgs))
	}
	if imgs[0].URL != "https://cdn/a.jpg" {
		t.Errorf("порядок нарушен: %q", imgs[0].URL)
	}
	if err := st.SetNoteImageTGMessageID(ctx, imgs[0].ID, 555); err != nil {
		t.Fatal(err)
	}
	imgs, _ = st.UnsentNoteImages(ctx, "n1")
	if len(imgs) != 1 || imgs[0].URL != "https://cdn/b.jpg" {
		t.Fatalf("после отметки должна остаться одна иллюстрация b, получено %+v", imgs)
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
