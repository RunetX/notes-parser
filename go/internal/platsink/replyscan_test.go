package platsink

// Подсказки приёмника (ReplyScanner.Nudge) — очередь в памяти, и проверяется
// здесь именно она: сайта и базы этим правилам не нужно.

import (
	"testing"
	"time"
)

func newScanner() *ReplyScanner { return NewReplyScanner(nil, nil, nil) }

// Подсказка про одну и ту же заметку схлопывается: бойкий тред называет себя на
// каждую реплику, а обходить его надо один раз.
func TestNudgeCollapsesPerNote(t *testing.T) {
	s := newScanner()
	now := time.Now()
	for i := 0; i < 5; i++ {
		s.Nudge(313095)
	}
	if got := s.dueNudged(now, NudgeBatch); len(got) != 1 || got[0] != 313095 {
		t.Fatalf("подсказки: got %v, want [313095]", got)
	}
	if got := s.dueNudged(now, NudgeBatch); len(got) != 0 {
		t.Errorf("та же заметка выдана дважды: %v", got)
	}
}

// Между двумя обходами одной заметки проходит NudgeGap: чаще, чем зеркало само
// заглядывает в тред, ходить туда незачем. Названная слишком рано заметка при
// этом не теряется — она дожидается своего срока.
func TestNudgeHonoursGap(t *testing.T) {
	s := newScanner()
	now := time.Now()
	s.Nudge(313095)
	s.dueNudged(now, NudgeBatch)

	s.Nudge(313095)
	if got := s.dueNudged(now.Add(NudgeGap/2), NudgeBatch); len(got) != 0 {
		t.Errorf("обход раньше срока: %v", got)
	}
	if got := s.dueNudged(now.Add(NudgeGap+time.Second), NudgeBatch); len(got) != 1 {
		t.Errorf("подсказка потеряна на ожидании: %v", got)
	}
}

// Заметка, дважды подряд отказавшая, подсказок больше не слушается. 500 сайт
// отдаёт на длинных тредах устойчиво, и повторять его каждые полминуты значит
// ждать по 45 секунд на каждую реплику; дальше её судьбу решает общая очередь,
// где счётчик неудач живёт в базе.
func TestNudgeMutesAfterFailures(t *testing.T) {
	s := newScanner()
	now := time.Now()
	for i := 0; i < NudgeFails; i++ {
		s.Nudge(313095)
		if got := s.dueNudged(now.Add(time.Duration(i)*(NudgeGap+time.Second)), NudgeBatch); len(got) != 1 {
			t.Fatalf("подсказка %d не выдана: %v", i, got)
		}
		s.nudgeFailed(313095)
	}
	s.Nudge(313095)
	if got := s.dueNudged(now.Add(time.Hour), NudgeBatch); len(got) != 0 {
		t.Errorf("замолчавшая заметка снова в очереди: %v", got)
	}

	// Удачный обход прощает прошлые отказы: 500 бывает и разовым.
	s.nudgeDone(313095)
	s.Nudge(313095)
	if got := s.dueNudged(now.Add(time.Hour), NudgeBatch); len(got) != 1 {
		t.Errorf("после удачного обхода подсказки не слышны: %v", got)
	}
}

// За сутки заметка перестаёт быть живой, и помнить про неё нечего: общая
// очередь свежих тредов её тоже больше не видит (FreshWindow).
func TestNudgeForgetsStaleNotes(t *testing.T) {
	s := newScanner()
	now := time.Now()
	s.Nudge(313095)
	s.dueNudged(now, NudgeBatch)
	s.dueNudged(now.Add(FreshWindow+time.Minute), NudgeBatch)
	if len(s.last) != 0 || len(s.fails) != 0 {
		t.Errorf("память о старой заметке осталась: last=%v fails=%v", s.last, s.fails)
	}
}
