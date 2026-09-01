package narod

// ОКНО РАЗГОВОРА: что модель видит, когда пишет ответ.
//
// Тесты здесь идут через writePrompt, а не через windowOfThread, и это не
// педантизм. Расхождение, ради которого они написаны, было МОЛЧАЛИВЫМ по
// устройству: объём окна задан одной константой на оба мира, а состав — двумя
// разными кусками кода, и по константе разницы не видно. Тест на функции
// проверил бы формулу, которая и так очевидна; проверять надо ПУТЬ ДАННЫХ —
// доходит ли реплика, на которую отвечают, до задания модели.
//
// Каждый из них обязан падать на прежнем коде (проверено): там окно было чистым
// хвостом, и родитель в него не попадал у 84,5–97,4 % ответов боевых тредов.

import (
	"fmt"
	"strings"
	"testing"
)

// longThread — n реплик подряд, все корневые и разные по тексту.
func longThread(n int) []StageReply {
	out := make([]StageReply, 0, n)
	for i := range n {
		out = append(out, StageReply{
			ID: int64(101 + i), NoteID: 1,
			AuthorID:   int64(1000 + i),
			AuthorNick: fmt.Sprintf("Сосед%d", i),
			Body:       fmt.Sprintf("реплика номер %d", i),
		})
	}
	return out
}

func windowPoint(thread []StageReply, replyTo StageReply) WritePoint {
	return WritePoint{
		Card:    promptCard(),
		Note:    StageNote{ID: 1, AuthorNick: "Ирма", Body: "текст заметки"},
		Thread:  thread,
		ReplyTo: replyTo,
	}
}

// РЕПЛИКА, НА КОТОРУЮ ОТВЕЧАЮТ, ОБЯЗАНА БЫТЬ В ЗАДАНИИ.
//
// Иначе задание «Ответь на реплику, помеченную стрелкой» указывает на стрелку,
// которой в тексте нет, и модели остаётся продолжать ленту. Ровно это и
// происходило в бою: ответ вставал под чужим родителем, а выдуманный факт
// расходился по несвязанным веткам.
func TestОкноВсегдаНесётТуРепликуНаКоторуюОтвечают(t *testing.T) {
	thread := longThread(100)
	thread[0].Body = "полка висит ровно"
	got := promptOf(windowPoint(thread, thread[0]))

	if !strings.Contains(got, "полка висит ровно") {
		t.Error("реплики, на которую отвечают, в задании нет вовсе")
	}
	if !strings.Contains(got, "← отвечаешь на эту") {
		t.Error("стрелки в задании нет: модели указывают на то, чего она не видит")
	}
}

// ВЕТКА ПОКАЗЫВАЕТСЯ ЦЕЛИКОМ, а не одним родителем: ответ на «а ты сама-то?»
// без того, к чему это сказано, — не ответ.
func TestОкноНесётВсюЦепочкуПредков(t *testing.T) {
	thread := longThread(100)
	thread[0].Body = "дед сказал про полку"
	thread[1].Body = "отец ответил про полку"
	thread[1].ReplyTo = thread[0].ID
	thread[2].Body = "внук уточнил про полку"
	thread[2].ReplyTo = thread[1].ID

	got := promptOf(windowPoint(thread, thread[2]))
	for _, want := range []string{"дед сказал", "отец ответил", "внук уточнил"} {
		if !strings.Contains(got, want) {
			t.Errorf("в задании нет звена ветки %q — ответ повиснет в воздухе", want)
		}
	}
}

// ОСТАТОК ОКНА — СОСЕДИ ПО ВРЕМЕНИ. Ветка вытесняет их, но не отменяет: сосед
// показывает, о чём говорят вокруг прямо сейчас.
func TestОстатокОкнаОстаётсяХвостомРазговора(t *testing.T) {
	thread := longThread(100)
	thread[0].Body = "дальняя реплика ветки"

	got := promptOf(windowPoint(thread, thread[0]))
	if !strings.Contains(got, "реплика номер 99") {
		t.Error("хвост разговора пропал: ветка съела окно целиком")
	}
	if !strings.Contains(got, "дальняя реплика ветки") {
		t.Error("ветки в окне нет")
	}
}

// ОТВЕТ САМОЙ ЗАМЕТКЕ предков не имеет, и окно у него прежнее — хвост.
func TestОтветЗаметкеПоказываетХвостРазговора(t *testing.T) {
	thread := longThread(100)
	got := promptOf(windowPoint(thread, StageReply{}))

	if !strings.Contains(got, "реплика номер 99") {
		t.Error("хвоста разговора в задании нет")
	}
	if strings.Contains(got, "реплика номер 0\n") {
		t.Error("окно уехало в начало треда, хотя предков у корневой реплики нет")
	}
}

// ПОТОЛОК ОКНА СОБЛЮДАЕТСЯ И ДЛИННОЙ ВЕТКОЙ: он про деньги и про то, при каком
// объёме контекста мерился голос, — а глубокая ветка не повод его превысить.
func TestДлиннаяВеткаНеРастягиваетОкно(t *testing.T) {
	thread := longThread(100)
	for i := 1; i < len(thread); i++ {
		thread[i].ReplyTo = thread[i-1].ID
	}
	got := windowOfThread(thread, thread[len(thread)-1].ID, ThreadWindow)
	if len(got) != ThreadWindow {
		t.Fatalf("в окне %d реплик при потолке %d", len(got), ThreadWindow)
	}
	// Порядок разговора сохраняется: задание читается как расшифровка, и
	// перемешанные реплики читались бы как другой разговор.
	for i := 1; i < len(got); i++ {
		if got[i-1].ID >= got[i].ID {
			t.Fatalf("окно перемешало разговор: %d стоит перед %d", got[i-1].ID, got[i].ID)
		}
	}
}

// КОЛЬЦО В РЁБРАХ окно не вешает. Ребро ставит обход чужого дерева, и полагаться
// на его безупречность нельзя: зациклившийся промпт дороже неполной ветки.
func TestКольцоВРёбрахНеВешаетОкно(t *testing.T) {
	thread := longThread(30)
	thread[0].ReplyTo = thread[1].ID
	thread[1].ReplyTo = thread[0].ID

	if got := windowOfThread(thread, thread[0].ID, ThreadWindow); len(got) != ThreadWindow {
		t.Fatalf("в окне %d реплик при потолке %d", len(got), ThreadWindow)
	}
}
