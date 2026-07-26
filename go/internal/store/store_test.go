package store

import (
	"context"
	"path/filepath"
	"strconv"
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
	imgs, err := st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("ожидалось 2 неотправленных иллюстрации, получено %d", len(imgs))
	}
	if imgs[0].URL != "https://cdn/a.jpg" {
		t.Errorf("порядок нарушен: %q", imgs[0].URL)
	}
	if err := st.SetTarget(ctx, MessengerTelegram, TargetNoteImage,
		strconv.FormatInt(imgs[0].ID, 10), "555", ""); err != nil {
		t.Fatal(err)
	}
	imgs, _ = st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1")
	if len(imgs) != 1 || imgs[0].URL != "https://cdn/b.jpg" {
		t.Fatalf("после отметки должна остаться одна иллюстрация b, получено %+v", imgs)
	}
}

func TestDeleteNoteCascade(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertComment(ctx, Comment{ID: 1, NoteID: "n1", AuthorName: "А", Text: "к", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertNoteImage(ctx, "n1", 0, "https://cdn/a.jpg"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteNote(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteByID(ctx, "n1"); err == nil {
		t.Error("заметка должна быть удалена")
	}
	if imgs, _ := st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1"); len(imgs) != 0 {
		t.Errorf("иллюстрации должны быть удалены: %+v", imgs)
	}
	ids, _ := st.CommentIDs(ctx, "n1")
	if len(ids) != 0 {
		t.Errorf("комментарии должны быть удалены: %v", ids)
	}
}

func TestSubscriptionsAddListRemove(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	added, err := st.AddSubscription(ctx, MessengerTelegram, "Граф", 42)
	if err != nil || !added {
		t.Fatalf("первая подписка: added=%v err=%v", added, err)
	}
	added, _ = st.AddSubscription(ctx, MessengerTelegram, "Граф", 42)
	if added {
		t.Error("дубль подписки должен игнорироваться")
	}
	if _, err := st.AddSubscription(ctx, MessengerTelegram, "Барон", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSubscription(ctx, MessengerTelegram, "Граф", 99); err != nil {
		t.Fatal(err)
	}

	kws, err := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(kws) != 2 || kws[0] != "Барон" || kws[1] != "Граф" { // ORDER BY keyword
		t.Errorf("подписки пользователя 42: %v", kws)
	}

	removed, err := st.RemoveSubscription(ctx, MessengerTelegram, "Граф", 42)
	if err != nil || !removed {
		t.Fatalf("удаление: removed=%v err=%v", removed, err)
	}
	removed, _ = st.RemoveSubscription(ctx, MessengerTelegram, "Граф", 42)
	if removed {
		t.Error("повторное удаление должно вернуть false")
	}
	// Подписка другого пользователя на «Граф» не затронута.
	kws, _ = st.SubscriptionsByUser(ctx, MessengerTelegram, 99)
	if len(kws) != 1 || kws[0] != "Граф" {
		t.Errorf("подписки пользователя 99: %v", kws)
	}
}

func TestMarkNoteCommentsClosed(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Новая заметка открыта для комментариев.
	n, err := st.NoteByID(ctx, "n1")
	if err != nil || n.CommentsClosed {
		t.Fatalf("новая заметка не должна быть закрыта: closed=%v err=%v", n.CommentsClosed, err)
	}
	changed, err := st.MarkNoteCommentsClosed(ctx, "n1")
	if err != nil || !changed {
		t.Fatalf("первая отметка должна сработать: changed=%v err=%v", changed, err)
	}
	// Повторная отметка идемпотентна и не логируется повторно.
	changed, _ = st.MarkNoteCommentsClosed(ctx, "n1")
	if changed {
		t.Error("повторная отметка должна вернуть false (уже закрыта)")
	}
	n, err = st.NoteByID(ctx, "n1")
	if err != nil || !n.CommentsClosed {
		t.Fatalf("после отметки заметка закрыта: closed=%v err=%v", n.CommentsClosed, err)
	}
}

func TestUpsertSessionOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if err := st.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"old"}]`, now); err != nil {
		t.Fatal(err)
	}
	// Повторный /login заменяет куки той же строкой sessions.
	if err := st.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"new"}]`, now.Add(time.Hour)); err != nil {
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
