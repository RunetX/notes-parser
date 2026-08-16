package digest

// Пятничный планировщик в демоне: в слот выпуска строит черновик и материалы.
// Дальше развилка по `auto_publish`. В бою он включён — выпуск уходит в каналы
// сам (так вышли 2026-W31 и W32); ветка с ЛС админу и премодерацией через
// `lovegw digest publish` осталась для `auto_publish: false`. Кнопки
// «Опубликовать» нет намеренно: при автопубликации подтверждать нечего, а
// вернуть премодерацию — значит завести глагол по образцу `dmbot.cbNews`.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

	// LLM заполняет LLM-рубрики автоматически (nil — черновик с
	// плейсхолдерами под полуручной цикл). Ошибка редактуры не срывает
	// выпуск: пишется черновик с плейсхолдерами и зовётся админ.
	LLM JSONGenerator
	// AutoPublish — публиковать выпуск сразу после генерации через
	// Publishers. С настроенным LLM выпуск уходит с редактурой; без LLM —
	// «сухим» (LLM-секции выпадают). Если LLM настроен, но редактура не
	// удалась, автопубликация не выполняется — премодерация админа.
	AutoPublish bool
	Publishers  []Publisher
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

	var llmNote string
	if cfg.LLM != nil {
		if ed, err := GenerateEditorial(ctx, cfg.LLM, is); err != nil {
			log.Error("дайджест: LLM-редактура не удалась, откат на полуручной цикл",
				"week", w.ID, "err", err)
			llmNote = fmt.Sprintf(" LLM-редактура не удалась (%v) — рубрики нужно заполнить вручную.", err)
		} else {
			is.Editorial = ed
		}
	}

	draftPath, matPath, err := WriteIssueFiles(is, cfg.OutDir)
	if err != nil {
		return fmt.Errorf("выпуск %s: %w", w.ID, err)
	}
	log.Info("дайджест: черновик готов", "week", w.ID, "llm", is.Editorial != nil,
		"заметок", is.Stats.Notes, "комментариев", is.Stats.Comments, "draft", draftPath)

	// Автопубликация: с редактурой — всегда, «насухо» — только когда LLM не
	// настроен вовсе (сбой редактуры оставляет выпуск админу).
	if cfg.AutoPublish && (is.Editorial != nil || cfg.LLM == nil) {
		summary, err := publishDraft(ctx, st, cfg, w, draftPath)
		if err != nil {
			log.Error("дайджест: автопубликация не удалась", "week", w.ID, "err", err)
			notify(ctx, cfg, fmt.Sprintf(
				"📰 Дайджест %s готов (%s), но автопубликация сорвалась: %v. Докатите: lovegw digest publish",
				w.ID, draftPath, err))
			return nil
		}
		log.Info("дайджест: выпуск опубликован", "week", w.ID, "итог", summary)
		notify(ctx, cfg, fmt.Sprintf("📰 Дайджест %s опубликован автоматически: %s.%s Черновик: %s",
			w.ID, summary, llmNote, draftPath))
		return nil
	}

	notify(ctx, cfg, fmt.Sprintf(
		"📰 Дайджест %s готов: %s (материалы: %s).%s Проверьте и опубликуйте: lovegw digest publish",
		w.ID, draftPath, matPath, llmNote))
	return nil
}

func notify(ctx context.Context, cfg ScheduleConfig, text string) {
	if cfg.Notify != nil {
		cfg.Notify(ctx, text)
	}
}

// publishDraft публикует свежесобранный черновик во все приёмники и
// возвращает сводку по частям. Частичный сбой безопасен: публикация
// идемпотентна, admin докатывает командой digest publish.
func publishDraft(ctx context.Context, st *store.Store, cfg ScheduleConfig, w Window, draftPath string) (string, error) {
	if len(cfg.Publishers) == 0 {
		return "", errors.New("нет приёмников публикации")
	}
	f, err := os.Open(draftPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	d, err := ParseDraft(f, false) // не-strict: «сухой» выпуск тоже публикуем
	if err != nil {
		return "", err
	}
	var parts []string
	for _, p := range cfg.Publishers {
		sent, err := Publish(ctx, st, p, d, w.ID, cfg.SiteBase)
		if err != nil {
			return "", fmt.Errorf("%s: %w", p.Name(), err)
		}
		parts = append(parts, fmt.Sprintf("%s — %d ч.", p.Name(), sent))
	}
	summary := strings.Join(parts, ", ")
	if d.Dropped > 0 {
		summary += fmt.Sprintf(" (без LLM-рубрик: %d секций выпало)", d.Dropped)
	}
	return summary, nil
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

// writeFile пишет файл целиком или не пишет вовсе: сперва во временный, потом
// переименованием. Обрыв на полуслове оставлял бы обрезанный черновик, а
// processSlot считает его наличие признаком «черновик уже есть» — то есть
// обрезок заблокировал бы пересборку слота, и правит его человек руками.
func writeFile(path string, write func(*os.File) error) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := write(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// На Windows rename поверх существующего файла падает; писатель один,
	// гонки здесь нет.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
