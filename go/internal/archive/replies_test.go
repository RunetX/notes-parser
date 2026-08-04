package archive

import (
	"context"
	"testing"
	"time"
)

// Слой обогащения знает то, чего эвристика знать не может: КАКОЙ именно
// реплике адресован ответ, если собеседник высказывался несколько раз, — и
// разрешает ответы вообще без обращения «Ник, …».
func TestReplyTreeBeatsHeuristic(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ягода"}, {ID: 3, Name: "Гость"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "первая мысль Ягоды", PublishedAt: at},
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "вторая мысль Ягоды", PublishedAt: at},
		// Обращение указывает на человека, но не на реплику: эвристика взяла бы
		// последнюю (1002), а отвечают первой.
		{ID: 1003, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "Ягода, вот про это", PublishedAt: at},
		// Обращения нет вовсе — эвристике такой ответ недоступен в принципе.
		{ID: 1004, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "согласен полностью", PublishedAt: at},
	}, at)

	res, err := s.SaveReplyTree(ctx, 100, map[int64]int64{
		1000: 0, 1001: 0, 1002: 0,
		1003: 1001,
		1004: 1002,
	})
	if err != nil {
		t.Fatalf("SaveReplyTree: %v", err)
	}
	if res.Seen != 5 || res.Stored != 2 || res.Unknown != 0 {
		t.Errorf("итог: %+v, ожидалось seen=5 stored=2 unknown=0", res)
	}

	st, err := s.BuildAddressees(ctx, nil)
	if err != nil {
		t.Fatalf("BuildAddressees: %v", err)
	}
	if st.Reply != 2 {
		t.Errorf("разрешено по дереву: %d, want 2", st.Reply)
	}
	assertAddressee(t, s, 1003, 2, "reply")
	assertAddressee(t, s, 1004, 2, "reply") // без обращения — только через слой

	// Точная пара сохранена именно на 1001, а не на последней реплике Ягоды.
	var target int64
	if err := s.db.QueryRow(`SELECT reply_to FROM comment_reply WHERE comment_id = 1003`).
		Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != 1001 {
		t.Errorf("цель ответа 1003 = %d, want 1001", target)
	}
}

// Точная пара должна побеждать эвристику: обращение «Хозяйка, …» указывает на
// одного человека, а сайт знает, что отвечают другому (так бывает, когда ник
// упомянут в тексте, а reply-кнопка нажата на чужой реплике).
func TestReplyTreeWinsOverBranch(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ягода"}, {ID: 3, Name: "Гость"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "реплика Ягоды", PublishedAt: at},
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 3, Text: "Хозяйка, а Ягода права", PublishedAt: at},
	}, at)

	// Без слоя — адресат «по ветке», хозяйка.
	if _, err := s.BuildAddressees(ctx, nil); err != nil {
		t.Fatal(err)
	}
	assertAddressee(t, s, 1002, 1, "branch")

	if _, err := s.SaveReplyTree(ctx, 100, map[int64]int64{1000: 0, 1001: 0, 1002: 1001}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BuildAddressees(ctx, nil); err != nil {
		t.Fatal(err)
	}
	assertAddressee(t, s, 1002, 2, "reply")

	// Граф пересобрался: ребро ушло от хозяйки к Ягоде.
	if got := replyEdge(t, s, 3, 2); got != 1 {
		t.Errorf("v_reply_edges 3→2 = %d, want 1", got)
	}
	if got := replyEdge(t, s, 3, 1); got != 0 {
		t.Errorf("v_reply_edges 3→1 = %d, want 0 (ребро должно было уйти)", got)
	}
}

// Обход идемпотентен и резюмируем: размеченная заметка больше не берётся,
// не отдавшаяся — берётся только с -retry, повторный проход не двоит пары.
func TestReplyScanTargetsAndIdempotency(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "А"}, {ID: 2, Name: "Б"}}
	// 100 — три комментария, 200 — два: порядок обхода «сначала многолюдные».
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "раз", PublishedAt: at},
		{ID: 1002, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "два", PublishedAt: at},
	}, at)
	saveThread(t, s, 200, 2000, 1, users, []Comment{
		{ID: 2001, NoteID: 200, ParentID: 2000, AuthorID: 2, Text: "раз", PublishedAt: at},
	}, at)

	targets, err := s.ReplyScanTargets(ctx, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0] != 100 {
		t.Fatalf("цели обхода: %v, ожидались [100 200]", targets)
	}

	if _, err := s.SaveReplyTree(ctx, 100, map[int64]int64{1000: 0, 1001: 0, 1002: 1001}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReplyScanFailed(ctx, 200); err != nil {
		t.Fatal(err)
	}

	if targets, err = s.ReplyScanTargets(ctx, 0, 0, false); err != nil {
		t.Fatal(err)
	} else if len(targets) != 0 {
		t.Errorf("после обхода целей быть не должно: %v", targets)
	}
	if targets, err = s.ReplyScanTargets(ctx, 0, 0, true); err != nil {
		t.Fatal(err)
	} else if len(targets) != 1 || targets[0] != 200 {
		t.Errorf("-retry должен вернуть только упавшую: %v", targets)
	}
	// Длинные треды отсекаются заранее — на них страница падает в 500.
	if targets, err = s.ReplyScanTargets(ctx, 0, 2, true); err != nil {
		t.Fatal(err)
	} else if len(targets) != 1 || targets[0] != 200 {
		t.Errorf("-max-comments 2: %v, ожидалась только заметка 200", targets)
	}

	// Повторный проход не двоит и не плодит записей.
	if _, err := s.SaveReplyTree(ctx, 100, map[int64]int64{1000: 0, 1001: 0, 1002: 1001}); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ReplyScanStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Notes != 2 || stats.ScannedOK != 1 || stats.Failed != 1 || stats.Pairs != 1 {
		t.Errorf("покрытие: %+v", stats)
	}
}

// Комментарии, появившиеся на сайте после выгрузки, внешним ключом бы уронили
// запись: они считаются отдельно как сигнал «заметку пора обновить».
func TestReplyTreeSkipsUnknownComments(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	at := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

	users := []User{{ID: 1, Name: "А"}, {ID: 2, Name: "Б"}}
	saveThread(t, s, 100, 1000, 1, users, []Comment{
		{ID: 1001, NoteID: 100, ParentID: 1000, AuthorID: 2, Text: "раз", PublishedAt: at},
	}, at)

	res, err := s.SaveReplyTree(ctx, 100, map[int64]int64{
		1000: 0, 1001: 0,
		1002: 1001, // свежий, в архиве его нет
	})
	if err != nil {
		t.Fatalf("свежие комментарии не должны валить запись: %v", err)
	}
	if res.Unknown != 1 || res.Stored != 0 {
		t.Errorf("итог: %+v, ожидалось unknown=1 stored=0", res)
	}
}
