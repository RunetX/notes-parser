package platform

// Правила записи против настоящего Postgres: они держатся на состоянии строк
// (edited_at, comment_count, banned_until, hide_all) и на транзакции, а такое
// подделкой не проверить.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Публикация и постановка в очередь модерации — ОДНА транзакция. «Опубликовано,
// но в очередь не попало» должно быть состоянием, которого не бывает, иначе
// автомат Ш7 однажды получит корпус с дырами и никто не заметит.
func TestPublishingEnqueuesTheCheck(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")

	noteID, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "заметка"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	commentID, err := p.CreateComment(ctx, NewComment{NoteID: noteID, AuthorID: author, Body: "реплика"})
	if err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	for _, c := range []struct {
		kind string
		id   int64
	}{{SubjectNote, noteID}, {SubjectComment, commentID}} {
		var queued int
		if err := p.pool.QueryRow(ctx, `
			SELECT count(*) FROM moderation_queue
			 WHERE subject_kind = $1 AND subject_id = $2 AND checked_at IS NULL`,
			c.kind, c.id).Scan(&queued); err != nil {
			t.Fatalf("очередь: %v", err)
		}
		if queued != 1 {
			t.Errorf("%s %d в очереди %d раз, ожидался 1", c.kind, c.id, queued)
		}
	}
}

// Своя заметка правится один раз, первые десять минут и только пока под ней нет
// ответов. Время двигаем в базе: ждать десять минут в тесте — не проверка, а
// ожидание.
func TestEditNoteWindow(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Рио")

	// Свежая и без ответов — правится.
	id := mustNote(t, p, author, "первый вариант")
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "второй вариант"}); err != nil {
		t.Fatalf("правка в окне: %v", err)
	}
	n, _ := p.NoteRow(ctx, id)
	if n.Body != "второй вариант" || n.EditedAt == nil {
		t.Fatalf("правка не записалась: body=%q edited_at=%v", n.Body, n.EditedAt)
	}
	// Второй раз — уже нет: «поправить опечатку» это одно действие, а серия
	// правок в окне есть та же смена позиции, только мелкими шагами.
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "третий вариант"}); !errors.Is(err, ErrEditWindowClosed) {
		t.Errorf("повторная правка: %v, ожидался ErrEditWindowClosed", err)
	}

	// Первый комментарий закрывает окно, даже если десять минут ещё не вышли:
	// текст, изменившийся под чужим ответом, выставляет ответившего дураком.
	id = mustNote(t, p, author, "под этой ответят")
	if _, err := p.CreateComment(ctx, NewComment{NoteID: id, AuthorID: author, Body: "ответ"}); err != nil {
		t.Fatalf("комментарий: %v", err)
	}
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "поздно"}); !errors.Is(err, ErrEditWindowClosed) {
		t.Errorf("правка под ответом: %v, ожидался ErrEditWindowClosed", err)
	}

	// Десять минут.
	id = mustNote(t, p, author, "старая")
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = now() - interval '11 minutes' WHERE id = $1`, id); err != nil {
		t.Fatalf("сдвиг времени: %v", err)
	}
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "поздно"}); !errors.Is(err, ErrEditWindowClosed) {
		t.Errorf("правка после окна: %v, ожидался ErrEditWindowClosed", err)
	}

	// Чужую — никогда.
	other := mustUser(t, p, "Мавр")
	id = mustNote(t, p, author, "моя заметка")
	if err := p.EditNote(ctx, other, NoteEdit{NoteID: id, Body: "не твоя"}); !errors.Is(err, ErrNotYours) {
		t.Errorf("правка чужой: %v, ожидался ErrNotYours", err)
	}
}

// Анонимную свою заметку править можно на тех же условиях: настоящий автор у неё
// хранится, поэтому правило работает без исключений.
func TestOwnAnonymousNoteIsEditable(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Ванилька")
	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Anonymous: true, Body: "анонимно"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}
	if err := p.EditNote(ctx, author, NoteEdit{NoteID: id, Body: "анонимно, но иначе"}); err != nil {
		t.Fatalf("правка своей анонимки: %v", err)
	}
	// А на странице автор всё равно не показывается.
	v, err := p.NoteViewByID(ctx, Viewer{UserID: author}, id)
	if err != nil {
		t.Fatalf("вид: %v", err)
	}
	if v.Author.ID != 0 || !v.Own {
		t.Errorf("анонимка показала автора (%d) или потеряла «моё» (%v)", v.Author.ID, v.Own)
	}
}

// Частота считается по НАТИВНЫМ публикациям, и скрытая заметка из счёта не
// выпадает: иначе «скрой свою и пиши заново» обходило бы лимит.
func TestRateLimitCountsHiddenToo(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Примус")

	id := mustNote(t, p, author, "первая")
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "вторая"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("вторая заметка подряд: %v, ожидался ErrRateLimited", err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE notes SET status = $2 WHERE id = $1`, id, StatusHiddenMod); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "третья"}); !errors.Is(err, ErrRateLimited) {
		t.Error("скрытие своей заметки обнулило ограничение частоты")
	}
}

// Кто писать не вправе: тень (за неё никто не доказал владения анкетой),
// забаненный и тот, кто отозвал согласие на распространение. Последнее не
// формальность: публиковать при отозванном согласии значило бы распространять
// то, на что согласия нет.
func TestWhoMayNotWrite(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	shadow, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1496130, Nick: "Lady in red"})
	if err != nil {
		t.Fatalf("тень: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: shadow, Body: "я не входил"}); !errors.Is(err, ErrNotMember) {
		t.Errorf("тень опубликовала заметку: %v", err)
	}

	banned := mustUser(t, p, "СВОЛОЧЪ")
	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET banned_until = now() + interval '30 days' WHERE id = $1`, banned); err != nil {
		t.Fatalf("бан: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: banned, Body: "всё равно"}); !errors.Is(err, ErrBanned) {
		t.Errorf("забаненный опубликовал заметку: %v", err)
	}

	// Отозвавший согласие. Отвечать ему надо «согласие отозвано», а не
	// «подпишите новую редакцию»: нажал он сам и знает, что нажал.
	quit := mustUser(t, p, "Тихая")
	if err := p.RevokeConsent(ctx, quit, ConsentDistribution); err != nil {
		t.Fatalf("отзыв: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: quit, Body: "а меня не видно"}); !errors.Is(err, ErrConsentRevoked) {
		t.Errorf("отозвавший согласие опубликовал заметку: %v", err)
	}
}

// Ник, выбранный у нас, вход по анкете НГС больше не переписывает: текст
// согласия обещает «ник вы меняете сами», и обещание должно пережить следующий
// же вход.
func TestOwnNickSurvivesLogin(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	const profile = 1493279
	if _, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: profile, Nick: "Рио"}, GenderMale); err != nil {
		t.Fatalf("вход: %v", err)
	}
	if err := p.SetOwnNick(ctx, profile, "Паноптикум"); err != nil {
		t.Fatalf("смена ника: %v", err)
	}
	if _, err := p.CompleteNGSLogin(ctx, MirroredAuthor{ID: profile, Nick: "Рио"}, GenderMale); err != nil {
		t.Fatalf("повторный вход: %v", err)
	}
	u, err := p.UserByID(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	if u.Nick != "Паноптикум" {
		t.Errorf("вход переписал выбранный ник на %q", u.Nick)
	}
}

// Своё и зеркальное — один тред, и порядок в нём остаётся деревом: нативный
// ответ встаёт под тем зеркальным комментарием, которому отвечает.
func TestNativeReplyJoinsTheMirroredTree(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	at := time.Date(2026, 8, 14, 18, 30, 4, 0, time.UTC)
	if _, err := p.IngestNote(ctx, MirroredNote{
		ID: 312811, Author: MirroredAuthor{ID: 1493279, Nick: "Рио"},
		Body: "зеркальная заметка", PublishedAt: at, PublishedExact: true,
	}); err != nil {
		t.Fatalf("приём заметки: %v", err)
	}
	if _, err := p.IngestComment(ctx, MirroredComment{
		ID: 63207290, NoteID: 312811, Author: MirroredAuthor{ID: 1038894, Nick: "Пух"},
		Body: "зеркальная реплика", PublishedAt: at,
	}); err != nil {
		t.Fatalf("приём комментария: %v", err)
	}

	me := mustUser(t, p, "Новенький")
	id, err := p.CreateComment(ctx, NewComment{
		NoteID: 312811, AuthorID: me, Body: "отвечаю через год", ReplyToID: 63207290,
	})
	if err != nil {
		t.Fatalf("нативный ответ в зеркальный тред: %v", err)
	}
	c, err := p.CommentRow(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Depth != 2 || c.BranchRootID != 63207290 || c.ReplySource != ReplyNative {
		t.Errorf("ответ встал мимо ветки: depth=%d root=%d source=%d",
			c.Depth, c.BranchRootID, c.ReplySource)
	}
	// Обход дерева — это побайтовый порядок пути, и наш ответ идёт СЛЕДОМ за
	// зеркальным родителем. Пока НГС не принимает комментарии, это ровно
	// хронология; оживёт — новая зеркальная реплика встанет перед нашими, и это
	// известная цена «id и есть порядок».
	thread, err := p.Thread(ctx, Viewer{}, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if len(thread) != 2 || thread[0].ID != 63207290 || thread[1].ID != id {
		t.Errorf("порядок треда: %v", ids(thread))
	}
}

func ids(cs []CommentView) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// mustNote заводит свежую заметку. Прежние заметки автора сдвигаются назад ПЕРЕД
// публикацией, иначе тест упрётся в «одна в пять минут» — то самое правило,
// которое проверяет соседний тест.
func mustNote(t *testing.T, p *Platform, author int64, body string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '6 minutes' WHERE author_id = $1`,
		author); err != nil {
		t.Fatalf("сдвиг времени: %v", err)
	}
	id, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: body})
	if err != nil {
		t.Fatalf("заметка %q: %v", body, err)
	}
	return id
}

// Публиковать можно только по ДЕЙСТВУЮЩЕЙ редакции согласий. Проверка появилась
// вместе с открытием площадки поисковикам (23.08.2026): условия распространения
// стали другими, и выпустить новую редакцию, не спросив её, значило бы выпустить
// бумагу, которая ничего не меняет.
//
// Реакция и жалоба через эту проверку не идут намеренно: реакцию не видит никто,
// кроме счётчика, а жалоба — обращение к модератору, а не публикация. Иначе
// человек не смог бы пожаловаться ровно на то изменение, которое ему предлагают
// подписать.
func TestPublishingNeedsCurrentConsent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Амата")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "по действующей редакции"})
	if err != nil {
		t.Fatalf("заметка: %v", err)
	}

	// Вышла новая редакция: прежняя подпись к ней не относится.
	stale := mustUser(t, p, "Kowalski")
	if _, err := p.pool.Exec(ctx,
		`UPDATE consents SET version = version - 1 WHERE user_id = $1 AND kind = $2`,
		stale, ConsentDistribution); err != nil {
		t.Fatalf("устаревание согласия: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: stale, Body: "по старой"}); !errors.Is(err, ErrConsentOutdated) {
		t.Errorf("заметка по старой редакции: %v, ожидалось ErrConsentOutdated", err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: stale, Body: "и ответ"}); !errors.Is(err, ErrConsentOutdated) {
		t.Errorf("комментарий по старой редакции: %v, ожидалось ErrConsentOutdated", err)
	}
	if err := p.React(ctx, NewReaction{UserID: stale, NoteID: note, Code: "agree"}); err != nil {
		t.Errorf("реакция: %v, а она согласия не требует", err)
	}
	if err := p.AddReport(ctx, stale, NoteSubject(note), "не согласен с новой редакцией"); err != nil {
		t.Errorf("жалоба: %v, а она согласия не требует", err)
	}

	// Подписал новую — снова пишет.
	mustConsent(t, p, stale)
	if _, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: stale, Body: "теперь можно"}); err != nil {
		t.Errorf("комментарий после подписи: %v", err)
	}
}
