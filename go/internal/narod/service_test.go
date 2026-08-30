package narod

// Смотр службы: что она вообще замечает на сцене.
//
// Оба теста здесь написаны по боевым дефектам одного дня (30.08.2026), и оба
// про одно и то же — про то, что песочница живёт РУКАМИ администратора: её
// заводят и закрывают когда угодно и над какой угодно заметкой, а служба обязана
// это пережить.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// fakeStage — сцена без Postgres. Хранит песочницы списком, потому что весь
// предмет обоих тестов — КАКИЕ заметки служба увидит, а не что в них написано.
type fakeStage struct {
	notes []StageNote
}

func (f *fakeStage) StageNotesSince(_ context.Context, after int64, limit int) ([]StageNote, error) {
	var out []StageNote
	for _, n := range f.notes {
		if n.ID > after && len(out) < limit {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeStage) StageThread(context.Context, int64) ([]StageReply, error) { return nil, nil }

func (f *fakeStage) StagePost(context.Context, int64, int64, int64, string) (int64, error) {
	return 0, errors.New("в этих тестах никто не говорит")
}

// off убирает заметку со сцены — ровно как снятая администратором галочка.
func (f *fakeStage) off(id int64) {
	var keep []StageNote
	for _, n := range f.notes {
		if n.ID != id {
			keep = append(keep, n)
		}
	}
	f.notes = keep
}

func testService(t *testing.T, stage Stage) (*Service, *World) {
	t.Helper()
	w := openTestWorld(t)
	cfg := Defaults()
	cfg.Mode = ModeDryRun
	svc, err := NewService(cfg, w, stage, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return svc, w
}

// ЗЕРКАЛЬНУЮ ПЕСОЧНИЦУ СЛУЖБА ВИДИТ, хотя её номер НИЖЕ уже виденных.
//
// Дефект был такой: смотр читал песочницы «после номера N» и держался на том,
// что номер растёт со временем. Верно, пока песочницами были только нативные
// заметки; с 30.08.2026 ею становится и зеркальная, а её номер лежит в полосе
// НГС, то есть ВСЕГДА ниже любого нативного. Курсор, ушедший к 100000000028, не
// мог увидеть заметку 313128 никогда — та же грабля, что у живого добора на
// смешанном треде: порядок по id верен внутри полосы и неверен между ними.
func TestСмотрВидитЗеркальнуюПесочницуПослеНативной(t *testing.T) {
	ctx := context.Background()
	native := StageNote{ID: 100000000028, AuthorID: 1, Body: "своя заметка"}
	mirror := StageNote{ID: 313128, AuthorID: 2, Body: "старая запись с НГС"}

	stage := &fakeStage{notes: []StageNote{mirror, native}}
	svc, w := testService(t, stage)

	// Первый смотр видит обе — и запоминает.
	if err := svc.Scan(ctx); err != nil {
		t.Fatalf("первый смотр: %v", err)
	}
	for _, id := range []int64{native.ID, mirror.ID} {
		known, err := w.KnownThread(ctx, id)
		if err != nil || !known {
			t.Errorf("заметку %d служба не заметила (%v)", id, err)
		}
	}

	// А теперь то, на чём всё и ломалось: зеркальная песочница появляется
	// ПОСЛЕ нативной, и номер у неё меньше.
	later := StageNote{ID: 313200, AuthorID: 2, Body: "ещё одна с НГС"}
	stage.notes = append(stage.notes, later)
	if err := svc.Scan(ctx); err != nil {
		t.Fatalf("второй смотр: %v", err)
	}
	known, err := w.KnownThread(ctx, later.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Error("зеркальную песочницу с номером ниже нативной служба не увидела")
	}
}

// Виденную песочницу второй смотр НЕ считает новой: круги треда — величина, по
// ней меряется затухание, и накручивать их тактом было бы враньём.
func TestВиденнаяПесочницаНеСчитаетсяЗаново(t *testing.T) {
	ctx := context.Background()
	stage := &fakeStage{notes: []StageNote{{ID: 313128, AuthorID: 2, Body: "текст"}}}
	svc, w := testService(t, stage)

	for i := 0; i < 3; i++ {
		if err := svc.Scan(ctx); err != nil {
			t.Fatalf("смотр %d: %v", i, err)
		}
	}
	th, err := w.ThreadOf(ctx, 313128)
	if err != nil {
		t.Fatal(err)
	}
	if th.Rounds != 1 {
		t.Errorf("кругов у треда %d, а разговора не было вовсе: такт накрутил их сам", th.Rounds)
	}
}

// ЗАМЕТКА, УШЕДШАЯ СО СЦЕНЫ, НЕ РОНЯЕТ СМОТР ОСТАЛЬНЫМ.
//
// Дефект стоил владельцу зря поставленной галочки: заметка 100000000028 была
// снята со сцены, смотр падал на ней каждые тридцать секунд с «заметки нет на
// сцене» — и до новой песочницы не доходил вовсе. Снятие признака делает
// администратор рукой, то есть это штатное событие, а не сбой.
func TestУшедшаяСоСценыЗаметкаНеРоняетСмотр(t *testing.T) {
	ctx := context.Background()
	gone := StageNote{ID: 100000000028, AuthorID: 1, Body: "закрытая песочница"}
	live := StageNote{ID: 313128, AuthorID: 2, Body: "новая песочница"}

	stage := &fakeStage{notes: []StageNote{gone}}
	svc, w := testService(t, stage)
	if err := svc.Scan(ctx); err != nil {
		t.Fatalf("первый смотр: %v", err)
	}

	// Администратор снял признак, а тред в мире остался живым — ровно то
	// состояние, в котором служба и застряла на бою.
	stage.off(gone.ID)
	stage.notes = append(stage.notes, live)
	if err := svc.Scan(ctx); err != nil {
		t.Fatalf("смотр упал из-за заметки, снятой со сцены: %v", err)
	}
	// Новую песочницу служба увидела — то есть смотр дошёл до конца.
	known, err := w.KnownThread(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Error("до новой песочницы смотр не дошёл")
	}
}

// look отвечает про ушедшую заметку ТИПИЗОВАННОЙ ошибкой: по ней вызывающий и
// отличает штатное «песочницу закрыли» от настоящего отказа базы, где
// останавливаться как раз правильно.
func TestУшедшаяСоСценыЗаметкаНазываетСебя(t *testing.T) {
	svc, _ := testService(t, &fakeStage{})
	_, _, err := svc.look(context.Background(), 313128)
	if !errors.Is(err, ErrOffStage) {
		t.Errorf("ошибка не называет себя: %v", err)
	}
}
