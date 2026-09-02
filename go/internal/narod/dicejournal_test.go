package narod

// ЧТО ЗАПИСАНО О БРОСКЕ.
//
// Тест написан по дефекту 02.09.2026: колонки `dice.p` и `dice.roll` стояли в
// схеме мира с первого дня, а служба клала в них нули — 33 749 нулей из 33 749
// бросков за один только день. Кубик при этом работал (в тот же день по одной
// заметке выпало четыре «прийти» из шестидесяти), и потому дефект был
// молчаливым: журнал отвечал на вопрос «что вышло» и не отвечал на «с какой
// вероятностью», а разбор пустой песочницы упирается ровно во второй.
//
// Цена названа прямо: по этой самой колонке вышло «ожидание 0,00 корня,
// вероятность нуля 100 %» — то есть чинить чуть не пошли кубик вместо журнала.
//
// Стои́т тест НА ПУТИ ДАННЫХ (через scanThread и настоящий World), а не на
// формуле: формула и раньше считала вероятность верно, теряли её по дороге в
// базу. Тот же довод, что у wiring_test и window_test.

import (
	"context"
	"testing"
	"time"
)

// diceOfThread — журнал бросков жителя по одному проходу треда.
func diceOfThread(t *testing.T, card *Card, thread []StageReply) []Dice {
	t.Helper()
	ctx := context.Background()
	svc, w := testService(t, &fakeStage{notes: []StageNote{wiringNote}, thread: thread})
	svc.players = []Player{{Card: card, UserID: 42}}
	if err := w.UpsertActor(ctx, Actor{ID: card.ID, Kind: ActorPersona,
		PlatformUserID: 42, Nick: "Житель"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := svc.scanThread(ctx, wiringNote.ID); err != nil {
		t.Fatal(err)
	}
	var out []Dice
	for _, c := range thread {
		d, err := w.DiceOf(ctx, card.ID, eventKey(eventReply, c.ID))
		if err != nil {
			continue // монетки на эту реплику не бросали вовсе
		}
		out = append(out, d)
	}
	return out
}

// ВЕРОЯТНОСТЬ И БРОСОК доезжают до журнала — и у пришедшего, и у смолчавшего.
//
// Проверяются оба исхода сразу, потому что дефект был именно в них обоих:
// «пришёл» без вероятности не отличить от везения на нулевой ставке, а
// «смолчал» без неё — от точки, где решать было нечего. Карточка взята со
// средней ставкой, чтобы в одном проходе нашлись оба исхода.
//
// На прежнем коде тест падает: в журнале стояли нули.
func TestЖурналХранитВероятностьИБросок(t *testing.T) {
	card := wiringCard("test-dice-journal")
	rolls := diceOfThread(t, card, spread(40, time.Minute))
	if len(rolls) == 0 {
		t.Fatal("монетку не бросали ни разу — тред пуст?")
	}
	var came, kept int
	for _, d := range rolls {
		if d.P <= 0 || d.P > 1 {
			t.Fatalf("%s: вероятность в журнале %v, а бросали по ней", d.EventID, d.P)
		}
		if d.Roll < 0 || d.Roll >= 1 {
			t.Fatalf("%s: бросок в журнале %v, а он доля [0,1)", d.EventID, d.Roll)
		}
		if d.Reason != "" {
			t.Fatalf("%s: причина %q у точки, где монетку бросали", d.EventID, d.Reason)
		}
		switch d.Verdict {
		case DiceCome:
			came++
			if d.Roll >= d.P {
				t.Fatalf("%s: пришёл, а бросок %v не ниже вероятности %v", d.EventID, d.Roll, d.P)
			}
		case DiceSkip:
			kept++
			if d.Roll < d.P {
				t.Fatalf("%s: смолчал, а бросок %v ниже вероятности %v", d.EventID, d.Roll, d.P)
			}
		}
	}
	if came == 0 || kept == 0 {
		t.Fatalf("в проходе нужны оба исхода, вышло пришёл %d, смолчал %d", came, kept)
	}
}

// ТОЧКА БЕЗ БРОСКА НАЗЫВАЕТ СЕБЯ СЛОВАМИ.
//
// Личный потолок реплик на тред — единственный путь, на котором монетки нет
// вовсе. Без причины в журнале такая строка выглядела бы как брошенная с
// нулевой вероятностью, то есть ровно как та слепота, ради которой всё и
// правилось, только с другой стороны.
func TestПотолокТредаНазванПричиной(t *testing.T) {
	card := wiringCard("test-dice-cap")
	// Ставка предельная (говорим почти всегда), потолок низкий: первые реплики
	// житель разбирает монеткой, дальше упирается в себя же.
	card.Rate = ReplyRate{Buckets: []RateBucket{{Upto: 1 << 30, Chances: 1000, Answers: 999}}}
	card.Dice.MaxPerThread = 2
	rolls := diceOfThread(t, card, spread(40, time.Minute))

	var capped int
	for _, d := range rolls {
		if d.Reason == "" {
			continue
		}
		capped++
		if d.Reason != WhyThreadCap {
			t.Fatalf("%s: причина %q, а ожидалась %q", d.EventID, d.Reason, WhyThreadCap)
		}
		if d.Verdict != DiceSkip {
			t.Fatalf("%s: упёрся в потолок, а исход %q", d.EventID, d.Verdict)
		}
		if d.P != 0 {
			t.Fatalf("%s: броска не было, а вероятность %v", d.EventID, d.P)
		}
	}
	if capped == 0 {
		t.Fatal("житель ни разу не упёрся в потолок — тест ничего не проверил")
	}
}
