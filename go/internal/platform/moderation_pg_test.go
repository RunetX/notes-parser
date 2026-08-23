package platform

// Модерация против настоящего Postgres. Здесь проверяется то, что живёт в SQL и
// в транзакциях, а не в Go: денормализованный счётчик, границы видимости,
// частичные индексы очереди и порядок «решение и его исполнение — вместе».

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func moderator(t *testing.T, p *Platform) Viewer {
	t.Helper()
	return Viewer{UserID: mustUser(t, p, "модератор"), Role: RoleModerator}
}

// Скрытие — это ещё и счётчик заметки: иначе под ней стоит «Комментарии 42», а
// видно сорок.
func TestСкрытиеПравитСчётчик(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "заметка")
	ingestComment(t, p, 900001, noteID, 0, 0)

	before, err := p.NoteRow(ctx, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if before.CommentCount != 1 {
		t.Fatalf("счётчик до скрытия %d", before.CommentCount)
	}
	if err := p.HideSubject(ctx, mod, CommentSubject(900001), CatSpam, "реклама"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	after, err := p.NoteRow(ctx, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CommentCount != 0 {
		t.Fatalf("счётчик после скрытия %d", after.CommentCount)
	}
	// Читателю скрытого не видно, модератору — видно. Иначе вернуть реплику
	// можно было бы только из очереди, вслепую.
	if cs, err := p.Thread(ctx, Viewer{}, noteID); err != nil || len(cs) != 0 {
		t.Fatalf("читателю видно %d реплик (%v)", len(cs), err)
	}
	cs, err := p.Thread(ctx, mod, noteID)
	if err != nil || len(cs) != 1 || cs[0].Status != StatusHiddenMod {
		t.Fatalf("модератору тред отдался как %v (%v)", cs, err)
	}

	if err := p.RestoreSubject(ctx, mod, CommentSubject(900001), "ошибка"); err != nil {
		t.Fatalf("возврат: %v", err)
	}
	back, err := p.NoteRow(ctx, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if back.CommentCount != 1 {
		t.Fatalf("счётчик после возврата %d", back.CommentCount)
	}
}

// Повторное скрытие — не ошибка целостности, а «состояние уже такое»: два
// модератора, нажавших одно и то же, не должны получать разное.
func TestПовторноеСкрытие(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "заметка")

	if err := p.HideSubject(ctx, mod, NoteSubject(noteID), "", ""); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if err := p.HideSubject(ctx, mod, NoteSubject(noteID), "", ""); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("повтор: %v", err)
	}
}

// Прав нет — нет и действия. Проверка стоит в ЯДРЕ, а не только в морде: правило
// должно быть невозможно обойти, а не просто спрятано за отсутствием кнопки.
func TestБезПравНичегоНеПроисходит(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "заметка")
	nobody := Viewer{UserID: author}

	if err := p.HideSubject(ctx, nobody, NoteSubject(noteID), "", ""); !errors.Is(err, ErrNotModerator) {
		t.Fatalf("скрытие без прав: %v", err)
	}
	if err := p.BanUser(ctx, nobody, author, time.Now().Add(time.Hour), ""); !errors.Is(err, ErrNotModerator) {
		t.Fatalf("бан без прав: %v", err)
	}
	if err := p.SetThreadLocked(ctx, nobody, noteID, true, ""); !errors.Is(err, ErrNotModerator) {
		t.Fatalf("замок без прав: %v", err)
	}
}

// Отзыв согласия и решение модератора — РАЗНЫЕ рычаги, и путать их нельзя:
// возврат согласия не отменяет решения модератора, а его отзыв не должен
// присваивать модераторскому скрытию чужую причину.
func TestРубильникАвтораИРешениеМодератораНеПересекаются(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	hidden := mustNote(t, p, author, "скрытая модератором")
	own := mustNote(t, p, author, "своя")

	if err := p.HideSubject(ctx, mod, NoteSubject(hidden), CatSpam, "реклама"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	// Отзыв согласия прячет ВСЁ остальное, но модераторского решения не трогает.
	if err := p.GrantConsent(ctx, author, ConsentDistribution, 1, ""); err != nil {
		t.Fatalf("согласие: %v", err)
	}
	if err := p.RevokeConsent(ctx, author, ConsentDistribution); err != nil {
		t.Fatalf("отзыв: %v", err)
	}
	if n, err := p.NoteRow(ctx, own); err != nil || n.Status != StatusHiddenOwner {
		t.Fatalf("своя заметка в состоянии %d (%v)", n.Status, err)
	}
	if n, err := p.NoteRow(ctx, hidden); err != nil || n.Status != StatusHiddenMod {
		t.Fatalf("скрытая модератором стала %d (%v)", n.Status, err)
	}
	// И обратно: возврат согласия поднимает своё, но не модераторское.
	if err := p.GrantConsent(ctx, author, ConsentDistribution, 1, ""); err != nil {
		t.Fatalf("возврат согласия: %v", err)
	}
	if n, err := p.NoteRow(ctx, own); err != nil || n.Status != StatusVisible {
		t.Fatalf("своя заметка после возврата %d (%v)", n.Status, err)
	}
	if n, err := p.NoteRow(ctx, hidden); err != nil || n.Status != StatusHiddenMod {
		t.Fatalf("модераторское скрытие снялось возвратом согласия: %d (%v)", n.Status, err)
	}
}

// Очередь: публикация заводит карточку, автомат выносит мнение, скрытое
// исполняется тем же вызовом, а автор видит причину и может просить пересмотра.
func TestОчередьОтПубликацииДоПересмотра(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "купите слона, дёшево")

	pending, err := p.PendingChecks(ctx, 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Subject != NoteSubject(noteID) {
		t.Fatalf("в очереди %v", pending)
	}
	if pending[0].Body == "" || pending[0].AuthorID != author {
		t.Fatalf("строка очереди без текста или автора: %+v", pending[0])
	}

	if err := p.RecordVerdict(ctx, NoteSubject(noteID), VerdictRecord{
		Verdict: VerdictHidden, Category: CatSpam, Reason: "реклама",
		Quote: "купите слона", Model: "тест", PromptSHA: []byte{1, 2, 3},
	}); err != nil {
		t.Fatalf("вердикт: %v", err)
	}
	// Мнение и его исполнение — вместе: «в карточке скрыто, а на странице
	// видно» не должно существовать.
	if n, err := p.NoteRow(ctx, noteID); err != nil || n.Status != StatusHiddenMod {
		t.Fatalf("заметка в состоянии %d (%v)", n.Status, err)
	}
	if left, err := p.PendingChecks(ctx, 10, 3); err != nil || len(left) != 0 {
		t.Fatalf("проверенное осталось в очереди: %v (%v)", left, err)
	}

	mine, err := p.MyHidden(ctx, author, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Reason != "реклама" || !mine[0].CanAppeal() {
		t.Fatalf("автор видит %+v", mine)
	}
	if err := p.Appeal(ctx, author, NoteSubject(noteID)); err != nil {
		t.Fatalf("пересмотр: %v", err)
	}
	// Повторно просить нельзя: пересмотр это не голосование.
	if err := p.Appeal(ctx, author, NoteSubject(noteID)); !errors.Is(err, ErrNoAppeal) {
		t.Fatalf("повторный пересмотр: %v", err)
	}
	// И чужой публикации это тоже не касается.
	other := mustUser(t, p, "Мавр")
	if err := p.Appeal(ctx, other, NoteSubject(noteID)); !errors.Is(err, ErrNoAppeal) {
		t.Fatalf("чужой пересмотр: %v", err)
	}

	queue, err := p.ReviewQueue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || !queue[0].Appealed() || !queue[0].Hidden() {
		t.Fatalf("очередь человека: %+v", queue)
	}

	mod := moderator(t, p)
	if err := p.Decide(ctx, mod, NoteSubject(noteID), DecisionKeep, "автомат ошибся"); err != nil {
		t.Fatalf("решение: %v", err)
	}
	if n, err := p.NoteRow(ctx, noteID); err != nil || n.Status != StatusVisible {
		t.Fatalf("после «оставить» заметка %d (%v)", n.Status, err)
	}
	if q, err := p.ReviewQueue(ctx, 10); err != nil || len(q) != 0 {
		t.Fatalf("решённое осталось в очереди: %v (%v)", q, err)
	}
}

// Стенд просит очередь БЕЗ отсечки по попыткам, и именно спотыкающиеся строки
// ему интереснее всего: они объясняют, почему очередь стоит. Ноль читался как
// «attempts < 0», и прогон по всей очереди молча отвечал «прогонять нечего».
func TestОчередьБезОтсечкиПоПопыткам(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "текст, на котором модель спотыкается")

	// Три неудачных захода — боевой автомат такую строку больше не берёт.
	for range 3 {
		if err := p.BumpAttempts(ctx, []Subject{NoteSubject(noteID)}); err != nil {
			t.Fatalf("попытка: %v", err)
		}
	}
	if left, err := p.PendingChecks(ctx, 10, 3); err != nil || len(left) != 0 {
		t.Fatalf("исчерпавшая попытки строка осталась в боевой очереди: %v (%v)", left, err)
	}
	for _, max := range []int{0, -1} {
		got, err := p.PendingChecks(ctx, 10, max)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Subject != NoteSubject(noteID) {
			t.Fatalf("maxAttempts=%d: стенд получил %v", max, got)
		}
	}
}

// Публикации администрации в очередь не идут: автомат над ними бессилен по
// устройству (его максимум — скрыть, а модератор снимает скрытие нажатием), и
// очередь получала бы шум. На живом замере пять автоскрытий из пятнадцати
// пришлись именно на объявления площадки о самой себе.
func TestАдминистрациюВОчередьНеСтавят(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	author := mustUser(t, p, "участник")
	mustNote(t, p, author, "обычная заметка участника")
	if pending, err := p.PendingChecks(ctx, 10, 0); err != nil || len(pending) != 1 {
		t.Fatalf("заметка участника в очереди: %v (%v)", pending, err)
	}

	admin := mustUser(t, p, "администратор")
	if err := p.SetRole(ctx, Viewer{UserID: admin, Role: RoleAdmin}, admin, RoleAdmin); err != nil {
		t.Fatalf("роль: %v", err)
	}
	mustNote(t, p, admin, "Залетайте все, ссылка на справку https://t3h.ru/help")

	pending, err := p.PendingChecks(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].AuthorID != author {
		t.Fatalf("объявление администрации попало в очередь: %+v", pending)
	}
}

// Жалоба — единственный вход в модерацию для СТАРЫХ строк: классификатор по
// архиву не гоняется, и без неё 10,7 млн зеркальных реплик не модерируются вовсе.
func TestЖалобаПоднимаетЗеркальнуюРеплику(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestComment(t, p, 700100, 312811, 1038894, 0)
	reporter := mustUser(t, p, "жалобщик")

	if err := p.AddReport(ctx, reporter, CommentSubject(700100), "реклама"); err != nil {
		t.Fatalf("жалоба: %v", err)
	}
	queue, err := p.ReviewQueue(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Subject.ID != 700100 {
		t.Fatalf("очередь: %+v", queue)
	}
	if len(queue[0].Reports) != 1 || queue[0].Reports[0].Reason != "реклама" {
		t.Fatalf("жалоба не подцепилась: %+v", queue[0].Reports)
	}
	// Повторная жалоба того же человека не заводит вторую строку.
	if err := p.AddReport(ctx, reporter, CommentSubject(700100), "ещё раз"); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("повтор жалобы: %v", err)
	}
	// На себя жаловаться незачем — но проверить это можно только на УЧАСТНИКЕ.
	// Автор реплики пришёл зеркалом, то есть он тень, а вход в жалобу тот же,
	// что у публикации: сперва «участник ли ты» (так и задумано — жалоба заводит
	// работу другому человеку), и до правила о саможалобе тень не доходит вовсе.
	// Поэтому автор сначала входит: тест обязан проверять ErrSelfReport, а не
	// охранника, проверенного строкой выше.
	if err := p.Promote(ctx, 1038894); err != nil {
		t.Fatalf("вход автора реплики: %v", err)
	}
	if err := p.AddReport(ctx, 1038894, CommentSubject(700100), ""); !errors.Is(err, ErrSelfReport) {
		t.Fatalf("жалоба на себя: %v", err)
	}

	mod := moderator(t, p)
	if err := p.Decide(ctx, mod, CommentSubject(700100), DecisionHide, "и правда реклама"); err != nil {
		t.Fatalf("решение: %v", err)
	}
	if c, err := p.CommentRow(ctx, 700100); err != nil || c.Status != StatusHiddenMod {
		t.Fatalf("после решения комментарий %d (%v)", c.Status, err)
	}
	// Решение закрывает жалобы: очередь не должна показывать одно и то же дважды.
	if q, err := p.ReviewQueue(ctx, 10); err != nil || len(q) != 0 {
		t.Fatalf("очередь после решения: %v (%v)", q, err)
	}
}

// Запрет писать не выкидывает человека из учётной записи: чтение открыто всем, и
// страница «за что и до когда» обязана остаться ему доступной.
func TestЗапретНеГаситСессии(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	token, _, err := p.CreateSession(ctx, author, "тест")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BanUser(ctx, mod, author, time.Now().Add(24*time.Hour), "реклама"); err != nil {
		t.Fatalf("запрет: %v", err)
	}
	u, err := p.SessionUser(ctx, token)
	if err != nil {
		t.Fatalf("сессия погашена запретом: %v", err)
	}
	if !u.Banned(time.Now()) || u.BanReason != "реклама" {
		t.Fatalf("человек не видит своего запрета: %+v", u)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "снова"}); !errors.Is(err, ErrBanned) {
		t.Fatalf("забаненный опубликовал: %v", err)
	}
	if err := p.UnbanUser(ctx, mod, author, ""); err != nil {
		t.Fatalf("снятие: %v", err)
	}
	if err := p.UnbanUser(ctx, mod, author, ""); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("повторное снятие: %v", err)
	}
}

// Журнал дописывается при каждом действии — и людьми, и автоматом. Без него
// через месяц «за что скрыли» отвечается догадкой.
func TestЖурналПишетсяВсегда(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	mod := moderator(t, p)
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "текст")

	if err := p.HideSubject(ctx, mod, NoteSubject(noteID), CatSpam, "реклама"); err != nil {
		t.Fatal(err)
	}
	if err := p.SetThreadLocked(ctx, mod, noteID, true, "хватит"); err != nil {
		t.Fatal(err)
	}
	entries, err := p.SubjectAudit(ctx, NoteSubject(noteID), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("записей журнала %d: %+v", len(entries), entries)
	}
	for _, e := range entries {
		if e.Actor != mod.UserID || e.Nick == "" {
			t.Fatalf("запись без автора: %+v", e)
		}
	}
	if entries[1].Action != ActionHide || entries[1].Details["reason"] != "реклама" {
		t.Fatalf("скрытие записано как %+v", entries[1])
	}
}

// Обезличивание: тексты остаются на месте (иначе рушатся чужие разговоры), а
// личность уходит — вместе со связью с анкетой НГС.
func TestОбезличиваниеУноситЛичностьНоНеТексты(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := Viewer{UserID: mustUser(t, p, "админ"), Role: RoleAdmin}
	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestComment(t, p, 700200, 312811, 1038894, 0)

	res, err := p.AnonymizeUser(ctx, admin, 1038894)
	if err != nil {
		t.Fatalf("обезличивание: %v", err)
	}
	if res.Comments != 1 {
		t.Fatalf("переехало комментариев %d", res.Comments)
	}
	c, err := p.CommentRow(ctx, 700200)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != StatusVisible {
		t.Fatalf("текст исчез из треда: статус %d", c.Status)
	}
	if c.AuthorID == 1038894 {
		t.Fatal("реплика осталась за прежней анкетой")
	}
	if !IsNative(c.AuthorID) {
		t.Fatalf("автор переехал не в нативную полосу: %d", c.AuthorID)
	}
	grave, err := p.UserByID(ctx, c.AuthorID)
	if err != nil {
		t.Fatal(err)
	}
	if grave.Nick != GraveNick || grave.AnonymizedAt == nil {
		t.Fatalf("могила выглядит как %+v", grave)
	}
	old, err := p.UserByID(ctx, 1038894)
	if err != nil {
		t.Fatal(err)
	}
	if old.Nick != "" || old.AnonymizedAt == nil {
		t.Fatalf("прежняя строка осталась именной: %+v", old)
	}
	// Зеркало не должно возвращать обезличенному ни ник, ни фото.
	if _, err := p.EnsureShadow(ctx, MirroredAuthor{ID: 1038894, Nick: "Пух"}); err != nil {
		t.Fatal(err)
	}
	if again, err := p.UserByID(ctx, 1038894); err != nil || again.Nick != "" {
		t.Fatalf("зеркало вернуло имя обезличенному: %q (%v)", again.Nick, err)
	}
	// Повтор — не действие.
	if _, err := p.AnonymizeUser(ctx, admin, 1038894); !errors.Is(err, ErrAnonymized) {
		t.Fatalf("повторное обезличивание: %v", err)
	}
}

// Выгрузка отдаёт данные ОДНОГО человека и не отдаёт того, чего у нас нет
// (токенов сессий).
func TestВыгрузкаДанных(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "моя заметка про слона")
	if _, _, err := p.CreateSession(ctx, author, "тестовый браузер"); err != nil {
		t.Fatal(err)
	}
	other := mustUser(t, p, "Мавр")
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: other, Body: "чужая заметка"}); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := p.ExportUser(ctx, author, &b); err != nil {
		t.Fatalf("выгрузка: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "моя заметка про слона") {
		t.Error("в выгрузке нет собственной заметки")
	}
	if strings.Contains(out, "чужая заметка") {
		t.Error("в выгрузку попала чужая публикация")
	}
	if !strings.Contains(out, "тестовый браузер") {
		t.Error("в выгрузке нет сведений о сессиях")
	}
	if strings.Contains(out, "token_sha") {
		t.Error("в выгрузку попали внутренние поля сессии")
	}
	_ = noteID
}

// Проход по автору идёт ПОРЦИЯМИ и доводится до конца фоновой службой: у
// участника с 138 тыс. реплик один проход стоит 53 с, и в срок веб-запроса он не
// влезает.
func TestОтложенныйПроходДоводится(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Пух")
	noteID := mustNote(t, p, author, "заметка")
	for i := range 5 {
		ingestComment(t, p, int64(910000+i), noteID, 0, 0)
	}
	// Комментарии зеркала принадлежат тени, поэтому переписываем автора руками:
	// проверяется механика прохода, а не приём.
	if _, err := p.pool.Exec(ctx, `UPDATE comments SET author_id = $1 WHERE note_id = $2`,
		author, noteID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET hide_all = true, visibility_dirty = true WHERE id = $1`, author); err != nil {
		t.Fatal(err)
	}

	dirty, err := p.DirtyVisibility(ctx, 10)
	if err != nil || len(dirty) != 1 || dirty[0] != author {
		t.Fatalf("незавершённые проходы: %v (%v)", dirty, err)
	}
	done, err := p.SettleVisibility(ctx, author)
	if err != nil || !done {
		t.Fatalf("доводка: %v (%v)", done, err)
	}
	if left, err := p.DirtyVisibility(ctx, 10); err != nil || len(left) != 0 {
		t.Fatalf("флаг не снят: %v (%v)", left, err)
	}
	n, err := p.NoteRow(ctx, noteID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != StatusHiddenOwner || n.CommentCount != 0 {
		t.Fatalf("после прохода заметка %d, счётчик %d", n.Status, n.CommentCount)
	}
	if cs, err := p.Thread(ctx, Viewer{}, noteID); err != nil || len(cs) != 0 {
		t.Fatalf("видно %d реплик (%v)", len(cs), err)
	}
}

// Планы запросов — часть контракта: страница треда у МОДЕРАТОРА обязана идти по
// тем же индексам, что у читателя. Иначе она однажды станет полным перебором на
// таблице в 10,7 млн строк, и не провалит ни одного теста на поведение.
func TestПланыЗапросовМодератора(t *testing.T) {
	p := testPlatform(t)
	seedForPlans(t, p)

	cases := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{"тред модератора", threadModQuery, []any{int64(0), int64(200001), 100}, "comments_tree"},
		{"линейный вид модератора", flatModQuery, []any{int64(0), int64(200001), 30, 0}, "comments_flat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, p, c.query, c.args...)
			if !strings.Contains(plan, c.index) {
				t.Fatalf("план не берёт индекс %s:\n%s", c.index, plan)
			}
			if strings.Contains(plan, "Seq Scan on comments") {
				t.Fatalf("полный перебор в плане:\n%s", plan)
			}
		})
	}
}
