package platform

import (
	"context"
	"errors"
	"testing"
)

// Галочка выключена по умолчанию, и это про согласие: публикуя здесь, человек
// соглашался на публикацию ЗДЕСЬ. Пока он не нажал сам — очередь пуста.
func TestБезГалочкиНичегоНеУносится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")

	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "заметка"}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Fatalf("в очереди %d строк при выключенной галочке", n)
	}
}

// С галочкой заметка встаёт в очередь, а анонимная — нет: на НГС её пришлось бы
// публиковать от имени автора, а он именно этого и не хотел.
func TestАнонимнаяЗаметкаНеУносится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}

	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "открыто"}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if n := outboxCount(t, p); n != 1 {
		t.Fatalf("после открытой заметки в очереди %d строк, ожидалась 1", n)
	}
	// Вторая заметка подряд упёрлась бы в потолок частоты, поэтому сдвигаем время.
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Anonymous: true, Body: "тайно"}); err != nil {
		t.Fatalf("анонимная заметка: %v", err)
	}
	if n := outboxCount(t, p); n != 1 {
		t.Errorf("анонимная заметка попала в очередь: строк %d", n)
	}
}

// Ответ под НАТИВНОЙ заметкой уносить некуда: на НГС такой заметки нет вовсе.
// Проверяется здесь, а не в службе, потому что решение принимается в момент
// публикации и той же транзакцией.
func TestОтветПодСвоейЗаметкойНеУносится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	noteID, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "своя заметка"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if IsNGS(noteID) {
		t.Fatal("заметка вышла в полосе НГС — тест проверяет не то")
	}
	before := outboxCount(t, p)
	if _, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "ответ"}); err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	if n := outboxCount(t, p); n != before {
		t.Errorf("ответ под нативной заметкой встал в очередь: было %d, стало %d", before, n)
	}
}

// Выдача считает попытку СРАЗУ, а не по исходу: сайт отвечает 500 и на принятый
// комментарий, поэтому «отправляется» неотличимо от «отправлено», и упавший
// посреди отправки демон иначе брал бы строку вечно.
func TestВыдачаСчитаетПопыткуИДоводитДоОтказа(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "текст заметки"}); err != nil {
		t.Fatalf("заметка: %v", err)
	}

	for i := 1; i <= NGSMaxAttempts; i++ {
		jobs, err := p.NextNGSJobs(ctx, 5)
		if err != nil {
			t.Fatalf("выдача %d: %v", i, err)
		}
		if len(jobs) != 1 {
			t.Fatalf("выдача %d вернула %d строк, ожидалась 1", i, len(jobs))
		}
		if jobs[0].Attempts != i {
			t.Errorf("попыток %d, ожидалось %d", jobs[0].Attempts, i)
		}
		if jobs[0].Body != "текст заметки" {
			t.Errorf("тело не дочитано: %q", jobs[0].Body)
		}
		if err := p.FinishNGSJob(ctx, jobs[0].ID, "", errors.New("сайт молчит")); err != nil {
			t.Fatalf("запись исхода: %v", err)
		}
	}
	// Попытки исчерпаны: строка больше не выдаётся и числится отказом.
	jobs, err := p.NextNGSJobs(ctx, 5)
	if err != nil {
		t.Fatalf("выдача после отказа: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("исчерпанная строка выдана снова: %d", len(jobs))
	}
	st, err := p.NGSOutboxStats(ctx)
	if err != nil {
		t.Fatalf("сводка: %v", err)
	}
	if st.Failed != 1 || st.Queued != 0 {
		t.Errorf("сводка: failed %d, queued %d", st.Failed, st.Queued)
	}
}

// Жителю галочка не даётся вовсе: у персонажа нет анкеты НГС, а значит и
// сессии, от чьего имени писать на сайте.
func TestЖителюОтправкаНеРазрешена(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	person := mustUser(t, p, "Житель")
	if _, err := p.pool.Exec(ctx, `UPDATE users SET persona = true WHERE id = $1`, person); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNGSSend(ctx, person, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("жителю разрешили отправку: %v", err)
	}
}

// mustNGSMember — участник в полосе НГС: тень плюс вход из бота. Нативный
// mustUser здесь не годится — очередь про перенос на сайт, где нативных нет.
func mustNGSMember(t *testing.T, p *Platform, id int64, nick string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: id, Nick: nick}); err != nil {
		t.Fatalf("тень %d: %v", id, err)
	}
	if _, err := p.CompleteBotLogin(ctx, id); err != nil {
		t.Fatalf("вход %d: %v", id, err)
	}
	mustConsent(t, p, id)
	return id
}

func outboxCount(t *testing.T, p *Platform) int {
	t.Helper()
	var n int
	if err := p.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ngs_outbox`).Scan(&n); err != nil {
		t.Fatalf("счёт очереди: %v", err)
	}
	return n
}
