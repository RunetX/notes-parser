package platform

// Очередь ЖИВЫХ тредов: за кем демон ходит на мобильную страницу, пока в треде
// разговаривают. Тест интеграционный по той же причине, что и остальные в
// пакете: правило целиком живёт в SQL, и заглушка подтвердила бы только себя.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// liveComment — реплика, датированная относительно СЕЙЧАС: очередь сравнивает
// время комментария с временем обхода, а обход штампуется now().
func liveComment(t *testing.T, p *Platform, id, noteID int64, ago time.Duration) {
	t.Helper()
	_, err := p.IngestComment(context.Background(), MirroredComment{
		ID:          id,
		NoteID:      noteID,
		Author:      MirroredAuthor{ID: 2, Nick: "Хатуль мадан"},
		Body:        fmt.Sprintf("реплика %d", id),
		PublishedAt: time.Now().Add(-ago),
	})
	if err != nil {
		t.Fatalf("приём комментария %d: %v", id, err)
	}
}

func freshQueue(t *testing.T, p *Platform, gap time.Duration) []int64 {
	t.Helper()
	ids, err := p.ReplyScanFresh(context.Background(), 10, 24*time.Hour, gap)
	if err != nil {
		t.Fatalf("очередь свежих тредов: %v", err)
	}
	return ids
}

// Заметка попадает в очередь, пока в ней дописывают, и уходит из неё, как
// только обход состоялся. Это и есть разница с очередью добора истории: та
// смотрит заметку ОДИН раз, и дописанное после неё остаётся с угаданными
// рёбрами навсегда.
func TestReplyScanFreshFollowsLiveThread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313054, 1, "Т 72Б")
	liveComment(t, p, 63238230, 313054, 10*time.Minute)

	if got := freshQueue(t, p, 0); !slices.Contains(got, 313054) {
		t.Fatalf("живой тред не попал в очередь: %v", got)
	}
	if err := p.MarkReplyScan(ctx, 313054, true); err != nil {
		t.Fatal(err)
	}
	if got := freshQueue(t, p, 0); len(got) != 0 {
		t.Errorf("обойдённый тред остался в очереди: %v", got)
	}

	// Дописали — снова в очередь: ровно этого не умеет историческая очередь.
	liveComment(t, p, 63238236, 313054, 0)
	if got := freshQueue(t, p, 0); !slices.Contains(got, 313054) {
		t.Errorf("новая реплика не вернула тред в очередь: %v", got)
	}
	// Но не чаще, чем раз в gap: иначе бойкий тред обходился бы на каждую реплику.
	if got := freshQueue(t, p, time.Hour); len(got) != 0 {
		t.Errorf("тред обходится чаще, чем раз в gap: %v", got)
	}
}

// Мёртвый тред очереди не касается: его добирает история. И отброшенный
// (reply_scan_skip) не возвращается никогда — это решение человека.
func TestReplyScanFreshIgnoresStaleAndSkipped(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	ingestNote(t, p, 313000, 1, "Т 72Б")
	liveComment(t, p, 63000000, 313000, 48*time.Hour) // позавчерашний разговор

	ingestNote(t, p, 313054, 1, "Т 72Б")
	liveComment(t, p, 63238230, 313054, time.Minute)
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO ingest_state (note_id, reply_scan_skip) VALUES ($1, true)`, int64(313054)); err != nil {
		t.Fatal(err)
	}

	if got := freshQueue(t, p, 0); len(got) != 0 {
		t.Errorf("в очередь попало лишнее: %v", got)
	}
}

// ОТВЕТ С НГС НАХОДИТ НАШУ РЕПЛИКУ ПО ЕЁ НОМЕРУ НА САЙТЕ.
//
// Живая жалоба владельца 03.09.2026: «ответы из НГС прикрепляются к
// комментариям, отправленным из Зазеркалья, а потом сбрасываются в корень
// заметки». Две половины фразы — это ровно два разных пути, и оба настоящие.
// ПРИКРЕПЛЯЛОСЬ живым приёмом: он ищет адресата по обращению «Ник, …» и нашу
// нативную строку находит. СБРАСЫВАЛОСЬ обходом дерева: мобильная страница
// говорит номерами САЙТА, а у нашей реплики там свой номер — родитель, которого
// в заметке нет вовсе.
//
// Замер по бою в тот же день: заметки 313146 и 313158, пять осиротевших реплик;
// у 313158 их три, и одна из них — ответ Дракоши нашему «Толщина имеет
// значение?» (наш 100000002244, на сайте 63259117), вставший в корень.
//
// Тест идёт ПО ПУТИ ДАННЫХ — через настоящий ApplyReplyTree с настоящей
// очередью выноса, — потому что дефект жил не в формуле, а в том, доходит ли
// перевод до неё.
func TestОтветСНГСНаходитНашуРепликуПоНомеруСайта(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 175869, "Паноптикум")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	// Заметка ЗЕРКАЛЬНАЯ: только под такой реплика и уносится на сайт.
	ingestNote(t, p, 313158, 498196, "ДВ")
	ingestComment(t, p, 63259111, 313158, 1038894, 0) // Дракоша, корень треда

	ours, err := p.CreateComment(ctx, NewComment{
		NoteID: 313158, AuthorID: author, ReplyToID: 63259111, Body: "Толщина имеет значение?"})
	if err != nil {
		t.Fatalf("наша реплика: %v", err)
	}
	// Сайт её принял, и номер оттуда вычитан (findPosted). Только с этого
	// момента ответы на неё вообще могут прийти.
	const onNGS = 63259117
	if _, err := p.pool.Exec(ctx,
		`UPDATE ngs_outbox SET state = $1, ngs_id = $2 WHERE kind = $3 AND object_id = $4`,
		NGSSent, "63259117", NGSComment, ours); err != nil {
		t.Fatal(err)
	}
	// Ответ Дракоши приезжает зеркалом: адресата живой приём взял из обращения
	// «Ник, …» и разрешил ВЕРНО — в нашу строку.
	_, err = p.IngestComment(ctx, MirroredComment{
		ID: 63259121, NoteID: 313158, Author: MirroredAuthor{ID: 1038894, Nick: "Дракоша"},
		Body: "Скажем так, температура нагревательного прибора", ReplyToID: ours,
		ReplySource: ReplyPrefix, PublishedAt: testTime,
	})
	if err != nil {
		t.Fatalf("ответ с НГС: %v", err)
	}

	// А теперь обход мобильного дерева. Сайт называет НАШ номер своим, и до
	// перевода ребро уезжало в несуществующий 63259117, а строка — в корень.
	// Про нашу реплику сайт говорит «корневая»: её родителя (63259111) на НГС
	// нет... впрочем, здесь он есть — важно, что мнение сайта о НАШЕЙ строке
	// ребра ей не меняет, оно поставлено здесь и знает больше.
	if _, err := p.ApplyReplyTree(ctx, 313158, map[int64]int64{
		63259111: 0, onNGS: 0, 63259121: onNGS}); err != nil {
		t.Fatalf("дерево: %v", err)
	}

	var (
		replyTo *int64
		depth   int
		path    string
	)
	if err := p.pool.QueryRow(ctx,
		`SELECT reply_to_id, depth, path FROM comments WHERE id = 63259121`).
		Scan(&replyTo, &depth, &path); err != nil {
		t.Fatal(err)
	}
	if idOf(replyTo) != ours {
		t.Errorf("адресат ответа %d, ожидалась наша реплика %d — она отвязалась", idOf(replyTo), ours)
	}
	// Место — не то же самое, что ребро, и проверять надо оба: номер нашей
	// реплики БОЛЬШЕ номера ответа (полосы!), и путь родителя не брался даже
	// тогда, когда ребро было верным.
	if depth != 3 {
		t.Errorf("глубина ответа %d, ожидалась 3 — он лёг в корень", depth)
	}
	var ourPath string
	if err := p.pool.QueryRow(ctx, `SELECT path FROM comments WHERE id = $1`, ours).Scan(&ourPath); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, ourPath+".") {
		t.Errorf("путь ответа %q не лежит под нашей репликой %q", path, ourPath)
	}
	// И наше собственное ребро цело: сайт назвал строку корневой, мы его не
	// послушали.
	if err := p.pool.QueryRow(ctx,
		`SELECT reply_to_id FROM comments WHERE id = $1`, ours).Scan(&replyTo); err != nil {
		t.Fatal(err)
	}
	if idOf(replyTo) != 63259111 {
		t.Errorf("наше ребро переписано сайтом: адресат %d, ожидался 63259111", idOf(replyTo))
	}
}
