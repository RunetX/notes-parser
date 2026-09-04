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
	// Тексты согласий публикуются ВМЕСТЕ со схемой — так их накатывает
	// администратор (одна команда `platform migrate`), и без них любое согласие
	// падает на внешнем ключе consents → consent_docs. Реквизиты пустые: они
	// подставляются в текст, а для теста важно, что редакция опубликована.
	if err := shared.EnsureConsentDocs(ctx, Operator{}); err != nil {
		t.Fatalf("тексты согласий: %v", err)
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

// Смешанный тред — тот, ради которого сортировка сестёр вообще заведена. Тест
// стои́т НА ПУТИ ДАННЫХ, а не на формуле: формулу проверяет tree_test, а дефект
// живёт в том, доходит ли она до выдачи. Порядок по номерам дал бы здесь
// 100, 200, 5000000002, 101, 5000000001 — то есть всё своё в хвосте ветки,
// независимо от того, когда это было сказано.
func TestThreadMixesBandsByTime(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	// Люди разные не для правдоподобия: потолок частоты — одна реплика в десять
	// секунд на человека, и вторая ушла бы в «слишком часто».
	author := mustUser(t, p, "Паноптикум")
	other := mustUser(t, p, "Фемида")
	ingestNote(t, p, 312811, 175869, "Гадёныш")

	ingestComment(t, p, 100, 312811, 1, 0)   // зеркальная корневая
	ingestComment(t, p, 101, 312811, 2, 0)   // зеркальная корневая
	ingestComment(t, p, 200, 312811, 3, 100) // зеркальный ответ на 100

	root, err := p.CreateComment(ctx, NewComment{NoteID: 312811, AuthorID: author, Body: "своя корневая"})
	if err != nil {
		t.Fatalf("своя корневая: %v", err)
	}
	reply, err := p.CreateComment(ctx, NewComment{NoteID: 312811, AuthorID: other,
		Body: "свой ответ", ReplyToID: 100})
	if err != nil {
		t.Fatalf("свой ответ: %v", err)
	}

	// Время расставляем сами: собственные реплики публикуются «сейчас», а весь
	// смысл проверки в том, что своя реплика бывает СТАРШЕ зеркальной.
	for i, id := range []int64{root, 100, reply, 200, 101} {
		if _, err := p.pool.Exec(ctx, `UPDATE comments SET published_at = $2 WHERE id = $1`,
			id, testTime.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("время реплики %d: %v", id, err)
		}
	}

	got, err := p.Thread(ctx, Viewer{}, 312811)
	if err != nil {
		t.Fatalf("тред: %v", err)
	}
	wantIDs := []int64{root, 100, reply, 200, 101}
	wantDepth := []int{1, 1, 2, 2, 1}
	if len(got) != len(wantIDs) {
		t.Fatalf("в треде %d реплик, ожидалось %d", len(got), len(wantIDs))
	}
	for i := range wantIDs {
		if got[i].ID != wantIDs[i] || got[i].Depth != wantDepth[i] {
			t.Fatalf("позиция %d: id=%d depth=%d, ожидалось id=%d depth=%d",
				i, got[i].ID, got[i].Depth, wantIDs[i], wantDepth[i])
		}
	}
}

// Живой добор отдаёт реплику вместе с её МЕСТОМ в дереве: глубиной и адресатом.
// Без них страница не знает, куда её поставить, и ответ на давнюю реплику уехал
// бы в конец треда.
func TestCommentsSinceCarriesPlaceInTree(t *testing.T) {
	p := testPlatform(t)
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 100, 312811, 1, 0)
	ingestComment(t, p, 200, 312811, 2, 100)
	// Всё, что выше, страница уже показала. Дальше — то, что пришло потом.
	ingestComment(t, p, 300, 312811, 3, 100)

	got, err := p.CommentsSince(context.Background(), Viewer{}, 312811, FreshAfter{NGS: 200}, FreshLimit)
	if err != nil {
		t.Fatalf("добор: %v", err)
	}
	if len(got) != 1 || got[0].ID != 300 {
		t.Fatalf("добор принёс %+v, ожидалась одна реплика 300", got)
	}
	if got[0].Depth != 2 {
		t.Errorf("глубина %d, ожидалась 2", got[0].Depth)
	}
	if got[0].ReplyTo == nil || got[0].ReplyTo.CommentID != 100 {
		t.Errorf("адресат %+v, ожидался 100", got[0].ReplyTo)
	}
	// Порядок ПО ВОЗРАСТАНИЮ, в отличие от линейного вида: страница дописывается
	// в том порядке, в каком реплики появлялись.
	ingestComment(t, p, 400, 312811, 1, 300)
	got, err = p.CommentsSince(context.Background(), Viewer{}, 312811, FreshAfter{NGS: 200}, FreshLimit)
	if err != nil {
		t.Fatalf("добор: %v", err)
	}
	if len(got) != 2 || got[0].ID != 300 || got[1].ID != 400 {
		t.Fatalf("порядок добора: %v", got)
	}
}

// Тред у площадки СМЕШАННЫЙ по устройству, и живой добор обязан это пережить.
// Полосы упорядочены по происхождению, а не по времени: реплика, написанная
// здесь, имеет номер больше любой ngs'ной, включая ту, что придёт завтра. С
// одной общей границей первая же своя реплика уводила добор в нативную полосу, и
// приходящие следом комментарии НГС не догоняли страницу НИКОГДА — в мессенджере
// они шли, на странице их не было до обновления (боевая заметка 313056).
func TestCommentsSinceCrossesBands(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 63238683, 312811, 1, 0)

	// Своя реплика в том же треде: её номер на два порядка больше ngs'ного.
	own, err := p.CreateComment(ctx, NewComment{
		NoteID: 312811, AuthorID: mustUser(t, p, "Рио"), Body: "и я тут"})
	if err != nil {
		t.Fatalf("своя реплика: %v", err)
	}
	if !IsNative(own) {
		t.Fatalf("реплика %d оказалась не в нативной полосе", own)
	}
	after := FreshAfter{}
	after.Seen(63238683)
	after.Seen(own)

	// Дальше приходит комментарий с НГС — с номером МЕНЬШЕ своей реплики.
	ingestComment(t, p, 63238684, 312811, 2, 63238683)
	got, err := p.CommentsSince(ctx, Viewer{}, 312811, after, FreshLimit)
	if err != nil {
		t.Fatalf("добор: %v", err)
	}
	if len(got) != 1 || got[0].ID != 63238684 {
		t.Fatalf("добор принёс %v, ожидалась одна реплика 63238684", ids(got))
	}

	// И наоборот: своя следующая реплика не теряется из-за того, что граница
	// ngs'ной полосы ушла вперёд.
	after.Seen(63238684)
	own2, err := p.CreateComment(ctx, NewComment{
		NoteID: 312811, AuthorID: mustUser(t, p, "Мавр"), Body: "и я"})
	if err != nil {
		t.Fatalf("своя реплика: %v", err)
	}
	got, err = p.CommentsSince(ctx, Viewer{}, 312811, after, FreshLimit)
	if err != nil {
		t.Fatalf("добор: %v", err)
	}
	if len(got) != 1 || got[0].ID != own2 {
		t.Fatalf("добор принёс %v, ожидалась одна реплика %d", ids(got), own2)
	}
}

// Линейный вид — «сначала новые» ПО ВРЕМЕНИ. По убыванию id он был верен, пока
// в треде жила одна полоса: нативная реплика позапрошлой недели имеет номер
// больше комментария, пришедшего с НГС сегодня, и всё написанное здесь вставало
// наверх страницы независимо от того, когда это было сказано.
func TestFlatOrdersByTimeNotByBand(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")

	own, err := p.CreateComment(ctx, NewComment{
		NoteID: 312811, AuthorID: mustUser(t, p, "Рио"), Body: "сказано на прошлой неделе"})
	if err != nil {
		t.Fatalf("своя реплика: %v", err)
	}
	// Времена ставим руками: CreateComment штампует now(), а весь вопрос теста в
	// том, что своя реплика СТАРШЕ пришедшей с НГС.
	if _, err := p.pool.Exec(ctx,
		`UPDATE comments SET published_at = $2 WHERE id = $1`, own, testTime); err != nil {
		t.Fatalf("время своей реплики: %v", err)
	}
	ingestComment(t, p, 63238684, 312811, 2, 0) // testTime + 63238684 секунд, то есть позже

	got, err := p.Flat(ctx, Viewer{}, 312811, 0, 30)
	if err != nil {
		t.Fatalf("линейный вид: %v", err)
	}
	if len(got) != 2 || got[0].ID != 63238684 || got[1].ID != own {
		t.Fatalf("порядок линейного вида: %v, ожидалось [63238684 %d]", ids(got), own)
	}
}

// Границу живого добора страница спрашивает у ядра — по максимуму в КАЖДОЙ
// полосе у всей заметки, а не по показанным строкам (в линейном виде на странице
// окно). Восстановленное (2010 год) границы не имеет вовсе: появиться на
// открытой странице оно не может.
func TestThreadFreshAfterIsPerBand(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 63238683, 312811, 1, 0)
	ingestComment(t, p, 63238684, 312811, 2, 0)

	after, err := p.ThreadFreshAfter(ctx, 312811)
	if err != nil {
		t.Fatalf("граница: %v", err)
	}
	if after.NGS != 63238684 || after.Native != 0 {
		t.Fatalf("граница %+v, ожидались полосы 63238684 и 0", after)
	}
	// Третья составляющая — переезды, и пустой она не бывает: пустая означает
	// «переездов не носим», и первая же правка дерева прошла бы мимо открытой
	// страницы (см. MovedAfter).
	if !after.Moved.On() {
		t.Fatal("граница переездов пуста")
	}

	own, err := p.CreateComment(ctx, NewComment{
		NoteID: 312811, AuthorID: mustUser(t, p, "Рио"), Body: "и я тут"})
	if err != nil {
		t.Fatalf("своя реплика: %v", err)
	}
	// Строка восстановленной полосы: её приносит разовая команда, и на границу
	// она влиять не должна.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO comments (id, note_id, body, path, depth, published_at)
		VALUES ($1, 312811, 'август 2010', $2, 1, $3)`,
		RestoredIDBase+7, pathSegment(RestoredIDBase+7), testTime); err != nil {
		t.Fatalf("восстановленная реплика: %v", err)
	}

	after, err = p.ThreadFreshAfter(ctx, 312811)
	if err != nil {
		t.Fatalf("граница: %v", err)
	}
	if after.NGS != 63238684 || after.Native != own {
		t.Fatalf("граница %+v, ожидались полосы 63238684 и %d", after, own)
	}
}

// Скрытое модератором в добор не идёт — ровно как и на страницу. Иначе живое
// обновление показывало бы то, чего обновление вручную уже не покажет.
func TestCommentsSinceHidesWhatThePageHides(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 175869, "Гадёныш")
	ingestComment(t, p, 100, 312811, 1, 0)
	ingestComment(t, p, 200, 312811, 2, 0)

	mod := moderator(t, p)
	if err := p.HideSubject(ctx, mod, CommentSubject(200), CatFlood, "проверка"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	got, err := p.CommentsSince(ctx, Viewer{}, 312811, FreshAfter{}, FreshLimit)
	if err != nil {
		t.Fatalf("добор: %v", err)
	}
	if len(got) != 1 || got[0].ID != 100 {
		t.Fatalf("скрытая реплика попала в добор: %v", got)
	}
	// Модератору она видна — там же, где он читает.
	got, err = p.CommentsSince(ctx, mod, 312811, FreshAfter{}, FreshLimit)
	if err != nil {
		t.Fatalf("добор модератора: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("модератор не увидел скрытое в доборе: %v", got)
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
	author := mustUser(t, p, "Ванилька")
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
	total, err := p.CountNotes(ctx, Viewer{})
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
	mod := Viewer{UserID: mustUser(t, p, "модератор"), Role: RoleModerator}
	if err := p.SetThreadLocked(ctx, mod, noteID, true, "хватит"); err != nil {
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
// mustUser — участник, какой он в бою: заведён И подписал действующие редакции
// согласий. Без подписи публиковать нельзя (writeGuard), и это не деталь теста:
// участник без согласий на площадке не появляется вовсе — экран согласий стоит
// последним шагом входа, а отказ на нём вход откатывает.
func mustUser(t *testing.T, p *Platform, nick string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := p.CreateNativeUser(ctx, nick)
	if err != nil {
		t.Fatalf("пользователь %q: %v", nick, err)
	}
	mustConsent(t, p, id)
	return id
}

// mustConsent подписывает за человека всё, что подписывают при входе.
func mustConsent(t *testing.T, p *Platform, userID int64) {
	t.Helper()
	docs, err := CurrentConsentDocs(Operator{})
	if err != nil {
		t.Fatalf("тексты согласий: %v", err)
	}
	for _, d := range docs {
		if err := p.GrantConsent(context.Background(), userID, d.Kind, d.Version, "тест"); err != nil {
			t.Fatalf("согласие %s: %v", d.Kind, err)
		}
	}
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

// seedForPlans заливает заготовку, на которой планировщик ведёт себя как на
// живой базе. Общая для планов чтения и планов модератора: заготовка — часть
// проверки, и две её копии однажды разъедутся.
func seedForPlans(t *testing.T, p *Platform) {
	t.Helper()
	ctx := context.Background()
	// Заливка идёт массовой вставкой, а не приёмом по одному: план запроса не
	// зависит от того, как строки появились, а тысячи приёмов через туннель —
	// это минуты ожидания вместо секунды.
	//
	// Полосы ОБЕ, и это тоже часть заготовки. У добора треда своя ветка на
	// полосу (commentsSinceSQL), и в базе без единой нативной реплики
	// планировщик берёт нативную ветку первичным ключом: диапазон полосы пуст,
	// note_id ему не нужен. На живой площадке за этим стоял бы обход всего
	// написанного здесь после границы — по всем заметкам сразу, на каждый такт
	// каждого открытого окна.
	//
	// Комментарии РАЗМАЗАНЫ по заметкам намеренно. Первая попытка сложила все
	// 600 в одну заметку — и планировщик честно выбрал обход по первичному
	// ключу: когда условию note_id удовлетворяет вся таблица, отдельный индекс
	// действительно не нужен. Такая заготовка проверяла бы не запрос, а саму
	// себя; на живой базе у заметки доли процента строк.
	//
	// ЧИСЛО строк на заметку тоже часть заготовки, и оплачено оно двумя
	// ложными падениями. При двенадцати репликах в треде обоим индексам
	// comments всё равно, с чего начинать (оба ведут с note_id), сортировка
	// дюжины строк ничего не стоит — и выбор между comments_tree и
	// comments_flat становится случайным: тест краснел на чистом дереве и
	// зеленел от перезапуска. Триста реплик на заметку делают разницу
	// настоящей: упорядоченный обход против сортировки трёхсот строк — это
	// уже решение, а не бросок монеты.
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
		  FROM generate_series(1, 120000) g;
		INSERT INTO comments (id, note_id, author_id, body, path, depth, published_at)
		SELECT 100000000000 + g, 200000 + (g % 400) + 1, 1000000 + (g % 200) + 1, 'своя ' || g,
		       lpad((100000000000 + g)::text, 13, '0'), 1,
		       timestamptz '2026-08-20 12:00Z' + g * interval '1 second'
		  FROM generate_series(1, 30000) g;
		ANALYZE notes, comments, users, media`); err != nil {
		t.Fatalf("заливка: %v", err)
	}

}

// Планы запросов — часть договора, а не деталь: молчаливый переезд ленты или
// треда на полный перебор не проваливает ни один тест на поведение, а на живой
// базе это отказ. Проверяем ровно тот SQL, который выполняется.
func TestQueryPlansUseIndexes(t *testing.T) {
	p := testPlatform(t)
	seedForPlans(t, p)

	cases := []struct {
		name  string
		query string
		args  []any
		index string
		// times — сколько раз индекс обязан встретиться в плане. Нужен добору
		// треда: веток у него две, по одной на полосу, и уехать в перебор может
		// любая — а «индекс в плане есть» этого не заметит.
		times int
	}{
		{"лента", feedQuery, []any{int64(0), 20, 0}, "notes_feed", 1},
		{"закреплённые", pinnedQuery, []any{int64(0), MaxPinned}, "notes_pinned", 1},
		{"тред", threadQuery, []any{int64(0), int64(200001), 100}, "comments_tree", 1},
		{"линейный вид", flatQuery, []any{int64(0), int64(200001), 30, 0}, "comments_flat_time", 1},
		// Живой добор ходит чаще всех: по разу на каждую новую реплику у каждого
		// открытого окна. Полный перебор здесь означал бы, что тред, который
		// читают, тем самым и кладёт площадку.
		{"добор треда", commentsSinceQuery,
			[]any{int64(0), int64(200001), int64(0), NativeIDBase - 1, 50}, "comments_flat", 2},
		// Переезды спрашивает тот же такт того же окна, и ответ у них почти
		// всегда пустой — тем более он обязан стоить одного захода в индекс, а
		// не обхода всех реплик заметки.
		{"переезды", commentsMovedQuery,
			[]any{int64(200001), time.Now().Add(-time.Hour), int64(0), 50}, "comments_moved", 1},
		// Адресата спрашивают на КАЖДУЮ зеркальную реплику с обращением, то есть
		// в темпе живого зеркала. Закрепляется здесь ровно то, что стоит
		// закреплять: обе ступени не выходят за ОДНУ ЗАМЕТКУ.
		//
		// Первая ступень при этом читает тред целиком и сортирует его: рано
		// оборвать обход планировщик не может, потому что «обращена ко мне»
		// проверяется либо переходом по ребру, либо разбором тела, и ни то ни
		// другое из индекса не видно. Цена измерена (23.08.2026, тред 375
		// реплик): 0,5 мс на ступень, 1,7 мс на весь вызов вместе с двумя
		// круговыми — столько же по порядку, сколько зеркало и сегодня тратит в
		// SQLite, где книга адресатов набирается тем же полным проходом по
		// треду. Индекс времени здесь не назван намеренно: он нужен там, где
		// обход обрывается рано, а тут выбор между ним и comments_flat — дело
		// планировщика.
		{"адресат: обращённая ко мне", addresseeToMeQuery,
			[]any{int64(200001), "ник", time.Now(), int64(0), "отвечающий"},
			"comments_flat", 1},
		{"адресат: последняя", addresseeLastQuery,
			[]any{int64(200001), "ник", time.Now(), int64(0)}, "comments_flat_time", 1},
		// Тот же индекс с другой стороны: живой канал спрашивает про ОТКРЫТЫЕ
		// треды на каждом своём проходе, и ответ у него почти всегда пустой.
		// Перебор здесь означал бы, что открытая вкладка сама по себе, без
		// единой новой реплики, гоняет таблицу на 10,7 млн строк.
		{"переезды по открытым тредам", movedNotesQuery,
			[]any{[]int64{200001, 200002}, time.Now().Add(-time.Hour)}, "comments_moved", 1},
		// Границу добора спрашивает КАЖДАЯ страница заметки, и это два прохода
		// с конца по одной строке. Перебор здесь стоил бы дороже самой страницы.
		{"граница добора", freshAfterQuery, []any{int64(200001), NativeIDBase, RestoredIDBase},
			"comments_flat", 2},
		// У той же границы есть и половина про переезды — её два прохода с конца
		// обязаны идти по своему индексу.
		{"граница переездов", freshAfterQuery, []any{int64(200001), NativeIDBase, RestoredIDBase},
			"comments_moved", 2},
		{"добор ленты", notesSinceQuery,
			[]any{int64(0), time.Now().Add(-time.Hour), int64(0), 50}, "notes_feed", 1},
		// Одна реплика — та, что нужна форме ответа. Имя индекса здесь не
		// закрепляется: по (note_id, id) годятся и первичный ключ, и
		// comments_flat, а выбор между ними дело планировщика. Закрепляется
		// другое — что это не перебор: запрос идёт на каждое «Ответить», а в
		// таблице 10,7 млн строк.
		{"одна реплика", commentQuery, []any{int64(0), int64(200002), int64(500001)}, "", 1},
		// Страница участника. Счётчики в profileQuery идут по тем же частичным
		// индексам, что и списки: у самого говорливого участника площадки 138
		// тысяч реплик, и перебор здесь был бы тем самым проходом на пятьдесят
		// секунд, из-за которого выгрузка живёт командой, а не кнопкой.
		{"карточка участника", profileQuery, []any{int64(1)}, "comments_author_time", 1},
		{"карточка участника: заметки", profileQuery, []any{int64(1)}, "notes_author", 1},
		{"заметки участника", authorNotesQuery, []any{int64(1), pubLimit}, "notes_author", 1},
		{"реплики участника", authorCommentsQuery, []any{int64(1), pubLimit}, "comments_author_time", 1},
		// Мордолента идёт на КАЖДОМ показе первой страницы ленты, и «когда этот
		// житель говорил» она спрашивает по разу на жителя. Индекс users_persona
		// здесь не назван намеренно: в заготовке двести анкет, и на такой
		// таблице планировщик прав, выбирая перебор, — цена у выбора появляется
		// только на боевых сотнях тысяч. А вот перебор comments ценой не
		// оправдан ни на какой заготовке, и его ловит общая проверка ниже.
		{"мордолента", personaFacesQuery, []any{60}, "comments_author_time", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, p, c.query, c.args...)
			if want := max(c.times, 1); c.index != "" && strings.Count(plan, c.index) < want {
				t.Fatalf("план берёт индекс %s %d раз вместо %d:\n%s",
					c.index, strings.Count(plan, c.index), want, plan)
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


// «Убрать фото» снимает ПРИВЯЗКУ, а файл остаётся лежать: имя файла есть его
// содержимое, поэтому на ту же картинку ссылаются и чужие строки, а уборки
// каталога у площадки нет вовсе.
//
// Главное же здесь — ссылка. Разовый добор медиа берёт как раз тех, у кого
// ссылка есть, а байтов нет: оставшись, она вернула бы снятое фото на место
// следующим же обходом — и человек, стёрший фото и здесь, и в анкете, увидел бы
// его снова.
func TestClearAvatarDropsTheLinkAndKeepsTheFile(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	const ngsURL = "https://n1s1.hsmedia.ru/preview/love/avatars/abc_100_100_c.jpg"
	id, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 312345, Nick: "Пух", AvatarURL: ngsURL})
	if err != nil {
		t.Fatalf("тень: %v", err)
	}
	m, err := store.Put(ctx, testPNG(t, 8, 5), ngsURL)
	if err != nil {
		t.Fatalf("приём медиа: %v", err)
	}
	if err := p.SetNGSAvatar(ctx, id, m.SHA256, ngsURL); err != nil {
		t.Fatalf("фото: %v", err)
	}

	if err := p.ClearAvatar(ctx, id); err != nil {
		t.Fatalf("снятие фото: %v", err)
	}
	u, err := p.UserByID(ctx, id)
	if err != nil {
		t.Fatalf("человек: %v", err)
	}
	if len(u.AvatarSHA) != 0 || u.NGSAvatarURL != "" {
		t.Fatalf("фото осталось привязанным: sha %x, ссылка %q", u.AvatarSHA, u.NGSAvatarURL)
	}
	missing, err := p.MissingAvatars(ctx, 10)
	if err != nil {
		t.Fatalf("добор аватаров: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("снятое фото попало в добор медиа: %+v — следующий обход вернёт его на место", missing)
	}
	if !store.Has(m.SHA256, m.MIME) {
		t.Fatal("файл удалён, а на ту же картинку ссылаются чужие строки")
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM media`).Scan(&n); err != nil {
		t.Fatalf("учёт медиа: %v", err)
	}
	if n != 1 {
		t.Fatalf("в учёте %d строк, ожидалась 1: снимается привязка, а не файл", n)
	}

	if err := p.ClearAvatar(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("снятие у несуществующего: %v, ожидалось ErrNotFound", err)
	}
}
