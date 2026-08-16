package alerts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPollWatchFiresOnceAndRecovers(t *testing.T) {
	var sent []string
	now := time.Unix(1000, 0)
	w := NewPollWatch("Telegram (постер)", func(_ context.Context, text string) {
		sent = append(sent, text)
	})
	w.now = func() time.Time { return now }
	conflict := errors.New("conflict: terminated by other getUpdates request")

	// Полоса быстрых ошибок (шаг 5 с): один алерт, не по алерту на ошибку.
	for i := 0; i < pollStreakCount+5; i++ {
		w.Error(context.Background(), conflict)
		now = now.Add(5 * time.Second)
	}
	if len(sent) != 1 {
		t.Fatalf("ожидался один алерт, получено %d: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], "409") || !strings.Contains(sent[0], "постер") {
		t.Errorf("текст алерта: %q", sent[0])
	}

	// Пауза длиннее pollStreakGap = были успешные опросы: следующая ошибка
	// приносит «восстановился» и открывает новую полосу, не алерт.
	now = now.Add(pollStreakGap + time.Minute)
	w.Error(context.Background(), errors.New("timeout"))
	if len(sent) != 2 || !strings.Contains(sent[1], "восстановился") {
		t.Fatalf("ожидалось восстановление, получено: %v", sent)
	}

	// Одиночные редкие ошибки полосу не образуют.
	for i := 0; i < 5; i++ {
		now = now.Add(pollStreakGap + time.Minute)
		w.Error(context.Background(), errors.New("timeout"))
	}
	if len(sent) != 2 {
		t.Fatalf("редкие ошибки не должны алертить: %v", sent[2:])
	}
}

func TestPollWatchFiresByStreakAge(t *testing.T) {
	var sent []string
	now := time.Unix(1000, 0)
	w := NewPollWatch("x", func(_ context.Context, text string) { sent = append(sent, text) })
	w.now = func() time.Time { return now }

	// Медленные ошибки (шаг 35 с, как при таймаутах long poll): порог по
	// числу не достигается, но полоса длиннее pollStreakAge поднимает алерт.
	for i := 0; i < 5; i++ {
		w.Error(context.Background(), errors.New("timeout"))
		now = now.Add(35 * time.Second)
	}
	if len(sent) != 1 {
		t.Fatalf("ожидался один алерт по длительности полосы, получено %d: %v", len(sent), sent)
	}
}
