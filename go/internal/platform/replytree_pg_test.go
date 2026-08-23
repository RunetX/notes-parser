package platform

// Очередь ЖИВЫХ тредов: за кем демон ходит на мобильную страницу, пока в треде
// разговаривают. Тест интеграционный по той же причине, что и остальные в
// пакете: правило целиком живёт в SQL, и заглушка подтвердила бы только себя.

import (
	"context"
	"fmt"
	"slices"
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
