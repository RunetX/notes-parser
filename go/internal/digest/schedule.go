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
	// Data — по чему считается выпуск: зеркало НГС или площадка. С 22.08.2026
	// это разные базы, и выбирает вызывающий: сводку публикуют на площадке,
	// значит и считать её надо по тому, что человек видит вокруг выпуска.
	Data    Source
	Loc     *time.Location
	Weekday time.Weekday
	Hour    int
	OutDir  string
	Grace   time.Duration                          // окно догона пропущенного слота; 0 — 48h
	Notify  func(ctx context.Context, text string) // ЛС админу (может быть nil)

	// LLM заполняет LLM-рубрики автоматически (nil — черновик с
	// плейсхолдерами под полуручной цикл). Ошибка редактуры не срывает
	// выпуск: пишется черновик с плейсхолдерами и зовётся админ.
	LLM JSONGenerator
	// AutoPublish — публиковать выпуск сразу после генерации. С настроенным LLM
	// выпуск уходит с редактурой; без LLM — «сухим» (LLM-секции выпадают). Если
	// LLM настроен, но редактура не удалась, автопубликация не выполняется —
	// премодерация админа.
	AutoPublish bool

	// Site — площадка. Есть она — выпуск выходит ЗДЕСЬ нативной заметкой, а в
	// Telegram и MAX его несёт исходящий обход (`platout`), как всякую
	// написанную здесь: один текст, один адрес, один тред.
	//
	// Нет её (площадка не настроена) — остаётся прежний путь, публикация прямо
	// в каналы через Publishers. Это не запасной сценарий на случай сбоя, а
	// работа без площадки вообще: без неё заметке взяться неоткуда.
	Site Site
	// SiteBaseURL — адрес ПЛОЩАДКИ: по нему собираются ссылки на заметки и в
	// теле выпуска-заметки, и в постах прямой публикации в каналы. Адрес НГС
	// здесь стоял до 27.08.2026 (ссылок на НГС проект не ставит нигде); пусто —
	// подписи в выпуске остаются текстом, а это ровно случай «площадки нет».
	SiteBaseURL string
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

// issuePublished — вышел ли уже выпуск этой недели. Спрашивать надо ровно то
// место, куда он выходит СЕЙЧАС: с площадкой это её отметка, без неё — отметки
// мессенджеров. Перепутав, планировщик либо выпустит второй раз, либо сочтёт
// неделю закрытой.
func issuePublished(ctx context.Context, st *store.Store, cfg ScheduleConfig, weekID string) (bool, error) {
	if cfg.Site != nil {
		_, _, found, err := st.Target(ctx, store.MessengerPlatform, store.TargetDigest, weekID)
		return found, err
	}
	for _, m := range []string{store.MessengerTelegram, store.MessengerMax} {
		_, _, found, err := st.Target(ctx, m, store.TargetDigest, weekID)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// draftState — что лежит на диске под этот слот.
type draftState int

const (
	draftNone    draftState = iota // черновика нет
	draftPending                   // черновик есть, но рубрики не заполнены
	draftReady                     // заполнен целиком — можно публиковать
)

// draftStateOf читает черновик слота. Готовность меряется ТОЙ ЖЕ разборкой,
// что и публикация (ParseDraft, счётчик Dropped), а не поиском строки в файле:
// второй ответ на вопрос «выпуск готов?» однажды разошёлся бы с первым.
//
// Нечитаемый черновик — это draftPending, а не ошибка: файл правит рука, и
// застать его посреди правки законно. Наше дело тогда одно — не трогать.
func draftStateOf(path string) (draftState, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return draftNone, nil
	}
	if err != nil {
		return draftNone, err
	}
	defer f.Close()
	d, err := ParseDraft(f, false)
	if err != nil || d.Dropped > 0 {
		return draftPending, nil
	}
	return draftReady, nil
}

// slotAction — решение по последнему прошедшему слоту.
type slotAction int

const (
	slotDraft         slotAction = iota // строить черновик
	slotPublishReady                    // черновик готов заранее — публиковать
	slotSkipPublished                   // выпуск уже опубликован
	slotSkipDrafted                     // черновик лежит недоделанным — правится
	slotSkipOld                         // слот старше Grace — задним числом не публикуем
)

// decideSlot — чистое решение по слоту (тестируемость). Порядок важен:
// готовый выпуск и лежащий черновик не считаются «пропущенным слотом».
//
// Готовый ЗАРАНЕЕ черновик публикуется в слот — это дорога «выпуск собирают
// накануне, а таймер отправляет в назначенный час». Заведена она 12.09.2026,
// когда канал до модели слёг и редактура перестала получаться на хосте вовсе;
// рука при этом остаётся в силе и в обычные недели ничего не меняет.
//
// Грань между «готов» и «правится» проводит сам черновик: незаполненный
// плейсхолдер значит, что работа не кончена, и такой файл по-прежнему
// неприкосновенен. Просроченный (старше Grace) не публикуется даже готовым —
// задним числом выпуск не выходит, это правило Grace, и рука его перебивает
// командой `digest publish`.
func decideSlot(now time.Time, w Window, grace time.Duration, published bool, ds draftState) slotAction {
	switch {
	case published:
		return slotSkipPublished
	case ds == draftReady && now.Sub(w.End) <= grace:
		return slotPublishReady
	case ds != draftNone:
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
	published, err := issuePublished(ctx, st, cfg, w.ID)
	if err != nil {
		return err
	}
	ds, err := draftStateOf(DraftPath(cfg.OutDir, w.ID))
	if err != nil {
		return err
	}

	switch decideSlot(now, w, cfg.Grace, published, ds) {
	case slotSkipPublished, slotSkipDrafted:
		return nil
	case slotSkipOld:
		log.Warn("дайджест: слот пропущен, догонять поздно", "week", w.ID, "slot", w.End)
		return nil
	case slotPublishReady:
		return publishPrepared(ctx, st, cfg, w, log)
	}

	is, err := Build(ctx, cfg.Data, w)
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
		summary, _, err := publishDraft(ctx, st, cfg, w, draftPath)
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

// publishPrepared публикует черновик, приготовленный ЗАРАНЕЕ. Ничего не
// пересобирает: в файле лежит ровно то, что человек видел и одобрил, а второй
// сбор дал бы другие числа и, вполне возможно, другую заметку недели.
func publishPrepared(ctx context.Context, st *store.Store, cfg ScheduleConfig, w Window, log *slog.Logger) error {
	draftPath := DraftPath(cfg.OutDir, w.ID)
	if !cfg.AutoPublish {
		// Черновик приготовил админ, публикует тоже он — сказать ему тут
		// нечего. И молчать здесь важно: слот обрабатывается ещё и на старте
		// демона, так что ЛС «выпуск готов» уходило бы на каждый рестарт.
		log.Debug("дайджест: готовый черновик ждёт руки", "week", w.ID, "draft", draftPath)
		return nil
	}
	summary, done, err := publishDraft(ctx, st, cfg, w, draftPath)
	if err != nil {
		log.Error("дайджест: готовый черновик не опубликован", "week", w.ID, "err", err)
		notify(ctx, cfg, fmt.Sprintf(
			"📰 Дайджест %s был готов, но публикация сорвалась: %v. Докатите: lovegw digest publish",
			w.ID, err))
		return nil
	}
	if !done {
		// Выпуск уже стоял: тик повторный, читателю ничего не досталось —
		// и владельцу говорить не о чем.
		log.Debug("дайджест: готовый черновик уже был опубликован", "week", w.ID, "итог", summary)
		return nil
	}
	log.Info("дайджест: опубликован готовый черновик", "week", w.ID, "итог", summary)
	notify(ctx, cfg, fmt.Sprintf("📰 Дайджест %s опубликован: %s Черновик: %s", w.ID, summary, draftPath))
	return nil
}

func notify(ctx context.Context, cfg ScheduleConfig, text string) {
	if cfg.Notify != nil {
		cfg.Notify(ctx, text)
	}
}

// publishDraft публикует свежесобранный черновик и возвращает сводку. Частичный
// сбой безопасен: публикация идемпотентна, админ докатывает командой
// digest publish.
//
// done говорит, ушло ли что-то НА САМОМ ДЕЛЕ: идемпотентность делает повторный
// заход безвредным для читателя, но не для владельца — без этого признака
// второй тик слал бы ему ЛС «выпуск опубликован» о выпуске, опубликованном
// в прошлый раз.
func publishDraft(ctx context.Context, st *store.Store, cfg ScheduleConfig, w Window, draftPath string) (summary string, done bool, err error) {
	d, err := readDraft(draftPath)
	if err != nil {
		return "", false, err
	}
	if cfg.Site != nil {
		summary, done, err = publishToSite(ctx, st, cfg, w, d)
	} else {
		summary, done, err = publishToChannels(ctx, st, cfg, w, d)
	}
	if err != nil {
		return "", false, err
	}
	if d.Dropped > 0 {
		summary += fmt.Sprintf(" (без LLM-рубрик: %d секций выпало)", d.Dropped)
	}
	return summary, done, nil
}

func readDraft(path string) (Draft, error) {
	f, err := os.Open(path)
	if err != nil {
		return Draft{}, err
	}
	defer f.Close()
	return ParseDraft(f, false) // не-strict: «сухой» выпуск тоже публикуем
}

// publishToSite — основной путь: выпуск выходит заметкой на площадке, в каналы
// его несёт исходящий обход. Незакреплённый выпуск — не повод считать
// публикацию неудавшейся, поэтому про закреп только сообщают.
func publishToSite(ctx context.Context, st *store.Store, cfg ScheduleConfig, w Window, d Draft) (string, bool, error) {
	noteID, created, err := PublishPlatform(ctx, st, cfg.Site, d, w.ID, cfg.SiteBaseURL)
	if err != nil {
		return "", false, err
	}
	if !created {
		return fmt.Sprintf("площадка — выпуск уже был опубликован (заметка %d)", noteID), false, nil
	}
	summary := fmt.Sprintf("площадка — заметка %d, в каналы отнесёт исходящий обход", noteID)
	if err := PinIssue(ctx, st, cfg.Site, noteID); err != nil {
		summary += fmt.Sprintf("; закрепить не вышло: %v", err)
	}
	return summary, true, nil
}

// publishToChannels — работа без площадки: выпуск уходит прямо в каналы своим
// сплитом и своими per-sink ссылками на треды.
func publishToChannels(ctx context.Context, st *store.Store, cfg ScheduleConfig, w Window, d Draft) (string, bool, error) {
	if len(cfg.Publishers) == 0 {
		return "", false, errors.New("нет приёмников публикации")
	}
	var parts []string
	var done bool
	for _, p := range cfg.Publishers {
		sent, err := Publish(ctx, st, p, d, w.ID, cfg.SiteBaseURL)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", p.Name(), err)
		}
		if sent > 0 {
			done = true
		}
		parts = append(parts, fmt.Sprintf("%s — %d ч.", p.Name(), sent))
	}
	return strings.Join(parts, ", "), done, nil
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
