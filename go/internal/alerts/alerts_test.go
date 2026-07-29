package alerts

import (
	"context"
	"testing"
)

func TestAlerterThrottle(t *testing.T) {
	ctx := context.Background()
	var sent []string
	a := New(func(_ context.Context, text string) { sent = append(sent, text) }, 3)

	a.Fail(ctx, "k", "d") // 1
	a.Fail(ctx, "k", "d") // 2
	if len(sent) != 0 {
		t.Fatalf("до порога алертов быть не должно: %v", sent)
	}
	a.Fail(ctx, "k", "boom") // 3 — срабатывает один раз
	a.Fail(ctx, "k", "d")    // 4 — молчит
	if len(sent) != 1 || sent[0] != "k: boom" {
		t.Fatalf("ровно один алерт при пороге: %v", sent)
	}
	a.OK(ctx, "k") // восстановление
	if len(sent) != 2 || sent[1] != "k: восстановилось" {
		t.Fatalf("восстановление: %v", sent)
	}
	a.OK(ctx, "k") // повторный успех — тишина
	if len(sent) != 2 {
		t.Fatalf("повторный ok слать не должен: %v", sent)
	}
}

func TestAlerterNilSendAndThreshold(t *testing.T) {
	ctx := context.Background()
	a := New(nil, 0) // nil-send + порог<1 поднимается до 1: не паникует
	a.Fail(ctx, "k", "d")
	a.OK(ctx, "k")
}
