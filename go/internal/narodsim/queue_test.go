package narodsim

import (
	"testing"
	"time"
)

func TestQueueOrdersByTime(t *testing.T) {
	var q Queue[string]
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	q.Push(base.Add(3*time.Minute), "третий")
	q.Push(base.Add(time.Minute), "первый")
	q.Push(base.Add(2*time.Minute), "второй")

	var got []string
	for {
		_, v, ok := q.Pop()
		if !ok {
			break
		}
		got = append(got, v)
	}
	want := []string{"первый", "второй", "третий"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("порядок %v, ожидался %v", got, want)
		}
	}
}

// Равное время разрешается номером постановки, а не кучей. В живом треде
// событий одной секунды сколько угодно, и без этого правила прогон с тем же
// зерном давал бы разный результат — то есть на вопрос «стало ли лучше от
// правки промпта» отвечать было бы нечем.
func TestQueueBreaksTiesByInsertion(t *testing.T) {
	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	order := func() []string {
		var q Queue[string]
		for _, v := range []string{"а", "б", "в", "г", "д"} {
			q.Push(at, v)
		}
		var out []string
		for {
			_, v, ok := q.Pop()
			if !ok {
				return out
			}
			out = append(out, v)
		}
	}
	first := order()
	if len(first) != 5 {
		t.Fatalf("вынуто %d из 5", len(first))
	}
	for i, v := range []string{"а", "б", "в", "г", "д"} {
		if first[i] != v {
			t.Fatalf("порядок при равном времени %v", first)
		}
	}
	// И он тот же от прогона к прогону.
	for i := 0; i < 20; i++ {
		if got := order(); got[0] != "а" || got[4] != "д" {
			t.Fatalf("прогон %d дал другой порядок: %v", i, got)
		}
	}
}

// Событие, поставленное во время УЖЕ идущей обработки, встаёт на своё место —
// именно так житель назначает свой ответ: «через сорок минут», то есть между
// чужими репликами.
func TestQueuePushDuringDrain(t *testing.T) {
	var q Queue[string]
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	q.Push(base.Add(time.Minute), "чужая-1")
	q.Push(base.Add(10*time.Minute), "чужая-2")

	at, v, _ := q.Pop()
	if v != "чужая-1" {
		t.Fatalf("первым вынулось %q", v)
	}
	q.Push(at.Add(4*time.Minute), "наш ответ")

	var got []string
	for {
		_, v, ok := q.Pop()
		if !ok {
			break
		}
		got = append(got, v)
	}
	if len(got) != 2 || got[0] != "наш ответ" || got[1] != "чужая-2" {
		t.Errorf("ответ не встал между чужими: %v", got)
	}
}

func TestQueuePeekAndEmpty(t *testing.T) {
	var q Queue[int]
	if _, ok := q.Peek(); ok {
		t.Error("пустая очередь показала ближайшее событие")
	}
	if _, _, ok := q.Pop(); ok {
		t.Error("пустая очередь что-то вынула")
	}
	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	q.Push(at, 1)
	if q.Len() != 1 {
		t.Errorf("длина %d", q.Len())
	}
	if got, ok := q.Peek(); !ok || !got.Equal(at) {
		t.Errorf("Peek вернул %v, %v", got, ok)
	}
	if q.Len() != 1 {
		t.Error("Peek вынул событие, а обязан только посмотреть")
	}
}
