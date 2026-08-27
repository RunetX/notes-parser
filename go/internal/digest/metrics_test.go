package digest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

const siteBase = "https://love.test"

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testWindow() Window {
	end := time.Date(2026, 7, 31, 19, 0, 0, 0, nsk)
	return Window{Start: end.AddDate(0, 0, -7), End: end, ID: "2026-W31"}
}

func addNote(t *testing.T, st *store.Store, id, authorID string, firstSeen time.Time, status string, lastComment time.Time) {
	t.Helper()
	if _, err := st.InsertNote(context.Background(), store.Note{
		ID: id, AuthorID: authorID, AuthorName: "Автор" + id, Text: "текст заметки " + id,
		Status: status, FirstSeenAt: firstSeen, LastCommentAt: lastComment,
	}); err != nil {
		t.Fatal(err)
	}
}

func addComment(t *testing.T, st *store.Store, id int64, noteID, link, text string, at time.Time) {
	t.Helper()
	if _, err := st.InsertComment(context.Background(), store.Comment{
		ID: id, NoteID: noteID, AuthorName: "Некто", AuthorLink: link,
		PublishedAt: at, Text: text, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTopNoteAndDisputes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	addNote(t, st, "A", "1", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	addNote(t, st, "B", "2", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	// A: перепалка двух авторов — 6 реплик через 2 минуты (пинг-понг, пик-час).
	for i := int64(0); i < 6; i++ {
		link := siteBase + "/profile/101/"
		if i%2 == 1 {
			link = siteBase + "/profile/102/"
		}
		addComment(t, st, 100+i, "A", link, "реплика", base.Add(time.Duration(i)*2*time.Minute))
	}
	// B: столько же комментариев, но от 6 участников — «заметка недели»
	// по тай-брейку числа участников.
	for i := int64(0); i < 6; i++ {
		addComment(t, st, 200+i, "B", siteBase+"/profile/20"+string(rune('0'+i))+"/",
			"мнение", base.Add(time.Duration(i)*3*time.Hour))
	}

	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if is.TopNote == nil || is.TopNote.Note.ID != "B" {
		t.Fatalf("заметка недели: %+v", is.TopNote)
	}
	if is.TopNote.Commenters != 6 {
		t.Errorf("участники B: %d", is.TopNote.Commenters)
	}
	if len(is.Disputes) != 1 || is.Disputes[0].Note.ID != "A" {
		t.Fatalf("спор недели: %+v", is.Disputes)
	}
	a := is.Disputes[0]
	if a.PingPong != 5 || a.PeakHourN != 6 || a.Heat <= 0 {
		t.Errorf("метрики перепалки A: pingpong=%d peak=%d heat=%v", a.PingPong, a.PeakHourN, a.Heat)
	}
	if is.Stats.Comments != 12 || is.Stats.Commenters != 8 || is.Stats.Notes != 2 {
		t.Errorf("сводка: %+v", is.Stats)
	}
}

func TestBuildQuotes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	addNote(t, st, "Q", "1", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	long := strings.Repeat("ц", 100)
	addComment(t, st, 1, "Q", siteBase+"/profile/5/", long, base)
	addComment(t, st, 2, "Q", siteBase+"/profile/5/", "свой же ответ", base.Add(5*time.Minute))
	addComment(t, st, 3, "Q", siteBase+"/profile/6/", "ответ раз", base.Add(10*time.Minute))
	addComment(t, st, 4, "Q", siteBase+"/profile/7/", "ответ два", base.Add(30*time.Minute))
	addComment(t, st, 5, "Q", siteBase+"/profile/8/", "поздний ответ", base.Add(3*time.Hour))
	addComment(t, st, 6, "Q", siteBase+"/profile/9/", strings.Repeat("щ", 700), base.Add(4*time.Hour))

	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if len(is.Quotes) != 1 {
		t.Fatalf("кандидаты цитаты (короткие и гигантские отсеяны): %+v", is.Quotes)
	}
	q := is.Quotes[0]
	if q.Comment.ID != 1 || q.RepliesAfter != 2 {
		t.Errorf("цитата: id=%d replies=%d (свой ответ и поздний не считаются)", q.Comment.ID, q.RepliesAfter)
	}
}

func TestBuildRecordWithinHorizon(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	addNote(t, st, "R", "1", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	for i := int64(0); i < 12; i++ {
		addComment(t, st, 300+i, "R", siteBase+"/profile/1/", "к", base.Add(time.Duration(i)*4*time.Minute))
	}
	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRecord(is, "самый длинный тред за год") {
		t.Errorf("нет рекорда горизонта: %+v", is.Records)
	}
	// Все 12 реплик уложились в час — заодно рекорд пик-часа.
	if !hasRecord(is, "за час — рекорд") {
		t.Errorf("нет рекорда пик-часа: %+v", is.Records)
	}
}

func TestBuildRecordSinceMonth(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	// Апрельский тред больше нынешнего: рекорд формулируется «с апреля».
	april := time.Date(2026, 4, 10, 12, 0, 0, 0, nsk)
	addNote(t, st, "P", "1", april, store.StatusArchived, time.Time{})
	for i := int64(0); i < 15; i++ {
		addComment(t, st, 400+i, "P", siteBase+"/profile/2/", "к", april.Add(time.Duration(i)*time.Hour))
	}
	addNote(t, st, "T", "3", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	for i := int64(0); i < 12; i++ {
		addComment(t, st, 500+i, "T", siteBase+"/profile/3/", "к", w.Start.Add(24*time.Hour).Add(time.Duration(i)*time.Hour))
	}
	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRecord(is, "самый длинный тред с апреля") {
		t.Errorf("нет рекорда «с апреля»: %+v", is.Records)
	}
}

func TestBuildRecordSkippedWhenBiggerRecent(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	// Больший тред был всего 9 дней назад — «рекорд» не объявляется.
	recent := w.Start.Add(-2 * 24 * time.Hour)
	addNote(t, st, "P", "1", recent, store.StatusArchived, time.Time{})
	for i := int64(0); i < 15; i++ {
		addComment(t, st, 400+i, "P", siteBase+"/profile/2/", "к", recent.Add(time.Duration(i)*time.Hour))
	}
	addNote(t, st, "T", "3", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	for i := int64(0); i < 12; i++ {
		addComment(t, st, 500+i, "T", siteBase+"/profile/3/", "к", w.Start.Add(24*time.Hour).Add(time.Duration(i)*time.Hour))
	}
	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if hasRecord(is, "самый длинный тред") {
		t.Errorf("рекорд треда не должен объявляться: %+v", is.Records)
	}
}

func TestBuildPersons(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	// Новичок 42: заметка в окне + два комментария той же анкетой — сливаются.
	addNote(t, st, "n42", "42", w.Start.Add(time.Hour), store.StatusPosted, time.Time{})
	addComment(t, st, 1, "n42", siteBase+"/profile/42/", "раз", base)
	addComment(t, st, 2, "n42", siteBase+"/profile/42/", "два", base.Add(time.Minute))
	// Вернувшийся 77: молчал 30 дней.
	addComment(t, st, 3, "n42", siteBase+"/profile/77/", "давно", w.Start.Add(-30*24*time.Hour))
	addComment(t, st, 4, "n42", siteBase+"/profile/77/", "снова тут", base.Add(time.Hour))
	// 88 писал 5 дней назад — ни новичок, ни возвращение.
	addComment(t, st, 5, "n42", siteBase+"/profile/88/", "было", w.Start.Add(-5*24*time.Hour))
	addComment(t, st, 6, "n42", siteBase+"/profile/88/", "есть", base.Add(2*time.Hour))

	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if len(is.Newcomers) != 1 {
		t.Fatalf("новички: %+v", is.Newcomers)
	}
	nc := is.Newcomers[0]
	if nc.Notes != 1 || nc.Comments != 2 {
		t.Errorf("слитый новичок 42: %+v", nc)
	}
	if len(is.Returnees) != 1 {
		t.Fatalf("возвращение: %+v", is.Returnees)
	}
}

func TestBuildStillAlive(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	w := testWindow()
	base := w.Start.Add(24 * time.Hour)
	// Топ-заметка активна, но из рубрики исключается.
	addNote(t, st, "T", "1", w.Start.Add(time.Hour), store.StatusPosted, w.End.Add(-30*time.Minute))
	for i := int64(0); i < 3; i++ {
		addComment(t, st, 600+i, "T", siteBase+"/profile/1/", "к", base.Add(time.Duration(i)*time.Hour))
	}
	addNote(t, st, "L", "2", w.Start.Add(time.Hour), store.StatusPosted, w.End.Add(-time.Hour))
	addComment(t, st, 700, "L", siteBase+"/profile/2/", "к", base)
	addNote(t, st, "L2", "3", w.Start.Add(time.Hour), store.StatusPosted, w.End.Add(-3*24*time.Hour))
	addNote(t, st, "L3", "4", w.Start.Add(time.Hour), store.StatusArchived, w.End.Add(-time.Hour))

	is, err := Build(ctx, NewStoreSource(st, siteBase), w)
	if err != nil {
		t.Fatal(err)
	}
	if is.TopNote == nil || is.TopNote.Note.ID != "T" {
		t.Fatalf("топ: %+v", is.TopNote)
	}
	if len(is.StillAlive) != 1 || is.StillAlive[0].Note.ID != "L" {
		t.Fatalf("ещё живо: %+v", is.StillAlive)
	}
	if is.StillAlive[0].Comments != 1 {
		t.Errorf("оконные метрики живой заметки: %+v", is.StillAlive[0])
	}
}

func hasRecord(is *Issue, substr string) bool {
	for _, r := range is.Records {
		if strings.Contains(r.Text, substr) {
			return true
		}
	}
	return false
}
