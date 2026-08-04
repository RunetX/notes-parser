package archive

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAddressPrefix(t *testing.T) {
	cases := []struct {
		text, want string
	}{
		{"Ягода, привет", "ягода"},
		{"ЯГОДА, привет", "ягода"},                        // регистр кириллицы
		{"  Noname Noface , ну да", "noname noface"},      // пробелы вокруг ника
		{"Ягода, привет\nвторая строка", "ягода"},         // обращение только в первой строке
		{"Просто текст без обращения", ""},                //
		{"а, ну да", ""},                                  // слишком короткий префикс
		{"Когда я разводилась в 35 лет, было тяжело", ""}, // придаточное, а не ник
		{",", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := addressPrefix(c.text); got != c.want {
			t.Errorf("addressPrefix(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

// newTestArchive поднимает пустой архив во временном каталоге.
func newTestArchive(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// saveThread кладёт заметку с веткой: корневой комментарий и ответы в нём.
func saveThread(t *testing.T, s *Store, noteID, rootID, rootAuthor int64, users []User, replies []Comment, at time.Time) {
	t.Helper()
	all := append([]Comment{{
		ID: rootID, NoteID: noteID, ParentID: 0, AuthorID: rootAuthor,
		Text: "корень ветки", PublishedAt: at,
	}}, replies...)
	note := Note{ID: noteID, AuthorID: rootAuthor, Text: "заметка", PublishedAt: at}
	if _, err := s.SaveGrab(context.Background(), note, all, users, at); err != nil {
		t.Fatalf("SaveGrab: %v", err)
	}
}

// TestBuildAddresseesBranch — базовый случай: адресат определяется по участнику
// ветки, а не по автору её корня. Именно здесь старый граф врал.
func TestBuildAddresseesBranch(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ягода"}, {ID: 3, Name: "Гость"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "Хозяйка, согласна", PublishedAt: at},
		// Реплика лежит в ветке хозяйки, но адресована Ягоде — старый граф
		// записал бы её хозяйке.
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "Ягода, а вот и нет", PublishedAt: at},
		// Без обращения — остаётся на авторе корня через COALESCE.
		{ID: 1003, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "просто мысль вслух", PublishedAt: at},
	}, at)

	st, err := s.BuildAddressees(ctx, nil)
	if err != nil {
		t.Fatalf("BuildAddressees: %v", err)
	}
	if st.Replies != 3 || st.WithPrefix != 2 {
		t.Errorf("ответов %d (want 3), с обращением %d (want 2)", st.Replies, st.WithPrefix)
	}
	if st.Branch != 2 {
		t.Errorf("разрешено по ветке: %d, want 2", st.Branch)
	}
	assertAddressee(t, s, 1001, 1, "branch")
	assertAddressee(t, s, 1002, 2, "branch")
	assertNoAddressee(t, s, 1003)

	// Ребро 3→2 существует только благодаря слою: по parent_id адресатом был бы 1.
	if got := replyEdge(t, s, 3, 2); got != 1 {
		t.Errorf("v_reply_edges 3→2 = %d, want 1", got)
	}
	// Реплика без обращения по-прежнему числится за автором корня.
	if got := replyEdge(t, s, 3, 1); got != 1 {
		t.Errorf("v_reply_edges 3→1 = %d, want 1 (fallback на корень ветки)", got)
	}
}

// TestBuildAddresseesOmonym — ник, который носят двое: пока в ветке оба, метод
// обязан промолчать, а не выбрать наугад.
func TestBuildAddresseesOmonym(t *testing.T) {
	s := newTestArchive(t)
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ника"}, {ID: 3, Name: "Ника"}, {ID: 4, Name: "Гость"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "реплика первой Ники", PublishedAt: at},
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "реплика второй Ники", PublishedAt: at},
		{ID: 1003, NoteID: 100, ParentID: 1000, AuthorID: 4, Text: "Ника, которая из вас?", PublishedAt: at},
	}, at)

	if _, err := s.BuildAddressees(context.Background(), nil); err != nil {
		t.Fatalf("BuildAddressees: %v", err)
	}
	assertNoAddressee(t, s, 1003)
}

// TestBuildAddresseesNickHistory — адресат сменил ник: users.name уже новый, а в
// реплике стоит старый. Историю восстанавливаем по обращениям в его же ветках.
func TestBuildAddresseesNickHistory(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	jan := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)

	// Пользователь 2 сейчас зовётся «Мурена», но в январе 2024 был «Смородина»:
	// в его собственной ветке к нему трижды обращаются старым ником.
	users := []User{{ID: 1, Name: "Гость"}, {ID: 2, Name: "Мурена"}, {ID: 3, Name: "Хозяйка"}}
	saveThread(t, s, 100, 1000, 2, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 1, Text: "Смородина, привет", PublishedAt: jan},
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "Смородина, как дела", PublishedAt: jan},
		{ID: 1003, NoteID: 100, ParentID: 1000, AuthorID: 1, Text: "Смородина, ну ты даёшь", PublishedAt: jan},
	}, jan)
	// В чужой ветке к тому же человеку обращаются старым ником — вот это и
	// должна разрешить история (по текущему имени «Смородина» не находится).
	saveThread(t, s, 101, 2000, 3, users, []Comment{
		{ID: 2001, NoteID: 101, ParentID: 2000, AuthorID: 1, Text: "Смородина, и тебе привет", PublishedAt: jan},
	}, jan)

	st, err := s.BuildAddressees(ctx, nil)
	if err != nil {
		t.Fatalf("BuildAddressees: %v", err)
	}
	if st.Nicks == 0 {
		t.Fatal("nick_history пуста: история ников не восстановлена")
	}
	// 2001 — в чужой ветке, где адресата нет среди участников: метод history.
	assertAddressee(t, s, 2001, 2, "history")
	// А в собственной ветке адресат — участник, значит history_branch.
	assertAddressee(t, s, 1001, 2, "history_branch")
}

// TestBuildAddresseesIdempotent — повторный пересчёт не задваивает и не меняет итог.
func TestBuildAddresseesIdempotent(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ягода"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "Хозяйка, ага", PublishedAt: at},
	}, at)

	first, err := s.BuildAddressees(ctx, nil)
	if err != nil {
		t.Fatalf("BuildAddressees #1: %v", err)
	}
	second, err := s.BuildAddressees(ctx, nil)
	if err != nil {
		t.Fatalf("BuildAddressees #2: %v", err)
	}
	if first != second {
		t.Errorf("пересчёт не идемпотентен: %+v != %+v", first, second)
	}
}

func assertAddressee(t *testing.T, s *Store, commentID, want int64, wantMethod string) {
	t.Helper()
	var got int64
	var method string
	err := s.db.QueryRow(`SELECT addressee_id, method FROM comment_addressee WHERE comment_id = ?`,
		commentID).Scan(&got, &method)
	if err != nil {
		t.Fatalf("адресат c%d: %v", commentID, err)
	}
	if got != want || method != wantMethod {
		t.Errorf("адресат c%d = %d (%s), want %d (%s)", commentID, got, method, want, wantMethod)
	}
}

func assertNoAddressee(t *testing.T, s *Store, commentID int64) {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM comment_addressee WHERE comment_id = ?`,
		commentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("c%d получил адресата, хотя не должен был", commentID)
	}
}

func replyEdge(t *testing.T, s *Store, from, to int64) int {
	t.Helper()
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(replies), 0) FROM v_reply_edges WHERE from_id = ? AND to_id = ?`,
		from, to).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
