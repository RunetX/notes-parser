package main

// Сборка службы народа (эпик «народ», этап 4).
//
// Здесь сходятся три мира, которые по отдельности друг о друге не знают: ядро
// эмуляции (narod), площадка (platform через platnarod) и клиент модели. Место
// для этого одно — cmd: только он и так знает всех троих, и только здесь можно
// собрать развилку провайдера, не протаскивая её ни в ядро, ни в площадку.
//
// РАЗВИЛКА ПРОВАЙДЕРА — по ПРОИСХОЖДЕНИЮ КОНТЕКСТА, а не по вкусу и не по цене.
// Пока песочница закрыта, в промпт попадают только карточки, архивные числа и
// тексты самих жителей — живых участников там нет ни строки, — и значит Claude
// законен. В тот день, когда песочницу откроют аудитории, в тред придут реплики
// живых людей, а согласие обещает не вывозить их за пределы России; тем же
// доводом 23.08.2026 автомат модерации уехал с Claude на Yandex. Поэтому клиент
// выбирается ЗДЕСЬ и по одному признаку, а не размазан по вызовам.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lovegw/internal/config"
	"lovegw/internal/narod"
	"lovegw/internal/platnarod"
	"lovegw/internal/store"
)

// setupNarod поднимает службу народа. Ошибку Run наружу не отдаём — народ не
// критичен для зеркалирования, и ронять им канал значило бы поменять местами
// цену вопроса.
func (d *daemon) setupNarod() error {
	cfg, log := d.cfg, d.log
	if !cfg.Narod.Enabled {
		return nil
	}
	if d.plat == nil {
		// Не отказ демона: площадка могла не подняться из-за разошедшейся схемы,
		// и это уже сказано алертом. Второй раз про то же кричать незачем.
		log.Warn("народ не подключён: площадка выключена или не поднялась")
		return nil
	}
	players, err := loadPlayers(cfg)
	if err != nil {
		return fmt.Errorf("народ: %w", err)
	}
	if len(players) == 0 {
		log.Warn("народ не подключён: в каталоге нет ни одного жителя",
			"каталог", cfg.Narod.CardsDir)
		return nil
	}
	world, err := narod.OpenWorld(context.Background(), cfg.Narod.DBPath)
	if err != nil {
		return fmt.Errorf("народ: %w", err)
	}
	d.narodWorld = world

	gen, model, err := narodGenerator(cfg)
	if err != nil {
		return fmt.Errorf("народ: %w", err)
	}
	svc, err := narod.NewService(narodConfig(cfg), world, platnarod.New(d.plat),
		gen, players, log)
	if err != nil {
		world.Close()
		d.narodWorld = nil
		return fmt.Errorf("народ: %w", err)
	}
	svc.SetSeed(uint64(max(cfg.Narod.Seed, 1)))
	svc.SetModel("anthropic", model)
	// Тумблер спрашивается У БАЗЫ на каждом такте, а не читается один раз: между
	// нажатием кнопки и рестартом демона проходят дни, и выключатель, который
	// начнёт действовать только после перезапуска, — не выключатель.
	svc.SetGate(func(ctx context.Context) bool {
		v, found, err := d.st.Flag(ctx, store.FlagNarodEnabled)
		if err != nil {
			log.Error("народ: не прочитан тумблер", "err", err)
			return false // молчать безопаснее, чем говорить вслепую
		}
		// Отсутствие флага значит «включён»: сама служба поднимается только при
		// narod.enabled в конфиге, и второй раз спрашивать разрешения незачем.
		return !found || v != "0"
	})
	d.starts = append(d.starts, func(ctx context.Context) error {
		if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("народ остановлен", "err", err)
		}
		return nil
	})
	// Ручка /narod — админам тех мессенджеров, где есть ЛС-бот команд. Как и все
	// Set*, ставится до стартов поллеров (фаза wire).
	ctrl := &narodControl{world: world, st: d.st, mode: cfg.Narod.Mode, cards: len(players)}
	if d.dm != nil && cfg.Messengers.Telegram.AdminUserID != 0 {
		d.dm.SetNarod(ctrl, cfg.Messengers.Telegram.AdminUserID)
	}
	if d.maxDM != nil && cfg.Messengers.Max.AdminUserID != 0 {
		d.maxDM.SetNarod(ctrl, cfg.Messengers.Max.AdminUserID)
	}
	log.Info("народ подключён", "режим", cfg.Narod.Mode, "жителей", len(players),
		"мир", cfg.Narod.DBPath)
	return nil
}

// narodConfig переводит секцию конфига в настройки службы. Нули отдаются
// умолчаниям ядра: там у каждого числа записан довод, а конфиг, забытый в
// половине полей, не должен обнулять потолки.
func narodConfig(cfg *config.Config) narod.Config {
	c := narod.Defaults()
	n := cfg.Narod
	if n.Mode != "" {
		c.Mode = n.Mode
	}
	if n.ScanEveryS > 0 {
		c.ScanEvery = time.Duration(n.ScanEveryS) * time.Second
	}
	if n.WorkEveryS > 0 {
		c.WorkEvery = time.Duration(n.WorkEveryS) * time.Second
	}
	if n.PerPersonaHour > 0 {
		c.PerPersonaHour = n.PerPersonaHour
	}
	if n.PerPersonaDay > 0 {
		c.PerPersonaDay = n.PerPersonaDay
	}
	if n.PerThread > 0 {
		c.PerThread = n.PerThread
	}
	if n.ThreadCloseH > 0 {
		c.ThreadCloseAfter = time.Duration(n.ThreadCloseH) * time.Hour
	}
	if n.PlanCapH > 0 {
		c.PlanCap = time.Duration(n.PlanCapH) * time.Hour
	}
	if n.LatencyScale > 0 {
		c.LatencyScale = n.LatencyScale
	}
	if n.DayCalls > 0 {
		c.DayCalls = n.DayCalls
	}
	return c
}

// narodGenerator — клиент модели для реплик жителей.
//
// Ветка сейчас одна — Claude, — и это состояние закрытой песочницы, а не
// упрощение: живых текстов в промпте нет по построению (в тред пишут только
// жители и администратор), значит вывозить за пределы России нечего. Развилка
// именно здесь: открывая песочницу аудитории, менять придётся ровно одну эту
// функцию, а не искать по коду все места, где зовётся модель.
func narodGenerator(cfg *config.Config) (narod.JSONGenerator, string, error) {
	gen, err := llmClientFor(cfg, cfg.Narod.Model, "", 90*time.Second)
	if err != nil {
		return nil, "", err
	}
	model := cfg.Narod.Model
	if model == "" {
		model = cfg.LLM.Model
	}
	return gen, model, nil
}

// loadPlayers читает каталог карточек и связывает их с анкетами площадки.
//
// Анкету жителю заводит `narod enroll`, и она лежит в МИРЕ (actors), а не в
// карточке: карточка — документ, который правят руками и кладут в git-игнор, а
// номер анкеты выдаёт Postgres. Жителю без анкеты служба ничего не поручает и
// молчит об этом в лог: это нормальное состояние между «карточку положили» и
// «жителя завели».
func loadPlayers(cfg *config.Config) ([]narod.Player, error) {
	cards, err := narod.LoadCards(cfg.Narod.CardsDir)
	if err != nil {
		return nil, err
	}
	// В live выходят ТОЛЬКО композиты, и это второе из двух мест, где правило
	// записано (первое — narod.NewService). Дублирование намеренное: между двумя
	// проверками лежит вся сборка, а цена ошибки — публикация под манерой письма
	// живого человека.
	if cfg.Narod.Mode == "live" {
		if err := narod.CheckLive(cards); err != nil {
			return nil, err
		}
	}
	world, err := narod.OpenWorld(context.Background(), cfg.Narod.DBPath)
	if err != nil {
		return nil, err
	}
	defer world.Close()
	actors, err := world.Actors(context.Background())
	if err != nil {
		return nil, err
	}
	byID := make(map[string]int64, len(actors))
	for _, a := range actors {
		byID[a.ID] = a.PlatformUserID
	}
	out := make([]narod.Player, 0, len(cards))
	for i := range cards {
		out = append(out, narod.Player{Card: &cards[i], UserID: byID[cards[i].ID]})
	}
	return out, nil
}

// narodControl — ручка /narod: тумблер и отчёт.
//
// Живёт она ЗДЕСЬ, а не в narod.Service, и это не мелочь: тумблер лежит в боевой
// SQLite демона, а ядро эмуляции про store не знает и знать не должно — иначе
// реплей на машине разработчика потребовал бы боевую базу. Здесь же сходятся
// оба: мир (что происходит) и store (можно ли сейчас).
type narodControl struct {
	world *narod.World
	st    *store.Store
	mode  string
	cards int
}

// NarodStatus — отчёт админу.
//
// Собирается он из МИРА, а не из памяти службы: демон рестартует, а вопрос
// «сколько народ сказал за сутки и почему молчит» задают как раз после
// рестарта. Последние попытки идут с причинами — в этом весь смысл журнала
// генерации: «промолчал» и «сломалось» обязаны различаться на глаз.
func (c *narodControl) NarodStatus(ctx context.Context) (string, bool) {
	enabled := c.enabled(ctx)
	var b strings.Builder
	state := "выключен"
	if enabled {
		state = "работает"
	}
	fmt.Fprintf(&b, "<b>Народ</b>: %s, режим %s, жителей %d\n", state, c.mode, c.cards)
	if c.mode == narod.ModeDryRun {
		b.WriteString("Сухой прогон: мир движется, на площадку не уходит ничего.\n")
	}
	if spent, err := c.world.SpentOn(ctx, time.Now()); err == nil {
		fmt.Fprintf(&b, "Обращений к модели за сутки: %d\n", spent)
	}
	runs, err := c.world.GenRuns(ctx, 5)
	if err != nil {
		return b.String(), enabled
	}
	if len(runs) == 0 {
		b.WriteString("\nЖители ещё не говорили.")
		return b.String(), enabled
	}
	b.WriteString("\nПоследнее:\n")
	for _, r := range runs {
		line := r.Reason
		if r.Verdict == narod.GenPosted {
			line = firstRunes(r.Text, 60)
		}
		fmt.Fprintf(&b, "· %s — %s: %s\n", r.At.Format("02.01 15:04"), r.ActorID, line)
	}
	return b.String(), enabled
}

// SetNarodEnabled переключает тумблер.
func (c *narodControl) SetNarodEnabled(ctx context.Context, on bool, by string) error {
	value := "0"
	if on {
		value = "1"
	}
	return c.st.SetFlag(ctx, store.FlagNarodEnabled, value, by, time.Now())
}

func (c *narodControl) enabled(ctx context.Context) bool {
	v, found, err := c.st.Flag(ctx, store.FlagNarodEnabled)
	if err != nil {
		return false
	}
	return !found || v != "0"
}

// firstRunes — начало реплики для отчёта. По рунам, а не по байтам: русский
// текст, обрезанный по байту, приезжает в мессенджер с битым знаком.
func firstRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
