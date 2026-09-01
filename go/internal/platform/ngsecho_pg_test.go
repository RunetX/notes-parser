package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sentComment — участник с галочкой, зеркальная заметка, свой ответ под ней и
// одна отданная демону попытка. Ровно то положение, в котором реплика уже лежит
// на НГС, а зеркало вот-вот принесёт её обратно.
func sentComment(t *testing.T, p *Platform, body string, fail bool) (author, noteID int64) {
	t.Helper()
	ctx := context.Background()
	author = mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	noteID = 312811
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: noteID, Author: MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "зеркальная заметка", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: body}); err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	jobs, err := p.NextNGSJobs(ctx, 5)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("выдача: %d строк, %v", len(jobs), err)
	}
	var cause error
	if fail {
		cause = errors.New("сайт ответил 500")
	}
	if err := p.FinishNGSJob(ctx, jobs[0].ID, "", cause); err != nil {
		t.Fatalf("запись исхода: %v", err)
	}
	return author, noteID
}

func echoNGSID(t *testing.T, p *Platform) string {
	t.Helper()
	var id string
	if err := p.pool.QueryRow(context.Background(),
		`SELECT ngs_id FROM ngs_outbox`).Scan(&id); err != nil {
		t.Fatalf("чтение ngs_id: %v", err)
	}
	return id
}

// Своя реплика узнаётся по тройке «автор, место, отпечаток текста», а не по
// одному тексту: побайтового совпадения не бывает — сайт схлопывает пробелы и
// делает nl2br, — зато совпадения всех трёх у чужой реплики не бывает тем более.
func TestСвояРепликаОпознаётсяПоТройке(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, noteID := sentComment(t, p, "Полку  прибил, а ровно ли — иди смотри.", false)

	own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author,
		"Полку прибил, а ровно ли — иди смотри.", "63238879", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if !own {
		t.Fatal("своя реплика не опознана — зеркало принесло бы её вторым экземпляром")
	}
	if got := echoNGSID(t, p); got != "63238879" {
		t.Errorf("id записи сайта не закреплён за строкой очереди: %q", got)
	}
	// Гашение эха работает МОЛЧА: удачное опознание не оставляет следа ни на
	// странице, ни в канале, и отличить «гасится» от «эха ещё не было» можно
	// только по этому счётчику — его и печатает platform doctor.
	st, err := p.NGSOutboxStats(ctx)
	if err != nil {
		t.Fatalf("сводка: %v", err)
	}
	if st.Echoed != 1 {
		t.Errorf("в сводке погашено %d эх, ожидалось 1", st.Echoed)
	}
}

// Главный предохранитель метода: ngs_id заполняется РОВНО ОДИН РАЗ, поэтому
// погасить эха больше, чем мы отправили, нельзя ни при какой сверке текстов.
// Ошибка отпечатка стоит одной непринесённой реплики, а не молчания треда.
func TestОднуОтправленнуюСтрокуНельзяОпознатьДважды(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, noteID := sentComment(t, p, "один и тот же текст", false)

	if own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author,
		"один и тот же текст", "1001", time.Now()); err != nil || !own {
		t.Fatalf("первое опознание: %v %v", own, err)
	}
	own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author,
		"один и тот же текст", "1002", time.Now())
	if err != nil {
		t.Fatalf("второе опознание: %v", err)
	}
	if own {
		t.Fatal("вторая реплика с тем же текстом опознана своей: человек потерял бы её насовсем")
	}
}

// Зеркало помнит ответ у себя в SQLite, и отказ этой записи не должен означать
// дубля на следующем такте: опознание идемпотентно по id самой записи.
func TestОпознаниеИдемпотентноПоIdЗаписи(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, noteID := sentComment(t, p, "текст реплики", false)

	for i := range 3 {
		own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author, "текст реплики", "2002", time.Now())
		if err != nil {
			t.Fatalf("опознание %d: %v", i, err)
		}
		if !own {
			t.Fatalf("повтор %d ответил «не наше» — зеркало завело бы дубль", i)
		}
	}
}

// Кандидатом считается строка, по которой была ХОТЯ БЫ ОДНА попытка, а не только
// удачная: сайт отвечает 500 и на ПРИНЯТЫЙ комментарий (замер 17.08.2026), и
// строка в состоянии failed сплошь и рядом лежит на НГС. Возьми мы одни лишь
// sent — именно эти реплики и вернулись бы дублем.
func TestОтвет500НеМешаетОпознатьСвоё(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, noteID := sentComment(t, p, "реплика, на которую сайт ответил ошибкой", true)

	own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author,
		"реплика, на которую сайт ответил ошибкой", "3003", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if !own {
		t.Fatal("строка после 500 не опознана — а сайт её принял")
	}
}

// Ждущая строка, которую демон ещё не брал в работу, своей быть не может: на
// сайте её нет. Иначе совпадение текста опознало бы ЧУЖУЮ реплику и погасило
// нашу собственную отправку заодно.
func TestНеотправленнаяСтрокаЭхомНеСчитается(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: 312811, Author: MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "зеркальная", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: 312811, AuthorID: author, Body: "ещё не ушло"}); err != nil {
		t.Fatal(err)
	}
	own, err := p.ClaimNGSEcho(ctx, NGSComment, 312811, author, "ещё не ушло", "4004", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if own {
		t.Fatal("строка без единой попытки опознана как лежащая на сайте")
	}
}

// Чужая реплика в том же треде и своя же в ДРУГОМ месте своими не считаются:
// совпасть обязаны все три признака сразу.
func TestЧужоеЗаСвоёНеПринимается(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author, noteID := sentComment(t, p, "наш текст под этой заметкой", false)

	cases := []struct {
		what   string
		note   int64
		author int64
		body   string
	}{
		{"другой автор", noteID, 498196, "наш текст под этой заметкой"},
		{"другая заметка", 312812, author, "наш текст под этой заметкой"},
		{"другой текст", noteID, author, "совсем другие слова, написанные кем-то ещё"},
	}
	for _, c := range cases {
		own, err := p.ClaimNGSEcho(ctx, NGSComment, c.note, c.author, c.body, "5005", time.Now())
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if own {
			t.Errorf("%s: опознано своим — реплика пропала бы из треда", c.what)
		}
	}
}

// Заметка опознаётся НАЧАЛОМ: лента отдаёт длинный текст срезом, и требовать от
// него полного совпадения значило бы не опознать ни одной длинной заметки.
func TestЗаметкаОпознаётсяСрезомЛенты(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	body := "Каждый раз одно и то же: приходишь домой, а там опять никого не ждали. " +
		"И ведь не сказать, что мне это в новинку, но всё равно каждый раз обидно до слёз."
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: body}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if _, err := p.NextNGSJobs(ctx, 5); err != nil {
		t.Fatalf("выдача: %v", err)
	}
	own, err := p.ClaimNGSEcho(ctx, NGSNote, 0, author, body[:150]+"…", "313200", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if !own {
		t.Fatal("своя заметка не опознана срезом ленты — вышла бы в ленте дважды")
	}
}

// СЛУЧАЙ ИЗ БОЯ 01.09.2026 (заметка 313146, первый же день работы галочки).
// Мы отправили реплику со смайликом «:D», а сайт подменил его эмодзи — и
// первая редакция сверки, требовавшая совпадения строк целиком, реплику не
// опознала. В треде площадки и в обоих каналах вышло по дублю.
func TestСмайликПодменённыйСайтомНеРазводитТексты(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ours := `В профиле появилась опция "Отправлять на НГС" :D`
	author, noteID := sentComment(t, p, ours, false)

	own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author,
		`В профиле появилась опция "Отправлять на НГС" 😃`, "63256939", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if !own {
		t.Fatal("реплика со смайликом не опознана — ровно этот дубль и вышел в бою")
	}
}

// Соседняя реплика того же прогона опозналась верно, и это надо удержать:
// живой эмодзи выпадает из сверки с обеих сторон одинаково, ломает не он.
func TestЖивойЭмодзиОпознаниюНеМешает(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	body := "Молодцы сибиряки и сибирячки, надо же как-то согреваться 💪"
	author, noteID := sentComment(t, p, body, false)

	own, err := p.ClaimNGSEcho(ctx, NGSComment, noteID, author, body, "63256934", time.Now())
	if err != nil {
		t.Fatalf("опознание: %v", err)
	}
	if !own {
		t.Fatal("реплика с эмодзи не опознана")
	}
}
