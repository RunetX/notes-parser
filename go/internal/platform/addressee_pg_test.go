package platform

// Кому отвечает «Ник, …» в СМЕШАННОМ треде.
//
// Против настоящего Postgres, потому что всё правило живёт в SQL: переход по
// reply_to_id, сведение регистра кириллицы, порядок по паре «время, id».

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"lovegw/internal/love"
)

// talkStart — начало разговора. Время задаётся явно и с шагом в минуту: правило
// упорядочивает реплики по published_at, и «сейчас» сделало бы тест зависимым от
// скорости машины.
var talkStart = time.Date(2026, 8, 24, 0, 19, 3, 0, time.UTC)

func at(min int) time.Time { return talkStart.Add(time.Duration(min) * time.Minute) }

// mirrored — зеркальная реплика с уже известным адресатом.
func mirrored(t *testing.T, p *Platform, id, noteID, author int64, nick string, replyTo int64, when time.Time) {
	t.Helper()
	mirroredText(t, p, id, noteID, author, nick, replyTo, when, "реплика")
}

// mirroredText — то же, но с настоящим текстом реплики, и кладётся он ТАК ЖЕ,
// как это делает приёмник: префикс уходит в ребро только когда адресат нашёлся
// (platsink.commentFrom). Иначе обращение осталось бы в теле там, где в бою его
// не бывает, — и тест проверял бы не то, что работает.
func mirroredText(t *testing.T, p *Platform, id, noteID, author int64, nick string,
	replyTo int64, when time.Time, text string) {
	t.Helper()
	body, source := text, ReplyNone
	if replyTo != 0 {
		body, source = love.TrimAddressPrefix(text), ReplyPrefix
	}
	if _, err := p.IngestComment(context.Background(), MirroredComment{
		ID: id, NoteID: noteID, ReplyToID: replyTo, ReplySource: source,
		Author:      MirroredAuthor{ID: author, Nick: nick},
		Body:        body,
		PublishedAt: when,
	}); err != nil {
		t.Fatalf("приём %d: %v", id, err)
	}
}

// nativeReply — реплика, написанная НА ПЛОЩАДКЕ, с заданным временем.
func nativeReply(t *testing.T, p *Platform, nick string, noteID, replyTo int64, when time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	author := mustUser(t, p, nick+" (нативный)")
	if _, err := p.pool.Exec(ctx, `UPDATE users SET nick = $2 WHERE id = $1`, author, nick); err != nil {
		t.Fatalf("ник: %v", err)
	}
	id, err := p.CreateComment(ctx, NewComment{
		NoteID: noteID, AuthorID: author, Body: "С какой целью интересуетесь?", ReplyToID: replyTo,
	})
	if err != nil {
		t.Fatalf("нативная реплика: %v", err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE comments SET published_at = $2 WHERE id = $1`, id, when); err != nil {
		t.Fatalf("время нативной реплики: %v", err)
	}
	return id
}

// ГЛАВНОЕ: ответ с НГС на реплику, написанную НА ПЛОЩАДКЕ, попадает именно к
// ней. Зеркало этого дать не может — нативной реплики в его базе нет вовсе, и
// обход мобильного дерева её тоже не видит, поэтому неверное ребро осталось бы
// навсегда (жалоба владельца 24.08.2026).
func TestAddresseeCrossesTheBands(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")

	// 00:19 — Паноптикум с НГС, обращается к The Cranberries.
	mirrored(t, p, 63238860, 313058, 175869, "Паноптикум", 0, at(0))
	// 00:22 — её ответ ему.
	mirrored(t, p, 63238861, 313058, 2, "The Cranberries", 63238860, at(3))
	// 00:29 и 00:34 — обмен без обращений.
	mirrored(t, p, 63238862, 313058, 175869, "Паноптикум", 0, at(10))
	mirrored(t, p, 63238863, 313058, 2, "The Cranberries", 0, at(15))

	// 00:35 — а вот это он написал ЗДЕСЬ, и на НГС этой реплики нет.
	native := nativeReply(t, p, "Паноптикум", 313058, 63238863, at(16))

	got, err := p.AddresseeInNote(ctx, 313058, "Паноптикум", "The Cranberries", at(17), 63238864)
	if err != nil {
		t.Fatalf("адресат: %v", err)
	}
	if got != native {
		t.Fatalf("ответ уехал к %d, а Паноптикум последний раз говорил в %d (нативная полоса)", got, native)
	}
}

// Ступень первая бьёт вторую: у говорливого адресата реплик в треде много, и
// выбирается та, что обращена к самому отвечающему.
func TestAddresseePrefersReplyToMe(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	mirrored(t, p, 63238870, 313058, 3, "Т 72Б", 0, at(0))
	mirrored(t, p, 63238871, 313058, 4, "Хатуль мадан", 63238870, at(1)) // ответил Т 72Б
	mirrored(t, p, 63238872, 313058, 5, "Лилит", 0, at(2))
	mirrored(t, p, 63238873, 313058, 4, "Хатуль мадан", 63238872, at(3)) // ответил Лилит

	got, err := p.AddresseeInNote(ctx, 313058, "Хатуль мадан", "Т 72Б", at(4), 63238874)
	if err != nil {
		t.Fatalf("адресат: %v", err)
	}
	if got != 63238871 {
		t.Fatalf("ответ Т 72Б уехал к %d, а обращён к нему был 63238871", got)
	}
}

// Ника в треде нет — ноль, и это рабочий ответ: реплика встанет корнем ветки.
func TestAddresseeUnknownNickIsZero(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	mirrored(t, p, 63238880, 313058, 2, "The Cranberries", 0, at(0))

	got, err := p.AddresseeInNote(ctx, 313058, "Кого-тут-нет", "The Cranberries", at(1), 63238881)
	if err != nil {
		t.Fatalf("адресат: %v", err)
	}
	if got != 0 {
		t.Fatalf("незнакомый ник разошёлся в %d", got)
	}
}

// Граница честная: реплика из середины треда не цепляется за сказанное ПОСЛЕ
// неё. Это и есть условие того, что досылка сверкой даёт тот же ответ, что дал
// бы живой приём в её момент.
func TestAddresseeIgnoresLaterComments(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	mirrored(t, p, 63238890, 313058, 2, "The Cranberries", 0, at(0))
	mirrored(t, p, 63238892, 313058, 2, "The Cranberries", 0, at(10))

	got, err := p.AddresseeInNote(ctx, 313058, "The Cranberries", "Паноптикум", at(5), 63238891)
	if err != nil {
		t.Fatalf("адресат: %v", err)
	}
	if got != 63238890 {
		t.Fatalf("реплика из середины треда уехала к %d — это сказано позже неё", got)
	}
}

// Регистр кириллицы сводит Postgres. У зеркала это делается в Go, потому что
// lower() в SQLite знает только ASCII; здесь проверка, что на нашей стороне
// такой оговорки нет.
func TestAddresseeIsCaseInsensitiveInCyrillic(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	mirrored(t, p, 63238895, 313058, 7, "Лампочка", 0, at(0))

	got, err := p.AddresseeInNote(ctx, 313058, "ЛАМПОЧКА", "Паноптикум", at(1), 63238896)
	if err != nil {
		t.Fatalf("адресат: %v", err)
	}
	if got != 63238895 {
		t.Fatalf("«ЛАМПОЧКА» не разошлась в «Лампочка»: %d", got)
	}
}

// ДВА ВЫРАЖЕНИЯ ОДНОГО ПРАВИЛА обязаны отвечать одинаково. Пока весь тред
// зеркальный, у love.Addressees (по тексту) и у здешнего SQL (по рёбрам) данных
// поровну — расхождение тут означало бы, что правило разъехалось, а не что
// площадка знает больше.
func TestAddresseeAgreesWithMirrorOnPureNGSThread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")

	// Разговор в сыром виде, как его видит зеркало: обращение в ТЕКСТЕ.
	talk := []struct {
		id     int64
		author string
		text   string
	}{
		{63238900, "Паноптикум", "The Cranberries, начнём"},
		{63238901, "The Cranberries", "Паноптикум, ответ"},
		{63238902, "Лилит", "всем привет"},
		{63238903, "Паноптикум", "Лилит, и тебе"},
		{63238904, "The Cranberries", "просто реплика"},
	}
	book := love.NewAddressees[int64]()
	for i, s := range talk {
		var replyTo int64
		if nick := love.AddressPrefix(s.text); nick != "" {
			replyTo, _ = book.Resolve(nick, s.author)
		}
		mirroredText(t, p, s.id, 313058, int64(100+i), s.author, replyTo, at(i), s.text)
		book.Add(s.id, s.author, s.text)
	}

	for _, q := range []struct{ nick, replier string }{
		{"Паноптикум", "The Cranberries"},
		{"The Cranberries", "Паноптикум"},
		{"Лилит", "Паноптикум"},
		{"Паноптикум", "Лилит"},
	} {
		want, _ := book.Resolve(q.nick, q.replier)
		got, err := p.AddresseeInNote(ctx, 313058, q.nick, q.replier, at(len(talk)), 63238999)
		if err != nil {
			t.Fatalf("адресат: %v", err)
		}
		if got != want {
			t.Errorf("«%s, …» от %s: площадка сказала %d, зеркало — %d",
				q.nick, q.replier, got, want)
		}
	}
}

// Та же сверка, но на РАЗГОВОРЕ, которого никто не сочинял: сорок реплик,
// четыре собеседника, обращения куда попало. Пять человек в переписке дают
// формы, которые вручную не придумаешь, — обращение к тому, кто ещё молчал,
// два подряд к одному и тому же, ответ через десять реплик.
//
// Зерно постоянное: вердикт, меняющийся от прогона к прогону, не значит ничего
// (то же правило, по которому выбирается модель классификатора).
func TestAddresseeAgreesWithMirrorOnGeneratedTalk(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")

	nicks := []string{"Паноптикум", "The Cranberries", "Лилит", "Т 72Б"}
	rnd := rand.New(rand.NewSource(20260824))
	book := love.NewAddressees[int64]()
	var ids []int64

	for i := range 40 {
		who := rnd.Intn(len(nicks))
		author := nicks[who]
		text := fmt.Sprintf("реплика %d", i)
		// Две трети реплик — с обращением, и адресуются они кому угодно из
		// участников, включая тех, кто ещё не сказал ни слова.
		if rnd.Intn(3) != 0 {
			text = nicks[rnd.Intn(len(nicks))] + ", " + text
		}
		var replyTo int64
		if nick := love.AddressPrefix(text); nick != "" {
			replyTo, _ = book.Resolve(nick, author)
		}
		// Анкета выводится ИЗ НИКА, а не бросается отдельно: у одного человека
		// один номер, и случайная пара «ник ↔ анкета» проверяла бы разнобой
		// заготовки, а не правило.
		id := int64(63240000 + i)
		mirroredText(t, p, id, 313058, int64(200+who), author, replyTo, at(i), text)
		book.Add(id, author, text)
		ids = append(ids, id)
	}

	// Спрашиваем обоих обо ВСЕХ парах: правило про «кто кому», и промах бывает
	// именно на паре, а не на человеке.
	var checked int
	for _, nick := range nicks {
		for _, replier := range nicks {
			want, _ := book.Resolve(nick, replier)
			got, err := p.AddresseeInNote(ctx, 313058, nick, replier, at(len(ids)), 63249999)
			if err != nil {
				t.Fatalf("адресат: %v", err)
			}
			if got != want {
				t.Errorf("«%s, …» от %s: площадка сказала %d, зеркало — %d", nick, replier, got, want)
			}
			checked++
		}
	}
	if checked != len(nicks)*len(nicks) {
		t.Fatalf("сверено пар %d — заготовка не та", checked)
	}
}
