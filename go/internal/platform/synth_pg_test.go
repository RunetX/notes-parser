package platform

// СМЕЖНОЕ обсуждение (миграция 0022) против настоящего Postgres.
//
// Проверять здесь надо ровно то, ради чего двойник и заведён: он берётся у
// ЛЮБОЙ заметки, включая ту, где уже идёт живой разговор, и при этом не отнимает
// у неё ничего. Прежняя дорога (SetNoteStageAsAdmin) обе половины нарушала:
// заметку с репликами не брала вовсе, а взятую отдавала жителям целиком.

import (
	"context"
	"errors"
	"testing"
)

// ДВОЙНИК БЕРЁТСЯ У ЗАМЕТКИ С ЖИВЫМ ТРЕДОМ — то, чего перевод в песочницу не
// умел и уметь не мог. И берётся, ничего не отнимая: оригинал остаётся обычной
// заметкой, в которой люди говорят как говорили.
func TestДвойникБерётсяУЗаметкиСЖивымТредом(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	author := mustUser(t, p, "Ирма")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "мужчина обещал перезвонить"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: author, Body: "и пропал"}); err != nil {
		t.Fatal(err)
	}
	// Прежняя дорога на такой заметке закрыта — здесь это и проверяется рядом,
	// чтобы разница между двумя ручками была видна тестом, а не памятью.
	if err := p.SetNoteStageAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note, true, ""); !errors.Is(err, ErrStageHasThread) {
		t.Fatalf("перевод заметки с репликами дал %v, ожидался ErrStageHasThread", err)
	}

	twin, err := p.CreateSynthThreadAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note)
	if err != nil {
		t.Fatalf("двойник: %v", err)
	}

	got, err := p.NoteViewByID(ctx, Viewer{}, twin)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stage {
		t.Error("двойник не песочница: в нём смогут писать посторонние")
	}
	if got.SynthOf != note {
		t.Errorf("двойник указывает на заметку %d вместо %d", got.SynthOf, note)
	}
	// Оригинал не тронут — ни признаком, ни тредом.
	orig, err := p.NoteViewByID(ctx, Viewer{}, note)
	if err != nil {
		t.Fatal(err)
	}
	if orig.Stage {
		t.Error("оригинал стал песочницей: у людей отняли их же заметку")
	}
	if orig.CommentCount != 1 {
		t.Errorf("в оригинале %d реплик вместо одной", orig.CommentCount)
	}
	if orig.SynthOf != 0 {
		t.Errorf("оригинал сам показывает synth_of = %d", orig.SynthOf)
	}
}

// Двойник у заметки ОДИН, и держит это база: двух администраторов, нажавших
// кнопку разом, проверка в коде не разводит, а уникальный индекс разводит.
// Повтор при этом отвечает НОМЕРОМ уже заведённого — спрашивали адрес, а не
// разрешение.
func TestДвойникУЗаметкиОдин(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	actor := Viewer{UserID: admin, Role: RoleAdmin}
	note, err := p.CreateNote(ctx, NewNote{AuthorID: mustUser(t, p, "Ирма"), Body: "текст"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.CreateSynthThreadAsAdmin(ctx, actor, note)
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.CreateSynthThreadAsAdmin(ctx, actor, note)
	if !errors.Is(err, ErrSynthExists) {
		t.Fatalf("повтор дал %v, ожидался ErrSynthExists", err)
	}
	if again != first {
		t.Errorf("повтор вернул номер %d вместо %d", again, first)
	}
	twin, ok, err := p.SynthTwin(ctx, note)
	if err != nil || !ok || twin.ID != first {
		t.Errorf("поиск двойника дал (%+v, %v, %v)", twin, ok, err)
	}
}

// Дверь администраторская — тот же довод, что у перевода в песочницу: решается
// не про слова, а про то, кто здесь вправе говорить. И синтетика поверх
// синтетики не заводится: у песочницы и у самого двойника его не бывает.
func TestДвойникаНеДаютМодераторуИПесочнице(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	actor := Viewer{UserID: admin, Role: RoleAdmin}
	note, err := p.CreateNote(ctx, NewNote{AuthorID: mustUser(t, p, "Ирма"), Body: "текст"})
	if err != nil {
		t.Fatal(err)
	}
	mod := mustUser(t, p, "Модератор")
	if _, err := p.CreateSynthThreadAsAdmin(ctx, Viewer{UserID: mod, Role: RoleModerator}, note); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("модератору двойник дал %v, ожидался ErrNotAdmin", err)
	}

	stage := mustStageNote(t, p, admin)
	if _, err := p.CreateSynthThreadAsAdmin(ctx, actor, stage); !errors.Is(err, ErrSynthOfStage) {
		t.Errorf("у песочницы двойник дал %v, ожидался ErrSynthOfStage", err)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx, actor, note)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateSynthThreadAsAdmin(ctx, actor, twin); !errors.Is(err, ErrSynthOfStage) {
		t.Errorf("у двойника двойник дал %v, ожидался ErrSynthOfStage", err)
	}
	if _, err := p.CreateSynthThreadAsAdmin(ctx, actor, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("у несуществующей заметки двойник дал %v, ожидался ErrNotFound", err)
	}
}

// В ЛЕНТЕ двойник ЕСТЬ — решение владельца 31.08.2026: «двойник должен быть в
// ленте — я запустил». Тест этот прежде утверждал обратное, и довод был «это не
// самостоятельная запись, а приложение к чужой заметке». Довод отменён по
// существу: двойника заводят, чтобы разговор случился и его прочли, а лента —
// единственное место, где на площадку смотрят без адреса в руках.
//
// До правки условие `synth_of IS NULL` стояло в ленте ЧИТАТЕЛЯ и не стояло в
// ленте модератора — то есть правило действовало для всех, кроме того
// единственного человека, который двойников и заводит; отсюда и жалоба. Теперь
// обе ленты показывают его одинаково, и проверяются здесь обе.
func TestДвойникСтоитВЛенте(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: mustUser(t, p, "Ирма"), Body: "текст"})
	if err != nil {
		t.Fatal(err)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note)
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []struct {
		name string
		v    Viewer
	}{
		{"читатель", Viewer{}},
		{"модератор", Viewer{UserID: admin, Role: RoleAdmin}},
	} {
		feed, err := p.Feed(ctx, who.v, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		var sawNote, sawTwin bool
		for _, n := range feed {
			switch n.ID {
			case note:
				sawNote = true
			case twin:
				sawTwin = true
			}
		}
		if !sawNote {
			t.Errorf("%s: оригинал пропал из ленты", who.name)
		}
		if !sawTwin {
			t.Errorf("%s: двойника в ленте нет", who.name)
		}
	}
	// А на СТРАНИЦЕ он открывается как всякая заметка: ссылка с оригинала ведёт
	// именно туда, и отвечать на неё «такой страницы нет» было бы поломкой.
	if _, err := p.NoteViewByID(ctx, Viewer{}, twin); err != nil {
		t.Errorf("страница двойника: %v", err)
	}
}

// Двойники заводятся ПОДРЯД: потолок частоты держит УЧАСТНИКА, а двойника
// заводит администратор своей дверью и своим темпом — «заполнить старые
// заметки» есть работа пачками, а не публикация. Цена размена названа и в коде:
// пачка двойников займёт ленту, и удержит её только рука администратора.
func TestДвойникиЗаводятсяПодряд(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	actor := Viewer{UserID: mustAdmin(t, p, "Садовник"), Role: RoleAdmin}

	for i, nick := range []string{"Ирма", "Кузьмич", "Веснушка"} {
		author := mustUser(t, p, nick)
		note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "текст"})
		if err != nil {
			t.Fatalf("заметка %d: %v", i, err)
		}
		if _, err := p.CreateSynthThreadAsAdmin(ctx, actor, note); err != nil {
			t.Fatalf("двойник %d подряд: %v", i+1, err)
		}
	}
}

// КОРНЕВЫЕ РЕПЛИКИ ЖИТЕЛЕЙ НЕ ДЁРГАЮТ АДМИНИСТРАТОРА. Песочницу и двойника он
// заводит не ради разговора с собой: это сцена, и реплик в ней десятки. А вот
// ответ ЕМУ САМОМУ повод даёт по-прежнему — это разговор, а не сцена.
func TestПесочницаНеШлётПоводовАвторуСцены(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	note := mustStageNote(t, p, admin)
	persona := mustPersona(t, p, "Кедрачъ")

	if _, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: persona, Body: "о, тема"}); err != nil {
		t.Fatal(err)
	}
	mine, err := p.CreateComment(ctx, NewComment{NoteID: note, AuthorID: admin, Body: "и правда"})
	if err != nil {
		t.Fatal(err)
	}
	// Отвечает ВТОРОЙ житель: у комментария потолок «раз в десять секунд», и
	// первый упёрся бы в него — в бою жители говорят с замеренной задержкой в
	// минуты, а тест идёт за миллисекунды.
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: mustPersona(t, p, "Мазай"), ReplyToID: mine,
		Body: "а вот и нет"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FanOut(ctx, 100); err != nil {
		t.Fatal(err)
	}
	list, err := p.Notifications(ctx, admin, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var roots, replies int
	for _, n := range list {
		switch n.Reason {
		case ReasonReplyToNote:
			roots++
		case ReasonReplyToComment:
			replies++
		}
	}
	if roots != 0 {
		t.Errorf("корневых поводов из песочницы %d, ожидалось ноль", roots)
	}
	if replies != 1 {
		t.Errorf("поводов «ответили вам» %d, ожидался один", replies)
	}
}

// ЦИТАТА ОРИГИНАЛА — то, из чего состоит карточка двойника (жалоба владельца
// 31.08.2026: «кажется, не хватает цитаты исходного текста»). Копии текста при
// этом нигде нет: слова выводятся соединением по synth_of в момент показа, и
// разойтись с оригиналом не могут ни правкой, ни обезличиванием.
func TestЦитатаДвойникаБерётсяУОригинала(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	author := mustUser(t, p, "Ирма")
	note, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "мужчина обещал перезвонить"})
	if err != nil {
		t.Fatal(err)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note)
	if err != nil {
		t.Fatal(err)
	}

	got, err := p.SynthOrigins(ctx, []int64{twin})
	if err != nil {
		t.Fatal(err)
	}
	o, ok := got[twin]
	if !ok {
		t.Fatalf("оригинала для двойника %d нет вовсе", twin)
	}
	if o.ID != note {
		t.Errorf("цитата ведёт на заметку %d вместо %d", o.ID, note)
	}
	if o.Body != "мужчина обещал перезвонить" {
		t.Errorf("тело цитаты %q", o.Body)
	}
	if o.Nick != "Ирма" {
		t.Errorf("подпись цитаты %q вместо ника автора оригинала", o.Nick)
	}

	// ПРАВКА ОРИГИНАЛА ДОЕЗЖАЕТ ДО ЦИТАТЫ САМА — ради этого копии и нет.
	if err := p.EditNoteAsAdmin(ctx, Viewer{UserID: admin, Role: RoleAdmin}, note,
		"мужчина обещал перезвонить и пропал", "опечатка"); err != nil {
		t.Fatal(err)
	}
	got, err = p.SynthOrigins(ctx, []int64{twin})
	if err != nil {
		t.Fatal(err)
	}
	if got[twin].Body != "мужчина обещал перезвонить и пропал" {
		t.Errorf("после правки оригинала цитата осталась прежней: %q", got[twin].Body)
	}
}

// СКРЫТЫЙ ОРИГИНАЛ ЦИТАТЫ НЕ ДАЁТ: показать убранный модератором текст на
// соседней странице значило бы вернуть его на вид в обход решения. Двойник при
// этом остаётся — он просто говорит о заметке, которой читателю не видно.
func TestСкрытыйОригиналНеЦитируется(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	actor := Viewer{UserID: admin, Role: RoleAdmin}
	note, err := p.CreateNote(ctx, NewNote{AuthorID: mustUser(t, p, "Ирма"), Body: "текст"})
	if err != nil {
		t.Fatal(err)
	}
	twin, err := p.CreateSynthThreadAsAdmin(ctx, actor, note)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.HideSubject(ctx, actor, NoteSubject(note), "", "жалоба"); err != nil {
		t.Fatal(err)
	}

	got, err := p.SynthOrigins(ctx, []int64{twin})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[twin]; ok {
		t.Error("скрытая заметка процитирована на странице двойника")
	}
}

// Пустой список — не запрос: страница без двойников не платит за них ничего.
// Ровно ради этого цитаты и живут отдельным вызовом, а не колонкой в ленте.
func TestБезДвойниковЗапросаНеДелаем(t *testing.T) {
	p := testPlatform(t)
	got, err := p.SynthOrigins(context.Background(), nil)
	if err != nil || got != nil {
		t.Errorf("пустой список дал %v, %v", got, err)
	}
}
