package platform

// Шина событий против настоящего Postgres.
//
// Подделкой это не проверить вовсе: вся адресация живёт в SQL (fanOutRules),
// вместе с ON CONFLICT, IS DISTINCT FROM и разбором текста на слова. Заглушка
// подтвердила бы только то, что заглушка работает.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// relaxRates отодвигает назад время нативных публикаций автора. Пороги частоты
// (комментарий раз в 10 с) стерегут боевой поток, а фикстуре они мешают: ждать
// в тесте десять секунд — это не проверка, а ожидание.
func relaxRates(t *testing.T, p *Platform) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE comments SET published_at = published_at - interval '1 hour' WHERE id >= $1`,
		`UPDATE notes SET published_at = published_at - interval '1 day' WHERE id >= $1`,
	} {
		if _, err := p.pool.Exec(ctx, q, NativeIDBase); err != nil {
			t.Fatalf("сдвиг времени: %v", err)
		}
	}
}

// say — нативная реплика без оглядки на пороги частоты.
func say(t *testing.T, p *Platform, noteID, author, replyTo int64, body string) int64 {
	t.Helper()
	relaxRates(t, p)
	id, err := p.CreateComment(context.Background(),
		NewComment{NoteID: noteID, AuthorID: author, Body: body, ReplyToID: replyTo})
	if err != nil {
		t.Fatalf("реплика %q: %v", body, err)
	}
	return id
}

// reasons — какие поводы достались человеку.
func reasons(t *testing.T, p *Platform, userID int64) map[Reason]int {
	t.Helper()
	rows, err := p.pool.Query(context.Background(),
		`SELECT reason, count(*) FROM notifications WHERE user_id = $1 GROUP BY reason`, userID)
	if err != nil {
		t.Fatalf("поводы %d: %v", userID, err)
	}
	defer rows.Close()
	out := map[Reason]int{}
	for rows.Next() {
		var r Reason
		var n int
		if err := rows.Scan(&r, &n); err != nil {
			t.Fatalf("поводы %d: %v", userID, err)
		}
		out[r] = n
	}
	return out
}

func fanOut(t *testing.T, p *Platform) int {
	t.Helper()
	n, err := p.FanOut(context.Background(), 100)
	if err != nil {
		t.Fatalf("раздача: %v", err)
	}
	return n
}

// Ответ на заметку — повод её автору, ответ на реплику — её автору. Разные
// поводы у одного и того же вида факта, и различает их адресат.
func TestReplyGivesReasonToAddressee(t *testing.T) {
	p := testPlatform(t)
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")
	mavr := mustUser(t, p, "Мавр")

	note := mustNote(t, p, rio, "заметка")
	first := say(t, p, note, puh, 0, "первый ответ")
	say(t, p, note, mavr, first, "а я не согласен")
	fanOut(t, p)

	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 1 {
		t.Errorf("автору заметки поводов %v, ожидался один «ответ на заметку»", got)
	}
	if got := reasons(t, p, puh); got[ReasonReplyToComment] != 1 {
		t.Errorf("автору реплики поводов %v, ожидался один «ответ на реплику»", got)
	}
	// Ответ ВНУТРИ треда автору заметки не адресован: под заметкой с девятью
	// сотнями реплик это была бы рассылка, а не уведомление.
	if got := reasons(t, p, rio); got[ReasonReplyToNote] > 1 {
		t.Errorf("автор заметки получил повод о чужом разговоре: %v", got)
	}
}

// Сам себе повода не бывает: ответивший на собственную реплику молчания и ждёт.
func TestNoNoticeToYourself(t *testing.T) {
	p := testPlatform(t)
	rio := mustUser(t, p, "Рио")

	note := mustNote(t, p, rio, "заметка")
	own := say(t, p, note, rio, 0, "сам себе")
	say(t, p, note, rio, own, "и снова сам себе")
	fanOut(t, p)

	if got := reasons(t, p, rio); len(got) != 0 {
		t.Errorf("человек уведомлён о себе самом: %v", got)
	}
}

// Тень уведомлять некуда: она сюда не входила и согласия не давала. Проверка
// важна тем, что теней на площадке большинство — весь зеркальный след.
func TestShadowGetsNoReason(t *testing.T) {
	p := testPlatform(t)
	ingestNote(t, p, 312811, 1493279, "Рио") // автор — тень зеркала
	puh := mustUser(t, p, "Пух")

	say(t, p, 312811, puh, 0, "ответ теневому автору")
	fanOut(t, p)

	if got := reasons(t, p, 1493279); len(got) != 0 {
		t.Errorf("тень получила повод: %v", got)
	}
}

// Скрытая публикация поводов не порождает вовсе. Это и есть то, ради чего
// раздача идёт фоном: автомат модерации успевает погасить реплику раньше, чем о
// ней скажут людям.
func TestHiddenPublicationGivesNoReason(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")
	mod := mustUser(t, p, "Модератор")

	note := mustNote(t, p, rio, "заметка")
	c := say(t, p, note, puh, 0, "грубость")
	if err := p.HideSubject(ctx, Viewer{UserID: mod, Role: RoleModerator},
		CommentSubject(c), CatOther, "не надо так"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	fanOut(t, p)

	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 0 {
		t.Errorf("повод о скрытой реплике роздан: %v", got)
	}
	// А вот автору скрытого сказать обязаны — иначе это молчаливое исчезновение.
	if got := reasons(t, p, puh); got[ReasonAboutYou] != 1 {
		t.Errorf("автору скрытого поводов %v, ожидался один «о вас»", got)
	}
}

// Скрытие ПОСЛЕ раздачи снимает непрочитанные приглашения прийти и прочитать —
// исполняем, а не проверяем на показе.
func TestHidingRemovesUnreadReasons(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")
	mod := mustUser(t, p, "Модератор")

	note := mustNote(t, p, rio, "заметка")
	c := say(t, p, note, puh, 0, "ответ")
	fanOut(t, p)
	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 1 {
		t.Fatalf("повод не роздан: %v", got)
	}

	if err := p.HideSubject(ctx, Viewer{UserID: mod, Role: RoleModerator},
		CommentSubject(c), CatOther, "причина"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 0 {
		t.Errorf("приглашение читать скрытое осталось: %v", got)
	}

	// Прочитанное при этом остаётся: человек его уже видел, и переписывать его
	// прошлое хуже, чем оставить ссылку с честной пометкой «запись скрыта».
	c2 := say(t, p, note, puh, 0, "второй ответ")
	fanOut(t, p)
	if err := p.MarkRead(ctx, rio, 0); err != nil {
		t.Fatalf("отметка прочитанного: %v", err)
	}
	if err := p.HideSubject(ctx, Viewer{UserID: mod, Role: RoleModerator},
		CommentSubject(c2), CatOther, "причина"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 1 {
		t.Errorf("прочитанный повод стёрт скрытием: %v", got)
	}
}

// Повторная раздача ничего не удваивает: ключ (user_id, event_id) делает обрыв
// посередине безопасным без единой строки разбора.
func TestFanOutIsIdempotent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")

	note := mustNote(t, p, rio, "заметка")
	say(t, p, note, puh, 0, "ответ")
	fanOut(t, p)

	// Возвращаем факты в очередь: так выглядит проход, оборвавшийся после
	// вставки поводов, но до отметки.
	if _, err := p.pool.Exec(ctx, `UPDATE events SET fanned_at = NULL`); err != nil {
		t.Fatalf("сброс отметки: %v", err)
	}
	fanOut(t, p)

	if got := reasons(t, p, rio); got[ReasonReplyToNote] != 1 {
		t.Errorf("повторная раздача удвоила поводы: %v", got)
	}
}

// Упоминание по нику достаётся УЧАСТНИКУ ТРЕДА. Посторонний с тем же словом в
// нике повода не получает — иначе «лампочка» в чужом разговоре дёргала бы
// человека через всю площадку.
func TestMentionReachesOnlyThreadParticipant(t *testing.T) {
	p := testPlatform(t)
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")
	mavr := mustUser(t, p, "Мавр")
	outsider := mustUser(t, p, "Лампочка")

	note := mustNote(t, p, rio, "заметка")
	say(t, p, note, puh, 0, "я тут")
	say(t, p, note, mavr, 0, "Пух, ты не прав, а Лампочка вообще молчит")
	fanOut(t, p)

	if got := reasons(t, p, puh); got[ReasonMention] != 1 {
		t.Errorf("участнику треда поводов %v, ожидалось одно упоминание", got)
	}
	if got := reasons(t, p, outsider); len(got) != 0 {
		t.Errorf("посторонний получил повод по нику: %v", got)
	}
}

// Реакции схлопываются в одно событие на объект и знак, а имени нажавшего нет
// нигде — правило Ш5г держится тем, что колонку просто не заполняют.
func TestReactionsCollapseAndStayAnonymous(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")
	mavr := mustUser(t, p, "Мавр")

	note := mustNote(t, p, rio, "заметка")
	for _, u := range []int64{puh, mavr} {
		if err := p.React(ctx, NewReaction{NoteID: note, UserID: u, Code: "popcorn"}); err != nil {
			t.Fatalf("реакция: %v", err)
		}
	}
	if n, err := p.NoticeReactions(ctx, 100); err != nil || n != 1 {
		t.Fatalf("разбор реакций: объектов %d, ошибка %v — ожидался один", n, err)
	}
	fanOut(t, p)

	list, err := p.Notifications(ctx, rio, 0, 10)
	if err != nil {
		t.Fatalf("события: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("поводов %d, ожидался один на два нажатия", len(list))
	}
	if list[0].Count != 2 || list[0].Code != "popcorn" {
		t.Errorf("повод о реакциях: код %q, число %d", list[0].Code, list[0].Count)
	}
	if list[0].ActorNick != "" {
		t.Errorf("в поводе о реакции названо имя %q", list[0].ActorNick)
	}
	var actor *int64
	if err := p.pool.QueryRow(ctx,
		`SELECT actor_id FROM events WHERE kind = $1`, EventReaction).Scan(&actor); err != nil {
		t.Fatalf("событие реакции: %v", err)
	}
	if actor != nil {
		t.Errorf("нажавший записан в журнал: %d — правило Ш5г нарушено в самой базе", *actor)
	}
}

// Новые нажатия дописываются в НЕПРОЧИТАННЫЙ повод, а не заводят второй: иначе
// автор реплики получал бы строку на каждое движение чужой мыши.
func TestReactionsMergeIntoUnread(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")

	note := mustNote(t, p, rio, "заметка")
	for _, nick := range []string{"Пух", "Мавр", "Ягода"} {
		u := mustUser(t, p, nick)
		if err := p.React(ctx, NewReaction{NoteID: note, UserID: u, Code: "agree"}); err != nil {
			t.Fatalf("реакция: %v", err)
		}
		if _, err := p.NoticeReactions(ctx, 100); err != nil {
			t.Fatalf("разбор реакций: %v", err)
		}
		fanOut(t, p)
	}
	list, err := p.Notifications(ctx, rio, 0, 10)
	if err != nil {
		t.Fatalf("события: %v", err)
	}
	if len(list) != 1 || list[0].Count != 3 {
		t.Fatalf("поводов %d (число %v), ожидался один со счётом 3", len(list), list)
	}
}

// Запрет публиковать человек обязан УВИДЕТЬ: сессии при бане не гасятся ровно
// затем, чтобы он прочитал, за что и до какого числа.
func TestBanNoticesTheBanned(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	puh := mustUser(t, p, "Пух")
	mod := mustUser(t, p, "Модератор")

	if err := p.BanUser(ctx, Viewer{UserID: mod, Role: RoleModerator},
		puh, time.Now().Add(24*time.Hour), "флуд"); err != nil {
		t.Fatalf("запрет: %v", err)
	}
	fanOut(t, p)

	list, err := p.Notifications(ctx, puh, 0, 10)
	if err != nil {
		t.Fatalf("события: %v", err)
	}
	if len(list) != 1 || list[0].Kind != EventBanned {
		t.Fatalf("забаненному поводов %d, ожидался один о запрете", len(list))
	}
	if list[0].Detail != "флуд" {
		t.Errorf("причина запрета не доехала: %q", list[0].Detail)
	}
}

// Обезличивание вычищает шину насовсем. Переносить события на могилу, как
// публикации, нельзя: «X ответил Y» — это связь между двумя людьми, и она
// восстановила бы ровно то, отсутствие чего и делает обезличивание таковым.
func TestAnonymizeClearsBus(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")

	note := mustNote(t, p, rio, "заметка")
	say(t, p, note, puh, 0, "ответ")
	fanOut(t, p)

	if _, err := p.AnonymizeUser(ctx, Viewer{UserID: rio, Role: RoleAdmin}, puh); err != nil {
		t.Fatalf("обезличивание: %v", err)
	}
	var events, notices int
	if err := p.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM events WHERE actor_id = $1 OR subject_user_id = $1),
		       (SELECT count(*) FROM notifications WHERE user_id = $1)`, puh).
		Scan(&events, &notices); err != nil {
		t.Fatalf("остатки: %v", err)
	}
	if events != 0 || notices != 0 {
		t.Errorf("после обезличивания осталось событий %d, поводов %d", events, notices)
	}
}

// Уборка идёт по срокам и не трогает свежее.
func TestPruneBusKeepsFresh(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	rio := mustUser(t, p, "Рио")
	puh := mustUser(t, p, "Пух")

	note := mustNote(t, p, rio, "заметка")
	say(t, p, note, puh, 0, "ответ")
	fanOut(t, p)

	if got, err := p.PruneBus(ctx, 100); err != nil || got.Any() {
		t.Fatalf("уборка тронула свежее: %+v (%v)", got, err)
	}
	// Состариваем повод так, будто он прочитан давно.
	if _, err := p.pool.Exec(ctx,
		`UPDATE notifications SET read_at = now() - interval '400 days'`); err != nil {
		t.Fatalf("состаривание: %v", err)
	}
	got, err := p.PruneBus(ctx, 100)
	if err != nil {
		t.Fatalf("уборка: %v", err)
	}
	if got.Read != 1 {
		t.Errorf("уборка сняла прочитанных %d, ожидался один", got.Read)
	}
}

// Планы запросов шины — часть договора, как у ленты и треда. Счётчик
// непрочитанного спрашивается на КАЖДОЙ странице вошедшего, а очередь раздачи —
// каждые пять секунд: полный перебор в любом из них съел бы то самое ядро, ради
// которого все остальные числа и считаются.
func TestBusQueryPlansUseIndexes(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	// Заготовка людная и РАЗМАЗАННАЯ по людям: сложив всё одному человеку, мы
	// проверяли бы саму заготовку — когда условию удовлетворяет вся таблица,
	// отдельный индекс планировщику действительно не нужен.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO users (id, nick)
		SELECT 1000000 + g, 'ник' || g FROM generate_series(1, 200) g;
		INSERT INTO events (id, kind, at)
		SELECT g, 2, now() - g * interval '1 minute' FROM generate_series(1, 5000) g;
		INSERT INTO notifications (user_id, event_id, reason, read_at)
		SELECT 1000000 + (g % 200) + 1, g, 1,
		       CASE WHEN g % 50 = 0 THEN NULL ELSE now() END
		  FROM generate_series(1, 5000) g;
		UPDATE events SET fanned_at = now() WHERE id % 100 <> 0;
		ANALYZE events, notifications, users`); err != nil {
		t.Fatalf("заготовка: %v", err)
	}

	for _, c := range []struct {
		name   string
		query  string
		args   []any
		index  string
		forbid string
	}{
		// У страницы событий договор — «читаем поводы ЭТОГО человека по ключу».
		// Про join к фактам он ничего не обещает: там обращение по первичному
		// ключу, и на заготовке в пять тысяч строк планировщик законно берёт
		// хеш вместо вложенного цикла. Требовать здесь отсутствия перебора
		// значило бы проверять размер заготовки, а не запрос.
		{"счётчик непрочитанного", unreadQuery, []any{int64(1000001), UnreadCap},
			"notifications_unread", "Seq Scan on notifications"},
		{"страница событий", noticesQuery, []any{int64(1000001), 20, 0, excerptRunes},
			"notifications_pkey", "Seq Scan on notifications"},
		{"очередь раздачи", pendingQuery, []any{100}, "events_pending", "Seq Scan on events"},
	} {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, p, c.query, c.args...)
			if !strings.Contains(plan, c.index) {
				t.Fatalf("план не берёт индекс %s:\n%s", c.index, plan)
			}
			if strings.Contains(plan, c.forbid) {
				t.Fatalf("в плане %s:\n%s", c.forbid, plan)
			}
		})
	}
}
