package platform

// Заметка, которая публикуется НЕ ЗДЕСЬ (решение владельца 02.09.2026: «если
// галка отправки стоит, то свою не создаём, отправляем на НГС, а с НГС забираем
// как обычно»).

import (
	"context"
	"errors"
	"testing"
)

// ЧЕРНОВИК — ЭТО ОТСУТСТВИЕ ЗАМЕТКИ, и проверять надо именно это. Прежде она
// выходила ДВАЖДЫ — строкой здесь и копией там, — и весь смысл правки в том,
// что второй копии больше нет: тред у заметки один, на сайте, и оттуда её со
// всем разговором приносит зеркало.
func TestЧерновикНеЗаводитЗаметкуЗдесь(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")

	id, err := p.QueueNGSNote(ctx, NewNote{AuthorID: author, Body: "поедет на сайт"})
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}
	if id == 0 {
		t.Fatal("черновик без номера")
	}
	var notes int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM notes`).Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != 0 {
		t.Errorf("заметка всё-таки заведена здесь: строк %d — она выйдет дважды", notes)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Errorf("черновик встал ещё и в очередь копий: строк %d", n)
	}
	// В пути её видно на «Моей странице»: без этой строки минута ожидания
	// читается как потерянный текст.
	if n, err := p.NGSDraftsPending(ctx, author); err != nil || n != 1 {
		t.Errorf("в пути числится %d (%v), ожидалась одна", n, err)
	}
}

// ПОТОЛОК ЧАСТОТЫ СЧИТАЕТ ЧЕРНОВИКИ, и без этого он у такого человека не
// работал бы ВОВСЕ: его заметки приезжают сюда зеркалом, то есть в полосе НГС, а
// notesRecentQuery отсекает всё ниже NativeIDBase. То есть считать его темп
// больше не по чему, кроме очереди.
func TestПотолокЧастотыСчитаетЧерновики(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")

	if _, err := p.QueueNGSNote(ctx, NewNote{AuthorID: author, Body: "первая"}); err != nil {
		t.Fatalf("первый черновик: %v", err)
	}
	_, err := p.QueueNGSNote(ctx, NewNote{AuthorID: author, Body: "вторая подряд"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("вторая заметка подряд прошла: %v", err)
	}
}

// СОГЛАСИЕ СПРАШИВАЮТ И ЗДЕСЬ. Проверка стоит в ЯДРЕ, а не в форме, по тому же
// доводу, что у CreateNote: писать можно не только из формы, и второй список
// правил однажды разошёлся бы с этим.
func TestЧерновикТребуетДействующегоСогласия(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	// Участник без подписанных согласий: тень, вошедшая ботом, и только.
	const id = 1493279
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: id, Nick: "Рио"}); err != nil {
		t.Fatalf("тень: %v", err)
	}
	if _, err := p.CompleteBotLogin(ctx, id); err != nil {
		t.Fatalf("вход: %v", err)
	}
	if _, err := p.QueueNGSNote(ctx, NewNote{AuthorID: id, Body: "без согласия"}); err == nil {
		t.Fatal("заметка ушла на сайт без действующего согласия")
	}
}

// САЙТ НЕ ВЗЯЛ — ПУБЛИКУЕМ ЗДЕСЬ. Текст человека не пропадает ни при каком
// отказе чужого сайта, и это главное свойство всей затеи: без отката заметку,
// которую написали, было бы негде найти.
//
// Заводится она ОБЫЧНЫМ путём, значит получает очередь модерации и событие шины
// наравне со всеми, — а вот в очередь копий НЕ встаёт: три попытки уже
// потрачены, и вторая очередь на ту же заметку однажды опубликовала бы её
// дважды.
func TestЧерновикПубликуетсяЗдесьКогдаСайтНеВзял(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, author, true); err != nil {
		t.Fatalf("галочка: %v", err)
	}
	draft, err := p.QueueNGSNote(ctx, NewNote{AuthorID: author, Anonymous: true, Body: "не долетело"})
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}

	noteID, err := p.PublishNGSDraftHere(ctx, draft)
	if err != nil {
		t.Fatalf("откат: %v", err)
	}
	if !IsNative(noteID) {
		t.Fatalf("заметка вышла с номером %d — не в нативной полосе", noteID)
	}
	var (
		anon  bool
		state string
		note  *int64
	)
	if err := p.pool.QueryRow(ctx,
		`SELECT anonymous FROM notes WHERE id = $1`, noteID).Scan(&anon); err != nil {
		t.Fatal(err)
	}
	if !anon {
		t.Error("анонимность потерялась при откате — текст вышел под именем автора")
	}
	if err := p.pool.QueryRow(ctx,
		`SELECT state, note_id FROM ngs_drafts WHERE id = $1`, draft).Scan(&state, &note); err != nil {
		t.Fatal(err)
	}
	if state != NGSDraftLocal || note == nil || *note != noteID {
		t.Errorf("черновик закрыт как %q, заметка %v", state, note)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Errorf("откатившаяся заметка встала в очередь на НГС: строк %d — она уедет четвёртой попыткой", n)
	}
	// Второй раз тот же черновик не откатывается: иначе повтор прохода завёл бы
	// вторую заметку с тем же текстом.
	if _, err := p.PublishNGSDraftHere(ctx, draft); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторный откат прошёл: %v", err)
	}
}

// ВЫДАЧА СЧИТАЕТ ПОПЫТКУ СРАЗУ — то же правило, что у очереди копий: сайт
// отвечает 500 и на ПРИНЯТУЮ заметку, значит «отправляется» неотличимо от
// «отправлено», и считать по исходу означало бы однажды опубликовать дважды.
func TestВыдачаЧерновикаСчитаетПопытку(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustNGSMember(t, p, 1493279, "Рио")
	if _, err := p.QueueNGSNote(ctx, NewNote{AuthorID: author, Body: "текст"}); err != nil {
		t.Fatalf("черновик: %v", err)
	}
	got, err := p.NextNGSDrafts(ctx, 5)
	if err != nil {
		t.Fatalf("выдача: %v", err)
	}
	if len(got) != 1 || got[0].Attempts != 1 || got[0].Body != "текст" {
		t.Fatalf("выдано %+v", got)
	}
	if err := p.SentNGSDraft(ctx, got[0].ID); err != nil {
		t.Fatalf("исход: %v", err)
	}
	// Отправленный черновик очередь больше не выдаёт.
	if again, err := p.NextNGSDrafts(ctx, 5); err != nil || len(again) != 0 {
		t.Errorf("ушедший черновик выдан снова: %v, %v", again, err)
	}
}
