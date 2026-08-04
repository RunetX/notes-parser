package store

import (
	"context"
	"testing"
	"time"
)

func TestTargetRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if _, _, found, err := st.Target(ctx, MessengerMax, TargetNotePost, "n1"); err != nil || found {
		t.Fatalf("пустая база: found=%v err=%v", found, err)
	}
	if err := st.SetTarget(ctx, MessengerMax, TargetNotePost, "n1", "mid.abc123", ""); err != nil {
		t.Fatal(err)
	}
	msg, thread, found, err := st.Target(ctx, MessengerMax, TargetNotePost, "n1")
	if err != nil || !found || msg != "mid.abc123" || thread != "" {
		t.Fatalf("target: %q %q %v %v", msg, thread, found, err)
	}
	// Дозапись thread_id не затирает message_id.
	if err := st.SetTarget(ctx, MessengerMax, TargetNotePost, "n1", "", "mid.thread9"); err != nil {
		t.Fatal(err)
	}
	msg, thread, _, _ = st.Target(ctx, MessengerMax, TargetNotePost, "n1")
	if msg != "mid.abc123" || thread != "mid.thread9" {
		t.Errorf("после дозаписи: msg=%q thread=%q", msg, thread)
	}
}

func TestSetTargetWriteThroughLegacy(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPending, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTarget(ctx, MessengerTelegram, TargetNotePost, "n1", "77", ""); err != nil {
		t.Fatal(err)
	}
	n, err := st.NoteByID(ctx, "n1")
	if err != nil || n.TGMessageID != 77 {
		t.Errorf("write-through tg_message_id: %d %v", n.TGMessageID, err)
	}

	// MAX-значения легаси-колонки не трогают.
	if err := st.SetTarget(ctx, MessengerMax, TargetNotePost, "n1", "mid.xyz", ""); err != nil {
		t.Fatal(err)
	}
	n, _ = st.NoteByID(ctx, "n1")
	if n.TGMessageID != 77 {
		t.Errorf("MAX-таргет затёр tg_message_id: %d", n.TGMessageID)
	}
}

func TestUnsentCommentsPerMessenger(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, err := st.InsertComment(ctx, Comment{ID: id, NoteID: "n1", AuthorName: "А", Text: "к", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	// Комментарий 1 отправлен только в telegram.
	if err := st.SetTarget(ctx, MessengerTelegram, TargetComment, "1", "500", ""); err != nil {
		t.Fatal(err)
	}
	tg, err := st.UnsentCommentsFor(ctx, MessengerTelegram, "n1")
	if err != nil || len(tg) != 1 || tg[0].ID != 2 {
		t.Fatalf("незашедшие в telegram: %+v %v", tg, err)
	}
	mx, err := st.UnsentCommentsFor(ctx, MessengerMax, "n1")
	if err != nil || len(mx) != 2 {
		t.Fatalf("незашедшие в max: %+v %v", mx, err)
	}

	// Write-through виден в легаси-колонке.
	c, err := st.CommentByTarget(ctx, MessengerTelegram, "500")
	if err != nil || c.ID != 1 || c.TGMessageID != 500 {
		t.Fatalf("comment by target: %+v %v", c, err)
	}
}

func TestAddresseeMessage(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Ягода высказывалась дважды; адресатом должна стать её последняя реплика.
	comments := []Comment{
		{ID: 1, NoteID: "n1", AuthorName: "Ягода", Text: "первая"},
		{ID: 2, NoteID: "n1", AuthorName: "ПЁТР", Text: "своё"},
		{ID: 3, NoteID: "n1", AuthorName: "Ягода", Text: "вторая"},
		{ID: 4, NoteID: "n1", AuthorName: "Гость", Text: "не отзеркален"},
	}
	for _, c := range comments {
		c.CreatedAt = time.Now()
		if _, err := st.InsertComment(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for id, mid := range map[string]string{"1": "101", "2": "102", "3": "103"} {
		if err := st.SetTarget(ctx, MessengerTelegram, TargetComment, id, mid, ""); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name     string
		nick     string
		beforeID int64
		want     string
	}{
		{"последняя реплика адресата", "ягода", 10, "103"},
		{"более ранняя реплика", "ягода", 3, "101"},
		{"регистр кириллицы", "пётр", 10, "102"},
		{"адресат ещё не отзеркален", "гость", 10, ""},
		{"ник не встречался", "хатуль", 10, ""},
		{"обращения нет", "", 10, ""},
		{"сам себе не адресат", "ягода", 1, ""},
	}
	for _, c := range cases {
		got, err := st.AddresseeMessage(ctx, MessengerTelegram, "n1", c.beforeID, c.nick)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// Цели у каждого мессенджера свои: в MAX те же комментарии не отправлены.
	if got, err := st.AddresseeMessage(ctx, MessengerMax, "n1", 10, "ягода"); err != nil || got != "" {
		t.Errorf("адресат из чужого мессенджера: %q %v", got, err)
	}
}

func TestReplyDedupPerMessenger(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	first, err := st.TryMarkReplyProcessed(ctx, MessengerMax, "mid.r1", now)
	if err != nil || !first {
		t.Fatalf("первая пометка: %v %v", first, err)
	}
	// Тот же id в другом мессенджере — независим.
	tg, err := st.TryMarkReplyProcessed(ctx, MessengerTelegram, "mid.r1", now)
	if err != nil || !tg {
		t.Fatalf("другой мессенджер независим: %v %v", tg, err)
	}
	second, _ := st.TryMarkReplyProcessed(ctx, MessengerMax, "mid.r1", now)
	if second {
		t.Error("повторная пометка должна вернуть false")
	}
}
