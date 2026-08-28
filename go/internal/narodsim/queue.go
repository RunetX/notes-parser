// Пакет narodsim — реплей-харнесс эмуляции: поднять архивный тред, прогнать на
// месте его участника жителя и свести получившееся с оригиналом.
//
// Импортирует narod и archive, но НЕ platform: Postgres для калибровки не нужен
// вовсе, а значит прогон идёт на машине разработчика, где площадка не поднята.
// Это не удобство — это условие того, что калибровку вообще будут гонять: цикл
// «правка промпта → прогон → отчёт» обязан замыкаться там же, где правят.
package narodsim

import (
	"container/heap"
	"time"
)

// Queue — очередь событий по времени: симуляция всегда берёт САМОЕ РАННЕЕ и
// переводит на него стрелки виртуальных часов.
//
// Порядок при равном времени решает номер постановки, а не куча. Без этого два
// события одной секунды — а в живом треде их сколько угодно — вынимались бы в
// том порядке, в каком куча их разложила, и прогон с тем же зерном давал бы
// разный результат. Воспроизводимость здесь не удобство: без неё нельзя
// ответить, стало ли лучше от правки промпта.
type Queue[T any] struct {
	h   itemHeap[T]
	seq int64
}

// Push ставит событие на момент at.
func (q *Queue[T]) Push(at time.Time, v T) {
	q.seq++
	heap.Push(&q.h, item[T]{at: at, seq: q.seq, val: v})
}

// Pop вынимает самое раннее событие. Второе значение — false, если очередь
// пуста.
func (q *Queue[T]) Pop() (time.Time, T, bool) {
	if len(q.h) == 0 {
		var zero T
		return time.Time{}, zero, false
	}
	it := heap.Pop(&q.h).(item[T])
	return it.at, it.val, true
}

// Peek — время ближайшего события, не вынимая его.
func (q *Queue[T]) Peek() (time.Time, bool) {
	if len(q.h) == 0 {
		return time.Time{}, false
	}
	return q.h[0].at, true
}

// Len — сколько событий ждёт.
func (q *Queue[T]) Len() int { return len(q.h) }

type item[T any] struct {
	at  time.Time
	seq int64
	val T
}

type itemHeap[T any] []item[T]

func (h itemHeap[T]) Len() int { return len(h) }

func (h itemHeap[T]) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}

func (h itemHeap[T]) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *itemHeap[T]) Push(x any) { *h = append(*h, x.(item[T])) }

func (h *itemHeap[T]) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}
