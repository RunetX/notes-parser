package digest

// Пятничный планировщик в демоне: в слот выпуска строит черновик и материалы
// и зовёт админа; публикация остаётся за админом (lovegw digest publish) —
// премодерация до стабилизации тона. Кнопка «Опубликовать» появится вместе с
// callback-инфраструктурой ботов (эпик A бэклога).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"lovegw/internal/store"
)

// ScheduleConfig — параметры планировщика выпусков.
type ScheduleConfig struct {
	Loc      *time.Location
	Weekday  time.Weekday
	Hour     int
	OutDir   string
	SiteBase string
	Grace    time.Duration                          // окно догона пропущенного слота; 0 — 48h
	Notify   func(ctx context.Context, text string) // ЛС админу (может быть nil)
}

const defaultGrace = 48 * time.Hour

// DraftPath / MaterialsPath — имена файлов выпуска в каталоге черновиков
// (общие для CLI и планировщика).
func DraftPath(dir, weekID string) string {
	return filepath.Join(dir, "digest-"+weekID+".draft.txt")
}

func MaterialsPath(dir, weekID string) string {
	return filepath.Join(dir, "digest-"+weekID+".materials.md")
}

// RunSchedule ждёт слоты выпуска и обрабатывает каждый (на старте — догон
// последнего, если он в пределах Grace). Ошибка слота не валит демон —
// логируется и ждём следующий. Блокируется до отмены контекста.
func RunSchedule(ctx context.Context, st *store.Store, cfg ScheduleConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Grace <= 0 {
		cfg.Grace = defaultGrace
	}
	for {
		if err := processSlot(ctx, st, cfg, time.Now(), log); err != nil {
			log.Error("дайджест: слот не обработан", "err", err)
		}
		// Таймер до следующего слота, не тикер: слот привязан к локальному
		// времени, интервал пересчитывается после каждого срабатывания.
		timer := time.NewTimer(time.Until(NextSlot(time.Now(), cfg.Loc, cfg.Weekday, cfg.Hour)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// slotAction — решение по последнему прошедшему слоту.
type slotAction int

const (
	slotDraft         slotAction = iota // строить черновик
	slotSkipPublished                   // выпуск уже опубликован
	slotSkipDrafted                     // черновик уже лежит (возможно, правится)
	slotSkipOld                         // слот старше Grace — задним числом не публикуем
)

// decideSlot — чистое решение по слоту (тестируемость). Порядок важен:
// готовый выпуск и лежащий черновик не считаются «пропущенным слотом».
func decideSlot(now time.Time, w Window, grace time.Duration, published, drafted bool) slotAction {
	switch {
	case published:
		return slotSkipPublished
	case drafted:
		return slotSkipDrafted
	case now.Sub(w.End) > grace:
		return slotSkipOld
	default:
		return slotDraft
	}
}

// processSlot обрабатывает последний прошедший слот: если выпуск не готов и
// слот не протух — строит черновик, пишет файлы и зовёт админа.
func processSlot(ctx context.Context, st *store.Store, cfg ScheduleConfig, now time.Time, log *slog.Logger) error {
	w := SlotFor(now, cfg.Loc, cfg.Weekday, cfg.Hour, 0)
	published := false
	for _, m := range []string{store.MessengerTelegram, store.MessengerMax} {
		_, _, found, err := st.Target(ctx, m, store.TargetDigest, w.ID)
		if err != nil {
			return err
		}
		if found {
			published = true
		}
	}
	_, statErr := os.Stat(DraftPath(cfg.OutDir, w.ID))
	drafted := statErr == nil

	switch decideSlot(now, w, cfg.Grace, published, drafted) {
	case slotSkipPublished, slotSkipDrafted:
		return nil
	case slotSkipOld:
		log.Warn("дайджест: слот пропущен, догонять поздно", "week", w.ID, "slot", w.End)
		return nil
	}

	is, err := Build(ctx, st, w, cfg.SiteBase)
	if err != nil {
		return fmt.Errorf("выпуск %s: %w", w.ID, err)
	}
	draftPath, matPath, err := WriteIssueFiles(is, cfg.OutDir)
	if err != nil {
		return fmt.Errorf("выпуск %s: %w", w.ID, err)
	}
	log.Info("дайджест: черновик готов", "week", w.ID,
		"заметок", is.Stats.Notes, "комментариев", is.Stats.Comments, "draft", draftPath)
	if cfg.Notify != nil {
		cfg.Notify(ctx, fmt.Sprintf(
			"📰 Дайджест %s готов: %s (материалы: %s). Проверьте и опубликуйте: lovegw digest publish",
			w.ID, draftPath, matPath))
	}
	return nil
}

// WriteIssueFiles пишет черновик и материалы выпуска в dir (создавая его).
func WriteIssueFiles(is *Issue, dir string) (draftPath, materialsPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	draftPath = DraftPath(dir, is.Window.ID)
	materialsPath = MaterialsPath(dir, is.Window.ID)
	if err := writeFile(draftPath, func(f *os.File) error { return WriteDraft(f, is) }); err != nil {
		return "", "", err
	}
	if err := writeFile(materialsPath, func(f *os.File) error { return WriteMaterials(f, is) }); err != nil {
		return "", "", err
	}
	return draftPath, materialsPath, nil
}

func writeFile(path string, write func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
