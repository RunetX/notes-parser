package narod

// ПОЛ СОБЕСЕДНИКА — со сцены, а не по нику.
//
// Оба теста здесь написаны по одному боевому дефекту (30.08.2026, первая живая
// песочница): «Лисёнок в кедах, ты про себя это и сказал» — женщина написала это
// женщине. Дефект оказался не в промпте и не в модели, а в СЦЕНЕ: пола в ней не
// было вовсе, и потому мимо него шли ОБА потребителя — модель, которой русский
// глагол прошедшего времени пола не прощает, и кубик, у которого отклик по полу
// говорящего замерен на 300 тыс. рёбер и всё это время лежал без входных данных.
//
// В калибровке пол приезжал из архива, и рычаг там работал. Расхождение
// молчаливое: формула одна на оба мира, а данные разные, — потому тесты и стоят
// на ПЛУТИ, а не на формуле (её проверяет decide_test).

import (
	"context"
	"strings"
	"testing"
	"time"
)

func genderCard(t *testing.T, sex string) *Card {
	t.Helper()
	c := testCard()
	// Слуг обязателен: без него служба считает своими ВСЕ реплики треда — в мире
	// «кто это сказал» хранится слугом жителя, а пустой совпадает с пустым.
	c.ID = "test-" + sex
	c.Persona.Gender = sex
	return &c
}

// Промпт называет пол КАЖДОГО, кто говорит: автора заметки, всех в треде и
// адресата в самом задании. Неизвестный пол не называется никак.
func TestПромптНазываетПолГоворящих(t *testing.T) {
	card := genderCard(t, "female")
	point := WritePoint{
		Card: card,
		Note: StageNote{ID: 100000000029, AuthorID: 1,
			AuthorNick: "Паноптикум", AuthorGender: "male", Body: "текст заметки"},
		Thread: []StageReply{
			{ID: 10, AuthorID: 2, AuthorNick: "Лисёнок в кедах", Gender: "female",
				Body: "верно, издалека"},
			{ID: 11, AuthorID: 3, AuthorNick: "Механик Сева", Gender: "male",
				Body: "а я бы полку прибил"},
			{ID: 12, AuthorID: 4, AuthorNick: "Гость", Body: "мимо шёл"},
		},
	}
	point.ReplyTo = point.Thread[0]

	got := writePrompt(point, "")
	for _, want := range []string{
		"Заметка «Паноптикум, мужчина»",
		"[Лисёнок в кедах, женщина]",
		"[Механик Сева, мужчина]",
		"Ответь на реплику Лисёнок в кедах, женщина",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("в промпте нет %q", want)
		}
	}
	// Неизвестный пол остаётся неизвестным: названное вслух отсутствие модель
	// заполнит догадкой, а догадка по нику и была дефектом.
	if !strings.Contains(got, "[Гость] мимо шёл") {
		t.Error("у безымянного по полу участника появилась приписка")
	}
	if strings.Contains(got, "неизвест") {
		t.Error("промпт объявляет пол неизвестным вслух")
	}
}

// ПОЛ ДОЕЗЖАЕТ ДО КУБИКА. Замер (ReplyRate.ToMale/ToFemale) говорит, что на
// реплику женщины мужчина откликается почти втрое охотнее, чем на реплику
// мужчины; здесь у карточки этот перекос доведён до предела, чтобы вопрос
// «доехали ли данные» не пришлось решать статистикой.
func TestКубикВидитПолСоСцены(t *testing.T) {
	ctx := context.Background()
	note := StageNote{ID: 100000000029, AuthorID: 1, AuthorNick: "Паноптикум",
		AuthorGender: "male", Body: "заметка-песочница"}

	came := func(sex string) int {
		stage := &fakeStage{notes: []StageNote{note}}
		for i := 0; i < 20; i++ {
			stage.thread = append(stage.thread, StageReply{
				ID: int64(100 + i), NoteID: note.ID, AuthorID: 7,
				AuthorNick: "Собеседник", Gender: sex, Body: "реплика",
			})
		}
		card := genderCard(t, "male")
		card.Rate = ReplyRate{
			Buckets:  []RateBucket{{Upto: 1 << 30, Chances: 1000, Answers: 500}},
			ToMale:   RateBucket{Chances: 1000, Answers: 1},
			ToFemale: RateBucket{Chances: 1000, Answers: 999},
		}
		svc, w := testService(t, stage)
		svc.players = []Player{{Card: card, UserID: 42}}
		if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
			PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := svc.scanThread(ctx, note.ID); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, c := range stage.thread {
			d, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID))
			if err != nil {
				continue // монетки на эту реплику не бросали вовсе
			}
			if d.Verdict == DiceCome {
				n++
			}
		}
		return n
	}

	toMale, toFemale := came("male"), came("female")
	if toFemale <= toMale {
		t.Errorf("пол мимо кубика: на женщин %d приходов, на мужчин %d", toFemale, toMale)
	}
}
