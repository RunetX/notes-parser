package platform

// Иллюстрация нативной заметки против настоящего Postgres. Проверяется здесь
// ровно то, чего подделкой не проверишь: что картинка привязывается ТОЙ ЖЕ
// транзакцией, что и заметка, и что откат уносит её вместе с заметкой.

import (
	"context"
	"errors"
	"testing"
)

// mustShot кладёт картинку в хранилище и возвращает её учётную запись.
func mustShot(t *testing.T, p *Platform, w, h int) Media {
	t.Helper()
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	m, err := store.PutSized(context.Background(), testPNG(t, w, h), "", w, h)
	if err != nil {
		t.Fatalf("приём картинки: %v", err)
	}
	return m
}

func TestCreateNoteAttachesTheImage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 1600, 900)

	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}

	imgs, err := p.NoteImages(ctx, id)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("иллюстраций %d, ожидалась одна", len(imgs))
	}
	got := imgs[0]
	if string(got.SHA256) != string(shot.SHA256) {
		t.Error("привязан не тот файл")
	}
	if got.URL != shot.URL {
		t.Errorf("адрес %q, ожидался %q", got.URL, shot.URL)
	}
	if got.Width != 1600 || got.Height != 900 {
		t.Errorf("размеры %d×%d: у webp их не прочесть, они обязаны храниться как заданы",
			got.Width, got.Height)
	}
	// В колонке url у своей строки стоит НАШ путь, а происхождение отличается по
	// пустому source_url (см. шапку media.go).
	var position int
	var sourceURL string
	if err := p.pool.QueryRow(ctx, `
		SELECT i.position, m.source_url
		  FROM note_images i JOIN media m ON m.sha256 = i.sha256
		 WHERE i.note_id = $1`, id).Scan(&position, &sourceURL); err != nil {
		t.Fatalf("строка иллюстрации: %v", err)
	}
	if position != 0 {
		t.Errorf("position = %d, ожидался 0", position)
	}
	if sourceURL != "" {
		t.Errorf("source_url = %q: у принесённой картинки его быть не должно", sourceURL)
	}
}

// Главный тест порядка: отказ ядра обязан унести и картинку. Иначе на странице
// заведётся строка без заметки, а в базе — учёт того, чего никто не публиковал.
func TestCreateNoteRollsBackTheImage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		breakIt func(t *testing.T, author int64)
		want    error
	}{
		{"частота", func(t *testing.T, author int64) {
			// Первая заметка съедает порог «одна в пять минут».
			if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "первая"}); err != nil {
				t.Fatalf("первая заметка: %v", err)
			}
		}, ErrRateLimited},
		{"бан", func(t *testing.T, author int64) {
			if _, err := p.pool.Exec(ctx,
				`UPDATE users SET banned_until = now() + interval '1 day' WHERE id = $1`, author); err != nil {
				t.Fatalf("бан: %v", err)
			}
		}, ErrBanned},
		{"отозванное согласие", func(t *testing.T, author int64) {
			if err := p.RevokeConsent(ctx, author, ConsentDistribution); err != nil {
				t.Fatalf("отзыв: %v", err)
			}
		}, ErrConsentRevoked},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPlatform(t)
			author := mustUser(t, p, "Рио")
			shot := mustShot(t, p, 800, 600)
			c.breakIt(t, author)

			_, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
			if !errors.Is(err, c.want) {
				t.Fatalf("получили %v, ожидалось %v", err, c.want)
			}
			var rows int
			if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM note_images`).Scan(&rows); err != nil {
				t.Fatalf("подсчёт: %v", err)
			}
			if rows != 0 {
				t.Fatalf("после отказа осталось %d строк иллюстраций", rows)
			}
		})
	}
}

// Предварительная проверка обязана отвечать ТО ЖЕ, что и транзакция: иначе
// морда либо пропустит того, кому откажут (и оставит файл на диске), либо
// откажет тому, кому можно.
func TestMayPublishNoteMatchesTheTransaction(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		breakIt func(t *testing.T, p *Platform, author int64)
		want    error
	}{
		{"можно", func(*testing.T, *Platform, int64) {}, nil},
		{"частота", func(t *testing.T, p *Platform, author int64) {
			if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "первая"}); err != nil {
				t.Fatalf("первая заметка: %v", err)
			}
		}, ErrRateLimited},
		{"бан", func(t *testing.T, p *Platform, author int64) {
			if _, err := p.pool.Exec(ctx,
				`UPDATE users SET banned_until = now() + interval '1 day' WHERE id = $1`, author); err != nil {
				t.Fatalf("бан: %v", err)
			}
		}, ErrBanned},
		{"отозванное согласие", func(t *testing.T, p *Platform, author int64) {
			if err := p.RevokeConsent(ctx, author, ConsentDistribution); err != nil {
				t.Fatalf("отзыв: %v", err)
			}
		}, ErrConsentRevoked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := testPlatform(t)
			author := mustUser(t, p, "Рио")
			c.breakIt(t, p, author)

			pre := p.MayPublishNote(ctx, author)
			_, real := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "проба"})
			switch {
			case c.want == nil && (pre != nil || real != nil):
				t.Fatalf("отказали, хотя можно: заранее %v, в транзакции %v", pre, real)
			case c.want != nil && !errors.Is(pre, c.want):
				t.Fatalf("заранее вернули %v, ожидалось %v", pre, c.want)
			case c.want != nil && !errors.Is(real, c.want):
				t.Fatalf("в транзакции вернули %v, ожидалось %v", real, c.want)
			}
		})
	}
}

// Снять картинку можно тем же одним действием, что и поправить текст, — и
// только пока окно правки открыто.
func TestEditNoteDropsTheImage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "без картинки", DropImage: true}); err != nil {
		t.Fatalf("правка: %v", err)
	}
	imgs, err := p.NoteImages(ctx, id)
	if err != nil {
		t.Fatalf("иллюстрации: %v", err)
	}
	if len(imgs) != 0 {
		t.Fatalf("картинка осталась: %d строк", len(imgs))
	}
	// Строка media и файл на диске остаются: имя файла есть его содержимое, и
	// тот же файл может быть привязан к другой заметке или стоять аватаром.
	var media int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM media`).Scan(&media); err != nil {
		t.Fatalf("учёт медиа: %v", err)
	}
	if media != 1 {
		t.Fatalf("в учёте %d строк: удалять сам файл здесь нельзя", media)
	}
}

func TestEditKeepsTheImageWhenNotAsked(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "другой текст"}); err != nil {
		t.Fatalf("правка: %v", err)
	}
	if imgs, _ := p.NoteImages(ctx, id); len(imgs) != 1 {
		t.Fatalf("правка текста унесла картинку: %d строк", len(imgs))
	}
}

// Заметка с картинкой попадает к ЧЕЛОВЕКУ всегда: автомат её не смотрит вовсе,
// премодерации нет, и очередь — единственное место, где на неё посмотрят.
func TestReviewQueueAlwaysShowsNotesWithImages(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	plain, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "просто текст"})
	if err != nil {
		t.Fatalf("заметка без картинки: %v", err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '6 minutes' WHERE id = $1`, plain); err != nil {
		t.Fatalf("сдвиг времени: %v", err)
	}
	withShot, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка с картинкой: %v", err)
	}

	items, err := p.ReviewQueue(ctx, 50)
	if err != nil {
		t.Fatalf("очередь: %v", err)
	}
	var seen bool
	for _, it := range items {
		if it.Subject.ID == plain {
			t.Error("в очередь попала заметка без картинки, о которой автомат ничего не сказал")
		}
		if it.Subject.ID == withShot {
			seen = true
			if it.ImageURL != shot.URL {
				t.Errorf("модератору показан адрес %q, ожидался %q", it.ImageURL, shot.URL)
			}
		}
	}
	if !seen {
		t.Fatal("заметка с картинкой в очередь не попала — смотреть её будет некому")
	}
}

// Номер субъекта в очереди сам по себе не значит НИЧЕГО: у заметок и
// комментариев внутри нативной полосы свои последовательности, и номера у них
// пересекаются. Правило «заметка с картинкой видна модератору всегда» обязано
// спрашивать ВИД субъекта — иначе комментарий, которому достался номер заметки с
// картинкой, приезжает в очередь без причины и с чужой миниатюрой.
func TestQueueDoesNotConfuseACommentWithANoteOfTheSameNumber(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка с картинкой: %v", err)
	}
	// Следующий комментарий получит РОВНО номер этой заметки — то самое
	// совпадение, которое в бою случается само собой.
	// is_called = false, а не «номер минус один»: у последовательности есть
	// MINVALUE, и первая же нативная заметка стоит ровно на нём.
	if _, err := p.pool.Exec(ctx,
		`SELECT setval('comments_native_seq', $1, false)`, note); err != nil {
		t.Fatalf("сдвиг последовательности: %v", err)
	}
	cid, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: author, Body: "реплика"})
	if err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	if cid != note {
		t.Fatalf("совпадения номеров не вышло: заметка %d, комментарий %d", note, cid)
	}

	items, err := p.ReviewQueue(ctx, 50)
	if err != nil {
		t.Fatalf("очередь: %v", err)
	}
	var sawNote bool
	for _, it := range items {
		switch it.Subject.Kind {
		case SubjectNote:
			sawNote = true
			if it.ImageURL != shot.URL {
				t.Errorf("у заметки в очереди адрес картинки %q, ожидался %q", it.ImageURL, shot.URL)
			}
		case SubjectComment:
			t.Errorf("комментарий %d приехал в очередь по чужой картинке", it.Subject.ID)
			if it.ImageURL != "" {
				t.Errorf("и получил чужую миниатюру %q", it.ImageURL)
			}
		}
	}
	if !sawNote {
		t.Fatal("заметка с картинкой из очереди пропала")
	}
}

// Исходящий обход несёт картинку в каналы: заметка с фотографией, пришедшая в
// Telegram без фотографии, — это заметка о чём-то другом.
func TestOutboundNotesCarryTheImage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot}); err != nil {
		t.Fatalf("заметка: %v", err)
	}
	notes, err := p.OutboundNotes(ctx, NativeIDBase-1, 10)
	if err != nil {
		t.Fatalf("исходящие: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("заметок %d, ожидалась одна", len(notes))
	}
	if string(notes[0].ImageSHA) != string(shot.SHA256) {
		t.Error("картинка не поехала в канал")
	}
	if notes[0].ImageMIME != shot.MIME {
		t.Errorf("тип %q, ожидался %q", notes[0].ImageMIME, shot.MIME)
	}
}

// Лента спрашивает картинки ОДНИМ запросом на страницу.
func TestNoteThumbsReadsManyAtOnce(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")
	shot := mustShot(t, p, 800, 600)

	withShot, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "с картинкой", Image: &shot})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '6 minutes' WHERE id = $1`, withShot); err != nil {
		t.Fatalf("сдвиг времени: %v", err)
	}
	plain, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "без картинки"})
	if err != nil {
		t.Fatalf("вторая заметка: %v", err)
	}

	thumbs, err := p.NoteThumbs(ctx, []int64{withShot, plain})
	if err != nil {
		t.Fatalf("миниатюры: %v", err)
	}
	if len(thumbs) != 1 {
		t.Fatalf("отдано %d миниатюр, ожидалась одна", len(thumbs))
	}
	if thumbs[withShot].URL != shot.URL {
		t.Errorf("адрес %q, ожидался %q", thumbs[withShot].URL, shot.URL)
	}
	if _, err := p.NoteThumbs(ctx, nil); err != nil {
		t.Errorf("пустой список: %v", err)
	}
}
