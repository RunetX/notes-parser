package mirror

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/store"
)

// echoSink — приёмник, умеющий сказать «это наше» (как площадка).
type echoSink struct {
	fakeSink
	own  map[string]bool // «вид:id записи» → своё
	err  error
	asks int
}

func (e *echoSink) OwnEcho(_ context.Context, kind, siteID, _, _, _ string, _ time.Time) (bool, error) {
	e.asks++
	if e.err != nil {
		return false, e.err
	}
	return e.own[kind+":"+siteID], nil
}

func echoMirror(t *testing.T, site *fakeSite, sink *echoSink) (*Mirror, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, site, []Sink{sink}, Config{
		NotesLimit:   5,
		FeedInterval: time.Minute,
		SubNotify:    map[string]SubNotify{sink.Name(): sink.notify},
	}, slog.Default())
	return m, st
}

// Своя заметка, уехавшая на НГС, обратно не берётся: копия уже лежит на
// площадке нативной строкой, а в канал её отнёс platout. Возьми зеркало её —
// и заметка вышла бы дважды в обоих местах сразу.
func TestOwnNoteEchoIsNotMirrored(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{
		{ID: "n1", AuthorID: "500", Text: "наша, уехала на сайт"},
		{ID: "n2", AuthorID: "600", Text: "чужая"},
	}}
	sink := &echoSink{own: map[string]bool{"note:n1": true}}
	m, st := echoMirror(t, site, sink)

	m.feedCycle(ctx, false)

	if _, err := st.NoteByID(ctx, "n1"); err == nil {
		t.Error("своя заметка попала в зеркало")
	}
	if _, err := st.NoteByID(ctx, "n2"); err != nil {
		t.Errorf("чужая заметка потерялась: %v", err)
	}
	for _, c := range sink.calls {
		if c.kind == "note" && c.noteID == "n1" {
			t.Fatalf("своя заметка ушла в канал: %v", sink.calls)
		}
	}

	// Второй обход площадку не спрашивает вовсе: про чужую заметку ответ лежит
	// в notes, про свою — в ngs_echo, а лента будет показывать обе ещё сутки.
	// Без этой памяти каждый такт стоил бы запроса в Postgres на заметку.
	asks := sink.asks
	m.feedCycle(ctx, false)
	if sink.asks != asks {
		t.Errorf("повторный обход спросил площадку ещё %d раз(а)", sink.asks-asks)
	}
}

// Своя реплика в треде НЕ сохраняется, но ЧИСЛИТСЯ известной. Второе не менее
// важно первого: над saveComments стои́т сверка с серверным счётчиком треда, и
// «одной не хватает» зеркало лечит добором страниц — до девяти лишних запросов
// к сайту на каждую нашу отправку.
func TestOwnCommentEchoIsCountedButNotStored(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {
				{ID: 2, AuthorID: "500", AuthorName: "Я", Text: "наша реплика"},
				{ID: 1, AuthorID: "600", AuthorName: "А", Text: "чужая"},
			},
		},
		totals: map[string]int{"n1": 2},
	}
	sink := &echoSink{own: map[string]bool{"comment:2": true}}
	m, st := echoMirror(t, site, sink)
	m.feedCycle(ctx, false)

	n, _ := st.NoteByID(ctx, "n1")
	m.pollComments(ctx, n, &noteState{})

	ids, _ := st.CommentIDs(ctx, "n1")
	if ids[2] {
		t.Error("своя реплика сохранена в зеркале")
	}
	if !ids[1] {
		t.Error("чужая реплика потерялась")
	}
	if site.pageCalls != 0 {
		t.Errorf("зеркало полезло добирать страницы: %d — значит эхо не зачлось в счётчик",
			site.pageCalls)
	}
}

// Нерешённое равно несохранённому. Отказ площадки (лежит Postgres) читать как
// «не наше» нельзя: сохранив запись, зеркало отдаст её всем приёмникам, и дубль
// станет неотменимым. Отложенная же запись вернётся следующим обходом.
func TestUndecidedEchoDefersNoteInsteadOfDuplicating(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{notes: []love.Note{{ID: "n1", AuthorID: "500", Text: "наша"}}}
	sink := &echoSink{err: errors.New("Postgres недоступен")}
	m, st := echoMirror(t, site, sink)

	m.feedCycle(ctx, false)
	if _, err := st.NoteByID(ctx, "n1"); err == nil {
		t.Fatal("заметка сохранена, хотя своя она или нет — неизвестно")
	}

	// Площадка ожила — заметка не потеряна.
	sink.err = nil
	m.feedCycle(ctx, false)
	if _, err := st.NoteByID(ctx, "n1"); err != nil {
		t.Fatalf("отложенная заметка не вернулась: %v", err)
	}
}

// То же для треда, и цена ошибки здесь та же: реплика, сохранённая до ответа,
// уедет и на площадку вторым экземпляром, и в оба канала.
func TestUndecidedEchoDefersWholeThreadTick(t *testing.T) {
	ctx := context.Background()
	site := &fakeSite{
		notes: []love.Note{{ID: "n1", Text: "т"}},
		comments: map[string][]love.Comment{
			"n1": {{ID: 1, AuthorID: "500", AuthorName: "А", Text: "реплика"}},
		},
	}
	sink := &echoSink{}
	m, st := echoMirror(t, site, sink)
	m.feedCycle(ctx, false)
	n, _ := st.NoteByID(ctx, "n1")

	sink.err = errors.New("Postgres недоступен")
	m.pollComments(ctx, n, &noteState{})
	if ids, _ := st.CommentIDs(ctx, "n1"); len(ids) != 0 {
		t.Fatalf("реплика сохранена до ответа площадки: %v", ids)
	}

	sink.err = nil
	m.pollComments(ctx, n, &noteState{})
	if ids, _ := st.CommentIDs(ctx, "n1"); !ids[1] {
		t.Fatalf("отложенная реплика не вернулась: %v", ids)
	}
}
