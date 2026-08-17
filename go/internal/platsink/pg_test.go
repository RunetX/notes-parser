package platsink

// Интеграционные тесты приёма — против настоящего Postgres и настоящего SQLite.
//
// Подделывать тут нечего: весь смысл пакета в том, что две базы сходятся, а
// сходятся они в SQL (идемпотентность по id, счётчики, дерево путей). Заглушка
// подтвердила бы только саму себя.
//
// Запуск: LOVEGW_TEST_PG_DSN=postgres://... go test ./internal/platsink/
// Без переменной эти тесты пропускаются, и `go test ./...` на машине без
// Postgres остаётся зелёным.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
	"lovegw/internal/store"
)

var shared *platform.Platform

func TestMain(m *testing.M) {
	dsn := os.Getenv("LOVEGW_TEST_PG_DSN")
	if dsn == "" {
		os.Exit(m.Run()) // без переменной интеграционные тесты пропускаются
	}
	ctx := context.Background()
	p, err := platform.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Postgres: %v\n", err)
		os.Exit(1)
	}
	// Тот же предохранитель, что и в пакете platform: тесты сносят схему
	// целиком, и боевая база не должна оказаться на другом конце DSN даже при
	// опечатке в переменной окружения.
	var db string
	if err := p.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
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

// env — обе базы под один тест: чистая схема в Postgres и своя SQLite во
// временном каталоге.
type env struct {
	st    *store.Store
	p     *platform.Platform
	sink  *Sink
	rec   *Reconciler
	media *platform.MediaStore
	dir   string
}

// quietLog — логгер в никуда: тесты проверяют состояние баз, а не вывод.
func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newEnv(t *testing.T) env {
	t.Helper()
	if shared == nil {
		t.Skip("нет LOVEGW_TEST_PG_DSN — интеграционные тесты пропущены")
	}
	ctx := t.Context()
	if _, err := shared.Pool().Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("очистка схемы: %v", err)
	}
	if err := shared.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "lovegw.db"))
	if err != nil {
		t.Fatalf("SQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	media, err := platform.NewMediaStore(shared, filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("хранилище медиа: %v", err)
	}
	quiet := quietLog()
	return env{
		st:    st,
		p:     shared,
		sink:  New(shared, media, quiet),
		rec:   NewReconciler(st, shared, quiet),
		media: media,
		dir:   filepath.Join(dir, "media"),
	}
}

var seenAt = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

func note(id, author, nick string) store.Note {
	return store.Note{
		ID: id, AuthorID: author, AuthorName: nick, Text: "заметка " + id,
		Status: store.StatusPosted, FirstSeenAt: seenAt,
	}
}

func comment(id int64, noteID, nick, profile, text string) store.Comment {
	c := store.Comment{
		ID: id, NoteID: noteID, AuthorName: nick, Text: text,
		PublishedAt: seenAt.Add(time.Duration(id) * time.Second),
		CreatedAt:   seenAt.Add(time.Duration(id) * time.Second),
	}
	if profile != "" {
		c.AuthorLink = "https://love.ngs.ru/profile/" + profile + "/"
	}
	return c
}

// Живой приём: зеркало отдаёт заметку и две реплики, вторая — с обращением.
// Проверяем ровно то, ради чего площадка вообще берёт поток: обращение стало
// ребром, тело от него очистилось, а подпись адресата дорисовалась из ника.
func TestSinkTakesLiveFlow(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	n := note("312811", "1495073", "Птичка")
	n.AuthorAvatarURL = "https://n1s1.hsmedia.ru/cache/love/avatars/abc_100_100_c.jpg"

	thread, err := e.sink.StartThread(ctx, n, "")
	if err != nil || thread != n.ID {
		t.Fatalf("тред площадки: %q, %v (ожидалась сама заметка)", thread, err)
	}
	if _, err := e.sink.PostNote(ctx, n, testPNG(t)); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}
	first := comment(63207290, n.ID, "Птичка", "1495073", "первый")
	if _, err := e.sink.PostComment(ctx, n, thread, "", first, nil); err != nil {
		t.Fatalf("приём первой реплики: %v", err)
	}
	second := comment(63207431, n.ID, "Мавр", "1331380", "Птичка, согласен")
	if _, err := e.sink.PostComment(ctx, n, thread, "63207290", second, nil); err != nil {
		t.Fatalf("приём второй реплики: %v", err)
	}

	got, _, err := e.p.Thread(ctx, platform.Viewer{}, 312811, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("в треде %d реплик, ожидалось 2", len(got))
	}
	reply := got[1]
	if reply.Depth != 2 {
		t.Errorf("ответ встал на глубину %d, ожидалась 2", reply.Depth)
	}
	if reply.Body != "согласен" {
		t.Errorf("обращение осталось в теле: %q", reply.Body)
	}
	if reply.ReplyTo == nil || reply.ReplyTo.CommentID != 63207290 || reply.ReplyTo.Nick != "Птичка" {
		t.Errorf("адресат: %+v", reply.ReplyTo)
	}

	// Аватар автора заметки лежит у нас, а не ссылкой на hsmedia.ru.
	author, err := e.p.UserByID(ctx, 1495073)
	if err != nil {
		t.Fatal(err)
	}
	if len(author.AvatarSHA) == 0 {
		t.Fatal("аватар автора не сохранён")
	}
	view, err := e.p.NoteViewByID(ctx, platform.Viewer{}, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(view.Author.AvatarURL, platform.MediaURLPrefix) {
		t.Errorf("ссылка на аватар ведёт мимо нашего хранилища: %q", view.Author.AvatarURL)
	}
	if _, err := os.Stat(filepath.Join(e.dir, filepath.FromSlash(
		strings.TrimPrefix(view.Author.AvatarURL, platform.MediaURLPrefix)))); err != nil {
		t.Errorf("файла аватара нет на диске: %v", err)
	}
}

// Силуэт по умолчанию в хранилище не попадает: «аватар есть у всех» — это фон,
// а не аватар, и рисовать его должна разметка площадки.
func TestSinkSkipsPlaceholderAvatar(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	n := note("312812", "1495074", "Ромашка")
	n.AuthorAvatarURL = "/static/i/new/profile/female300px.png"
	if _, err := e.sink.PostNote(ctx, n, testPNG(t)); err != nil {
		t.Fatal(err)
	}
	u, err := e.p.UserByID(ctx, 1495074)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.AvatarSHA) != 0 {
		t.Error("силуэт по умолчанию сохранён как аватар")
	}
}

// Первый проход сверки на пустой площадке — это и есть бэкфилл: переносится всё
// зеркало разом, вместе с деревом ответов, иллюстрациями и отметкой «закрыто».
func TestReconcileBackfillsMirror(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	seedNote(t, e.st, note("312811", "1495073", "Птичка"))
	seedNote(t, e.st, note("312812", "0", "Анонимно"))
	if err := e.st.InsertNoteImage(ctx, "312811", 0, "https://n1s1.hsmedia.ru/images/1.jpg"); err != nil {
		t.Fatal(err)
	}
	seedComment(t, e.st, comment(63207290, "312811", "Птичка", "1495073", "первый"))
	seedComment(t, e.st, comment(63207431, "312811", "Мавр", "1331380", "Птичка, согласен"))
	seedComment(t, e.st, comment(63207500, "312811", "Гость", "", "птичка, и я"))
	seedComment(t, e.st, comment(63207600, "312812", "Мавр", "1331380", "к анониму"))

	st, err := e.rec.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Notes != 2 || st.Comments != 4 || st.Images != 1 {
		t.Errorf("итог бэкфилла: %+v", st)
	}

	// Дерево: обе реплики с обращением встали под первой, включая ту, где ник
	// написан со строчной, — сравнение регистронезависимое.
	thread, _, err := e.p.Thread(ctx, platform.Viewer{}, 312811, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	depth := map[int64]int{}
	for _, c := range thread {
		depth[c.ID] = c.Depth
	}
	if depth[63207290] != 1 || depth[63207431] != 2 || depth[63207500] != 2 {
		t.Errorf("глубины реплик: %v", depth)
	}
	// Безанкетный комментатор показывается снимком ника: показать его иначе нечем.
	for _, c := range thread {
		if c.ID == 63207500 && c.Name() != "Гость" {
			t.Errorf("подпись безанкетного: %q", c.Name())
		}
	}

	anon, err := e.p.NoteViewByID(ctx, platform.Viewer{}, 312812)
	if err != nil {
		t.Fatal(err)
	}
	if !anon.Anonymous || anon.Author.ID != 0 {
		t.Errorf("аноним НГС разобран как %+v", anon)
	}

	// Второй проход не делает ничего: сверка идемпотентна, иначе она пересылала
	// бы всё зеркало каждые пять минут.
	again, err := e.rec.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Errorf("повторный проход что-то сделал: %+v", again)
	}
}

// Сайт помечает заметку «не актуальна» уже ПОСЛЕ публикации, а у приёмника
// зеркала события про это нет вовсе — значит перенести отметку может только
// сверка, и на уже принятой заметке.
func TestReconcileCarriesCommentsClosed(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	seedNote(t, e.st, note("312811", "1495073", "Птичка"))
	if _, err := e.rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.MarkNoteCommentsClosed(ctx, "312811"); err != nil {
		t.Fatal(err)
	}
	st, err := e.rec.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Closed != 1 {
		t.Errorf("перенос отметки «не актуальна»: %+v", st)
	}
	n, err := e.p.NoteViewByID(ctx, platform.Viewer{}, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if !n.CommentsClosed {
		t.Error("заметка осталась открытой для комментариев")
	}
}

// Сверка догоняет то, что живой приёмник пропустил (лежал Postgres), и не
// дублирует то, что он уже принял.
func TestReconcileCatchesUpAfterLiveSink(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	n := note("312811", "1495073", "Птичка")
	seedNote(t, e.st, n)
	if _, err := e.sink.PostNote(ctx, n, nil); err != nil {
		t.Fatal(err)
	}

	live := comment(63207290, n.ID, "Птичка", "1495073", "первый")
	seedComment(t, e.st, live)
	if _, err := e.sink.PostComment(ctx, n, n.ID, "", live, nil); err != nil {
		t.Fatal(err)
	}
	// А эту реплику живой приёмник не донёс — как если бы Postgres лежал.
	seedComment(t, e.st, comment(63207431, n.ID, "Мавр", "1331380", "Птичка, согласен"))

	st, err := e.rec.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Notes != 0 || st.Comments != 1 {
		t.Errorf("догон: %+v", st)
	}
	got, _, err := e.p.Flat(ctx, platform.Viewer{}, 312811, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("в заметке %d реплик, ожидалось 2", len(got))
	}
	row, err := e.p.NoteRow(ctx, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if row.CommentCount != 2 {
		t.Errorf("счётчик комментариев %d, ожидалось 2", row.CommentCount)
	}
}

// Реплика, дотянутая задним числом (`pull -full` добирает старый тред), имеет
// id МЕНЬШЕ уже принятых — сверка по одному лишь max её бы не заметила, поэтому
// счётчик у неё парный.
func TestReconcileSeesBackdatedComment(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	seedNote(t, e.st, note("312811", "1495073", "Птичка"))
	seedComment(t, e.st, comment(63207431, "312811", "Мавр", "1331380", "поздний"))
	if _, err := e.rec.Once(ctx); err != nil {
		t.Fatal(err)
	}

	seedComment(t, e.st, comment(63207290, "312811", "Птичка", "1495073", "ранний"))
	st, err := e.rec.Once(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Comments != 1 {
		t.Errorf("догон старой реплики: %+v", st)
	}
	got, _, err := e.p.Flat(ctx, platform.Viewer{}, 312811, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 63207290 {
		t.Errorf("плоский вид: %d реплик, первая %d", len(got), got[0].ID)
	}
}

// Иллюстрация не задваивается, даже если её привязали оба входа: ключ у
// note_images — ссылка, а не позиция (позиции они считают по-разному).
func TestNoteImageAttachedOnceFromBothSides(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	n := note("312811", "1495073", "Птичка")
	seedNote(t, e.st, n)
	url := "https://n1s1.hsmedia.ru/images/1.jpg"
	if err := e.st.InsertNoteImage(ctx, n.ID, 0, url); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	// Живой приёмник приносит ту же картинку, но уже с байтами.
	if _, err := e.sink.PostNoteImage(ctx, n.ID, url, testPNG(t)); err != nil {
		t.Fatal(err)
	}
	imgs, err := e.p.NoteImages(ctx, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 {
		t.Fatalf("иллюстраций %d, ожидалась одна", len(imgs))
	}
	if imgs[0].URL == "" {
		t.Error("байты пришли, а ссылка на наше хранилище не появилась")
	}
}

// Run уходит по отмене контекста, а не по таймеру и не по ошибке прохода:
// сверка живёт в общем errgroup демона, и её падение утащило бы за собой
// зеркало и ботов.
func TestReconcilerRunStopsOnContext(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.rec.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run вернул %v, ожидалась отмена контекста", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run не остановился по отмене контекста")
	}
}

func seedNote(t *testing.T, st *store.Store, n store.Note) {
	t.Helper()
	if _, err := st.InsertNote(t.Context(), n); err != nil {
		t.Fatalf("запись заметки %s: %v", n.ID, err)
	}
}

func seedComment(t *testing.T, st *store.Store, c store.Comment) {
	t.Helper()
	if _, err := st.InsertComment(t.Context(), c); err != nil {
		t.Fatalf("запись комментария %d: %v", c.ID, err)
	}
}

// testPNG — настоящая картинка: хранилище определяет тип по содержимому, а не
// по ссылке (геоблок отдаёт на запрос картинки HTML с кодом 200).
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
