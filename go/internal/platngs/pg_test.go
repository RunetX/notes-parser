package platngs

// Интеграционный тест выноса — против настоящего Postgres и настоящего SQLite.
//
// Подделывать здесь нечего ровно по той же причине, что в platsink: смысл
// пакета в том, ЧТО ИМЕННО доезжает до чужого сайта, а это складывается из
// очереди в Postgres, сессии в SQLite и правил ядра. Заглушка подтвердила бы
// сама себя.
//
// И главное: тест стоит НА ПУТИ ДАННЫХ, а не на формуле. Урок оплачен дважды —
// пол собеседника считался верно и не доезжал до промпта, обращение адресата
// считается верно и точно так же могло не доехать до сайта. Формуле здесь
// верить нельзя, надо смотреть, что легло в POST.
//
// Запуск: LOVEGW_TEST_PG_DSN=postgres://... go test ./internal/platngs/

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/store"
)

var shared *platform.Platform

func TestMain(m *testing.M) {
	dsn := os.Getenv("LOVEGW_TEST_PG_DSN")
	if dsn == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	p, err := platform.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Postgres: %v\n", err)
		os.Exit(1)
	}
	var db string
	if err := p.Pool().QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		fmt.Fprintf(os.Stderr, "имя базы: %v\n", err)
		os.Exit(1)
	}
	if !strings.Contains(db, "test") {
		fmt.Fprintf(os.Stderr, "отказ работать с базой %q: тесты сносят схему целиком\n", db)
		os.Exit(1)
	}
	shared = p
	code := m.Run()
	p.Close()
	os.Exit(code)
}

// fakeSite — чужой сайт. Записывает, ЧТО именно ему отдали: ради этого тест и
// заведён.
type fakeSite struct {
	noteID  string
	replyTo string
	text    string
	posts   int
	// hideMe — с какой анонимностью ушла заметка. Ради этого поля тест и
	// заведён: отправь мы анонимную заметку открытой, имя человека встало бы
	// под текстом, который он публиковал скрытно, и починить это было бы нечем.
	hideMe bool
	// page — что отдаёт страница треда после отправки (вычитывание номера).
	page []love.Comment
}

func (f *fakeSite) PostNote(_ context.Context, _ []*http.Cookie, text string, anonymous bool) error {
	f.text, f.hideMe = text, anonymous
	f.posts++
	return nil
}

func (f *fakeSite) PostComment(_ context.Context, _ []*http.Cookie, noteID, comAPIID, text string) error {
	f.noteID, f.replyTo, f.text = noteID, comAPIID, text
	f.posts++
	return nil
}

func (f *fakeSite) FetchCommentsPage(context.Context, string) (love.CommentsPage, error) {
	return love.CommentsPage{Comments: f.page}, nil
}

type env struct {
	p    *platform.Platform
	st   *store.Store
	site *fakeSite
	svc  *Service
}

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
	// Тексты согласий публикуются ВМЕСТЕ со схемой, одной командой админа: без
	// них любое согласие падает на внешнем ключе consents → consent_docs.
	if err := shared.EnsureConsentDocs(ctx, platform.Operator{}); err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "lovegw.db"))
	if err != nil {
		t.Fatalf("SQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	site := &fakeSite{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return env{p: shared, st: st, site: site, svc: New(shared, st, site, Config{}, quiet)}
}

// member — участник площадки в полосе НГС с живой сессией сайта и галочкой.
func member(t *testing.T, e env, id int64, nick string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := e.p.EnsureShadow(ctx, platform.MirroredAuthor{ID: id, Nick: nick}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	if _, err := e.p.CompleteBotLogin(ctx, id); err != nil {
		t.Fatalf("вход: %v", err)
	}
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	for _, d := range docs {
		if err := e.p.GrantConsent(ctx, id, d.Kind, d.Version, "тест"); err != nil {
			t.Fatalf("согласие %s: %v", d.Kind, err)
		}
	}
	if err := e.p.SetNGSSend(ctx, id, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	// Живая сессия сайта: без неё служба честно пропускает строку.
	if err := e.st.UpsertSession(ctx, store.MessengerTelegram, id,
		`[{"Name":"sid","Value":"x","Domain":"love.ngs.ru","Path":"/"}]`, time.Now()); err != nil {
		t.Fatalf("сессия: %v", err)
	}
	// id участника площадки РАВЕН номеру анкеты — на этом равенстве держится и
	// поиск сессии в бою.
	if err := e.st.SetSessionIdentity(ctx, store.MessengerTelegram, id,
		fmt.Sprint(id), "", nick); err != nil {
		t.Fatalf("анкета сессии: %v", err)
	}
	return id
}

func mirrored(t *testing.T, e env) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.p.IngestNote(ctx, platform.MirroredNote{
		ID: 312811, Author: platform.MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "зеркальная заметка", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if _, err := e.p.IngestComment(ctx, platform.MirroredComment{
		ID: 63238879, NoteID: 312811, Author: platform.MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "чужая реплика", PublishedAt: time.Now().Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("чужая реплика: %v", err)
	}
}

// ОБРАЩЕНИЕ ДОЕЗЖАЕТ ДО САЙТА. У нас адресат — ребро, а там он живёт префиксом
// «Ник, » в самом теле: ветка, указанная одним comApiId, кому реплика отвечает,
// не говорит. Проверяется именно то, что легло в POST.
func TestОбращениеУходитВТелеРеплики(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author := member(t, e, 1493279, "Рио")
	mirrored(t, e)
	if _, err := e.p.CreateComment(ctx, platform.NewComment{
		NoteID: 312811, AuthorID: author, ReplyToID: 63238879, Body: "мой ответ"}); err != nil {
		t.Fatalf("реплика: %v", err)
	}

	if err := e.svc.pass(ctx); err != nil {
		t.Fatalf("проход: %v", err)
	}
	if e.site.posts != 1 {
		t.Fatalf("отправок %d, ожидалась одна", e.site.posts)
	}
	if e.site.text != "ДВ, мой ответ" {
		t.Errorf("на сайт ушло %q — обращения в теле нет, и адресат там потерян", e.site.text)
	}
	if e.site.replyTo != "63238879" {
		t.Errorf("ветка указана как %q, ожидалось 63238879", e.site.replyTo)
	}
	if e.site.noteID != "312811" {
		t.Errorf("заметка указана как %q", e.site.noteID)
	}
}

// Ответ САМОЙ ЗАМЕТКЕ обращения не получает: «Ник, » назвал бы автора заметки,
// которому реплика не адресована.
func TestОтветЗаметкеУходитБезОбращения(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author := member(t, e, 1493279, "Рио")
	mirrored(t, e)
	if _, err := e.p.CreateComment(ctx, platform.NewComment{
		NoteID: 312811, AuthorID: author, Body: "в корень треда"}); err != nil {
		t.Fatalf("реплика: %v", err)
	}
	if err := e.svc.pass(ctx); err != nil {
		t.Fatalf("проход: %v", err)
	}
	if e.site.text != "в корень треда" || e.site.replyTo != "" {
		t.Errorf("на сайт ушло %q с веткой %q", e.site.text, e.site.replyTo)
	}
}

// ВОПРОС ВЛАДЕЛЬЦА 01.09.2026: ответ на реплику, которой на НГС нет вовсе.
// Такую не уносим — она отвечает собеседнику, которого на той стороне не видно,
// и читалась бы там бессмыслицей под именем живого человека.
func TestОтветНаНевидимуюРепликуНеУходит(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author := member(t, e, 1493279, "Рио")
	mirrored(t, e)
	parent, err := e.p.CreateComment(ctx, platform.NewComment{
		NoteID: 312811, AuthorID: author, Body: "своя реплика"})
	if err != nil {
		t.Fatal(err)
	}
	// Потолок частоты — реплика в десять секунд; сдвигаем время, а не спим.
	if _, err := e.p.Pool().Exec(ctx,
		`UPDATE comments SET published_at = published_at - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.CreateComment(ctx, platform.NewComment{
		NoteID: 312811, AuthorID: author, ReplyToID: parent, Body: "ответ себе"}); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.pass(ctx); err != nil {
		t.Fatalf("проход: %v", err)
	}
	st, err := e.p.NGSOutboxStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != 1 {
		t.Errorf("пропущено %d строк, ожидалась одна — ответ невидимому собеседнику", st.Skipped)
	}
	if e.site.posts != 1 {
		t.Errorf("отправок %d, ожидалась одна (только родитель)", e.site.posts)
	}
}

// НОМЕР СВОЕЙ РЕПЛИКИ вычитывается сразу после отправки: PostComment его не
// возвращает вовсе, а нужен он и ветке следующего ответа, и точному опознанию
// эха — сверка текстов уже подвела однажды, когда сайт подменил смайлик.
func TestНомерСвоейРепликиВычитываетсяСоСтраницы(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	author := member(t, e, 1493279, "Рио")
	mirrored(t, e)
	if _, err := e.p.CreateComment(ctx, platform.NewComment{
		NoteID: 312811, AuthorID: author, Body: "своя реплика"}); err != nil {
		t.Fatal(err)
	}
	// Чужая реплика с тем же текстом стоит выше нашей: брать «последнюю в треде»
	// нельзя — за строкой закрепился бы чужой номер, и погашено потом было бы не
	// то эхо.
	e.site.page = []love.Comment{
		{ID: 63300009, AuthorID: "498196", Text: "своя реплика"},
		{ID: 63300007, AuthorID: "1493279", Text: "своя реплика"},
		{ID: 63300005, AuthorID: "1493279", Text: "совсем другая"},
	}
	if err := e.svc.pass(ctx); err != nil {
		t.Fatalf("проход: %v", err)
	}
	var got string
	if err := e.p.Pool().QueryRow(ctx, `SELECT ngs_id FROM ngs_outbox`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "63300007" {
		t.Errorf("за строкой закреплён номер %q, ожидался 63300007", got)
	}
}

// АНОНИМНОСТЬ ДОЕЗЖАЕТ ДО САЙТА (02.09.2026, вопрос владельца «почему заметка
// не улетела из Зазеркалья на НГС»). До этого дня анонимную заметку не уносили
// вовсе, и довод стоял неверный — сайт принимает её сам, hideme=1.
//
// Тест стои́т НА ПУТИ ДАННЫХ и смотрит, ЧТО легло в POST, а не что посчиталось:
// урок оплачен дважды — пол собеседника считался верно и не доезжал до промпта,
// обращение считалось верно и точно так же могло не доехать до сайта.
func TestАнонимностьЗаметкиДоезжаетДоСайта(t *testing.T) {
	for _, c := range []struct {
		имя  string
		anon bool
	}{{"открытая", false}, {"анонимная", true}} {
		t.Run(c.имя, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			author := member(t, e, 1493279, "Рио")
			if _, err := e.p.CreateNote(ctx, platform.NewNote{
				AuthorID: author, Anonymous: c.anon, Body: "текст заметки"}); err != nil {
				t.Fatalf("заметка: %v", err)
			}
			if err := e.svc.pass(ctx); err != nil {
				t.Fatalf("проход: %v", err)
			}
			if e.site.posts != 1 {
				t.Fatalf("отправок %d, ожидалась одна", e.site.posts)
			}
			if e.site.hideMe != c.anon {
				t.Errorf("на сайт ушло с анонимностью %v, а заметка %s", e.site.hideMe, c.имя)
			}
		})
	}
}
