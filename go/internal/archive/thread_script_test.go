package archive

import (
	"context"
	"testing"
	"time"
)

// scriptFixture — тред из четырёх реплик со всеми четырьмя источниками ребра.
//
//	t0   заметка (автор 1 «Хозяйка»)
//	+1м  2000 автор 2: ответ заметке                     → root
//	+2м  2001 автор 3: «Ягода, …», настоящее ребро       → tree (на 2000)
//	+3м  2002 автор 2: ветка автора 3, обращения нет     → parent (на 2001)
//	+4м  2003 автор 4: «Гость, …» в ЧУЖОЙ ветке          → addressee (на 2001)
//
// Последняя строка и есть проверка порядка источников: лежит она в ветке 2000,
// то есть parent увёл бы её к анкете 2, а обращение — к анкете 3, и права здесь
// вторая.
func scriptFixture(t *testing.T) (*Store, time.Time) {
	t.Helper()
	s := newTestArchive(t)
	ctx := context.Background()
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Ягода"},
		{ID: 3, Name: "Гость"}, {ID: 4, Name: "Прохожий"}}
	note := Note{ID: 500, AuthorID: 1, Text: "заметка", PublishedAt: t0}
	comments := []Comment{
		{ID: 2000, NoteID: 500, ParentID: 0, AuthorID: 2, Text: "просто мысль", PublishedAt: at(1)},
		{ID: 2001, NoteID: 500, ParentID: 2000, AuthorID: 3, Text: "Ягода, согласен", PublishedAt: at(2)},
		{ID: 2002, NoteID: 500, ParentID: 2001, AuthorID: 2, Text: "и не говори", PublishedAt: at(3)},
		{ID: 2003, NoteID: 500, ParentID: 2000, AuthorID: 4, Text: "Гость, а вот и нет", PublishedAt: at(4)},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, t0); err != nil {
		t.Fatalf("SaveGrab: %v", err)
	}
	// Настоящее ребро знает только мобильное дерево — кладём его отдельно, как
	// это делает reply-scan.
	if _, err := s.SaveReplyTree(ctx, 500, map[int64]int64{2001: 2000}); err != nil {
		t.Fatalf("SaveReplyTree: %v", err)
	}
	if _, err := s.BuildAddressees(ctx, nil); err != nil {
		t.Fatalf("BuildAddressees: %v", err)
	}
	return s, t0
}

func TestLoadThreadScriptEdges(t *testing.T) {
	s, t0 := scriptFixture(t)
	sc, err := s.LoadThreadScript(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Comments) != 4 {
		t.Fatalf("реплик %d, ожидалось 4", len(sc.Comments))
	}
	if !sc.Note.PublishedAt.Equal(t0) {
		t.Errorf("время заметки %v", sc.Note.PublishedAt)
	}

	byID := map[int64]ScriptComment{}
	for _, c := range sc.Comments {
		byID[c.ID] = c
	}
	cases := []struct {
		id       int64
		wantTo   int64
		wantEdge string
		wantWho  int64
	}{
		{2000, 0, EdgeRoot, 1},         // заметке — адресат её автор
		{2001, 2000, EdgeTree, 2},      // настоящее ребро сильнее parent_id
		{2002, 2001, EdgeParent, 3},    // обращения нет, остаётся корень ветки
		{2003, 2001, EdgeAddressee, 3}, // обращение сильнее корня ветки
	}
	for _, tc := range cases {
		got := byID[tc.id]
		if got.Edge != tc.wantEdge || got.TargetID != tc.wantWho {
			t.Errorf("реплика %d: ребро %s → анкета %d; ожидалось %s → %d",
				tc.id, got.Edge, got.TargetID, tc.wantEdge, tc.wantWho)
		}
		if got.ReplyTo != tc.wantTo {
			t.Errorf("реплика %d: отвечает %d, ожидалось %d", tc.id, got.ReplyTo, tc.wantTo)
		}
	}
	// Источник ребра — часть контракта: по угаданному нельзя судить, верно ли
	// выбрал адресата житель, и отчёт обязан показывать эту долю.
	if sc.Edges[EdgeTree] != 1 || sc.Edges[EdgeParent] != 1 ||
		sc.Edges[EdgeAddressee] != 1 || sc.Edges[EdgeRoot] != 1 {
		t.Errorf("разбивка по источникам: %v", sc.Edges)
	}
}

// Обращение уезжает в своё поле, а тело остаётся чистым: ник — это ребро, и
// размазанный по телам он потом не переименовывается (правило площадки).
func TestLoadThreadScriptTrimsAddress(t *testing.T) {
	s, _ := scriptFixture(t)
	sc, err := s.LoadThreadScript(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sc.Comments {
		switch c.ID {
		case 2001:
			if c.Text != "согласен" || c.Address == "" {
				t.Errorf("2001: тело %q, обращение %q", c.Text, c.Address)
			}
		case 2000:
			if c.Address != "" {
				t.Errorf("2000: обращения не было, а поле %q", c.Address)
			}
		}
	}
}

// Задержка считается от РЕПЛИКИ АДРЕСАТА, а у корневых — от заметки. Это та же
// величина, что меряет MineLatency: разойдясь, сценарий и карточка мерили бы
// разное и сравнение стало бы бессмысленным.
func TestLoadThreadScriptDelay(t *testing.T) {
	s, _ := scriptFixture(t)
	sc, err := s.LoadThreadScript(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]time.Duration{
		2000: time.Minute,     // от заметки
		2001: time.Minute,     // от 2000
		2002: time.Minute,     // от 2001
		2003: 2 * time.Minute, // от 2001 — последней реплики Гостя до этой минуты
	}
	for _, c := range sc.Comments {
		if c.Delay != want[c.ID] {
			t.Errorf("реплика %d: задержка %v, ожидалось %v", c.ID, c.Delay, want[c.ID])
		}
	}
}

// Ребро, указывающее ВПЕРЁД, не принимается ни от какого источника: ответ на
// ещё не сказанное — признак, что ребро приехало не из этого разговора.
func TestLoadThreadScriptRejectsForwardEdge(t *testing.T) {
	s, _ := scriptFixture(t)
	ctx := context.Background()
	// 2000 «отвечает» более поздней 2003.
	if _, err := s.SaveReplyTree(ctx, 500, map[int64]int64{2000: 2003}); err != nil {
		t.Fatal(err)
	}
	sc, err := s.LoadThreadScript(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sc.Comments {
		if c.ID == 2000 && c.Edge != EdgeRoot {
			t.Errorf("ребро вперёд принято: %s → %d", c.Edge, c.ReplyTo)
		}
	}
}

// Порядок — по ВРЕМЕНИ, а не по id. На сайте они совпадают, но архив хранит и
// восстановленную эпоху с чужой нумерацией, а часы реплея идут по времени.
func TestLoadThreadScriptOrdersByTime(t *testing.T) {
	s := newTestArchive(t)
	ctx := context.Background()
	t0 := time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)
	users := []User{{ID: 1, Name: "Хозяйка"}, {ID: 2, Name: "Гость"}}
	note := Note{ID: 600, AuthorID: 1, Text: "заметка", PublishedAt: t0}
	// У 3001 номер больше, а сказано раньше.
	comments := []Comment{
		{ID: 3002, NoteID: 600, AuthorID: 2, Text: "второй", PublishedAt: t0.Add(2 * time.Minute)},
		{ID: 3001, NoteID: 600, AuthorID: 2, Text: "первый", PublishedAt: t0.Add(time.Minute)},
	}
	if _, err := s.SaveGrab(ctx, note, comments, users, t0); err != nil {
		t.Fatal(err)
	}
	sc, err := s.LoadThreadScript(ctx, 600)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Comments[0].Text != "первый" || sc.Comments[1].Text != "второй" {
		t.Errorf("порядок по id, а не по времени: %q, %q", sc.Comments[0].Text, sc.Comments[1].Text)
	}
}

// Реплика без времени в сценарий не попадает, но считается: часы дискретные, и
// поставить её некуда, а молча потерять — значит сравнивать эмуляцию с
// подрезанным оригиналом.
func TestLoadThreadScriptCountsUndated(t *testing.T) {
	s, _ := scriptFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE comments SET published_at = NULL WHERE id = 2002`); err != nil {
		t.Fatal(err)
	}
	sc, err := s.LoadThreadScript(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Undated != 1 || len(sc.Comments) != 3 {
		t.Errorf("без времени %d, реплик %d; ожидалось 1 и 3", sc.Undated, len(sc.Comments))
	}
}

// Заметка без времени — отказ, а не сценарий с выдуманным нулём.
func TestLoadThreadScriptNeedsNoteTime(t *testing.T) {
	s, _ := scriptFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `UPDATE notes SET published_at = NULL WHERE id = 500`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadThreadScript(ctx, 500); err == nil {
		t.Fatal("заметка без времени принята")
	}
}

// Ник берётся ТОТ, которым человека звали тогда. Проверяется на расхождении:
// в users.name лежит сегодняшнее имя, а в треде человека звали иначе.
func TestLoadThreadScriptUsesNickOfTheDay(t *testing.T) {
	s, _ := scriptFixture(t)
	ctx := context.Background()
	// Анкета 2 с тех пор переименовалась.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET name = 'СовсемДругая' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	sc, err := s.LoadThreadScript(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range sc.Participants {
		if a.UserID == 2 && a.Nick != "Ягода" {
			t.Errorf("ник анкеты 2 в сценарии %q — взят сегодняшний вместо тогдашнего", a.Nick)
		}
	}
}

// Регистр обращения сохраняется. love.AddressPrefix его намеренно теряет — там
// ник это ключ поиска, — а здесь он ИМЯ, которое увидит модель: «ягода» вместо
// «Ягода» она приняла бы за манеру писать людей со строчной.
func TestRawAddressKeepsCase(t *testing.T) {
	cases := []struct{ orig, trimmed, want string }{
		{"Ягода, согласен", "согласен", "Ягода"},
		{"СВОЛОЧЪ, ну ты дал", "ну ты дал", "СВОЛОЧЪ"},
		{"Полынь-Трава,  и вот", "и вот", "Полынь-Трава"},
		{"просто мысль", "просто мысль", ""}, // обращения не было
		{"Ягода,", "Ягода,", ""},             // остатка нет — тело не режется
		{"Ник,\nс новой строки", "с новой строки", "Ник"},
	}
	for _, tc := range cases {
		if got := rawAddress(tc.orig, tc.trimmed); got != tc.want {
			t.Errorf("rawAddress(%q, %q) = %q, ожидалось %q", tc.orig, tc.trimmed, got, tc.want)
		}
	}
}

func TestParseArchiveTime(t *testing.T) {
	for _, s := range []string{"2016-05-12T09:00:00Z", "2016-05-12T09:00:00", "2016-05-12 09:00:00"} {
		got, err := parseArchiveTime(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if !got.Equal(time.Date(2016, 5, 12, 9, 0, 0, 0, time.UTC)) {
			t.Errorf("%q → %v", s, got)
		}
	}
	if _, err := parseArchiveTime("вчера"); err == nil {
		t.Error("мусор принят за время")
	}
}
