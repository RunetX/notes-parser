package archive

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveGrabNormalizesAndDedups(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	pub := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	note := Note{ID: 1, AuthorID: 10, Text: "заметка", Images: []string{"http://img/1"}, PublishedAt: pub}
	users := []User{
		{ID: 10, Name: "Рантье", ProfileURL: "http://p/10", AvatarURL: "av10"},
		{ID: 20, Name: "Аня", Age: "30 лет", ProfileURL: "http://p/20", AvatarURL: "av20"},
	}
	// Два комментария от одного автора (20) + один от автора заметки (10) —
	// пользователи должны дедуплицироваться до двух строк.
	comments := []Comment{
		{ID: 100, NoteID: 1, ParentID: 0, AuthorID: 20, Text: "корень", PublishedAt: pub},
		{ID: 101, NoteID: 1, ParentID: 100, AuthorID: 10, Text: "ответ", PublishedAt: pub},
		{ID: 102, NoteID: 1, ParentID: 100, AuthorID: 20, Text: "ещё", PublishedAt: pub},
	}

	st, err := s.SaveGrab(ctx, note, comments, users, time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if st.NewUsers != 2 {
		t.Errorf("новых пользователей: got %d, want 2", st.NewUsers)
	}
	if st.CommentsInserted != 3 || st.CommentsTotal != 3 {
		t.Errorf("комментарии: inserted=%d total=%d, want 3/3", st.CommentsInserted, st.CommentsTotal)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM users"); got != 2 {
		t.Errorf("строк в users: got %d, want 2 (дедуп)", got)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM comments WHERE parent_id != 0"); got != 2 {
		t.Errorf("ответов (parent_id != 0): got %d, want 2", got)
	}
}

func TestSaveGrabAvatarLatestWins(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	note := Note{ID: 1, AuthorID: 10, Text: "n"}
	firstGrab := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	if _, err := s.SaveGrab(ctx, note,
		[]Comment{{ID: 100, NoteID: 1, AuthorID: 10, Text: "c"}},
		[]User{{ID: 10, Name: "Рантье", AvatarURL: "av-old"}}, firstGrab); err != nil {
		t.Fatal(err)
	}

	// Повторная выгрузка: у пользователя новый аватар и новое имя, комментарий
	// тот же (идемпотентность).
	secondGrab := firstGrab.Add(24 * time.Hour)
	st, err := s.SaveGrab(ctx, note,
		[]Comment{{ID: 100, NoteID: 1, AuthorID: 10, Text: "c"}},
		[]User{{ID: 10, Name: "Рантье NEW", AvatarURL: "av-new"}}, secondGrab)
	if err != nil {
		t.Fatal(err)
	}
	if st.NewUsers != 0 {
		t.Errorf("новых пользователей при повторе: got %d, want 0", st.NewUsers)
	}
	if st.AvatarChanged != 1 {
		t.Errorf("смена аватара не засчитана: got %d, want 1", st.AvatarChanged)
	}
	if st.NameChanged != 1 {
		t.Errorf("смена имени не засчитана: got %d, want 1", st.NameChanged)
	}
	if st.CommentsInserted != 0 {
		t.Errorf("комментарий задвоился: inserted=%d, want 0", st.CommentsInserted)
	}

	var name, avatar, firstSeen, lastSeen string
	if err := s.db.QueryRowContext(ctx,
		"SELECT name, avatar_url, first_seen, last_seen FROM users WHERE id = 10").
		Scan(&name, &avatar, &firstSeen, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if avatar != "av-new" || name != "Рантье NEW" {
		t.Errorf("latest-wins не сработал: name=%q avatar=%q", name, avatar)
	}
	if firstSeen != firstGrab.Format(time.RFC3339) {
		t.Errorf("first_seen изменился: got %q, want %q", firstSeen, firstGrab.Format(time.RFC3339))
	}
	if lastSeen != secondGrab.Format(time.RFC3339) {
		t.Errorf("last_seen не обновился: got %q, want %q", lastSeen, secondGrab.Format(time.RFC3339))
	}
}

func TestSaveGrabEmptyDoesNotClobber(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	// Первый раз — полные данные комментатора (с возрастом и ссылкой).
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 10, Text: "n"},
		[]Comment{{ID: 100, NoteID: 1, AuthorID: 10}},
		[]User{{ID: 10, Name: "Аня", Age: "30 лет", ProfileURL: "http://p/10", AvatarURL: "av"}}, now); err != nil {
		t.Fatal(err)
	}
	// Второй раз тот же человек как автор заметки — без возраста и ссылки: они
	// НЕ должны затереться пустыми значениями.
	if _, err := s.SaveGrab(ctx, Note{ID: 2, AuthorID: 10, Text: "n2"},
		nil, []User{{ID: 10, Name: "Аня", ProfileURL: "", Age: "", AvatarURL: "av"}}, now); err != nil {
		t.Fatal(err)
	}
	var age, profile string
	if err := s.db.QueryRowContext(ctx,
		"SELECT age, profile_url FROM users WHERE id = 10").Scan(&age, &profile); err != nil {
		t.Fatal(err)
	}
	if age != "30 лет" || profile != "http://p/10" {
		t.Errorf("пустые значения затёрли непустые: age=%q profile=%q", age, profile)
	}
}

func TestViews(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	// Заметка автора 10; комментарии: 100 (корень, автор 20),
	// 101 (ответ на 100, автор 10), 102 (ответ на 100, автор 20).
	users := []User{{ID: 10, Name: "A", AvatarURL: "a"}, {ID: 20, Name: "B", AvatarURL: "b"}}
	comments := []Comment{
		{ID: 100, NoteID: 1, ParentID: 0, AuthorID: 20, Text: "x"},
		{ID: 101, NoteID: 1, ParentID: 100, AuthorID: 10, Text: "y"},
		{ID: 102, NoteID: 1, ParentID: 100, AuthorID: 20, Text: "z"},
	}
	if _, err := s.SaveGrab(ctx, Note{ID: 1, AuthorID: 10, Text: "n"}, comments, users,
		time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	// v_comment_tree: у ответа 101 родитель — комментарий автора B.
	var parentName string
	if err := s.db.QueryRowContext(ctx,
		"SELECT parent_author_name FROM v_comment_tree WHERE id = 101").Scan(&parentName); err != nil {
		t.Fatal(err)
	}
	if parentName != "B" {
		t.Errorf("v_comment_tree.parent_author_name у 101: got %q, want B", parentName)
	}
	// У корня 100 родителя нет.
	var pid any
	if err := s.db.QueryRowContext(ctx,
		"SELECT parent_author_id FROM v_comment_tree WHERE id = 100").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if pid != nil {
		t.Errorf("у корня 100 parent_author_id должен быть NULL, got %v", pid)
	}

	// v_reply_edges: ребро A(10)->B(20) с весом 1.
	var replies int
	if err := s.db.QueryRowContext(ctx,
		"SELECT replies FROM v_reply_edges WHERE from_id = 10 AND to_id = 20").Scan(&replies); err != nil {
		t.Fatal(err)
	}
	if replies != 1 {
		t.Errorf("v_reply_edges 10->20: got %d, want 1", replies)
	}

	// v_user_activity: у B(20) два комментария.
	var bComments int
	if err := s.db.QueryRowContext(ctx,
		"SELECT comments FROM v_user_activity WHERE id = 20").Scan(&bComments); err != nil {
		t.Fatal(err)
	}
	if bComments != 2 {
		t.Errorf("v_user_activity.comments у 20: got %d, want 2", bComments)
	}

	// v_note_overview: 3 комментария, 2 участника.
	var nc, np int
	if err := s.db.QueryRowContext(ctx,
		"SELECT comments, participants FROM v_note_overview WHERE id = 1").Scan(&nc, &np); err != nil {
		t.Fatal(err)
	}
	if nc != 3 || np != 2 {
		t.Errorf("v_note_overview: comments=%d participants=%d, want 3/2", nc, np)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	pub := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	note := Note{ID: 1, AuthorID: 10, Text: "заметка", Images: []string{"http://img/1"},
		CommentsClosed: true, PublishedAt: pub}
	users := []User{
		{ID: 10, Name: "Рантье", ProfileURL: "http://p/10", AvatarURL: "av10"},
		{ID: 20, Name: "Аня", Age: "30 лет", ProfileURL: "http://p/20", AvatarURL: "av20"},
	}
	comments := []Comment{
		{ID: 100, NoteID: 1, ParentID: 0, AuthorID: 20, Text: "корень", PublishedAt: pub},
		{ID: 101, NoteID: 1, ParentID: 100, AuthorID: 10, Text: "ответ", PublishedAt: pub},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.LoadNote(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("LoadNote: ok=%v err=%v", ok, err)
	}
	if got.Author == nil || got.Author.Name != "Рантье" {
		t.Errorf("автор заметки не загружен: %+v", got.Author)
	}
	if !got.CommentsClosed || !got.PublishedAt.Equal(pub) {
		t.Errorf("поля заметки: closed=%v published=%v", got.CommentsClosed, got.PublishedAt)
	}
	if len(got.Images) != 1 || got.Images[0] != "http://img/1" {
		t.Errorf("images не разобраны: %v", got.Images)
	}

	cs, err := s.LoadComments(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("комментариев: got %d, want 2", len(cs))
	}
	// Отсортированы по id; у второго — денормализованный автор и parent.
	if cs[1].ID != 101 || cs[1].ParentID != 100 || cs[1].Author.Name != "Рантье" {
		t.Errorf("комментарий 101: %+v", cs[1])
	}
	if cs[0].Author.Age != "30 лет" {
		t.Errorf("возраст автора 100: got %q, want 30 лет", cs[0].Author.Age)
	}

	// Несуществующая заметка — ok=false, без ошибки.
	if _, ok, err := s.LoadNote(ctx, 999); ok || err != nil {
		t.Errorf("LoadNote(999): ok=%v err=%v, want false,nil", ok, err)
	}
}

func count(t *testing.T, s *Store, query string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
