package platform

// Жители и песочница (эпик «народ», миграция 0018) против настоящего Postgres.
//
// Подделкой это не проверить: правило песочницы живёт в транзакции публикации и
// читает две колонки из двух таблиц, а исключение по согласиям — вопрос о том,
// какой строки в `consents` НЕТ.

import (
	"context"
	"errors"
	"testing"
)

// mustPersona — заведённый житель. Согласий ему не подписывают НАМЕРЕННО: в этом
// вся суть исключения, и подписать их «на всякий случай» значило бы проверить не
// то правило.
func mustPersona(t *testing.T, p *Platform, nick string) int64 {
	t.Helper()
	id, err := p.CreatePersonaUser(context.Background(), nick)
	if err != nil {
		t.Fatalf("житель %q: %v", nick, err)
	}
	return id
}

// mustStageNote — заметка-песочница от администратора.
func mustStageNote(t *testing.T, p *Platform, admin int64) int64 {
	t.Helper()
	id, err := p.CreateNote(context.Background(), NewNote{
		AuthorID: admin, Body: "о чём поговорим", Stage: true,
	})
	if err != nil {
		t.Fatalf("песочница: %v", err)
	}
	return id
}

// mustAdmin — участник с правами администратора.
func mustAdmin(t *testing.T, p *Platform, nick string) int64 {
	t.Helper()
	id := mustUser(t, p, nick)
	if err := p.SetRole(context.Background(), Viewer{UserID: id, Role: RoleAdmin}, id, RoleAdmin); err != nil {
		t.Fatalf("права администратора: %v", err)
	}
	return id
}

// ЖИТЕЛЬ ПУБЛИКУЕТ БЕЗ СОГЛАСИЙ, и это не послабление, а единственный возможный
// ответ: согласие на обработку персональных данных даёт СУБЪЕКТ, а у персонажа
// персональных данных нет. Строка от его имени в `consents` была бы записью о
// согласии, которого никто не давал.
func TestPersonaPublishesWithoutConsents(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note := mustStageNote(t, p, admin)
	persona := mustPersona(t, p, "Кедрачъ")

	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: persona, Body: "а по-моему всё наоборот",
	}); err != nil {
		t.Fatalf("житель не смог написать в песочнице: %v", err)
	}
	// И согласий у него по-прежнему НЕТ: право дал признак, а не подпись.
	var signed int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM consents WHERE user_id = $1`, persona).Scan(&signed); err != nil {
		t.Fatal(err)
	}
	if signed != 0 {
		t.Errorf("за жителя подписано %d согласий — подделка доказательственной таблицы", signed)
	}
	// А обычному участнику без согласий по-прежнему нельзя: исключение точечное.
	plain, err := p.CreateNativeUser(ctx, "Новичок")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: plain, Body: "а мне можно?",
	}); !errors.Is(err, ErrConsentOutdated) && !errors.Is(err, ErrStageClosed) {
		t.Errorf("участник без согласий написал в песочнице: %v", err)
	}
}

// Песочница ЧИТАЕТСЯ всеми, но пишут в неё двое: житель и администратор. У
// обычного участника отказ ИМЕННО про песочницу, а не «нет такой заметки»:
// песочница не тайна — она в ленте, помечена значком и объяснена в справке.
func TestStageIsClosedToMembers(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note := mustStageNote(t, p, admin)
	member := mustUser(t, p, "Читатель")

	_, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: member, Body: "влезу"})
	if !errors.Is(err, ErrStageClosed) {
		t.Fatalf("участник написал в песочнице: %v", err)
	}
	// Администратор может: садовник вправе войти в разговор своих жителей, не
	// открывая песочницу всем.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: admin, Body: "а вот это интересно",
	}); err != nil {
		t.Errorf("администратор не смог написать в своей песочнице: %v", err)
	}
	// Модератору — нельзя: он решает про СЛОВА, а участвовать в разговоре не его
	// роль, и его реплика в песочнице выглядела бы служебной.
	mod := mustUser(t, p, "Модератор")
	if err := p.SetRole(ctx, Viewer{UserID: admin, Role: RoleAdmin}, mod, RoleModerator); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: mod, Body: "и я скажу",
	}); !errors.Is(err, ErrStageClosed) {
		t.Errorf("модератор написал в песочнице: %v", err)
	}
}

// Вторая половина того же правила: ЖИТЕЛЬ ПИШЕТ ТОЛЬКО В ПЕСОЧНИЦЕ.
//
// Ради неё правило и собрано в одном месте: благодаря ей «машинная реплика» и
// «песочница» перестают быть разными вопросами, и всё, что ниже по течению
// (каналы, недельная сводка, поводы шины), вправе спрашивать про ОДНУ заметку
// вместо join'а к users на десяти миллионах комментариев.
func TestPersonaCannotWriteOffStage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Человек")
	plain, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "обычная заметка"})
	if err != nil {
		t.Fatal(err)
	}
	persona := mustPersona(t, p, "Кедрачъ")
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: plain, AuthorID: persona, Body: "а я и тут скажу",
	}); !errors.Is(err, ErrPersonaOffStage) {
		t.Fatalf("житель заговорил вне песочницы: %v", err)
	}
	// И заметку он заводит только со сценой.
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: persona, Body: "моя заметка"}); !errors.Is(err, ErrPersonaOffStage) {
		t.Errorf("житель завёл обычную заметку: %v", err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: persona, Body: "моя сцена", Stage: true}); err != nil {
		t.Errorf("житель не смог завести песочницу: %v", err)
	}
}

// Песочницу и жителей не видит НИ ОДИН исходящий обход в каналы: решение эпика —
// наружу из песочницы не идёт ничего.
func TestOutboundSkipsStage(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	stage := mustStageNote(t, p, admin)
	persona := mustPersona(t, p, "Кедрачъ")
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: stage, AuthorID: persona, Body: "реплика жителя",
	}); err != nil {
		t.Fatal(err)
	}
	// Обычную заметку пишет ДРУГОЙ человек: у заметки потолок 1/5 мин, а садовник
	// только что завёл песочницу.
	neighbour := mustUser(t, p, "Сосед")
	plain, err := p.CreateNote(ctx, NewNote{AuthorID: neighbour, Body: "объявление площадки"})
	if err != nil {
		t.Fatal(err)
	}

	notes, err := p.OutboundNotes(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		if n.ID == stage {
			t.Error("песочница ушла в исходящий обход")
		}
	}
	if len(notes) == 0 || notes[len(notes)-1].ID != plain {
		t.Error("обычная заметка из обхода пропала вместе с песочницей")
	}
	comments, err := p.OutboundComments(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range comments {
		if c.NoteID == stage {
			t.Error("реплика из песочницы ушла в исходящий обход")
		}
	}
}

// Поводов жителю не раздаётся: колокольчика у него нет, читать их некому, а
// служба узнаёт о новом своим курсором. Без этого условия `notifications` копила
// бы строки, которые никто никогда не прочтёт.
func TestFanOutSkipsPersona(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note := mustStageNote(t, p, admin)
	persona := mustPersona(t, p, "Кедрачъ")

	first, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: persona, Body: "начну"})
	if err != nil {
		t.Fatal(err)
	}
	// Администратор отвечает жителю: живому такой ответ дал бы повод.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: admin, ReplyToID: first, Body: "отвечаю",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FanOut(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var pokes int
	if err := p.pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1`, persona).Scan(&pokes); err != nil {
		t.Fatal(err)
	}
	if pokes != 0 {
		t.Errorf("жителю роздано %d поводов — их некому читать", pokes)
	}
}

// Своя заметка становится песочницей — и ровно до тех пор, пока в ней никто не
// сказал ни слова. Материал для сцены чаще всего уже лежит в ленте, и копия
// текста новой записью означала бы тот же текст дважды.
func TestNoteBecomesStageWhileSilent(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	viewer := Viewer{UserID: admin, Role: RoleAdmin}
	note, err := p.CreateNote(ctx, NewNote{AuthorID: admin, Body: "третье свидание"})
	if err != nil {
		t.Fatal(err)
	}
	persona := mustPersona(t, p, "Кедрачъ")
	// До перевода житель туда не может: заметка обычная.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: persona, Body: "рано",
	}); !errors.Is(err, ErrPersonaOffStage) {
		t.Fatalf("житель заговорил в обычной заметке: %v", err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, viewer, note, true, "первая сцена"); err != nil {
		t.Fatalf("перевод в песочницу: %v", err)
	}
	// После перевода — может, а обычный участник больше нет.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: persona, Body: "а по-моему наоборот",
	}); err != nil {
		t.Fatalf("житель не смог написать в переведённой заметке: %v", err)
	}
	member := mustUser(t, p, "Читатель")
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: member, Body: "влезу",
	}); !errors.Is(err, ErrStageClosed) {
		t.Errorf("участник написал в переведённой заметке: %v", err)
	}
	// Повторный перевод — не действие, а состояние: путать их нельзя.
	if err := p.SetNoteStageAsAdmin(ctx, viewer, note, true, ""); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("повторный перевод сошёл за действие: %v", err)
	}
	// И журнал: «кто здесь вправе говорить» — такая же часть истории, как «кого
	// скрыли».
	entries, err := p.AuditTail(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Action == ActionStage {
			found = true
		}
	}
	if !found {
		t.Error("перевод в песочницу не попал в журнал")
	}
}

// Заговорили — поздно, и правило симметрично. Вперёд оно бережёт участников
// живого треда (после перевода они не смогли бы ответить в разговоре, который
// сами вели), назад — обещание «наружу из песочницы не уходит ничего».
func TestStageFlagFrozenOnceSomeoneSpoke(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	viewer := Viewer{UserID: admin, Role: RoleAdmin}
	member := mustUser(t, p, "Человек")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: member, Body: "живой разговор"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: member, Body: "сам себе отвечу",
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, viewer, note, true, ""); !errors.Is(err, ErrStageHasThread) {
		t.Fatalf("живой тред отдан жителям: %v", err)
	}

	// И обратно — тоже нельзя: сказанное жителями ушло бы в ленту, в каналы и в
	// недельную сводку.
	stage := mustStageNote(t, p, admin)
	persona := mustPersona(t, p, "Кедрачъ")
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: stage, AuthorID: persona, Body: "сказал",
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, viewer, stage, false, ""); !errors.Is(err, ErrStageHasThread) {
		t.Errorf("песочница с репликами выпущена наружу: %v", err)
	}
}

// Дверь администраторская, и зеркальную копию она не открывает.
func TestStageFlagDoorAndSilence(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: admin, Body: "своя заметка"})
	if err != nil {
		t.Fatal(err)
	}
	mod := mustUser(t, p, "Модератор")
	if err := p.SetRole(ctx, Viewer{UserID: admin, Role: RoleAdmin}, mod, RoleModerator); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, Viewer{UserID: mod, Role: RoleModerator}, note, true, ""); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("модератор отдал заметку жителям: %v", err)
	}
	// ЗЕРКАЛЬНУЮ ТОЖЕ, и это правка от 30.08.2026: старая заметка с НГС без
	// единой реплики и есть готовая сцена. Признак копию не правит — текст,
	// автор и дата остаются как были, меняется только то, кто вправе говорить
	// ЗДЕСЬ. Строку заводим сырым INSERT'ом: полоса НГС нативным путём не
	// заводится вовсе.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO users (id, nick) VALUES (777001, 'Тень');
		INSERT INTO notes (id, author_id, body, published_at)
		VALUES (777002, 777001, 'зеркальная молчащая', now()),
		       (777003, 777001, 'зеркальная с разговором', now())`); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, 777002, true, ""); err != nil {
		t.Errorf("молчащую зеркальную не отдали жителям: %v", err)
	}
	// А вот там, где уже говорили, правило прежнее — и оно одно на обе полосы.
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO comments (id, note_id, author_id, body, path, depth, published_at)
		VALUES (777004, 777003, 777001, 'первое слово', '0000000777004', 0, now())`); err != nil {
		t.Fatal(err)
	}
	if err := p.SetNoteStageAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, 777003, true, ""); !errors.Is(err, ErrStageHasThread) {
		t.Errorf("зеркальную с разговором отдали жителям: %v", err)
	}
}

// РЕПЛИКА ЖИТЕЛЯ НЕ ИДЁТ В ОЧЕРЕДЬ АВТОМАТА, а реплика человека в той же
// песочнице идёт. Проверять это надо именно парой: «очередь пуста» само по себе
// доказывало бы и то, что enqueueCheck сломан целиком.
//
// Строка очереди — платный запрос к Yandex AI Studio, а жители говорят десятками
// реплик в час; мат у них при этом запрещён своим евалом, по тому же словарю,
// которым судит автомат.
func TestPersonaRepliesSkipTheAutomaton(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note := mustStageNote(t, p, admin)

	byPersona, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: mustPersona(t, p, "Кедрачъ"), Body: "а у нас в гараже"})
	if err != nil {
		t.Fatal(err)
	}
	// Администратор в песочницу писать вправе — его реплика тоже мимо очереди,
	// но по СВОЕМУ, давнему правилу; берём поэтому обычного участника заметки.
	byHuman := mustNoteComment(t, p, mustUser(t, p, "Ирма"))

	for _, c := range []struct {
		id   int64
		want int
		who  string
	}{{byPersona, 0, "житель"}, {byHuman, 1, "участник"}} {
		var n int
		if err := p.pool.QueryRow(ctx, `
			SELECT count(*) FROM moderation_queue
			 WHERE subject_kind = $1 AND subject_id = $2`, SubjectComment, c.id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != c.want {
			t.Errorf("%s: строк очереди %d, ожидалось %d", c.who, n, c.want)
		}
	}

	// Жалоба читателя заводит строку САМА — путь к человеку у жителя остаётся
	// тот же, что у публикации администрации.
	if err := p.AddReport(ctx, mustUser(t, p, "Веснушка"), CommentSubject(byPersona), "грубит"); err != nil {
		t.Fatalf("жалоба на жителя: %v", err)
	}
	q, err := p.ReviewQueue(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, it := range q {
		if it.Subject.Kind == SubjectComment && it.Subject.ID == byPersona {
			seen = true
		}
	}
	if !seen {
		t.Error("жалоба на реплику жителя не дошла до модератора")
	}
}

// mustNoteComment — реплика участника в его собственной обычной заметке.
func mustNoteComment(t *testing.T, p *Platform, author int64) int64 {
	t.Helper()
	ctx := context.Background()
	note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "обычная заметка"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: author, Body: "сама себе отвечу"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
