package platform

// ПОРЯДОК ИЛЛЮСТРАЦИЙ против настоящего Postgres: главная первой, и одна и та
// же во всех трёх местах показа.
//
// Живой случай, ради которого правило заведено, — заметка 313234 (10.09.2026):
// администратор поставил свою картинку, потом автор дважды сменил её на НГС, и
// в note_images легли четыре строки. Ключ у строки её АДРЕС, поэтому замена
// приезжает новой строкой, а не заменяет прежнюю, — и по времени появления
// первой оказывалась неверная.
//
// Подделкой этого не проверить: правило живёт в ORDER BY, а не в Go.

import (
	"context"
	"testing"
	"time"
)

// shotNote — зеркальная заметка с картинками в порядке их появления: сперва
// привезённые с НГС (у каждой свой чужой адрес), затем, если просят, наша.
func shotNote(t *testing.T, p *Platform, id int64, ngs int, own bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: id, Author: MirroredAuthor{ID: 498196, Nick: "ДВ"},
		Body: "заметка с картинками", PublishedAt: time.Now().Add(-time.Hour), PublishedExact: true,
	}); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	for i := 0; i < ngs; i++ {
		url := "https://n1s1.hsmedia.ru/static/love/images/" + string(rune('a'+i)) + ".jpg"
		m, err := store.Put(ctx, testPNG(t, 390+i, 280+i), url)
		if err != nil {
			t.Fatalf("картинка с НГС: %v", err)
		}
		if err := p.AttachNoteImage(ctx, id, m.SHA256, url); err != nil {
			t.Fatalf("привязка: %v", err)
		}
	}
	if own {
		m, err := store.Put(ctx, testPNG(t, 1312, 1199), "")
		if err != nil {
			t.Fatalf("наша картинка: %v", err)
		}
		if err := p.AttachNoteImage(ctx, id, m.SHA256, m.URL); err != nil {
			t.Fatalf("привязка: %v", err)
		}
	}
}

// Наша картинка сильнее любой привезённой, даже если легла раньше всех: её
// поставил администратор осознанно.
func TestOwnImageWinsOverEveryMirroredOne(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	shotNote(t, p, 313234, 3, true)

	imgs, err := p.NoteImages(ctx, 313234)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	if len(imgs) != 4 {
		t.Fatalf("иллюстраций %d, ждали 4", len(imgs))
	}
	if imgs[0].SourceURL != "" {
		t.Errorf("первой стои́т привезённая (%s), а у заметки есть своя", imgs[0].SourceURL)
	}
	// Лента обязана показать ТУ ЖЕ: разойдись правила, читатель увидел бы в
	// ленте одну картинку, а на странице другую.
	thumbs, err := p.NoteThumbs(ctx, []int64{313234})
	if err != nil {
		t.Fatalf("иллюстрации ленты: %v", err)
	}
	if thumbs[313234].URL != imgs[0].URL {
		t.Errorf("в ленте %q, на странице %q", thumbs[313234].URL, imgs[0].URL)
	}
}

// Своей нет — берётся ПОСЛЕДНЯЯ с НГС: прежнюю потому и заменили, что она была
// неверной, и показывать её первой значит показывать заведомо не то.
func TestWithoutOwnImageTheNewestMirroredWins(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	shotNote(t, p, 313235, 3, false)

	imgs, err := p.NoteImages(ctx, 313235)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	if len(imgs) != 3 {
		t.Fatalf("иллюстраций %d, ждали 3", len(imgs))
	}
	if imgs[0].SourceURL != "https://n1s1.hsmedia.ru/static/love/images/c.jpg" {
		t.Errorf("главной оказалась %q, ждали последнюю привезённую", imgs[0].SourceURL)
	}
	// И остальные идут от свежих к старым — исправленная раньше неверной.
	if imgs[1].SourceURL != "https://n1s1.hsmedia.ru/static/love/images/b.jpg" {
		t.Errorf("второй оказалась %q", imgs[1].SourceURL)
	}
}

// Строка, у которой известна одна ссылка, главной не станет никогда: нарисовать
// её нечем (MediaURL пуст), а source_url у неё читается пустым — то есть без
// проверки байтов она притворилась бы нашей и вытеснила настоящую картинку.
func TestImageWithoutBytesNeverBecomesMain(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	shotNote(t, p, 313236, 1, false)
	if err := p.AttachNoteImage(ctx, 313236,
		nil, "https://n1s1.hsmedia.ru/static/love/images/ещё-не-забрали.jpg"); err != nil {
		t.Fatalf("привязка: %v", err)
	}

	imgs, err := p.NoteImages(ctx, 313236)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	if len(imgs) != 2 {
		t.Fatalf("иллюстраций %d, ждали 2", len(imgs))
	}
	if imgs[0].URL == "" {
		t.Error("главной стала строка без байтов: на странице вышло бы пустое место")
	}
}

// Канал берёт картинку тем же правилом: пост в мессенджере и страница обязаны
// показывать одно и то же. Проверяется на НАТИВНОЙ — в каналы уходит только она.
func TestOutboundTakesTheSameMainImage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	first, err := store.Put(ctx, testPNG(t, 800, 600), "")
	if err != nil {
		t.Fatalf("картинка: %v", err)
	}
	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "своя заметка", Image: &first})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	// Вторая своя картинка, приложенная позже: у нативной их бывает одна, но
	// правило обязано выбирать и здесь — молчаливый жребий хуже правила.
	second, err := store.Put(ctx, testPNG(t, 1000, 700), "")
	if err != nil {
		t.Fatalf("вторая картинка: %v", err)
	}
	if err := p.AttachNoteImage(ctx, id, second.SHA256, second.URL); err != nil {
		t.Fatalf("привязка: %v", err)
	}

	imgs, err := p.NoteImages(ctx, id)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	notes, err := p.OutboundNotes(ctx, NativeIDBase-1, 10)
	if err != nil || len(notes) != 1 {
		t.Fatalf("исходящие: %d строк, %v", len(notes), err)
	}
	if MediaURL(notes[0].ImageSHA, notes[0].ImageMIME) != imgs[0].URL {
		t.Errorf("в канал уходит не та картинка, что стои́т на странице")
	}

	// И тот же выбор у прохода иллюстраций: он догоняет приложенное позже, а
	// разойдись он с постом — в тред приехала бы вторая картинка заметки.
	late, err := p.OutboundNoteImages(ctx, NativeIDBase-1, 10)
	if err != nil || len(late) != 1 {
		t.Fatalf("исходящие иллюстрации: %d строк, %v", len(late), err)
	}
	if MediaURL(late[0].SHA, late[0].MIME) != imgs[0].URL {
		t.Error("проход иллюстраций выбрал не ту картинку")
	}
}

// Заметка БЕЗ картинки в проход иллюстраций не попадает вовсе: спрашивать о ней
// нечего, а круг по всей полосе стоит тем дешевле, чем меньше в нём лишнего.
func TestOutboundImagesSkipNotesWithoutOne(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "без картинки"}); err != nil {
		t.Fatalf("заметка: %v", err)
	}

	late, err := p.OutboundNoteImages(ctx, NativeIDBase-1, 10)
	if err != nil {
		t.Fatalf("исходящие иллюстрации: %v", err)
	}
	if len(late) != 0 {
		t.Errorf("строк %d, а картинок у заметки нет", len(late))
	}
}
