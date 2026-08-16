package pulpit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// ——— фейки ———

type postCall struct {
	NoteID   string
	ComAPIID string
	Text     string
}

type fakeSite struct {
	mu       sync.Mutex
	notes    []love.Note
	threads  map[string][]love.Comment
	pageErr  map[string]error
	posts    []postCall
	profile  love.ProfileControl
	profErr  error
	nick     string
	nextID   int64
	autoShow bool // после POST реплика появляется в треде (обычное поведение сайта)
}

func newFakeSite(notes ...love.Note) *fakeSite {
	return &fakeSite{
		notes:    notes,
		threads:  map[string][]love.Comment{},
		pageErr:  map[string]error{},
		nick:     myNick,
		nextID:   500,
		autoShow: true,
	}
}

func (f *fakeSite) FetchNotes(context.Context) ([]love.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]love.Note(nil), f.notes...), nil
}

func (f *fakeSite) FetchCommentsPage(_ context.Context, noteID string) (love.CommentsPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.pageErr[noteID]; err != nil {
		return love.CommentsPage{}, err
	}
	n := love.Note{ID: noteID, AuthorID: "u1", AuthorName: "Автор", Text: "текст заметки"}
	return love.CommentsPage{Comments: append([]love.Comment(nil), f.threads[noteID]...), Note: &n}, nil
}

func (f *fakeSite) TreeComments(_ context.Context, noteID string) ([]love.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.pageErr[noteID]; err != nil {
		return nil, err
	}
	return append([]love.Comment(nil), f.threads[noteID]...), nil
}

func (f *fakeSite) PostComment(_ context.Context, _ []*http.Cookie, noteID, comAPIID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, postCall{NoteID: noteID, ComAPIID: comAPIID, Text: text})
	if !f.autoShow {
		return nil
	}
	f.nextID++
	var parent int64
	if comAPIID != "" {
		fmt.Sscanf(comAPIID, "%d", &parent)
	}
	f.threads[noteID] = append(f.threads[noteID], love.Comment{
		ID: f.nextID, ParentID: parent, AuthorID: ownID, AuthorName: f.nick, Text: text,
	})
	return nil
}

func (f *fakeSite) ProfileControl(context.Context, []*http.Cookie) (love.ProfileControl, error) {
	if f.profErr != nil {
		return love.ProfileControl{}, f.profErr
	}
	return f.profile, nil
}

func (f *fakeSite) SiteIdentity(context.Context, []*http.Cookie) (string, string, string, error) {
	return ownID, "p1", f.nick, nil
}

func (f *fakeSite) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

type fakeGen struct {
	mu      sync.Mutex
	prompts []string
	answer  func(system, prompt string) (string, error)
}

func (g *fakeGen) GenerateJSON(_ context.Context, system, prompt string, _ map[string]any) ([]byte, error) {
	g.mu.Lock()
	g.prompts = append(g.prompts, prompt)
	g.mu.Unlock()
	if g.answer != nil {
		out, err := g.answer(system, prompt)
		return []byte(out), err
	}
	// Форму берём из разрешённых в этом запросе — так же, как это сделала бы
	// модель: набор сужается кулдауном, и подставлять «буквально» всегда значило бы
	// ловить брак там, где его в бою не будет.
	return []byte(`{"text":"Обида кормится вниманием. Не корми ее - и она уйдет следом.",
		"form":"` + allowedForm(prompt) + `","idea":"обида"}`), nil
}

// allowedForm вытаскивает из промпта приём, который модель обязана назвать: у
// черновика — первый из предложенных, у правки — тот же, что был в черновике.
// Подставлять всегда quipForms[0] значило бы ловить брак там, где его в бою нет:
// кулдаун сужает набор, и первый приём в списке бывает запрещён.
func allowedForm(prompt string) string {
	if i := strings.Index(prompt, "(приём: "); i >= 0 {
		rest := prompt[i+len("(приём: "):]
		if j := strings.Index(rest, ")"); j > 0 {
			return rest[:j]
		}
	}
	const marker = "В этот раз возьми один из приёмов: "
	i := strings.Index(prompt, marker)
	if i < 0 {
		return quipForms[0]
	}
	rest := prompt[i+len(marker):]
	if j := strings.IndexAny(rest, ",."); j > 0 {
		return rest[:j]
	}
	return quipForms[0]
}

func (g *fakeGen) lastPrompt() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.prompts) == 0 {
		return ""
	}
	return g.prompts[len(g.prompts)-1]
}

// draftPrompt — промпт черновика, а не правки шутки: последним запросом идёт
// второй проход, и по нему проверять содержимое первого нельзя.
func (g *fakeGen) draftPrompt() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.prompts) - 1; i >= 0; i-- {
		if !strings.Contains(g.prompts[i], "## Черновик реплики") {
			return g.prompts[i]
		}
	}
	return ""
}

// anyPrompt — встречалась ли подстрока хоть в одном запросе.
func (g *fakeGen) anyPrompt(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.prompts {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

// ——— стенд ———

func newTestService(t *testing.T, site Site, gen JSONGenerator, edit func(*Config)) (*Service, *store.Store, *[]string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.UpsertSession(ctx, store.MessengerTelegram, 1,
		`[{"name":"sid","value":"x"}]`, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSessionIdentity(ctx, store.MessengerTelegram, 1, ownID, "p1", myNick); err != nil {
		t.Fatal(err)
	}

	var alertsSent []string
	cfg := Config{
		OwnerProfileID: ownID,
		BaseURL:        "https://love.ngs.ru",
		Model:          "claude-opus-5",
		AllowEmoji:     true,
		AlertSend: func(_ context.Context, text string) {
			alertsSent = append(alertsSent, text)
		},
	}
	if edit != nil {
		edit(&cfg)
	}
	svc := New(st, site, gen, cfg, slog.New(slog.DiscardHandler))
	// Холодный старт и переход «выключено → включено» проверяются отдельными
	// тестами; здесь служба стартует так, будто уже отработала такт.
	svc.cold, svc.wasEnabled = false, true
	return svc, st, &alertsSent
}

// forceReplyScan снимает пятиминутную паузу обхода веток: в бою она бережёт
// сайт, а в тесте такты идут подряд.
func forceReplyScan(s *Service) {
	s.mu.Lock()
	s.replyAt = time.Time{}
	s.mu.Unlock()
}

func note(id string) love.Note {
	return love.Note{ID: id, AuthorID: "u1", AuthorName: "Автор",
		Text: "Муж ушел к другой, а я осталась с ипотекой и котом"}
}

// ——— сценарии ———

// TestCycleWritesOncePerNote — под заметкой ровно одна реплика, сколько бы
// тактов ни прошло: дубль комментария необратим.
func TestCycleWritesOncePerNote(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)

	svc.cycle(ctx)
	svc.cycle(ctx)
	svc.cycle(ctx)

	if got := site.postCount(); got != 1 {
		t.Fatalf("отправок %d, ожидалась одна: %+v", got, site.posts)
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitConfirmed || row.CommentID == 0 {
		t.Fatalf("строка после цикла: %+v", row)
	}
}

// TestClaimRaceSinglePost — свой обход ленты и колбэк зеркала видят заметку
// одновременно: отправка всё равно одна.
func TestClaimRaceSinglePost(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite()
	svc, _, _ := newTestService(t, site, &fakeGen{}, nil)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.handleNote(ctx, note("n1"), false, -1)
		}()
	}
	wg.Wait()

	if got := site.postCount(); got != 1 {
		t.Fatalf("отправок %d, ожидалась одна", got)
	}
}

// TestResumeAfterCrashConfirmsWithoutDuplicate — демон упал между «пишем
// posting» и ответом сайта. Переотправки быть не должно: судьбу строки решает
// верификация треда.
func TestResumeAfterCrashConfirmsWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)

	now := time.Now()
	if _, err := st.TryClaimPulpitNote(ctx, "n1", store.PulpitQueued, "", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TryStartPulpitPost(ctx, "n1", "буквально", "своя реплика", now); err != nil {
		t.Fatal(err)
	}
	// Реплика на сайте на самом деле есть — POST дошёл до падения.
	site.threads["n1"] = []love.Comment{
		{ID: 777, AuthorID: ownID, AuthorName: myNick, Text: "своя реплика"},
	}

	svc.cycle(ctx)

	if got := site.postCount(); got != 0 {
		t.Fatalf("переотправка запрещена, а отправок %d", got)
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitConfirmed || row.CommentID != 777 {
		t.Fatalf("строка после верификации: %+v", row)
	}
}

// TestFuseTripsAfterThreeMisses — три реплики подряд не появились в тредах:
// фича гасит себя сама и зовёт админа ровно один раз.
func TestFuseTripsAfterThreeMisses(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"), note("n2"), note("n3"))
	site.autoShow = false // сайт отвечает 200, но реплики в треде нет
	svc, st, alertsSent := newTestService(t, site, &fakeGen{}, nil)

	// Первый такт публикует и проверяет (checks=1), второй добивает до missing
	// и взводит предохранитель.
	svc.cycle(ctx)
	svc.cycle(ctx)

	enabled, err := svc.Enabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("после трёх промахов амвон должен выключиться")
	}
	if len(*alertsSent) != 1 {
		t.Fatalf("алертов %d, ожидался один: %v", len(*alertsSent), *alertsSent)
	}
	reason, _, _ := st.Flag(ctx, store.FlagPulpitOffReason)
	if !strings.Contains(reason, "запрет") {
		t.Errorf("причина выключения: %q", reason)
	}

	// Выключенная фича молчит: новых отправок нет.
	before := site.postCount()
	svc.cycle(ctx)
	if site.postCount() != before {
		t.Error("выключенный амвон продолжает писать")
	}
}

// TestLoadErrorIsNotAMiss — сбой загрузки треда промахом не считается: у сайта
// бывают короткие 5xx-штормы, и они не повод гасить фичу.
func TestLoadErrorIsNotAMiss(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	site.autoShow = false
	svc, st, alertsSent := newTestService(t, site, &fakeGen{}, nil)

	svc.cycle(ctx)
	site.pageErr["n1"] = errors.New("статус 502")
	svc.cycle(ctx)
	svc.cycle(ctx)

	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Checks != 1 {
		t.Fatalf("проверок %d: ошибка загрузки не должна их растить (%+v)", row.Checks, row)
	}
	if row.State == store.PulpitMissing {
		t.Error("реплика объявлена пропавшей по ошибке загрузки")
	}
	if len(*alertsSent) != 0 {
		t.Errorf("алерты при штормe сайта: %v", *alertsSent)
	}
}

// TestVanishedNoteIsNotAMiss — снесённая заметка не про нас: без этого первый
// же снос модератором выключил бы фичу.
func TestVanishedNoteIsNotAMiss(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	site.autoShow = false
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)

	svc.cycle(ctx)
	site.pageErr["n1"] = fmt.Errorf("GET: %w", love.ErrNotFound)
	svc.cycle(ctx)

	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitVanished || row.Reason != reasonNoteGone {
		t.Fatalf("строка исчезнувшей заметки: %+v", row)
	}
	if outcomeOf(row) != outcomeVanished {
		t.Error("исчезнувшая заметка не должна считаться промахом")
	}
}

// TestColdStartMarksWithoutPosting — рестарт демона не должен выдавать очередь
// реплик под старьё из ленты.
func TestColdStartMarksWithoutPosting(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"), note("n2"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)
	svc.cold = true

	svc.cycle(ctx)
	if got := site.postCount(); got != 0 {
		t.Fatalf("на холодном старте отправок быть не должно: %d", got)
	}
	for _, id := range []string{"n1", "n2"} {
		row, err := st.PulpitNote(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State != store.PulpitSkipped || row.Reason != reasonColdStart {
			t.Fatalf("%s: %+v", id, row)
		}
	}
	// Следующий такт уже боевой — но старые заметки помечены и не воскреснут.
	svc.cycle(ctx)
	if got := site.postCount(); got != 0 {
		t.Fatalf("помеченные заметки не должны оживать: %d", got)
	}
}

// TestDisabledWritesNothing — выключенный тумблер молчит вовсе: ни строк в БД,
// ни запросов на сайт.
func TestDisabledWritesNothing(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, nil)
	if err := svc.SetPulpitEnabled(ctx, false, "admin:1"); err != nil {
		t.Fatal(err)
	}

	svc.cycle(ctx)

	if site.postCount() != 0 {
		t.Error("выключенный амвон пишет на сайт")
	}
	if _, err := st.PulpitNote(ctx, "n1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("выключенный амвон трогает БД: %v", err)
	}

	// Включение взводит холодный старт: под старыми заметками ленты реплик
	// быть не должно.
	if err := svc.SetPulpitEnabled(ctx, true, "admin:1"); err != nil {
		t.Fatal(err)
	}
	svc.cycle(ctx)
	if site.postCount() != 0 {
		t.Error("включение выдало реплику под старой заметкой")
	}
}

// TestQuotaStopsRunaway — суточный предохранитель: сайт выкатил в ленту архив.
func TestQuotaStopsRunaway(t *testing.T) {
	ctx := context.Background()
	var notes []love.Note
	for i := range 5 {
		notes = append(notes, note(fmt.Sprintf("n%d", i)))
	}
	site := newFakeSite(notes...)
	svc, _, _ := newTestService(t, site, &fakeGen{}, func(c *Config) { c.MaxPerDay = 2 })

	svc.cycle(ctx)

	if got := site.postCount(); got != 2 {
		t.Fatalf("отправок %d, потолок 2", got)
	}
}

// TestAnonymousNoteKeepsNameOut — у анонимной заметки автора нет даже у сайта:
// обращаться по имени не к кому.
func TestAnonymousNoteKeepsNameOut(t *testing.T) {
	ctx := context.Background()
	anon := love.Note{ID: "n1", AuthorID: "0", AuthorName: "Анонимно", Text: "Живу с нелюбимым ради детей"}
	site := newFakeSite(anon)
	gen := &fakeGen{}
	svc, _, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	prompt := gen.draftPrompt()
	if !strings.Contains(prompt, "Автор скрыл имя") {
		t.Errorf("промпт не предупреждает об анонимности:\n%s", prompt)
	}
	if strings.Contains(prompt, "Автор: ") {
		t.Errorf("в промпте появилось имя анонима:\n%s", prompt)
	}
}

// TestGenerationRetryUsesReason — брак ответа не срывает реплику: причина
// уезжает в промпт, и второй заход не слепой.
func TestGenerationRetryUsesReason(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	calls := 0
	gen := &fakeGen{answer: func(_, _ string) (string, error) {
		calls++
		if calls == 1 {
			return `{"text":"**Гордыня**","form":"буквально","idea":"гордыня"}`, nil
		}
		return `{"text":"Обида кормится вниманием. Не корми ее - и она уйдет следом.","form":"буквально","idea":"обида"}`, nil
	}}
	svc, st, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	// Три вызова: забракованный черновик, переспрос и правка шутки.
	if calls != 3 {
		t.Fatalf("вызовов модели %d, ожидалось 3", calls)
	}
	if !gen.anyPrompt("Переспрос") {
		t.Errorf("причина брака не попала в промпт:\n%s", gen.draftPrompt())
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil || row.State != store.PulpitConfirmed {
		t.Fatalf("строка: %+v %v", row, err)
	}
}

// TestGenerationFailureDoesNotPost — не сгенерировали, значит не отправили.
func TestGenerationFailureDoesNotPost(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	gen := &fakeGen{answer: func(_, _ string) (string, error) {
		return `{"text":"[b]коротко[/b]","form":"буквально","idea":"и"}`, nil
	}}
	svc, st, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	if site.postCount() != 0 {
		t.Fatal("забракованный текст ушёл на сайт")
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitFailed {
		t.Fatalf("строка: %+v", row)
	}
}

// TestPunchUpSharpensDraft — на сайт уходит правка, а не черновик: второй
// проход и есть то место, где убирают пояснение после удара.
func TestPunchUpSharpensDraft(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	const draft = "Обида кормится вниманием, и это, если подумать, довольно грустно."
	const sharp = "Обида кормится вниманием. Моя вон уже с меня ростом."
	gen := &fakeGen{answer: func(_, prompt string) (string, error) {
		if strings.Contains(prompt, "## Черновик реплики") {
			return `{"skip":false,"text":"` + sharp + `","form":"буквально","idea":"обида"}`, nil
		}
		return `{"skip":false,"text":"` + draft + `","form":"буквально","idea":"обида"}`, nil
	}}
	svc, _, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	if site.postCount() != 1 {
		t.Fatalf("постов %d, ожидался один", site.postCount())
	}
	if got := site.posts[0].Text; got != sharp {
		t.Errorf("на сайт ушёл не правленый текст: %q", got)
	}
	if !gen.anyPrompt("## Черновик реплики") {
		t.Error("второго прохода не было вовсе")
	}
}

// TestPunchUpFailureKeepsDraft — правка не удалась, а реплика всё равно уходит:
// черновик уже прошёл валидацию, и второй проход был улучшением, а не условием.
func TestPunchUpFailureKeepsDraft(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	const draft = "Обида кормится вниманием. Не корми ее - и она уйдет следом."
	gen := &fakeGen{answer: func(_, prompt string) (string, error) {
		if strings.Contains(prompt, "## Черновик реплики") {
			return `{"skip":false,"text":"**жирным**","form":"буквально","idea":"брак"}`, nil
		}
		return `{"skip":false,"text":"` + draft + `","form":"буквально","idea":"обида"}`, nil
	}}
	svc, st, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	if site.postCount() != 1 {
		t.Fatalf("постов %d, ожидался один", site.postCount())
	}
	if got := site.posts[0].Text; got != draft {
		t.Errorf("вместо черновика ушло что-то другое: %q", got)
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil || row.State != store.PulpitConfirmed {
		t.Fatalf("строка: %+v %v", row, err)
	}
}

// TestSkipUnderGrief — модель отказалась шутить: на сайт не уходит ничего, а
// строка помечается пропуском, а не неудачей. Красная линия — часть голоса:
// под настоящей бедой молчание и есть правильный ответ, и переспрашивать
// («ну попробуй пошутить») нельзя.
func TestSkipUnderGrief(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	calls := 0
	gen := &fakeGen{answer: func(_, _ string) (string, error) {
		calls++
		return `{"skip":true,"text":"","form":"","idea":"умерла мать"}`, nil
	}}
	svc, st, _ := newTestService(t, site, gen, nil)

	svc.cycle(ctx)

	if site.postCount() != 0 {
		t.Fatal("под заметкой о беде появилась реплика")
	}
	if calls != 1 {
		t.Fatalf("попыток генерации %d, отказ шутить не переспрашивают", calls)
	}
	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitSkipped || row.Reason != reasonNoJoke {
		t.Fatalf("строка: %+v", row)
	}
}

// TestReplyCoinFlippedOnce — монетка бросается ровно один раз на реплику:
// иначе 15 % за десяток тактов превратятся в 80 %.
func TestReplyCoinFlippedOnce(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, func(c *Config) { c.ReplyProbability = 0.15 })
	flips := 0
	svc.rand = func() float64 { flips++; return 0.9 } // всегда «не отвечать»

	svc.cycle(ctx) // публикация и подтверждение своей реплики
	mine, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	site.threads["n1"] = append(site.threads["n1"], love.Comment{
		ID: 601, ParentID: mine.CommentID, AuthorID: "u2", AuthorName: "Лампочка",
		Text: myNick + ", ты опять за своё",
	})

	postsBefore := site.postCount()
	for range 3 {
		forceReplyScan(svc)
		svc.cycle(ctx)
	}

	if flips != 1 {
		t.Fatalf("монетку бросали %d раз, ожидался один", flips)
	}
	if site.postCount() != postsBefore {
		t.Error("при «не отвечать» ответ всё-таки ушёл")
	}
	replies, err := st.PulpitRepliesByNote(ctx, "n1")
	if err != nil || len(replies) != 1 {
		t.Fatalf("решения: %+v %v", replies, err)
	}
	if replies[0].State != store.PulpitSkipped || replies[0].Reason != reasonCoin {
		t.Fatalf("решение: %+v", replies[0])
	}
}

// TestReplySentWithPrefix — ответ уходит реплаем на чужую реплику, а обращение
// подставляет инструмент.
func TestReplySentWithPrefix(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{answer: func(system, _ string) (string, error) {
		if strings.Contains(system, "Тебе ответили") {
			return `{"text":"Смирение не в том, чтобы молчать.","idea":"смирение"}`, nil
		}
		return `{"text":"Обида кормится вниманием. Не корми ее - и она уйдет следом.","form":"буквально","idea":"обида"}`, nil
	}}, func(c *Config) { c.ReplyProbability = 1 })
	svc.rand = func() float64 { return 0 } // всегда «отвечать»

	svc.cycle(ctx)
	mine, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	site.threads["n1"] = append(site.threads["n1"], love.Comment{
		ID: 601, ParentID: mine.CommentID, AuthorID: "u2", AuthorName: "Лампочка",
		Text: myNick + ", ты опять за своё",
	})

	forceReplyScan(svc)
	svc.cycle(ctx)
	forceReplyScan(svc)
	svc.cycle(ctx) // второй такт не должен добавить ещё один ответ

	var replies []postCall
	for _, p := range site.posts {
		if p.ComAPIID != "" {
			replies = append(replies, p)
		}
	}
	if len(replies) != 1 {
		t.Fatalf("ответов %d, ожидался один: %+v", len(replies), replies)
	}
	if replies[0].ComAPIID != "601" {
		t.Errorf("ответ ушёл не на ту реплику: %+v", replies[0])
	}
	if !strings.HasPrefix(replies[0].Text, "Лампочка, ") {
		t.Errorf("обращение подставляет инструмент: %q", replies[0].Text)
	}
	rows, err := st.PulpitRepliesByNote(ctx, "n1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("решения: %+v %v", rows, err)
	}
	if rows[0].State != store.PulpitConfirmed {
		t.Errorf("свой ответ должен подтвердиться по дереву: %+v", rows[0])
	}
}

// TestDeletedReplyDetected — подтверждённую реплику вычистила модерация.
func TestDeletedReplyDetected(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, st, _ := newTestService(t, site, &fakeGen{}, func(c *Config) { c.ReplyProbability = 0.15 })

	svc.cycle(ctx)
	site.threads["n1"] = nil // реплику удалили
	forceReplyScan(svc)
	svc.cycle(ctx)

	row, err := st.PulpitNote(ctx, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.PulpitMissing || row.Reason != reasonDeleted {
		t.Fatalf("строка: %+v", row)
	}
}

// TestStatusReport — ручка /pulpit: без службы админ не увидит ни состояния,
// ни последней реплики.
func TestStatusReport(t *testing.T) {
	ctx := context.Background()
	site := newFakeSite(note("n1"))
	svc, _, _ := newTestService(t, site, &fakeGen{}, nil)
	svc.cycle(ctx)

	report, enabled, offReason := svc.PulpitStatus(ctx)
	if !enabled || offReason != "" {
		t.Fatalf("состояние: enabled=%v off=%q", enabled, offReason)
	}
	for _, want := range []string{"включён", "claude-opus-5", "anchor-"} {
		if !strings.Contains(report, want) {
			t.Errorf("в отчёте нет %q:\n%s", want, report)
		}
	}
}
