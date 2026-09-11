package platfriday

import (
	"context"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// fakePoster — площадка, какой её видит рубрика.
type fakePoster struct {
	nextID    int64
	issues    map[string]int64
	notes     []string
	comments  []platform.NewComment
	asked     map[int64][]time.Time
	now       time.Time
	failNote  bool
	systemNum int
}

func newFakePoster(now time.Time) *fakePoster {
	return &fakePoster{
		nextID: 100000000001,
		issues: map[string]int64{},
		asked:  map[int64][]time.Time{},
		now:    now,
	}
}

func (f *fakePoster) EnsureSystemUser(context.Context, string) (int64, error) {
	f.systemNum++
	return 42, nil
}

func (f *fakePoster) CreateNote(_ context.Context, in platform.NewNote) (int64, error) {
	if f.failNote {
		return 0, context.Canceled
	}
	f.nextID++
	f.notes = append(f.notes, in.Body)
	return f.nextID, nil
}

func (f *fakePoster) CreateComment(_ context.Context, in platform.NewComment) (int64, error) {
	f.nextID++
	f.comments = append(f.comments, in)
	f.asked[in.NoteID] = append(f.asked[in.NoteID], f.now)
	return f.nextID, nil
}

func (f *fakePoster) FridayIssue(_ context.Context, week string) (int64, error) {
	return f.issues[week], nil
}

func (f *fakePoster) StartFridayIssue(_ context.Context, week string, noteID int64) (int64, error) {
	if got, ok := f.issues[week]; ok {
		return got, nil
	}
	f.issues[week] = noteID
	return noteID, nil
}

func (f *fakePoster) FridayAsked(_ context.Context, noteID int64) (int, error) {
	return len(f.asked[noteID]), nil
}

func (f *fakePoster) QuizStats(_ context.Context, noteID int64) ([]platform.QuizStat, error) {
	var out []platform.QuizStat
	for _, t := range f.asked[noteID] {
		out = append(out, platform.QuizStat{NoteID: noteID, Asked: t})
	}
	return out, nil
}

func testConfig() Config {
	return Config{
		Weekday: time.Friday, Hour: 16, Questions: 3,
		Gap: time.Hour, SiteNick: "Зазеркалье", Live: true,
	}
}

// Пятница, 17:00 Нск.
func fridayEvening() time.Time {
	return time.Date(2026, 9, 11, 17, 0, 0, 0, nskLoc())
}

// Первый проход открывает выпуск заметкой И задаёт первый вопрос: вечер
// начинается сразу, а не через час после заметки.
func TestFirstStepOpensIssueAndAsks(t *testing.T) {
	f := newFakePoster(fridayEvening())
	src := &fakeSource{notes: sampleNotes()}

	res, err := Step(context.Background(), f, src, testConfig(), fridayEvening())
	if err != nil {
		t.Fatalf("проход: %v", err)
	}
	if len(f.notes) != 1 {
		t.Fatalf("заметок опубликовано %d", len(f.notes))
	}
	if len(f.comments) != 1 {
		t.Fatalf("вопросов задано %d", len(f.comments))
	}
	if f.comments[0].Quiz == nil {
		t.Fatal("вопрос опубликован без вариантов")
	}
	if res.Week != "2026-W37" {
		t.Errorf("неделя выпуска %q", res.Week)
	}
}

// Повторный проход в ту же минуту не публикует НИЧЕГО: ключ-неделя держит
// заметку, а час между вопросами — сами вопросы.
func TestSecondStepIsIdempotent(t *testing.T) {
	f := newFakePoster(fridayEvening())
	src := &fakeSource{notes: sampleNotes()}
	cfg := testConfig()

	for i := 0; i < 3; i++ {
		if _, err := Step(context.Background(), f, src, cfg, fridayEvening()); err != nil {
			t.Fatalf("проход %d: %v", i, err)
		}
	}
	if len(f.notes) != 1 {
		t.Errorf("заметка опубликована %d раз", len(f.notes))
	}
	if len(f.comments) != 1 {
		t.Errorf("вопросов задано %d, ждали один", len(f.comments))
	}
}

// Через час выходит следующий вопрос — и не больше одного за раз.
func TestNextQuestionAfterGap(t *testing.T) {
	start := fridayEvening()
	f := newFakePoster(start)
	src := &fakeSource{notes: sampleNotes()}
	cfg := testConfig()

	if _, err := Step(context.Background(), f, src, cfg, start); err != nil {
		t.Fatalf("первый: %v", err)
	}
	f.now = start.Add(time.Hour + time.Minute)
	if _, err := Step(context.Background(), f, src, cfg, f.now); err != nil {
		t.Fatalf("второй: %v", err)
	}
	if len(f.comments) != 2 {
		t.Fatalf("вопросов %d, ждали два", len(f.comments))
	}
	// Вопросы РАЗНЫЕ: второй не повторяет первый.
	if f.comments[0].Body == f.comments[1].Body {
		t.Error("второй вопрос повторил первый")
	}
	// Больше запланированного не задаём.
	for i := 0; i < 5; i++ {
		f.now = f.now.Add(time.Hour + time.Minute)
		if _, err := Step(context.Background(), f, src, cfg, f.now); err != nil {
			t.Fatalf("догон %d: %v", i, err)
		}
	}
	if len(f.comments) != cfg.Questions {
		t.Errorf("задано %d вопросов при плане %d", len(f.comments), cfg.Questions)
	}
}

// До слота рубрика молчит: заметка выходит в пятницу, а не когда служба
// проснулась.
func TestBeforeSlotNothingHappens(t *testing.T) {
	f := newFakePoster(fridayEvening())
	src := &fakeSource{notes: sampleNotes()}
	// Среда.
	wednesday := time.Date(2026, 9, 9, 20, 0, 0, 0, nskLoc())

	res, err := Step(context.Background(), f, src, testConfig(), wednesday)
	if err != nil {
		t.Fatalf("проход: %v", err)
	}
	if len(f.notes) != 0 || len(f.comments) != 0 {
		t.Errorf("опубликовано до слота: заметок %d, вопросов %d", len(f.notes), len(f.comments))
	}
	if res.Skipped == "" {
		t.Error("проход не объяснил, почему промолчал")
	}
}

// СУХОЙ ПРОГОН собирает выпуск целиком и не публикует ни строки — тот же
// тумблер и тот же довод, что у народа: службу заводят раньше, чем ей доверяют.
func TestDryRunPublishesNothing(t *testing.T) {
	f := newFakePoster(fridayEvening())
	src := &fakeSource{notes: sampleNotes()}
	cfg := testConfig()
	cfg.Live = false

	res, err := Step(context.Background(), f, src, cfg, fridayEvening())
	if err != nil {
		t.Fatalf("сухой проход: %v", err)
	}
	if len(f.notes) != 0 || len(f.comments) != 0 {
		t.Fatalf("сухой прогон опубликовал: заметок %d, вопросов %d", len(f.notes), len(f.comments))
	}
	if res.Skipped == "" {
		t.Error("сухой прогон не сказал, что молчит")
	}
}

// Заметка выпуска подписана СЛУЖЕБНОЙ анкетой площадки. Это не оформление:
// именно подпись отводит рубрику от выноса на НГС (там спрашивается
// kind = KindMember), то есть «только в Зазеркалье» держится структурой.
func TestIssueIsSignedByThePlatform(t *testing.T) {
	f := newFakePoster(fridayEvening())
	src := &fakeSource{notes: sampleNotes()}

	if _, err := Step(context.Background(), f, src, testConfig(), fridayEvening()); err != nil {
		t.Fatalf("проход: %v", err)
	}
	if f.systemNum == 0 {
		t.Fatal("служебная анкета не спрошена — выпуск подписан кем-то другим")
	}
	if f.comments[0].AuthorID != 42 {
		t.Errorf("вопрос подписан %d", f.comments[0].AuthorID)
	}
}
