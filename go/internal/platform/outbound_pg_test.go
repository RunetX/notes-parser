package platform

// Исходящее чтение против настоящего Postgres: тут проверяется ровно то, что
// живёт в SQL, — маскирование анонима, отбор по полосе идентификаторов и
// выдержка перед отдачей. Подделкой это не проверить.

import (
	"context"
	"testing"
)

// В каналы уходит только СВОЁ и только видимое. Зеркальную заметку туда уже
// отнесло само зеркало, а снятую модератором или автором нельзя нести вовсе.
func TestOutboundTakesOnlyOwnAndVisible(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313028, 175869, "Гадёныш") // зеркальная: не наша забота
	author := mustUser(t, p, "Рио")
	visible := mustNote(t, p, author, "видимая")
	hidden := mustNote(t, p, author, "скрытая")
	if _, err := p.pool.Exec(ctx, `UPDATE notes SET status = 2 WHERE id = $1`, hidden); err != nil {
		t.Fatal(err)
	}

	got, err := p.OutboundNotes(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != visible {
		t.Fatalf("отдано %d заметок: %+v", len(got), got)
	}
}

// У анонимной заметки автор не покидает базу вовсе: маскирование стоит в
// SELECT, и в OutNote просто нет поля, куда его положить. Канал — такой же
// посторонний, как читатель страницы.
func TestOutboundMasksAnonymousAuthor(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "тайна", Anonymous: true})
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.OutboundNotes(ctx, 0, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("отдано %+v (%v)", got, err)
	}
	n := got[0]
	if n.ID != id || !n.Anonymous {
		t.Fatalf("заметка: %+v", n)
	}
	if n.AuthorID != 0 || n.AuthorNick != AnonNick {
		t.Errorf("автор анонимки уехал наружу: id=%d nick=%q", n.AuthorID, n.AuthorNick)
	}
	if len(n.AvatarSHA) != 0 {
		t.Error("у анонимки уехал аватар — по нему автора узна́ют без всякого ника")
	}
}

// Участник без анкеты НГС (пришёл по приглашению) уходит в канал ИМЕНЕМ, без
// номера анкеты: композер поста делает из номера ссылку на love.ngs.ru, а такой
// анкеты не существует.
func TestOutboundNativeUserHasNoProfileNumber(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Приглашённый")
	if !IsNative(author) {
		t.Fatalf("автор не из нативной полосы: %d", author)
	}
	mustNote(t, p, author, "текст")

	got, err := p.OutboundNotes(ctx, 0, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("отдано %+v (%v)", got, err)
	}
	if got[0].AuthorID != 0 {
		t.Errorf("нативному участнику приписан номер анкеты %d", got[0].AuthorID)
	}
	if got[0].AuthorNick != "Приглашённый" {
		t.Errorf("ник: %q", got[0].AuthorNick)
	}
}

// Свежая реплика ОТЛЁЖИВАЕТСЯ. Пришедшую из мессенджера мост помечает
// отправленной туда сразу после публикации, но записи идут в разные базы, и
// между ними есть щель; обход, попавший тактом в неё, принёс бы человеку копию
// его же сообщения. Выдержка эту щель закрывает.
func TestOutboundCommentWaitsOutTheDelay(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID := mustNote(t, p, author, "заметка")
	id, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "реплика"})
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := p.OutboundComments(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("свежая реплика отдана сразу: %+v", fresh)
	}

	if _, err := p.pool.Exec(ctx,
		`UPDATE comments SET published_at = published_at - $2::interval WHERE id = $1`,
		id, 2*OutboundDelay); err != nil {
		t.Fatal(err)
	}
	got, err := p.OutboundComments(ctx, 0, 100)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("отлежавшаяся реплика не отдана: %+v (%v)", got, err)
	}
	if got[0].NoteID != noteID {
		t.Errorf("заметка реплики: %d", got[0].NoteID)
	}
}

// Ребро адресата уезжает вместе с репликой: в мессенджере оно превращается в
// реплай, и цитату рисует сам мессенджер — обращения «Ник, » в теле нет и быть
// не должно.
func TestOutboundCommentCarriesReplyEdge(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	noteID := mustNote(t, p, author, "заметка")
	first, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "первая"})
	if err != nil {
		t.Fatal(err)
	}
	// Сдвигаем первую назад ДО публикации второй: иначе тест упрётся в «одна
	// реплика в десять секунд» — правило, которое здесь не проверяется.
	if _, err := p.pool.Exec(ctx,
		`UPDATE comments SET published_at = published_at - $1::interval`,
		2*OutboundDelay); err != nil {
		t.Fatal(err)
	}
	second, err := p.CreateComment(ctx,
		NewComment{NoteID: noteID, AuthorID: author, Body: "вторая", ReplyToID: first})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE comments SET published_at = published_at - $1::interval WHERE id = $2`,
		2*OutboundDelay, second); err != nil {
		t.Fatal(err)
	}

	got, err := p.OutboundComments(ctx, 0, 100)
	if err != nil || len(got) != 2 {
		t.Fatalf("отдано %+v (%v)", got, err)
	}
	// Порядок обязателен: в треде реплика не может появиться раньше того, на
	// что отвечает, — цитировать будет нечего.
	if got[0].ID != first || got[1].ID != second {
		t.Fatalf("порядок: %d, %d", got[0].ID, got[1].ID)
	}
	if got[1].ReplyToID != first {
		t.Errorf("ребро адресата: %d", got[1].ReplyToID)
	}
	if got[1].Body != "вторая" {
		t.Errorf("тело реплики: %q", got[1].Body)
	}
}
