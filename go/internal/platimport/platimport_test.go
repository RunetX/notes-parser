package platimport

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// fixture — крошечный архив на диске. Настоящий SQLite, а не подделка: разбор
// времени, LEFT JOIN на рёбра и порядок по id — это ровно то, что проверяется.
func fixture(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '',
		    age TEXT NOT NULL DEFAULT '', profile_url TEXT NOT NULL DEFAULT '',
		    avatar_url TEXT NOT NULL DEFAULT '', first_seen TEXT NOT NULL DEFAULT '',
		    last_seen TEXT NOT NULL DEFAULT '', gender TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, author_id INTEGER, text TEXT NOT NULL DEFAULT '',
		    images TEXT NOT NULL DEFAULT '[]', comments_closed INTEGER NOT NULL DEFAULT 0,
		    published_at TEXT, grabbed_at TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE comments (id INTEGER PRIMARY KEY, note_id INTEGER NOT NULL,
		    parent_id INTEGER NOT NULL DEFAULT 0, author_id INTEGER NOT NULL,
		    text TEXT NOT NULL DEFAULT '', published_at TEXT)`,
		`CREATE TABLE comment_reply (comment_id INTEGER PRIMARY KEY, reply_to INTEGER NOT NULL)`,
		`CREATE TABLE comment_addressee (comment_id INTEGER PRIMARY KEY,
		    addressee_id INTEGER NOT NULL, method TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE import_provenance (kind TEXT NOT NULL, row_id INTEGER NOT NULL,
		    source TEXT NOT NULL, nick TEXT, time_kind TEXT, PRIMARY KEY (kind, row_id, source))`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// thread прогоняет readThread по заметке фикстуры.
func thread(t *testing.T, db *sql.DB, noteID int64) ([][]any, Stats) {
	t.Helper()
	ctx := context.Background()
	a, err := readArchiveIndex(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx, `
		SELECT c.id, c.author_id, c.text, c.published_at, c.parent_id,
		       coalesce(r.reply_to, 0), coalesce(ad.addressee_id, 0)
		  FROM comments c
		  LEFT JOIN comment_reply     r  ON r.comment_id  = c.id
		  LEFT JOIN comment_addressee ad ON ad.comment_id = c.id
		 WHERE c.note_id = ?
		 ORDER BY c.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	var st Stats
	rows, err := readThread(ctx, stmt, &noteRow{id: noteID}, a, &st)
	if err != nil {
		t.Fatal(err)
	}
	return rows, st
}

// Колонки строки COPY комментария — в порядке writeChunk.
const (
	colID = iota
	colNote
	colAuthor
	colDisplay
	colBody
	colBranch
	colReplyTo
	colSource
	colPath
	colDepth
	colPublished
)

// Мобильное дерево сильнее обращения, обращение сильнее parent_id: источники
// упорядочены по измеренной достоверности, и порядок этот — суть переноса.
func TestEdgeOfPrefersMobileTree(t *testing.T) {
	known := map[int64]bool{10: true, 20: true, 30: true}
	last := map[int64]int64{7: 20}

	if got, src := edgeOf(40, 10, 7, 30, known, last); got != 10 || src != platform.ReplyMobileTree {
		t.Errorf("дерево должно побеждать: получено %d/%d", got, src)
	}
	if got, src := edgeOf(40, 0, 7, 30, known, last); got != 20 || src != platform.ReplyPrefix {
		t.Errorf("без дерева работает обращение: получено %d/%d", got, src)
	}
	if got, src := edgeOf(40, 0, 0, 30, known, last); got != 30 || src != platform.ReplyDesktopParent {
		t.Errorf("без обращения остаётся ветка: получено %d/%d", got, src)
	}
	if got, src := edgeOf(40, 0, 0, 0, known, last); got != 0 || src != platform.ReplyNone {
		t.Errorf("корень ветки: получено %d/%d", got, src)
	}
	// Ребро в будущее — испорченные данные, а не ответ: путь родителя к этому
	// моменту ещё не посчитан, и дерево вышло бы висячим.
	if got, _ := edgeOf(10, 20, 0, 0, known, last); got != 0 {
		t.Errorf("ссылка вперёд должна отбрасываться, получено %d", got)
	}
	// Адресат, ещё не писавший в этой заметке, ребром не становится.
	if got, _ := edgeOf(40, 0, 99, 0, known, last); got != 0 {
		t.Errorf("неписавший адресат не ребро, получено %d", got)
	}
}

// Ветка выстраивается путями, а обращение уходит из тела в ребро: показ
// дорисует «Ник, » сам, и оставленный в теле префикс дал бы «Ник, Ник, …».
func TestReadThreadBuildsPathsAndTrimsAddress(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES (1, 'Аня'), (2, 'Боря'), (3, 'Витя')`)
	exec(t, db, `INSERT INTO notes (id, author_id, published_at) VALUES (500, 1, '2015-03-01T10:00:00Z')`)
	exec(t, db, `INSERT INTO comments (id, note_id, parent_id, author_id, text, published_at) VALUES
		(10, 500, 0, 1, 'первая мысль',            '2015-03-01T10:01:00Z'),
		(20, 500, 0, 2, 'Аня, согласен',           '2015-03-01T10:02:00Z'),
		(30, 500, 10, 3, 'Боря, а вот и нет',      '2015-03-01T10:03:00Z'),
		(40, 500, 0, 1, 'Кстати, я передумала',    '2015-03-01T10:04:00Z')`)
	// У 20 адресат известен только по обращению, у 30 — из мобильного дерева.
	exec(t, db, `INSERT INTO comment_addressee (comment_id, addressee_id, method) VALUES (20, 1, 'prefix')`)
	exec(t, db, `INSERT INTO comment_reply (comment_id, reply_to) VALUES (30, 20)`)

	rows, st := thread(t, db, 500)
	if len(rows) != 4 {
		t.Fatalf("комментариев %d, ожидалось 4", len(rows))
	}
	byID := map[int64][]any{}
	for _, r := range rows {
		byID[r[colID].(int64)] = r
	}

	if got := byID[10][colPath].(string); got != platform.RootPath(10) {
		t.Errorf("корневой путь %q", got)
	}
	if got, want := byID[20][colPath].(string), platform.RootPath(10)+"."+platform.RootPath(20); got != want {
		t.Errorf("путь ответа %q, ожидался %q", got, want)
	}
	if got, want := byID[30][colPath].(string), byID[20][colPath].(string)+"."+platform.RootPath(30); got != want {
		t.Errorf("путь внука %q, ожидался %q", got, want)
	}
	if got := byID[30][colDepth].(int16); got != 3 {
		t.Errorf("глубина внука %d, ожидалась 3", got)
	}
	if got := byID[30][colBranch]; got != int64(10) {
		t.Errorf("корень ветки внука %v, ожидался 10", got)
	}
	if got := byID[10][colBranch]; got != nil {
		t.Errorf("у корня ветки нет ссылки на себя, получено %v", got)
	}

	// Обращение срезано у обоих, где нашлось ребро…
	if got := byID[20][colBody].(string); got != "согласен" {
		t.Errorf("тело 20 %q, обращение не снято", got)
	}
	if got := byID[30][colBody].(string); got != "а вот и нет" {
		t.Errorf("тело 30 %q, обращение не снято", got)
	}
	// …и НЕ съедено там, где «Кстати,» — это начало фразы, а не ник.
	if got := byID[40][colBody].(string); got != "Кстати, я передумала" {
		t.Errorf("тело 40 %q: срезано начало фразы", got)
	}
	if st.Trimmed != 2 {
		t.Errorf("снято обращений %d, ожидалось 2", st.Trimmed)
	}
	if st.EdgeTree != 1 || st.EdgeAddr != 1 || st.EdgeNone != 2 {
		t.Errorf("рёбра: дерево %d, обращение %d, нет %d", st.EdgeTree, st.EdgeAddr, st.EdgeNone)
	}
	if got := byID[30][colSource].(int16); platform.ReplySource(got) != platform.ReplyMobileTree {
		t.Errorf("источник ребра 30 = %d", got)
	}
}

// Ветка глубже потолка схлопывается по раскладке, но ребро адресата остаётся
// настоящим: путь — это вёрстка, адресат — факт.
func TestReadThreadClampsDepth(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES (1, 'Аня')`)
	exec(t, db, `INSERT INTO notes (id, author_id, published_at) VALUES (600, 1, '2015-03-01T10:00:00Z')`)
	for i := int64(1); i <= 20; i++ {
		exec(t, db, `INSERT INTO comments (id, note_id, parent_id, author_id, text, published_at)
			VALUES (?, 600, 0, 1, 'реплика', '2015-03-01T10:00:00Z')`, i)
		if i > 1 {
			exec(t, db, `INSERT INTO comment_reply (comment_id, reply_to) VALUES (?, ?)`, i, i-1)
		}
	}
	rows, _ := thread(t, db, 600)
	for _, r := range rows {
		if d := r[colDepth].(int16); d > int16(platform.MaxDepth) {
			t.Fatalf("глубина %d больше потолка %d", d, platform.MaxDepth)
		}
	}
	last := rows[len(rows)-1]
	if last[colReplyTo] != int64(19) {
		t.Errorf("ребро схлопнутой ветки %v, ожидалось 19", last[colReplyTo])
	}
}

// Строки без настоящего id сайта (дамп theloser.ru 2010) не переносятся:
// полоса идентификаторов таких не принимает, а придумывать им ключи — значит
// ломать обещание «id строки равен id на сайте».
func TestReadThreadSkipsSyntheticIDs(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES (1, 'Аня')`)
	exec(t, db, `INSERT INTO notes (id, author_id, published_at) VALUES (700, 1, '2010-03-01T10:00:00Z')`)
	exec(t, db, `INSERT INTO comments (id, note_id, parent_id, author_id, text, published_at) VALUES
		(-150871000, 700, 0, 1, 'из чужого дампа', '2010-03-01T10:01:00Z'),
		(800,        700, 0, 1, 'настоящая',       '2010-03-01T10:02:00Z')`)

	rows, st := thread(t, db, 700)
	if len(rows) != 1 || rows[0][colID] != int64(800) {
		t.Fatalf("перенесено %d строк, ожидалась одна настоящая", len(rows))
	}
	if st.SkipComments != 1 {
		t.Errorf("пропущено %d, ожидалась 1", st.SkipComments)
	}
}

// Заметка, чьё время восстановлено при импорте чужого дампа, едет с
// published_exact = false — колонка ровно про это.
func TestReadNoteMarksInexactTime(t *testing.T) {
	db := fixture(t)
	exec(t, db, `INSERT INTO users (id, name) VALUES (1, 'Аня')`)
	exec(t, db, `INSERT INTO notes (id, author_id, text, images, published_at) VALUES
		(150747, 1, 'старая', '["https://n1s1.hsmedia.ru/a.jpg"]', '2010-03-01T10:00:00Z'),
		(300000, 1, 'свежая', '[]',                                '2020-03-01T10:00:00Z'),
		(300001, NULL, 'аноним', '[]',                             '2020-03-02T10:00:00Z')`)
	exec(t, db, `INSERT INTO import_provenance (kind, row_id, source, time_kind)
		VALUES ('note', 150747, 'theloser2010', 'shifted6h')`)

	ctx := context.Background()
	a, err := readArchiveIndex(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := db.PrepareContext(ctx,
		`SELECT author_id, text, images, comments_closed, published_at FROM notes WHERE id = ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	old, err := readNote(ctx, stmt, 150747, a)
	if err != nil {
		t.Fatal(err)
	}
	if old.exact {
		t.Error("восстановленное время должно ехать как приблизительное")
	}
	if len(old.images) != 1 {
		t.Errorf("иллюстраций %d, ожидалась 1", len(old.images))
	}
	fresh, err := readNote(ctx, stmt, 300000, a)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.exact || fresh.anonymous || fresh.author != 1 {
		t.Errorf("снятая с сайта заметка: exact=%v anon=%v author=%d", fresh.exact, fresh.anonymous, fresh.author)
	}
	anon, err := readNote(ctx, stmt, 300001, a)
	if err != nil {
		t.Fatal(err)
	}
	if !anon.anonymous || anon.author != 0 {
		t.Errorf("аноним: anon=%v author=%d", anon.anonymous, anon.author)
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2015, 3, 1, 10, 0, 0, 0, time.UTC)
	for _, s := range []string{"2015-03-01T10:00:00Z", "2015-03-01T10:00:00", "2015-03-01 10:00:00"} {
		got, err := parseTime(s)
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if !got.Equal(want) {
			t.Errorf("%q → %s, ожидалось %s", s, got, want)
		}
	}
	if _, err := parseTime(""); err == nil {
		t.Error("пустое время обязано быть ошибкой: заметка без даты не встанет в ленту")
	}
}

func TestGenderOf(t *testing.T) {
	cases := map[string]platform.Gender{
		"male": platform.GenderMale, "female": platform.GenderFemale,
		"": platform.GenderUnknown, "мужской": platform.GenderUnknown,
	}
	for in, want := range cases {
		if got := genderOf(in); got != want {
			t.Errorf("genderOf(%q) = %d, ожидалось %d", in, got, want)
		}
	}
}
