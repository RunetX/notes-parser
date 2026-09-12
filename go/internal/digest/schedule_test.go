package digest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

func TestDecideSlotPrecedence(t *testing.T) {
	w := testWindow()
	fresh := w.End.Add(time.Hour)
	stale := w.End.Add(72 * time.Hour)
	cases := []struct {
		name      string
		now       time.Time
		published bool
		draft     draftState
		want      slotAction
	}{
		{"свежий слот — строить", fresh, false, draftNone, slotDraft},
		{"опубликован — пропуск даже свежего", fresh, true, draftNone, slotSkipPublished},
		{"черновик недоделан — не перетирать", fresh, false, draftPending, slotSkipDrafted},
		{"протух — задним числом не строим", stale, false, draftNone, slotSkipOld},
		{"протух, но опубликован — это не «пропущенный»", stale, true, draftNone, slotSkipPublished},
		{"протух, но черновик есть — правится", stale, false, draftPending, slotSkipDrafted},
		// Дорога «собрали накануне — таймер отправил в назначенный час».
		{"готов заранее — публиковать", fresh, false, draftReady, slotPublishReady},
		{"готов, но уже опубликован — не второй раз", fresh, true, draftReady, slotSkipPublished},
		{"готов, но протух — задним числом не публикуем", stale, false, draftReady, slotSkipDrafted},
	}
	for _, tc := range cases {
		if got := decideSlot(tc.now, w, defaultGrace, tc.published, tc.draft); got != tc.want {
			t.Errorf("%s: %v, ожидалось %v", tc.name, got, tc.want)
		}
	}
}

// Готовность черновика меряется тем же разбором, что и публикация: файл с
// незаполненным плейсхолдером — работа не кончена, и трогать его нельзя.
func TestDraftStateOf(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const head = "<b>📰 Дайджест недели</b> · 05.09–12.09\n---\n"
	cases := []struct {
		name string
		path string
		want draftState
	}{
		{"файла нет", filepath.Join(dir, "нет.txt"), draftNone},
		{"плейсхолдер на месте", write("p.txt", head+llmWeekSummary+"\n"), draftPending},
		{"рубрики заполнены", write("r.txt", head+"Неделя прошла в спорах.\n"), draftReady},
	}
	for _, tc := range cases {
		got, err := draftStateOf(tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: %v, ожидалось %v", tc.name, got, tc.want)
		}
	}
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func scheduleCfg(t *testing.T, st *store.Store, notify func(context.Context, string)) ScheduleConfig {
	t.Helper()
	return ScheduleConfig{
		Data:        NewStoreSource(st, siteBase),
		Loc:         nsk,
		Weekday:     time.Friday,
		Hour:        19,
		OutDir:      t.TempDir(),
		SiteBaseURL: "https://t3h.ru",
		Grace:       defaultGrace,
		Notify:      notify,
	}
}

func TestProcessSlotDraftsOnceAndNotifies(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk) // полчаса после слота W31

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("уведомлений: %d (%v)", len(notes), notes)
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err != nil {
		t.Fatalf("черновик не создан: %v", err)
	}
	if _, err := os.Stat(MaterialsPath(cfg.OutDir, "2026-W31")); err != nil {
		t.Fatalf("материалы не созданы: %v", err)
	}

	// Повторный тик того же слота: черновик лежит — не трогаем, не спамим.
	if err := processSlot(ctx, st, cfg, now.Add(time.Hour), quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("повторное уведомление: %v", notes)
	}
}

func TestProcessSlotSkipsPublished(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// Выпуск W31 уже опубликован в один из мессенджеров.
	if err := st.SetTarget(ctx, store.MessengerMax, store.TargetDigest, "2026-W31", "m1", ""); err != nil {
		t.Fatal(err)
	}
	called := false
	cfg := scheduleCfg(t, st, func(context.Context, string) { called = true })
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("опубликованный выпуск не должен перегенерироваться")
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err == nil {
		t.Error("черновик не должен создаваться для опубликованного выпуска")
	}
}

func TestProcessSlotAutoPublishWithLLM(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	cfg.LLM = &fakeGen{resp: map[string]string{
		"week_summary": "Неделя была тихой.", "dispute_note_id": "",
		"dispute": "", "quote": "", "topics": "",
	}}
	pub := &fakePub{name: "tg"}
	cfg.AutoPublish = true
	cfg.Publishers = []Publisher{pub}
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(pub.posts) != 1 || !strings.Contains(pub.posts[0], "Неделя была тихой.") {
		t.Fatalf("выпуск с LLM-текстом должен быть опубликован: %q", pub.posts)
	}
	if _, _, done, _ := st.Target(ctx, "tg", store.TargetDigest, "2026-W31"); !done {
		t.Error("после автопубликации должна стоять головная запись выпуска")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "опубликован автоматически") {
		t.Errorf("уведомление: %v", notes)
	}
	// Повторный тик: выпуск опубликован — ничего не делаем.
	if err := processSlot(ctx, st, cfg, now.Add(time.Hour), quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(pub.posts) != 1 || len(notes) != 1 {
		t.Errorf("повторный тик не должен публиковать/спамить: posts=%d notes=%d", len(pub.posts), len(notes))
	}
}

func TestProcessSlotLLMFailureFallsBackToManual(t *testing.T) {
	noBackoff(t) // отказ запроса теперь повторяется: тест про откат, а не про темп
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	cfg.LLM = &fakeGen{err: errors.New("api недоступен")}
	pub := &fakePub{name: "tg"}
	cfg.AutoPublish = true
	cfg.Publishers = []Publisher{pub}
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(pub.posts) != 0 {
		t.Error("при сбое LLM автопубликация не выполняется — премодерация")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "не удалась") ||
		!strings.Contains(notes[0], "digest publish") {
		t.Errorf("уведомление о полуручном цикле: %v", notes)
	}
	data, err := os.ReadFile(DraftPath(cfg.OutDir, "2026-W31"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), llmMark) {
		t.Error("черновик должен остаться с плейсхолдерами под ручное заполнение")
	}
}

func TestProcessSlotAutoPublishDryWithoutLLM(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	pub := &fakePub{name: "max"}
	cfg.AutoPublish = true
	cfg.Publishers = []Publisher{pub}
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(pub.posts) != 1 {
		t.Fatalf("без LLM автопубликация шлёт «сухой» выпуск: %q", pub.posts)
	}
	if strings.Contains(pub.posts[0], llmMark) {
		t.Error("плейсхолдеры не должны попадать в публикацию")
	}
}

// Дорога «собрали накануне — таймер отправил в назначенный час»: готовый
// черновик уходит как есть, без пересборки и без единого обращения к модели.
// До 12.09.2026 такой файл закрывал слот и не публиковался никогда.
func TestProcessSlotPublishesPreparedDraft(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	gen := &fakeGen{resp: fullEditorial()}
	cfg.LLM = gen
	pub := &fakePub{name: "tg"}
	cfg.AutoPublish = true
	cfg.Publishers = []Publisher{pub}
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	w := SlotFor(now, cfg.Loc, cfg.Weekday, cfg.Hour, 0)
	const body = "Эту неделю собрали накануне, руками."
	if err := os.WriteFile(DraftPath(cfg.OutDir, w.ID),
		[]byte("<b>📰 Дайджест недели</b>\n---\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(pub.posts) != 1 {
		t.Fatalf("готовый черновик должен уйти в публикацию: %q", pub.posts)
	}
	if !strings.Contains(pub.posts[0], body) {
		t.Errorf("опубликован не приготовленный текст: %q", pub.posts[0])
	}
	// Пересборка дала бы другие числа и, возможно, другую заметку недели.
	if gen.calls != 0 {
		t.Errorf("готовый черновик не пересобирают: обращений к модели %d", gen.calls)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "опубликован") {
		t.Errorf("владельцу должно уйти ЛС о публикации: %q", notes)
	}
}

func TestProcessSlotSkipsStale(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	called := false
	cfg := scheduleCfg(t, st, func(context.Context, string) { called = true })
	// Вторник: последний слот (пятница W31) старше 48 часов.
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("протухший слот не догоняется")
	}
	if _, err := os.Stat(DraftPath(cfg.OutDir, "2026-W31")); err == nil {
		t.Error("черновик протухшего слота не должен создаваться")
	}
}

// С площадкой выпуск выходит ЗАМЕТКОЙ, а не постами в каналы: в мессенджеры её
// отнесёт исходящий обход, и публикация обоими путями сразу дала бы по два
// поста в Telegram и MAX.
func TestProcessSlotAutoPublishToPlatform(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	var notes []string
	cfg := scheduleCfg(t, st, func(_ context.Context, text string) { notes = append(notes, text) })
	cfg.LLM = &fakeGen{resp: map[string]string{
		"week_summary": "Неделя была тихой.", "dispute_note_id": "",
		"dispute": "", "quote": "", "topics": "",
	}}
	pub := &fakePub{name: "tg"}
	site := newFakeSite()
	cfg.AutoPublish = true
	cfg.Publishers = []Publisher{pub}
	cfg.Site = site
	cfg.SiteBaseURL = "https://t3h.ru"
	now := time.Date(2026, 7, 31, 19, 30, 0, 0, nsk)

	if err := processSlot(ctx, st, cfg, now, quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(site.bodies) != 1 || !strings.Contains(site.bodies[0], "Неделя была тихой.") {
		t.Fatalf("выпуск должен выйти заметкой на площадке: %q", site.bodies)
	}
	if len(pub.posts) != 0 {
		t.Errorf("в каналы напрямую выпуск больше не публикуется: %q", pub.posts)
	}
	if _, _, done, _ := st.Target(ctx, store.MessengerPlatform, store.TargetDigest, "2026-W31"); !done {
		t.Error("после публикации должна стоять отметка площадки")
	}
	if pinned := site.pins[site.nextID]; !pinned {
		t.Error("свежий выпуск должен быть закреплён наверху ленты")
	}
	// Повторный тик: неделя закрыта — второй заметки быть не должно.
	if err := processSlot(ctx, st, cfg, now.Add(time.Hour), quietLog()); err != nil {
		t.Fatal(err)
	}
	if len(site.bodies) != 1 || len(notes) != 1 {
		t.Errorf("повторный тик опубликовал ещё раз: заметок=%d уведомлений=%d", len(site.bodies), len(notes))
	}
}
