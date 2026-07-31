package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Фикстурное окно недели: (start, end].
var (
	digStart = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	digEnd   = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
)

func insComment(t *testing.T, st *Store, id int64, noteID, link string, published time.Time) {
	t.Helper()
	c := Comment{
		ID: id, NoteID: noteID, AuthorName: "Автор" + link, AuthorLink: link,
		PublishedAt: published, Text: fmt.Sprintf("комментарий %d", id),
		CreatedAt: published.Add(5 * time.Minute),
	}
	if published.IsZero() {
		c.AuthorName = "Автор без даты"
		c.CreatedAt = digStart.Add(time.Hour)
	}
	if _, err := st.InsertComment(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func TestCommentsBetweenBoundsAndFallback(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: digStart}); err != nil {
		t.Fatal(err)
	}
	insComment(t, st, 1, "n1", "/profile/1/", digStart)                // ровно start — вне окна
	insComment(t, st, 2, "n1", "/profile/1/", digStart.Add(time.Second)) // сразу после start — в окне
	insComment(t, st, 3, "n1", "/profile/2/", digEnd)                  // ровно end — в окне
	insComment(t, st, 4, "n1", "/profile/2/", digEnd.Add(time.Second)) // после end — вне окна
	insComment(t, st, 5, "n1", "/profile/3/", time.Time{})             // published NULL → фолбэк created_at (в окне)

	got, err := st.CommentsBetween(ctx, digStart, digEnd)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range got {
		ids = append(ids, c.ID)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 3 || ids[2] != 5 {
		t.Fatalf("окно (start, end]: ожидались комментарии [2 3 5], получено %v", ids)
	}
}

func TestNotesSeenBetweenSkipsSeeded(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	for _, n := range []Note{
		{ID: "old", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(-time.Hour)},
		{ID: "in1", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(time.Hour)},
		{ID: "in2", Text: "т", Status: StatusArchived, FirstSeenAt: digEnd},
		{ID: "seed", Text: "т", Status: StatusSeeded, FirstSeenAt: digStart.Add(2 * time.Hour)},
	} {
		if _, err := st.InsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NotesSeenBetween(ctx, digStart, digEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "in1" || got[1].ID != "in2" {
		t.Fatalf("ожидались [in1 in2] по порядку появления, получено %+v", got)
	}
}

func TestNotesByIDs(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	empty, err := st.NotesByIDs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("пустой запрос: %v, %v", empty, err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := st.InsertNote(ctx, Note{ID: id, Text: "т", Status: StatusPosted, FirstSeenAt: digStart}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NotesByIDs(ctx, []string{"a", "b", "нет"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"].ID != "a" || got["b"].ID != "b" {
		t.Fatalf("ожидались заметки a и b, получено %+v", got)
	}
}

func TestActiveNotesSince(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	cut := digEnd.Add(-48 * time.Hour)
	for _, n := range []Note{
		{ID: "live", Text: "т", Status: StatusPosted, FirstSeenAt: digStart, LastCommentAt: cut.Add(time.Hour)},
		{ID: "stale", Text: "т", Status: StatusPosted, FirstSeenAt: digStart, LastCommentAt: cut.Add(-time.Hour)},
		{ID: "arch", Text: "т", Status: StatusArchived, FirstSeenAt: digStart, LastCommentAt: cut.Add(2 * time.Hour)},
		{ID: "silent", Text: "т", Status: StatusPosted, FirstSeenAt: digStart},
	} {
		if _, err := st.InsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ActiveNotesSince(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("ожидалась только live, получено %+v", got)
	}
}

func TestCommenterHistory(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: digStart}); err != nil {
		t.Fatal(err)
	}
	// Новичок: два комментария в окне, до окна не появлялся.
	insComment(t, st, 10, "n1", "/profile/new/", digStart.Add(time.Hour))
	insComment(t, st, 11, "n1", "/profile/new/", digStart.Add(2*time.Hour))
	// Вернувшийся: был до окна.
	insComment(t, st, 12, "n1", "/profile/ret/", digStart.Add(-30*24*time.Hour))
	insComment(t, st, 13, "n1", "/profile/ret/", digStart.Add(3*time.Hour))
	// Без анкеты — не учитывается.
	insComment(t, st, 14, "n1", "", digStart.Add(4*time.Hour))

	got, err := st.CommenterHistory(ctx, digStart, digEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ожидались 2 комментатора, получено %+v", got)
	}
	// Порядок: по убыванию активности в окне.
	nc, ret := got[0], got[1]
	if nc.Link != "/profile/new/" || nc.InWindow != 2 || !nc.PrevSeenAt.IsZero() {
		t.Errorf("новичок: %+v", nc)
	}
	if !nc.FirstInWindow.Equal(digStart.Add(time.Hour)) {
		t.Errorf("первый комментарий новичка в окне: %v", nc.FirstInWindow)
	}
	if ret.Link != "/profile/ret/" || ret.InWindow != 1 ||
		!ret.PrevSeenAt.Equal(digStart.Add(-30*24*time.Hour)) {
		t.Errorf("вернувшийся: %+v", ret)
	}
}

func TestNoteAuthorHistory(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	prev := digStart.Add(-60 * 24 * time.Hour)
	for _, n := range []Note{
		// Прошлое автора 7 — seeded-заметка до окна: в прошлое входит.
		{ID: "s1", AuthorID: "7", AuthorName: "Семёрка", Text: "т", Status: StatusSeeded, FirstSeenAt: prev},
		{ID: "w1", AuthorID: "7", AuthorName: "Семёрка", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(time.Hour)},
		{ID: "w2", AuthorID: "8", AuthorName: "Новый", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(2 * time.Hour)},
		// Аноним не учитывается.
		{ID: "w3", AuthorID: "0", AuthorName: "Анонимно", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(3 * time.Hour)},
		// Seeded в окне не считается активностью недели.
		{ID: "s2", AuthorID: "9", AuthorName: "Сид", Text: "т", Status: StatusSeeded, FirstSeenAt: digStart.Add(4 * time.Hour)},
	} {
		if _, err := st.InsertNote(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.NoteAuthorHistory(ctx, digStart, digEnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ожидались авторы 7 и 8, получено %+v", got)
	}
	byID := map[string]AuthorSeen{got[0].AuthorID: got[0], got[1].AuthorID: got[1]}
	if a := byID["7"]; a.NotesInWindow != 1 || !a.PrevNoteAt.Equal(prev) {
		t.Errorf("автор 7: %+v", a)
	}
	if a := byID["8"]; a.NotesInWindow != 1 || !a.PrevNoteAt.IsZero() {
		t.Errorf("автор 8 должен быть новичком: %+v", a)
	}
}

func TestNoteCommentTotalsAndPeakHour(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	// Пустая база: пик отсутствует, не ошибка.
	if _, _, n, err := st.PeakCommentHour(ctx); err != nil || n != 0 {
		t.Fatalf("пустая база: n=%d err=%v", n, err)
	}

	for _, note := range []Note{
		{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: digStart.Add(-time.Hour)},
		{ID: "n2", Text: "т", Status: StatusPosted, FirstSeenAt: digStart},
	} {
		if _, err := st.InsertNote(ctx, note); err != nil {
			t.Fatal(err)
		}
	}
	hour := time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)
	// n1: три комментария в один час (пик), n2: два в разные часы.
	insComment(t, st, 20, "n1", "/profile/1/", hour.Add(5*time.Minute))
	insComment(t, st, 21, "n1", "/profile/2/", hour.Add(15*time.Minute))
	insComment(t, st, 22, "n1", "/profile/1/", hour.Add(45*time.Minute))
	insComment(t, st, 23, "n2", "/profile/1/", hour.Add(2*time.Hour))
	insComment(t, st, 24, "n2", "", hour.Add(5*time.Hour))

	totals, err := st.NoteCommentTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 || totals[0].NoteID != "n1" || totals[1].NoteID != "n2" {
		t.Fatalf("итоги по порядку появления заметок: %+v", totals)
	}
	n1 := totals[0]
	if n1.Comments != 3 || n1.Commenters != 2 ||
		!n1.FirstAt.Equal(hour.Add(5*time.Minute)) || !n1.LastAt.Equal(hour.Add(45*time.Minute)) {
		t.Errorf("итоги n1: %+v", n1)
	}
	if n2 := totals[1]; n2.Comments != 2 || n2.Commenters != 1 {
		t.Errorf("итоги n2 (безанкетный не считается участником): %+v", n2)
	}

	peakStart, noteID, n, err := st.PeakCommentHour(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if noteID != "n1" || n != 3 || !peakStart.Equal(hour) {
		t.Errorf("пик-час: note=%s n=%d start=%v", noteID, n, peakStart)
	}
}
