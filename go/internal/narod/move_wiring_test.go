package narod

import (
	"context"
	"strings"
	"testing"
)

// ЧТО ДОЕЗЖАЕТ ДО ЗАДАНИЯ В БОЮ — тот же жанр, что wiring_test.go, и то же
// правило: величина, введённая для реплики, обязана иметь тест НА ПУТИ ДАННЫХ,
// а не только на формуле. Уроков за неделю три (пол собеседника, четыре
// множителя кубика, темы заметки), и все они были молчаливыми.
//
// Каждый тест здесь падает на коде до 01.09.2026: до него хода не было вовсе,
// панч стоял в каждой реплике, канон не считался, а житель-шум читал ветку
// наравне со всеми.

// composeWith — точка письма, собранная службой, а не руками теста.
func composeWith(t *testing.T, card *Card, rates map[Move]float64,
	note StageNote, thread []StageReply, replyTo int64) WritePoint {
	t.Helper()
	svc, _ := testService(t, &fakeStage{notes: []StageNote{note}, thread: thread})
	if rates != nil {
		svc.cfg.MoveRates = rates
	}
	p := Player{Card: card, UserID: 42}
	pl := Plan{ID: 1, ActorID: card.ID, NoteID: note.ID, ReplyTo: replyTo}
	point, err := svc.compose(context.Background(), p, pl, note, thread)
	if err != nil {
		t.Fatalf("сборка точки письма: %v", err)
	}
	return point
}

func moveCard() *Card {
	c := testCard()
	c.ID = "hodok"
	c.Persona = Bio{Nick: "Ходок", Gender: "male", Job: "автослесарь в гараже"}
	c.Register.Runes = Dist{P10: 30, Median: 70, P90: 200, Max: 400}
	return &c
}

var moveNote = StageNote{ID: 100000000032, AuthorID: 1, AuthorNick: "Паноптикум",
	AuthorGender: "male", Body: "жена четыре года копила на ремонт"}

// ХОД доезжает до задания и им же и остаётся: короткий ход не спорит с длиной.
func TestХодДоезжаетДоЗадания(t *testing.T) {
	point := composeWith(t, moveCard(), map[Move]float64{MoveShort: 1},
		moveNote, nil, 0)
	if point.Move != MoveShort {
		t.Fatalf("ход не доехал: %q", point.Move)
	}
	task := writePrompt(point, "")
	if !strings.Contains(task, "КОРОТКАЯ") {
		t.Error("ход не назван в задании")
	}
	if !point.OneThought {
		t.Error("короткий ход не сделал реплику однофразовой")
	}
}

// ПАНЧ — теперь ход из семи, а не свойство каждой реплики. На чужом ходе его
// нет ни в точке, ни в задании.
func TestПанчТолькоНаСвоёмХоде(t *testing.T) {
	point := composeWith(t, moveCard(), map[Move]float64{MoveStory: 1},
		moveNote, nil, 0)
	if point.Punch != "" {
		t.Errorf("панч выпал на ходе «свой случай»: %q", point.Punch)
	}
	if task := writePrompt(point, ""); strings.Contains(task, "СМЕШНОЙ") {
		t.Error("требование шутить уехало в задание чужого хода")
	}
	punch := composeWith(t, moveCard(), map[Move]float64{MovePunch: 1},
		moveNote, nil, 0)
	if punch.Punch == "" {
		t.Error("на своём ходе панча нет")
	}
}

// КАНОН треда доезжает до задания запретом.
func TestКанонДоезжаетДоЗадания(t *testing.T) {
	var thread []StageReply
	for i, a := range []int64{11, 12, 13} {
		thread = append(thread, StageReply{
			ID: int64(500 + i), NoteID: moveNote.ID, AuthorID: a,
			AuthorNick: "Сосед", Body: "там же аквариум был",
		})
	}
	point := composeWith(t, moveCard(), map[Move]float64{MoveStory: 1},
		moveNote, thread, 0)
	if len(point.Canon) == 0 || point.Canon[0] != "аквариум" {
		t.Fatalf("канон не доехал: %v", point.Canon)
	}
	if !strings.Contains(writePrompt(point, ""), "аквариум") {
		t.Error("канон не назван в задании")
	}
}

// ЖИТЕЛЬ-ШУМ ветки не читает: он видит заметку, и только её.
func TestШумНеЧитаетВетку(t *testing.T) {
	card := moveCard()
	card.Noise = true
	thread := []StageReply{{ID: 501, NoteID: moveNote.ID, AuthorID: 11,
		AuthorNick: "Сосед", Body: "а у меня аквариум"}}
	point := composeWith(t, card, map[Move]float64{MoveShort: 1}, moveNote, thread, 0)
	if len(point.Thread) != 0 {
		t.Error("шуму показали ветку")
	}
	if len(point.Canon) != 0 {
		t.Error("шуму посчитали канон, которого он не видел")
	}
	if strings.Contains(writePrompt(point, ""), "Что уже сказано") {
		t.Error("ветка уехала в задание жителя-шума")
	}
}

// ПРИДИРКА поднимает ступень эскалации: без этого ссоре взяться неоткуда —
// лестница считается от накала пары, а в свежем треде накал у всех нулевой.
func TestПридиркаПоднимаетСтупень(t *testing.T) {
	thread := []StageReply{{ID: 601, NoteID: moveNote.ID, AuthorID: 11,
		AuthorNick: "Сосед", Gender: "male", Body: "сама виновата"}}
	quiet := composeWith(t, moveCard(), map[Move]float64{MoveStory: 1}, moveNote, thread, 601)
	nag := composeWith(t, moveCard(), map[Move]float64{MoveGrumble: 1}, moveNote, thread, 601)
	if strings.Contains(quiet.Mood, "переходишь с предмета") {
		t.Error("спокойный ход поднял ступень")
	}
	if !strings.Contains(nag.Mood, "переходишь с предмета") {
		t.Errorf("придирка не подняла ступень: %q", nag.Mood)
	}
}

// ЕВАЛЫ: ролевой зачин, чужое ремесло и подхваченный канон — брак.
func TestЕвалыРолиРемеслаИКанона(t *testing.T) {
	p := WritePoint{Card: moveCard(), Canon: []string{"аквариум"}}
	cases := []struct{ text, want string }{
		{"как автослесарь скажу: там стук", "ролевой зачин"},
		{"я сантехник, у меня то же самое", "ты не сантехник"},
		{"а аквариум тут при чём", "уже повторяют"},
		{"стучит у неё не первый год, а слушала молча", ""},
	}
	for _, c := range cases {
		got := checkDraft(c.text, p)
		if c.want == "" && got != "" {
			t.Errorf("%q: лишний брак %q", c.text, got)
			continue
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%q: брак %q, ожидалось про %q", c.text, got, c.want)
		}
	}
	// Своё ремесло называть можно — оно в карточке.
	if got := checkDraft("я автослесарь, у нас так же", p); got != "" {
		t.Errorf("своё ремесло объявлено чужим: %q", got)
	}
	// А тому, кому владелец разрешил зачин поимённо, он разрешён.
	allowed := moveCard()
	allowed.RoleLead = true
	if got := checkDraft("как автослесарь скажу: там стук", WritePoint{Card: allowed}); got != "" {
		t.Errorf("разрешённый зачин забракован: %q", got)
	}
}

// ЖИТЕЛЬ-ШУМ не судится по стилю, но приметы машины и чужое ремесло остаются:
// пустая реплика не перестаёт быть машинной оттого, что она пустая.
func TestШумНеСудитсяПоСтилю(t *testing.T) {
	card := moveCard()
	card.Noise = true
	yes := true
	p := WritePoint{Card: card, Emoji: &yes, Digit: true}
	if got := checkDraft("плюсую", p); got != "" {
		t.Errorf("шум забракован по стилю: %q", got)
	}
	if got := checkDraft("как сварщик скажу: плюсую", p); got == "" {
		t.Error("шуму сошёл с рук ролевой зачин")
	}
}
