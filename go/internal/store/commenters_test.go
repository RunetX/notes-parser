package store

import (
	"context"
	"testing"
	"time"
)

// Круг активных комментаторов по зеркалу: id анкеты берётся из ссылки автора,
// безанкетные пропускаются, ник — тот, под которым человек писал последний раз
// (на сайте ники меняют).
func TestCommenters(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "заметка", Status: StatusPosted, FirstSeenAt: now}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	add := func(id int64, link, name string, at time.Time) {
		t.Helper()
		if _, err := st.InsertComment(ctx, Comment{
			ID: id, NoteID: "n1", AuthorName: name, AuthorLink: link,
			PublishedAt: at, Text: "реплика", CreatedAt: now,
		}); err != nil {
			t.Fatalf("реплика %d: %v", id, err)
		}
	}
	link := "https://love.ngs.ru/profile/1431505/"
	add(1, link, "Актрисочка", now.Add(-72*time.Hour))
	add(2, link, "Актриса", now.Add(-48*time.Hour))
	add(3, "https://love.ngs.ru/profile/175869/", "Гадёныш", now.Add(-time.Hour))
	add(4, "", "Аноним", now.Add(-time.Hour))
	add(5, link, "Актриса", now.Add(-400*time.Hour)) // раньше окна

	people, err := st.Commenters(ctx, now.Add(-96*time.Hour), 2)
	if err != nil {
		t.Fatalf("круг: %v", err)
	}
	if len(people) != 1 {
		t.Fatalf("в круг попало %d человек (%+v), ожидался один: у остальных меньше двух реплик", len(people), people)
	}
	c := people[0]
	if c.UserID != 1431505 || c.Comments != 2 {
		t.Errorf("круг = %+v, ожидались две реплики анкеты 1431505", c)
	}
	if c.Nick != "Актриса" {
		t.Errorf("ник = %q, ожидался последний известный «Актриса»", c.Nick)
	}
	if !c.LastComment.Equal(now.Add(-48 * time.Hour)) {
		t.Errorf("последняя реплика = %s, ожидалась %s", c.LastComment, now.Add(-48*time.Hour))
	}
}
