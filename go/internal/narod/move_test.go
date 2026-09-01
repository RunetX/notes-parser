package narod

import (
	"math/rand/v2"
	"testing"
)

// Ход, не подходящий точке, в жребии НЕ УЧАСТВУЕТ, а доли остальных
// пересчитываются: два указания, столкнувшиеся лбами, решались бы порядком
// строк в промпте (тот же довод, что у pickPunch и «встречного вопроса»).
func TestХодПодбираетсяПоТочке(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	rates := DefaultMoveRates()

	// Пишем в саму заметку: придираться к соседу не к кому.
	for i := 0; i < 500; i++ {
		if m := PickMove(rng, rates, MoveFit{ToAuthor: true}); m == MoveGrumble {
			t.Fatal("придирка выпала там, где отвечать некому")
		}
	}
	// Отвечаем не автору: реплики автору здесь взяться неоткуда.
	for i := 0; i < 500; i++ {
		if m := PickMove(rng, rates, MoveFit{ToPeer: true}); m == MoveAuthor {
			t.Fatal("ход «автору» выпал там, где автор реплики не увидит")
		}
	}
}

// Доли соблюдаются: жребий — это распределение, а не список.
func TestДолиХодовСоблюдаются(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	got := map[Move]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		got[PickMove(rng, DefaultMoveRates(), MoveFit{ToAuthor: true, ToPeer: true})]++
	}
	for m, want := range DefaultMoveRates() {
		have := float64(got[m]) / n
		if have < want-0.02 || have > want+0.02 {
			t.Errorf("ход %s: выпал в %.3f при доле %.3f", m, have, want)
		}
	}
}

// Опечатка в ключе не должна молча обнулять целый ход: отказ на сборке.
func TestНеизвестныйХодОтвергается(t *testing.T) {
	if err := ValidateMoveRates(map[Move]float64{"панч": 1}); err == nil {
		t.Error("неизвестный ход принят")
	}
	if err := ValidateMoveRates(map[Move]float64{MovePunch: 0.2, MoveShort: 0.1}); err == nil {
		t.Error("сумма 0,3 принята за замысел")
	}
	if err := ValidateMoveRates(nil); err != nil {
		t.Errorf("пустой набор должен означать умолчания: %v", err)
	}
	if err := ValidateMoveRates(DefaultMoveRates()); err != nil {
		t.Errorf("умолчания не прошли собственную проверку: %v", err)
	}
}

// КАНОН: слово из заметки не выдумано, сколько бы человек его ни повторило.
func TestСловоИзЗаметкиНеКанон(t *testing.T) {
	note := StageNote{ID: 1, AuthorNick: "Автор", Body: "четыре года ждала свадьбу"}
	var thread []StageReply
	for i, a := range []int64{11, 12, 13, 14} {
		thread = append(thread, StageReply{ID: int64(i + 1), AuthorID: a, Body: "свадьбу все ждут"})
	}
	if got := canonWords(note, thread, 99); len(got) != 0 {
		t.Errorf("слово из заметки объявлено каноном: %v", got)
	}
}

// А выдуманное — канон, но лишь когда его подхватили ТРОЕ. Двое — это ещё
// разговор.
func TestВыдуманноеСловоСтановитсяКанономНаТретьем(t *testing.T) {
	note := StageNote{ID: 1, AuthorNick: "Автор", Body: "сосед шумит по ночам"}
	two := []StageReply{
		{ID: 1, AuthorID: 11, Body: "у них там аквариум небось"},
		{ID: 2, AuthorID: 12, Body: "аквариум и рыбки"},
	}
	if got := canonWords(note, two, 99); len(got) != 0 {
		t.Errorf("двое ещё не канон: %v", got)
	}
	three := append(two, StageReply{ID: 3, AuthorID: 13, Body: "тот самый аквариум"})
	got := canonWords(note, three, 99)
	if len(got) == 0 || got[0] != "аквариум" {
		t.Errorf("канон не найден: %v", got)
	}
	// Своё слово жителю не запрещается: человек, дважды помянувший собственную
	// историю, — это человек, а не заражение.
	if got := canonWords(note, three, 12); len(got) != 0 {
		t.Errorf("жителю запретили его же слово: %v", got)
	}
}

// Ники участников каноном не бывают: обращение по имени — это разговор.
func TestНикиНеКанон(t *testing.T) {
	note := StageNote{ID: 1, AuthorNick: "Паноптикум", Body: "сосед шумит"}
	var thread []StageReply
	for i, a := range []int64{11, 12, 13} {
		thread = append(thread, StageReply{
			ID: int64(i + 1), AuthorID: a, AuthorNick: "Каланча",
			Body: "Каланча дело говорит, Паноптикум тоже",
		})
	}
	for _, w := range canonWords(note, thread, 99) {
		if w == "каланча" || w == "паноптикум" {
			t.Errorf("ник объявлен каноном: %q", w)
		}
	}
}

func TestКанонНаходитсяВТексте(t *testing.T) {
	if got := canonHit("а что там с аквариумом", []string{"аквариум"}); got != "" {
		t.Errorf("другая форма слова принята за повтор: %q", got)
	}
	if got := canonHit("тот самый аквариум", []string{"аквариум"}); got != "аквариум" {
		t.Errorf("повтор не пойман: %q", got)
	}
}
