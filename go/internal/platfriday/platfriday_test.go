package platfriday

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Заметка, называющая свой год, в вопрос «в каком году» не годится.
//
// Правило оплачено разбором материала 11.09.2026: половина пятничных заметок
// архива — утренние, а они по устройству печатают дату прямо в первой строке.
// Без этой проверки добрая часть выпуска состояла бы из вопросов с ответом в
// тексте.
func TestYearQuestionRejectsTextThatNamesTheYear(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	morning := Note{
		ID:        284259,
		Published: time.Date(2019, 9, 20, 9, 0, 0, 0, time.UTC),
		Comments:  323,
		Body: "Доброе утро, жители острова Любви!! Сегодня 20 сентября 2019, пятница. " +
			"Погода в Новосибирске плюс двадцать четыре, солнце. Всем хорошего дня и настроения, " +
			"пусть неделя закончится так же славно, как началась, а выходные будут тёплыми.",
	}
	if q := yearQuestion(morning, rnd); q != nil {
		t.Fatalf("вопрос задан по тексту, который сам называет год: %q", q.Quiz.Options)
	}
}

// Годная заметка вопрос даёт — и правильный вариант в нём тот, что нужно.
func TestYearQuestionKeepsTheRightAnswer(t *testing.T) {
	rnd := rand.New(rand.NewSource(7))
	n := Note{
		ID:        270011,
		Published: time.Date(2016, 9, 9, 5, 0, 0, 0, time.UTC),
		Comments:  261,
		Body: "категории мужчин на сайте знакомств: кунимен, часто встречающийся вид, " +
			"настырен и приставуч. женатик обыкновенный — глубоко женатый мужчина, который " +
			"считает, что может себе позволить содержать любовницу. бомж-массажист обещает " +
			"намять вам всё что можно намять, главное — пригласите его к себе домой.",
	}
	q := yearQuestion(n, rnd)
	if q == nil {
		t.Fatal("вопрос не собрался")
	}
	if got := q.Quiz.Options[q.Quiz.Right]; got != "2016" {
		t.Errorf("правильным объявлен %q", got)
	}
	if len(q.Quiz.Options) != 3 {
		t.Errorf("вариантов %d", len(q.Quiz.Options))
	}
	if !strings.Contains(q.Quiz.Reveal, "261 реплика") {
		t.Errorf("разгадка без числа реплик: %q", q.Quiz.Reveal)
	}
	if q.Quiz.SourceNote != 270011 {
		t.Error("потеряна ссылка на разговор-источник")
	}
}

// Ники в тексте — стоп. Невод грубый по устройству, и цена ошибки
// несимметрична: лишний пропуск стоит одного кандидата из четырёхсот, а ник,
// уехавший в вопрос, — это имя живого человека, выставленное в игру.
func TestCleanRejectsNicknames(t *testing.T) {
	long := strings.Repeat("слова про жизнь и погоду, ничего особенного. ", 4)
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"чистый текст", long, true},
		{"ник посреди фразы", long + "а вот Ромашка так не думает", false},
		{"латиница", long + "как говорил Lacoste", false},
		{"ссылка", long + "вот тут http://example.com", false},
		{"мат", long + "да заебали уже", false},
	} {
		_, ok := clean(tc.text, 50, 900)
		if ok != tc.want {
			t.Errorf("%s: годность %v, ждали %v", tc.name, ok, tc.want)
		}
	}
}

// Обращение «Ник, » в теле реплики уже снято ядром (оно живёт ребром), но
// подпись адресата в СТАРЫХ зеркальных репликах встречается внутри текста —
// такие не берём.
func TestCleanRejectsAddressInside(t *testing.T) {
	if _, ok := clean("да ладно тебе, Мурена, я же не со зла это всё говорю, правда", 40, 190); ok {
		t.Error("реплика с обращением по нику прошла фильтр")
	}
}

// Выпуск ДЕТЕРМИНИРОВАН неделей: служба падает и поднимается, а владелец видит
// черновик заранее — опубликоваться обязано ровно то, что он видел.
func TestIssueIsDeterministicPerWeek(t *testing.T) {
	src := &fakeSource{notes: sampleNotes()}
	day := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)

	a, err := Build(context.Background(), src, day, 3)
	if err != nil {
		t.Fatalf("выпуск: %v", err)
	}
	b, err := Build(context.Background(), src, day, 3)
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if a.Week != "2026-W37" {
		t.Errorf("метка недели %q", a.Week)
	}
	if len(a.Questions) != len(b.Questions) {
		t.Fatalf("вопросов %d против %d", len(a.Questions), len(b.Questions))
	}
	for i := range a.Questions {
		if a.Questions[i].Body != b.Questions[i].Body ||
			a.Questions[i].Quiz.Right != b.Questions[i].Quiz.Right {
			t.Fatalf("вопрос %d разошёлся между прогонами", i)
		}
	}
	// Другая неделя — другой выпуск: иначе рубрика повторялась бы.
	c, err := Build(context.Background(), src, day.AddDate(0, 0, 7), 3)
	if err != nil {
		t.Fatalf("следующая неделя: %v", err)
	}
	if c.Week == a.Week {
		t.Error("метка недели не сдвинулась")
	}
}

// Пустой архив — не паника: бывает неделя без материала, и служба обязана
// промолчать, а не упасть.
func TestEmptyArchiveIsNotAPanic(t *testing.T) {
	if _, err := Build(context.Background(), &fakeSource{}, time.Now(), 3); err != ErrNoMaterial {
		t.Fatalf("пустой архив дал %v", err)
	}
}

// Склонение числа реплик считает Go, а не шаблон: «261 реплика», «2 реплики»,
// «5 реплик» — иначе разгадка читается машинной.
func TestRepliesWord(t *testing.T) {
	for n, want := range map[int]string{
		1: "1 реплика", 2: "2 реплики", 5: "5 реплик", 11: "11 реплик",
		21: "21 реплика", 261: "261 реплика", 114: "114 реплик",
	} {
		if got := repliesWord(n); got != want {
			t.Errorf("%d → %q, ждали %q", n, got, want)
		}
	}
}

type fakeSource struct {
	notes []Note
	pairs map[int64][]Pair
}

func (f *fakeSource) Candidates(_ context.Context, _ time.Time, _ int) ([]Note, error) {
	return f.notes, nil
}

func (f *fakeSource) ReplyPairs(_ context.Context, noteID int64) ([]Pair, []string, error) {
	return f.pairs[noteID], nil, nil
}

func sampleNotes() []Note {
	body := "вот пришли на свидание в первый раз, он и она, видят друг друга воочию, в реале, " +
		"впервые. до этого лишь телефонные звонки. кто-то кому-то не понравился, но не надо спешить: " +
		"одно дело зрительный контакт, другое тактильный. по парку идёте — возьмите под руку сразу."
	var out []Note
	for i := 0; i < 6; i++ {
		out = append(out, Note{
			ID:        306047 - int64(i)*100,
			Body:      body,
			Published: time.Date(2016+i%5, 9, 12+i%3, 6, 0, 0, 0, time.UTC),
			Comments:  120 + i*37,
		})
	}
	return out
}
