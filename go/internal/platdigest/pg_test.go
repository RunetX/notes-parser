package platdigest

// Интеграционные тесты источника — против настоящего Postgres.
//
// Подделывать здесь нечего: весь пакет и есть SQL, и проверять надо ровно то,
// что в нём решено, — граница показа (скрытое не считается), маскирование
// анонима, «прошлое человека» через LATERAL и планы запросов, без которых
// недельное окно превращается в чтение таблицы на 3 ГБ.
//
// Запуск: LOVEGW_TEST_PG_DSN=postgres://... go test ./internal/platdigest/
// Без переменной тесты пропускаются, и `go test ./...` на машине без Postgres
// остаётся зелёным.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"lovegw/internal/digest"
	"lovegw/internal/platform"
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
	// Тот же предохранитель, что в platform и platsink: тесты сносят схему
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

const ngsBase = "https://love.ngs.ru"

// Окно недели теста: слот в среду, чтобы «до окна» и «после» были явно разными
// датами.
var (
	slot    = time.Date(2026, 8, 22, 2, 0, 0, 0, time.UTC)
	window  = digest.Window{Start: slot.AddDate(0, 0, -7), End: slot, ID: "2026-W34"}
	longAgo = slot.AddDate(0, 0, -300) // старше returneeGap и внутри горизонта
)

func newSource(t *testing.T) (*Source, *platform.Platform) {
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
	// Тексты согласий публикуются вместе со схемой — так их накатывает
	// администратор одной командой `platform migrate`, и без них согласие
	// участника падает на внешнем ключе consents → consent_docs.
	if err := shared.EnsureConsentDocs(ctx, platform.Operator{}); err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	return New(shared, ngsBase), shared
}

func ingestNote(t *testing.T, p *platform.Platform, id, author int64, nick string, at time.Time) {
	t.Helper()
	_, err := p.IngestNote(t.Context(), platform.MirroredNote{
		ID:             id,
		Author:         platform.MirroredAuthor{ID: author, Nick: nick},
		Body:           fmt.Sprintf("заметка %d", id),
		PublishedAt:    at,
		PublishedExact: true,
	})
	if err != nil {
		t.Fatalf("приём заметки %d: %v", id, err)
	}
}

func ingestComment(t *testing.T, p *platform.Platform, id, noteID, author int64, nick string, at time.Time) {
	t.Helper()
	_, err := p.IngestComment(t.Context(), platform.MirroredComment{
		ID:          id,
		NoteID:      noteID,
		Author:      platform.MirroredAuthor{ID: author, Nick: nick},
		Body:        fmt.Sprintf("комментарий %d длиной побольше, чтобы годиться в цитату недели", id),
		PublishedAt: at,
	})
	if err != nil {
		t.Fatalf("приём комментария %d: %v", id, err)
	}
}

// Выпуск считается по площадке целиком: заметка НГС и заметка, написанная
// ЗДЕСЬ, попадают в одни и те же числа. Ради этого всё и затевалось — до
// 22.08.2026 нативная половина разговора в сводку не входила вовсе.
func TestBuildCountsNativeAndMirrored(t *testing.T) {
	src, p := newSource(t)
	ctx := t.Context()
	// Единственный тест файла, который считает по СКОЛЬЗЯЩЕМУ окну: нативную
	// половину пишет CreateNote часами базы, то есть «сейчас», и дотянуться до
	// неё календарным окном нельзя. Значит и зеркальная половина обязана стоять
	// относительно «сейчас», а не от фиксированного slot: привязанная к
	// календарю, она выпадала из окна ровно через сутки после написания теста
	// (и выпала — 23.08.2026, красный прогон CI на дню после зелёного).
	inWindow := time.Now().Add(-24 * time.Hour)

	ingestNote(t, p, 312811, 175869, "Гадёныш", inWindow)
	ingestComment(t, p, 63207290, 312811, 1409563, "Клубника", inWindow.Add(time.Minute))

	// Нативная половина: участник площадки пишет свою заметку и отвечает в ней.
	member := nativeMember(t, p, "Паноптикум")
	noteID, err := p.CreateNote(ctx, platform.NewNote{AuthorID: member, Body: "своя заметка"})
	if err != nil {
		t.Fatalf("нативная заметка: %v", err)
	}
	if _, err := p.CreateComment(ctx, platform.NewComment{
		NoteID: noteID, AuthorID: member, Body: "своя реплика длиной побольше"}); err != nil {
		t.Fatalf("нативная реплика: %v", err)
	}

	is, err := digest.Build(ctx, src, windowUntilNow())
	if err != nil {
		t.Fatalf("сборка выпуска: %v", err)
	}
	if is.Stats.Notes != 2 {
		t.Errorf("заметок в выпуске: %d, ожидалось 2 (НГС + своя)", is.Stats.Notes)
	}
	if is.Stats.Comments != 2 {
		t.Errorf("комментариев в выпуске: %d, ожидалось 2", is.Stats.Comments)
	}
	if is.Stats.Commenters != 2 {
		t.Errorf("участников в выпуске: %d, ожидалось 2", is.Stats.Commenters)
	}
}

// Скрытое модератором или автором не попадает в выпуск ни числом, ни цитатой:
// сводка читает ровно то же, что читатель страницы.
func TestHiddenStaysOutOfIssue(t *testing.T) {
	src, p := newSource(t)
	ctx := t.Context()
	inWindow := window.Start.Add(24 * time.Hour)

	ingestNote(t, p, 312811, 175869, "Гадёныш", inWindow)
	ingestComment(t, p, 1, 312811, 1409563, "Клубника", inWindow.Add(time.Minute))
	ingestComment(t, p, 2, 312811, 1431505, "Актриса", inWindow.Add(2*time.Minute))

	mod := moderator(t, p)
	if err := p.HideSubject(ctx, mod, platform.CommentSubject(2), platform.CatFlood, "проверка"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}

	comments, err := src.CommentsBetween(ctx, window.Start, window.End)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].ID != 1 {
		t.Fatalf("скрытая реплика попала в выпуск: %+v", comments)
	}
	seen, err := src.CommenterHistory(ctx, window.Start, window.End)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("скрытый автор остался в рубрике лиц: %+v", seen)
	}
}

// У анонимной заметки наружу не уходит ни автор, ни его идентичность: имя
// «Аноним», ключ пуст — иначе рубрика «новые лица» деанонимизировала бы его
// соседством чисел.
func TestAnonymousNoteKeepsAuthorHidden(t *testing.T) {
	src, p := newSource(t)
	ctx := t.Context()

	member := nativeMember(t, p, "Тайный")
	if _, err := p.CreateNote(ctx, platform.NewNote{
		AuthorID: member, Anonymous: true, Body: "анонимная заметка"}); err != nil {
		t.Fatalf("анонимная заметка: %v", err)
	}

	notes, err := src.NotesPublishedBetween(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("заметок: %d", len(notes))
	}
	if notes[0].Author != "" {
		t.Errorf("идентичность анонима утекла: %q", notes[0].Author)
	}
	if notes[0].AuthorName != platform.AnonNick {
		t.Errorf("имя анонима: %q", notes[0].AuthorName)
	}
	authors, err := src.NoteAuthorHistory(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(authors) != 0 {
		t.Errorf("анонимная заметка попала в рубрику лиц: %+v", authors)
	}
}

// «Новое лицо» против «возвращения»: у первого прошлого нет вовсе, у второго
// оно есть и лежит дальше returneeGap. Это и есть тот запрос, ради которого
// заведён индекс (author_id, published_at DESC).
func TestCommenterHistorySeparatesNewcomerFromReturnee(t *testing.T) {
	src, p := newSource(t)
	ctx := t.Context()
	inWindow := window.Start.Add(24 * time.Hour)

	ingestNote(t, p, 312811, 175869, "Гадёныш", longAgo)
	ingestComment(t, p, 1, 312811, 1044551, "Линда", longAgo)    // прошлое
	ingestComment(t, p, 2, 312811, 1044551, "Линда", inWindow)   // вернулась
	ingestComment(t, p, 3, 312811, 1496082, "Ромашка", inWindow) // новенькая

	seen, err := src.CommenterHistory(ctx, window.Start, window.End)
	if err != nil {
		t.Fatal(err)
	}
	byAuthor := map[string]digest.CommenterSeen{}
	for _, s := range seen {
		byAuthor[s.Author] = s
	}
	if got := byAuthor["1044551"]; got.PrevSeenAt.IsZero() || !got.PrevSeenAt.Equal(longAgo) {
		t.Errorf("прошлое вернувшейся: %v, ожидалось %v", got.PrevSeenAt, longAgo)
	}
	if got := byAuthor["1496082"]; !got.PrevSeenAt.IsZero() {
		t.Errorf("у новенькой нашлось прошлое: %v", got.PrevSeenAt)
	}
}

// Ссылка на анкету есть у пришедшего с НГС (id строки равен номеру анкеты) и
// нет у нативного участника: анкеты у него не существует, и ссылка вела бы в
// никуда.
func TestProfileURLOnlyForNGSProfiles(t *testing.T) {
	src, p := newSource(t)
	member := nativeMember(t, p, "Приглашённый")
	if got := src.ProfileURL("175869"); got != ngsBase+"/profile/175869/" {
		t.Errorf("ссылка на анкету НГС: %q", got)
	}
	if got := src.ProfileURL(fmt.Sprint(member)); got != "" {
		t.Errorf("у нативного участника появилась анкета: %q", got)
	}
	if got := src.ProfileURL(""); got != "" {
		t.Errorf("у анонима появилась анкета: %q", got)
	}
}

// Планы запросов — часть контракта, как и в ядре площадки: молчаливый переезд
// недельного окна на полный перебор не провалит ни одного теста на поведение, а
// на живой базе это чтение таблицы в 3 ГБ раз в неделю. Проверяется ровно тот
// SQL, который выполняется.
func TestQueryPlansUseTimeIndexes(t *testing.T) {
	_, p := newSource(t)
	seedForPlans(t, p)

	cases := []struct {
		name, query, index string
	}{
		{"окно недели", commentsWindowQuery, "comments_time"},
		{"прошлое человека", commenterHistoryQuery, "comments_author_time"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, p, c.query, window.Start, window.End)
			if !strings.Contains(plan, c.index) {
				t.Fatalf("план не берёт индекс %s:\n%s", c.index, plan)
			}
			if strings.Contains(plan, "Seq Scan on comments") {
				t.Fatalf("полный перебор комментариев в плане:\n%s", plan)
			}
		})
	}
}

// seedForPlans заливает заготовку, на которой планировщик ведёт себя как на
// живой базе: год комментариев, размазанных по заметкам и людям. Разброс по
// ВРЕМЕНИ здесь и есть суть — окно недели обязано быть избирательным, иначе
// индекс по published_at честно не нужен и тест проверял бы сам себя.
func seedForPlans(t *testing.T, p *platform.Platform) {
	t.Helper()
	if _, err := p.Pool().Exec(t.Context(), `
		INSERT INTO users (id, nick)
		SELECT 1000000 + g, 'ник' || g FROM generate_series(1, 200) g;
		INSERT INTO notes (id, author_id, body, published_at)
		SELECT 200000 + g, 1000000 + (g % 200) + 1, 'заметка ' || g,
		       timestamptz '2026-08-22 02:00Z' - g * interval '1 hour'
		  FROM generate_series(1, 400) g;
		INSERT INTO comments (id, note_id, author_id, body, path, depth, published_at)
		SELECT 500000 + g, 200000 + (g % 400) + 1, 1000000 + (g % 200) + 1, 'комментарий ' || g,
		       lpad((500000 + g)::text, 13, '0'), 1,
		       timestamptz '2026-08-22 02:00Z' - g * interval '1 hour'
		  FROM generate_series(1, 8000) g;
		ANALYZE notes, comments, users`); err != nil {
		t.Fatalf("заливка: %v", err)
	}
}

// --- вспомогательное -------------------------------------------------------

// windowUntilNow — окно, кончающееся сейчас: нативные публикации получают
// published_at = now(), и слот в прошлом их бы не увидел.
func windowUntilNow() digest.Window {
	end := time.Now().Add(time.Minute)
	return digest.Window{Start: end.AddDate(0, 0, -7), End: end, ID: "2026-W34"}
}

// nativeMember — участник площадки без анкеты НГС (вход по приглашению).
func nativeMember(t *testing.T, p *platform.Platform, nick string) int64 {
	t.Helper()
	ctx := t.Context()
	id, err := p.CreateNativeUser(ctx, nick)
	if err != nil {
		t.Fatalf("нативный участник: %v", err)
	}
	if err := p.Promote(ctx, id); err != nil {
		t.Fatalf("участник: %v", err)
	}
	// Согласия — часть входа, а не украшение: публиковать можно только по
	// действующей редакции (platform.publishGuard), и участник без подписи в бою
	// не появляется вовсе.
	docs, err := platform.CurrentConsentDocs(platform.Operator{})
	if err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(ctx, id, d.Kind, d.Version, "тест"); err != nil {
			t.Fatalf("согласие %s: %v", d.Kind, err)
		}
	}
	return id
}

// moderator — участник с правом скрывать.
func moderator(t *testing.T, p *platform.Platform) platform.Viewer {
	t.Helper()
	id := nativeMember(t, p, "Модератор")
	if err := p.SetRole(t.Context(), platform.Viewer{UserID: id, Role: platform.RoleAdmin}, id, platform.RoleModerator); err != nil {
		t.Fatalf("роль: %v", err)
	}
	return platform.Viewer{UserID: id, Role: platform.RoleModerator}
}

// explain возвращает план запроса строкой.
func explain(t *testing.T, p *platform.Platform, sql string, args ...any) string {
	t.Helper()
	rows, err := p.Pool().Query(t.Context(), "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
