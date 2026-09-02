package platform

import (
	"context"
	"errors"
	"testing"
	"time"
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

// Метка «унесено на НГС» на странице (решение владельца 02.09.2026) считается
// ЗАПРОСОМ ПАЧКОЙ, и правил у неё два: зеркальные номера отсеиваются до SQL, а
// унесённым считается и строка с ошибкой, у которой сайт всё-таки вернул номер
// реплики, — сайт отвечает 500 и на ПРИНЯТУЮ (замер 17.08.2026).
func TestУнесённоеНаНГССчитаетсяПачкой(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	// Заметка ЗЕРКАЛЬНАЯ: под нативной ответ не уносится вовсе — на НГС такой
	// заметки нет, и нести реплику некуда.
	const note = 312811
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: note, Author: MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "зеркальная заметка", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}

	// Три реплики: уехавшая, ждущая очереди и та, что отказала, но номер на НГС
	// всё же получила.
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: author, Body: "реплика"})
		if err != nil {
			t.Fatalf("реплика %d: %v", i, err)
		}
		ids = append(ids, id)
		// Потолок частоты у комментария — одна реплика в десять секунд.
		if _, err := p.pool.Exec(ctx, `UPDATE comments SET published_at = published_at - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE ngs_outbox SET state = $1 WHERE kind = $2 AND object_id = $3`,
		NGSSent, NGSComment, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE ngs_outbox SET state = $1, ngs_id = '63238879' WHERE kind = $2 AND object_id = $3`,
		NGSFailed, NGSComment, ids[2]); err != nil {
		t.Fatal(err)
	}

	// В список подмешан зеркальный номер: до SQL он не доходит вовсе, и строки
	// в очереди у него быть не может по построению — уносим мы своё.
	sent, err := p.NGSSentObjects(ctx, NGSComment, append(append([]int64{}, ids...), 63238879))
	if err != nil {
		t.Fatalf("унесённое: %v", err)
	}
	if !sent[ids[0]] {
		t.Error("уехавшая реплика не помечена")
	}
	if sent[ids[1]] {
		t.Error("ждущая очереди реплика помечена как уехавшая")
	}
	if !sent[ids[2]] {
		t.Error("реплика с номером на НГС не помечена: отказ отправки не значит, что её там нет")
	}
	if sent[63238879] {
		t.Error("зеркальная реплика помечена как унесённая")
	}

	// Заметка и реплика с ОДНИМ номером — разные объекты: последовательности у
	// них свои, и путать их нельзя.
	if got, err := p.NGSSentObjects(ctx, NGSNote, ids[:1]); err != nil {
		t.Fatalf("унесённые заметки: %v", err)
	} else if got[ids[0]] {
		t.Error("реплика сошла за заметку с тем же номером")
	}
}

// ПЕСОЧНИЦА НА НГС НЕ УХОДИТ, и проверяется здесь именно ДВОЙНИК: 01.09.2026 на
// love.ngs.ru заметкой 313147 вышло его служебное тело («о заметке говорят
// жители площадки, их реплики пишет машина») под именем администратора. Сама
// заметка при этом уносится и должна уноситься — приложение к ней нет.
func TestПесочницаИДвойникНеУносятся(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustNGSAdmin(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, admin, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}

	if _, err := p.CreateNote(ctx, NewNote{AuthorID: admin, Body: "о чём поговорим", Stage: true}); err != nil {
		t.Fatalf("песочница: %v", err)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Fatalf("песочница встала в очередь на НГС: строк %d", n)
	}
	// Вторая заметка подряд упёрлась бы в потолок частоты, поэтому сдвигаем время.
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}

	base, err := p.CreateNote(ctx, NewNote{AuthorID: admin, Body: "живая заметка"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if n := outboxCount(t, p); n != 1 {
		t.Fatalf("обычная заметка не встала в очередь: строк %d", n)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, base)
	if err != nil {
		t.Fatalf("двойник: %v", err)
	}
	if n := outboxCount(t, p); n != 1 {
		t.Errorf("двойник %d встал в очередь на НГС: строк %d", twin, n)
	}
}

// Реплика в ЗЕРКАЛЬНОЙ песочнице — случай отдельный, и соседним правилом он не
// закрывается: у такой заметки на НГС есть настоящий тред, то есть машинной
// сцене было бы куда лечь. (У нативной песочницы реплику не уносит правило «под
// нативной заметкой некуда», и принимать одно за другое нельзя.)
func TestРепликаВЗеркальнойПесочницеНеУносится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustNGSAdmin(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, admin, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	const note = 312811
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: note, Author: MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "зеркальная заметка", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note, true, "сцена"); err != nil {
		t.Fatalf("перевод в песочницу: %v", err)
	}

	if _, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: admin, Body: "реплика"}); err != nil {
		t.Fatalf("реплика: %v", err)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Errorf("реплика из песочницы встала в очередь на НГС: строк %d", n)
	}
}

// mustNGSAdmin — участник полосы НГС с правами администратора: песочницу и
// двойника заводит только он, а галочка отправки бывает только у участника.
func mustNGSAdmin(t *testing.T, p *Platform, id int64, nick string) int64 {
	t.Helper()
	id = mustNGSMember(t, p, id, nick)
	if err := p.SetRole(context.Background(), Viewer{UserID: id, Role: RoleAdmin}, id, RoleAdmin); err != nil {
		t.Fatalf("права администратора у %d: %v", id, err)
	}
	return id
}
