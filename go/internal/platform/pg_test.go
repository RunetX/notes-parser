package platform

// Интеграционные тесты — против НАСТОЯЩЕГО Postgres, а не подделки.
//
// Иначе проверять было бы нечего: половина решений этого пакета живёт в SQL —
// маскирование анонима в SELECT, порядок по COLLATE "C", частичные индексы,
// ON CONFLICT. Заглушка подтвердила бы только то, что заглушка работает.
//
// Запуск: LOVEGW_TEST_PG_DSN=postgres://... go test ./internal/platform/
// Без переменной тесты пропускаются, поэтому `go test ./...` на машине без
// Postgres остаётся зелёным.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// shared — пул на весь набор. Открывать его в каждом тесте дорого: тесты
// ходят в Postgres по ssh-туннелю с рабочей машины, и рукопожатие там заметно
// дороже самих запросов.
var shared *Platform

func TestMain(m *testing.M) {
	dsn := os.Getenv("LOVEGW_TEST_PG_DSN")
	if dsn == "" {
		os.Exit(m.Run()) // без переменной интеграционные тесты пропускаются
	}
	ctx := context.Background()
	p, err := Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Postgres: %v\n", err)
		os.Exit(1)
	}
	// Предохранитель: тесты сносят схему целиком, поэтому боевая база не должна
	// оказаться на другом конце DSN даже при опечатке в переменной окружения.
	var db string
	if err := p.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		fmt.Fprintf(os.Stderr, "имя базы: %v\n", err)
		os.Exit(1)
	}
	if !strings.Contains(db, "test") {
		fmt.Fprintf(os.Stderr, "отказ работать с базой %q: тесты сносят схему целиком, "+
			"имя тестовой базы обязано содержать «test»\n", db)
		os.Exit(1)
	}
	shared = p
	code := m.Run()
	p.Close()
	os.Exit(code)
}

func testPlatform(t *testing.T) *Platform {
	t.Helper()
	if shared == nil {
		t.Skip("нет LOVEGW_TEST_PG_DSN — интеграционные тесты пропущены")
	}
	ctx := context.Background()
	if _, err := shared.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("очистка схемы: %v", err)
	}
	if err := shared.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	return shared
}

var testTime = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

func ingestNote(t *testing.T, p *Platform, id, author int64, nick string) {
	t.Helper()
	_, err := p.IngestNote(context.Background(), MirroredNote{
		ID:             id,
		Author:         MirroredAuthor{ID: author, Nick: nick},
		Body:           fmt.Sprintf("заметка %d", id),
		PublishedAt:    testTime.Add(time.Duration(id) * time.Minute),
		PublishedExact: true,
	})
	if err != nil {
		t.Fatalf("приём заметки %d: %v", id, err)
	}
}

func ingestComment(t *testing.T, p *Platform, id, noteID, author, replyTo int64) {
	t.Helper()
	_, err := p.IngestComment(context.Background(), MirroredComment{
		ID:          id,
		NoteID:      noteID,
		Author:      MirroredAuthor{ID: author, Nick: fmt.Sprintf("ник%d", author)},
		Body:        fmt.Sprintf("комментарий %d", id),
		ReplyToID:   replyTo,
		ReplySource: ReplyPrefix,
		PublishedAt: testTime.Add(time.Duration(id) * time.Second),
	})
	if err != nil {
		t.Fatalf("приём комментария %d: %v", id, err)
	}
}

// Повтор обхода ленты — не исключение, а норма: зеркало видит одни и те же
// комментарии каждый такт. Второй приём обязан быть пустой операцией, включая
// счётчик заметки.
func TestIngestIsIdempotent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	note := MirroredNote{ID: 312811, Author: MirroredAuthor{ID: 175869, Nick: "Гадёныш"},
		Body: "текст", PublishedAt: testTime, PublishedExact: true}
	first, err := p.IngestNote(ctx, note)
	if err != nil || !first {
		t.Fatalf("первый приём заметки: %v, новая=%v", err, first)
	}
	again, err := p.IngestNote(ctx, note)
	if err != nil || again {
		t.Fatalf("повторный приём заметки: %v, новая=%v", err, again)
	}

	c := MirroredComment{ID: 63207290, NoteID: 312811,
		Author: MirroredAuthor{ID: 1409563, Nick: "Клубника со льдом"},
		Body:   "реплика", PublishedAt: testTime}
	if inserted, err := p.IngestComment(ctx, c); err != nil || !inserted {
		t.Fatalf("первый приём комментария: %v, новый=%v", err, inserted)
	}
	if inserted, err := p.IngestComment(ctx, c); err != nil || inserted {
		t.Fatalf("повторный приём комментария: %v, новый=%v", err, inserted)
	}

	n, err := p.NoteRow(ctx, 312811)
	if err != nil {
		t.Fatalf("чтение заметки: %v", err)
	}
	if n.CommentCount != 1 {
		t.Fatalf("счётчик комментариев %d, ожидалась 1 — повтор обхода не должен его двигать", n.CommentCount)
	}
	if n.LastCommentAt == nil || !n.LastCommentAt.Equal(testTime) {
		t.Fatalf("время последнего комментария %v", n.LastCommentAt)
	}
}

// Главное свойство путей: порядок выдачи — это обход дерева, и берётся он
// индексом, а не сортировкой в памяти.
func TestThreadOrderIsTreeOrder(t *testing.T) {
	p := testPlatform(t)
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	//   100 ── 200 ── 300
	//      └── 201
	//   101
	ingestComment(t, p, 100, 312811, 1, 0)
	ingestComment(t, p, 101, 312811, 2, 0)
	ingestComment(t, p, 200, 312811, 2, 100)
	ingestComment(t, p, 201, 312811, 3, 100)
	ingestComment(t, p, 300, 312811, 1, 200)

	got, err := p.Thread(context.Background(), Viewer{}, 312811)
	if err != nil {
		t.Fatalf("тред: %v", err)
	}
	wantIDs := []int64{100, 200, 300, 201, 101}
	wantDepth := []int{1, 2, 3, 2, 1}
	if len(got) != len(wantIDs) {
		t.Fatalf("в треде %d комментариев, ожидалось %d", len(got), len(wantIDs))
	}
	for i := range wantIDs {
		if got[i].ID != wantIDs[i] || got[i].Depth != wantDepth[i] {
			t.Fatalf("позиция %d: id=%d depth=%d, ожидалось id=%d depth=%d",
				i, got[i].ID, got[i].Depth, wantIDs[i], wantDepth[i])
		}
	}

	// Линейный вид — тот же набор, но от НОВЫХ к старым, как на НГС.
	flat, err := p.Flat(context.Background(), Viewer{}, 312811, 0, 50)
	if err != nil {
		t.Fatalf("линейный вид: %v", err)
	}
	for i, want := range []int64{300, 201, 200, 101, 100} {
		if flat[i].ID != want {
			t.Fatalf("линейный вид, позиция %d: %d, ожидалось %d", i, flat[i].ID, want)
		}
	}
}

// Ветка глубже потолка схлопывается по раскладке, но адресат обязан уцелеть:
// путь — это вёрстка, ребро — факт.
func TestDeepReplyClampsPathButKeepsEdge(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")

	var prev int64
	for i := 0; i <= MaxDepth+2; i++ {
		id := int64(1000 + i)
		ingestComment(t, p, id, 312811, 1, prev)
		prev = id
	}
	last, err := p.CommentRow(ctx, prev)
	if err != nil {
		t.Fatalf("чтение последнего комментария: %v", err)
	}
	if int(last.Depth) != MaxDepth {
		t.Fatalf("глубина %d, ожидался потолок %d", last.Depth, MaxDepth)
	}
	if last.ReplyToID != prev-1 {
		t.Fatalf("адресат %d, ожидался %d — схлопывание раскладки не должно терять ребро", last.ReplyToID, prev-1)
	}
}

// Родителя снесла модерация — комментарий становится корневым, а не теряется.
func TestOrphanReplyBecomesRoot(t *testing.T) {
	p := testPlatform(t)
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 500, 312811, 1, 999999) // адресата в базе нет

	c, err := p.CommentRow(context.Background(), 500)
	if err != nil {
		t.Fatalf("чтение комментария: %v", err)
	}
	if c.Depth != 1 || c.Path != RootPath(500) {
		t.Fatalf("сирота получила путь %q глубины %d", c.Path, c.Depth)
	}
	if c.ReplyToID != 999999 {
		t.Fatalf("ребро потеряно: %d", c.ReplyToID)
	}
	// В показе адресата нет: подписать снесённого нечем.
	got, err := p.Thread(context.Background(), Viewer{}, 312811)
	if err != nil {
		t.Fatalf("тред: %v", err)
	}
	if got[0].ReplyTo != nil {
		t.Fatalf("адресат нарисован при отсутствующей строке: %+v", got[0].ReplyTo)
	}
}

// Граница показа: автор анонимной заметки не покидает базу, но своей её видит.
func TestAnonymousAuthorNeverLeavesTheDatabase(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, err := p.CreateNativeUser(ctx, "Ванилька")
	if err != nil {
		t.Fatalf("создание пользователя: %v", err)
	}
	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Anonymous: true, Body: "анонимно"})
	if err != nil {
		t.Fatalf("публикация: %v", err)
	}

	stranger, err := p.NoteViewByID(ctx, Viewer{UserID: 42}, id)
	if err != nil {
		t.Fatalf("чтение посторонним: %v", err)
	}
	if stranger.Author.ID != 0 || stranger.Author.Nick != "" {
		t.Fatalf("аноним раскрыт постороннему: %+v", stranger.Author)
	}
	if stranger.Own {
		t.Fatal("чужая заметка помечена своей")
	}

	own, err := p.NoteViewByID(ctx, Viewer{UserID: author}, id)
	if err != nil {
		t.Fatalf("чтение автором: %v", err)
	}
	if !own.Own {
		t.Fatal("автор не видит свою анонимку своей — а он должен мочь её удалить")
	}
	if own.Author.ID != 0 {
		t.Fatalf("вид анонимки несёт автора даже своему: %+v", own.Author)
	}

	// Модератор — тоже посторонний: забанить флудера можно, не зная, кто он.
	mod, err := p.NoteViewByID(ctx, Viewer{UserID: 43, Role: RoleModerator}, id)
	if err != nil {
		t.Fatalf("чтение модератором: %v", err)
	}
	if mod.Author.ID != 0 {
		t.Fatalf("аноним раскрыт модератору: %+v", mod.Author)
	}

	// А в базе автор есть — иначе ни удалить свою заметку, ни ответить за неё.
	row, err := p.NoteRow(ctx, id)
	if err != nil {
		t.Fatalf("сырая строка: %v", err)
	}
	if row.AuthorID != author {
		t.Fatalf("в базе автор %d, ожидался %d", row.AuthorID, author)
	}
}

// Лента листается ключом: без дублей и пропусков.
func TestFeedPagesCoverEverythingOnce(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		ingestNote(t, p, 1000+i, 175869, "Гадёныш")
	}
	total, err := p.CountNotes(ctx)
	if err != nil || total != 5 {
		t.Fatalf("счётчик ленты: %d, %v", total, err)
	}
	seen := map[int64]bool{}
	for page := 0; page < 3; page++ {
		got, err := p.Feed(ctx, Viewer{}, page*2, 2)
		if err != nil {
			t.Fatalf("лента: %v", err)
		}
		for _, n := range got {
			if seen[n.ID] {
				t.Fatalf("заметка %d показана дважды", n.ID)
			}
			seen[n.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("лента отдала %d заметок из 5", len(seen))
	}
}

// Ник в подписи и в обращении — ТЕКУЩИЙ. На этом держится и переименование, и
// обезличивание: ник, размазанный по чужим телам, убрать было бы нечем.
func TestRenameChangesAddressPrefixEverywhere(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 100, 312811, 1409563, 0)
	ingestComment(t, p, 200, 312811, 1431505, 100)

	if err := p.SetNick(ctx, 1409563, "Земляника"); err != nil {
		t.Fatalf("смена ника: %v", err)
	}
	got, err := p.Thread(ctx, Viewer{}, 312811)
	if err != nil {
		t.Fatalf("тред: %v", err)
	}
	reply := got[1]
	if reply.ReplyTo == nil || reply.ReplyTo.Nick != "Земляника" {
		t.Fatalf("обращение не обновилось: %+v", reply.ReplyTo)
	}
	if got[0].Author.Nick != "Земляника" {
		t.Fatalf("подпись не обновилась: %q", got[0].Author.Nick)
	}
}

// Зеркало обновляет ник тени, но не трогает ни вошедшего участника, ни
// обезличенного: иначе обход отменял бы то выбор человека, то исполненное
// требование субъекта.
func TestShadowNickRules(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1044551, Nick: "Линда"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1044551, Nick: "Кристина"}); err != nil {
		t.Fatalf("тень повторно: %v", err)
	}
	if u, _ := p.UserByID(ctx, 1044551); u.Nick != "Кристина" {
		t.Fatalf("ник тени не обновился: %q", u.Nick)
	}

	if err := p.Promote(ctx, 1044551); err != nil {
		t.Fatalf("перевод в участники: %v", err)
	}
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1044551, Nick: "снова Линда"}); err != nil {
		t.Fatalf("тень после входа: %v", err)
	}
	u, _ := p.UserByID(ctx, 1044551)
	if u.Nick != "Кристина" {
		t.Fatalf("зеркало переписало ник вошедшему: %q", u.Nick)
	}
	if u.Kind != KindMember {
		t.Fatalf("вид пользователя %d, ожидался участник", u.Kind)
	}

	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET kind = $2, nick = 'Удалённый участник', anonymized_at = now() WHERE id = $1`,
		1044551, KindShadow); err != nil {
		t.Fatalf("обезличивание: %v", err)
	}
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1044551, Nick: "Кристина"}); err != nil {
		t.Fatalf("тень после обезличивания: %v", err)
	}
	if u, _ := p.UserByID(ctx, 1044551); u.Nick != "Удалённый участник" {
		t.Fatalf("зеркало вернуло ник обезличенному: %q", u.Nick)
	}
}

// Чужая отметка НГС «не актуальна» разговор у нас не запирает. Замер 18.08.2026:
// она стоит у 177 заметок зеркала из 285, и в них 45 695 комментариев из 61 177,
// — гейт по ней открыл бы площадку в режиме чтения ровно там, где идёт разговор.
func TestNGSClosedMarkDoesNotLockOurThread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "текст"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if _, err := p.SetCommentsClosed(ctx, noteID, true); err != nil {
		t.Fatalf("отметка «не актуальна»: %v", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "первый"}); err != nil {
		t.Fatalf("чужая отметка запретила писать у нас: %v", err)
	}
}

func TestOurLockClosesThread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "текст"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if err := p.SetThreadLocked(ctx, noteID, true); err != nil {
		t.Fatalf("замок: %v", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "второй"}); !errors.Is(err, ErrThreadLocked) {
		t.Fatalf("в закрытую заметку приняли комментарий: %v", err)
	}

	// Скрытая заметка для пишущего просто отсутствует: показывать работу
	// модерации посторонним незачем.
	if _, err := p.pool.Exec(ctx, `UPDATE notes SET status = $2, locked = false WHERE id = $1`,
		noteID, StatusHiddenMod); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "третий"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("в скрытую заметку приняли комментарий: %v", err)
	}
}

// mustUser заводит участника, которому можно писать: тень писать не вправе, а
// CreateNativeUser сразу делает участника.
func mustUser(t *testing.T, p *Platform, nick string) int64 {
	t.Helper()
	id, err := p.CreateNativeUser(context.Background(), nick)
	if err != nil {
		t.Fatalf("пользователь %q: %v", nick, err)
	}
	return id
}

func TestMediaPutIsIdempotent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	data := testPNG(t, 8, 5)

	m, err := store.Put(ctx, data, "https://hsmedia.ru/avatar.jpg")
	if err != nil {
		t.Fatalf("приём медиа: %v", err)
	}
	if m.MIME != "image/png" {
		t.Fatalf("тип определён по ссылке, а не по содержимому: %s", m.MIME)
	}
	if m.Width != 8 || m.Height != 5 {
		t.Fatalf("размеры %dx%d, ожидалось 8x5", m.Width, m.Height)
	}
	if !strings.HasSuffix(m.URL, ".png") {
		t.Fatalf("ссылка без расширения — прокси отдаст октет-поток: %q", m.URL)
	}
	if !store.Has(m.SHA256, m.MIME) {
		t.Fatalf("файл не лёг на диск: %s", store.FilePath(m.SHA256, m.MIME))
	}
	if _, err := store.Put(ctx, data, "https://hsmedia.ru/other.jpg"); err != nil {
		t.Fatalf("повторный приём: %v", err)
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM media`).Scan(&n); err != nil {
		t.Fatalf("учёт медиа: %v", err)
	}
	if n != 1 {
		t.Fatalf("в учёте %d строк, ожидалась 1 — имя файла есть его содержимое", n)
	}
	if _, err := store.Put(ctx, []byte("<html>403 Forbidden</html>"), "https://hsmedia.ru/blocked.jpg"); err == nil {
		t.Fatal("страница геоблока принята как картинка")
	}
}

// Планы запросов — часть договора, а не деталь: молчаливый переезд ленты или
// треда на полный перебор не проваливает ни один тест на поведение, а на живой
// базе это отказ. Проверяем ровно тот SQL, который выполняется.
func TestQueryPlansUseIndexes(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	// Заливка идёт массовой вставкой, а не приёмом по одному: план запроса не
	// зависит от того, как строки появились, а тысячи приёмов через туннель —
	// это минуты ожидания вместо секунды.
	//
	// Комментарии РАЗМАЗАНЫ по заметкам намеренно. Первая попытка сложила все
	// 600 в одну заметку — и планировщик честно выбрал обход по первичному
	// ключу: когда условию note_id удовлетворяет вся таблица, отдельный индекс
	// действительно не нужен. Такая заготовка проверяла бы не запрос, а саму
	// себя; на живой базе у заметки доли процента строк.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO users (id, nick)
		SELECT 1000000 + g, 'ник' || g FROM generate_series(1, 200) g;
		INSERT INTO notes (id, author_id, body, published_at)
		SELECT 200000 + g, 1000000 + (g % 200) + 1, 'заметка ' || g,
		       timestamptz '2026-08-17 12:00Z' + g * interval '1 minute'
		  FROM generate_series(1, 400) g;
		INSERT INTO comments (id, note_id, author_id, body, path, depth, published_at)
		SELECT 500000 + g, 200000 + (g % 400) + 1, 1000000 + (g % 200) + 1, 'комментарий ' || g,
		       lpad((500000 + g)::text, 13, '0'), 1,
		       timestamptz '2026-08-17 12:00Z' + g * interval '1 second'
		  FROM generate_series(1, 5000) g;
		ANALYZE notes, comments, users, media`); err != nil {
		t.Fatalf("заливка: %v", err)
	}

	cases := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{"лента", feedQuery, []any{int64(0), 20, 0}, "notes_feed"},
		{"тред", threadQuery, []any{int64(0), int64(200001), 100}, "comments_tree"},
		{"линейный вид", flatQuery, []any{int64(0), int64(200001), 30, 0}, "comments_flat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, p, c.query, c.args...)
			if !strings.Contains(plan, c.index) {
				t.Fatalf("план не берёт индекс %s:\n%s", c.index, plan)
			}
			if strings.Contains(plan, "Seq Scan on notes") || strings.Contains(plan, "Seq Scan on comments") {
				t.Fatalf("полный перебор в плане:\n%s", plan)
			}
		})
	}
}

func explain(t *testing.T, p *Platform, query string, args ...any) string {
	t.Helper()
	rows, err := p.pool.Query(context.Background(), "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN: %v", err)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	return b.String()
}
