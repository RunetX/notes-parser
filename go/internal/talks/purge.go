package talks

import (
	"context"
	"log/slog"
	"time"

	"lovegw/internal/store"
)

// PurgeLoop периодически удаляет сообщения talks старше retentionDays
// (приватность: в БД не копится переписка). Метаданные собеседников остаются —
// они лёгкие. Прогон раз в 12ч, первый — сразу на старте. retentionDays ≤ 0 —
// очистка выключена. Блокируется до отмены контекста.
func PurgeLoop(ctx context.Context, st *store.Store, retentionDays int, log *slog.Logger) error {
	if retentionDays <= 0 {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	purge := func() {
		cutoff := time.Now().AddDate(0, 0, -retentionDays)
		n, err := st.PurgeTalksOlderThan(ctx, cutoff)
		if err != nil {
			log.Error("retention talks", "err", err)
			return
		}
		if n > 0 {
			log.Info("retention talks: старые сообщения удалены", "n", n, "older_than_days", retentionDays)
		}
	}
	purge()
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			purge()
		}
	}
}
