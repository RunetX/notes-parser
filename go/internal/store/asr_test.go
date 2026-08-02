package store

import (
	"context"
	"testing"
)

func TestTranscriptCache(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if _, ok, err := st.Transcript(ctx, MessengerTelegram, "AgADXQ"); err != nil || ok {
		t.Fatalf("пустой кэш: ok=%v err=%v", ok, err)
	}
	if err := st.SaveTranscript(ctx, MessengerTelegram, "AgADXQ", "привет из треда", 7); err != nil {
		t.Fatal(err)
	}
	text, ok, err := st.Transcript(ctx, MessengerTelegram, "AgADXQ")
	if err != nil || !ok || text != "привет из треда" {
		t.Fatalf("кэш-хит: %q ok=%v err=%v", text, ok, err)
	}
	// Ключи не пересекаются между мессенджерами.
	if _, ok, _ := st.Transcript(ctx, MessengerMax, "AgADXQ"); ok {
		t.Error("расшифровка telegram видна в max")
	}
	// Повторная запись перезаписывает (перераспознали лучше).
	if err := st.SaveTranscript(ctx, MessengerTelegram, "AgADXQ", "уточнённый текст", 7); err != nil {
		t.Fatal(err)
	}
	if text, _, _ := st.Transcript(ctx, MessengerTelegram, "AgADXQ"); text != "уточнённый текст" {
		t.Errorf("после перезаписи: %q", text)
	}
}

func TestTryReserveASRQuota(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	const day = "2026-08-02"
	const limit = 100

	// Впритык — проходит.
	if ok, err := st.TryReserveASR(ctx, MessengerTelegram, 42, day, 60, limit); err != nil || !ok {
		t.Fatalf("первое списание: ok=%v err=%v", ok, err)
	}
	if ok, err := st.TryReserveASR(ctx, MessengerTelegram, 42, day, 40, limit); err != nil || !ok {
		t.Fatalf("списание впритык: ok=%v err=%v", ok, err)
	}
	if used, _ := st.ASRUsage(ctx, MessengerTelegram, 42, day); used != limit {
		t.Errorf("израсходовано: %d, ожидалось %d", used, limit)
	}
	// Сверх лимита — отказ, и расход не растёт.
	if ok, err := st.TryReserveASR(ctx, MessengerTelegram, 42, day, 1, limit); err != nil || ok {
		t.Fatalf("сверх лимита: ok=%v err=%v", ok, err)
	}
	if used, _ := st.ASRUsage(ctx, MessengerTelegram, 42, day); used != limit {
		t.Errorf("расход после отказа: %d", used)
	}

	// Другой день, другой пользователь и другой мессенджер считаются отдельно.
	for _, c := range []struct {
		name      string
		messenger string
		user      int64
		day       string
	}{
		{"следующий день", MessengerTelegram, 42, "2026-08-03"},
		{"другой пользователь", MessengerTelegram, 43, day},
		{"другой мессенджер", MessengerMax, 42, day},
	} {
		if ok, err := st.TryReserveASR(ctx, c.messenger, c.user, c.day, 90, limit); err != nil || !ok {
			t.Errorf("%s: ok=%v err=%v", c.name, ok, err)
		}
	}
}
