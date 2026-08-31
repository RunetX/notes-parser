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

// В ЛЕНТЕ двойника нет: это не самостоятельная запись, а приложение к чужой
// заметке, и место ему — ссылкой на её странице. Иначе лента показывала бы
// служебную строку про машинный разговор наравне с заметками людей.
func TestДвойникаНетВЛенте(t *testing.T) {
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
	feed, err := p.Feed(ctx, Viewer{}, 0, 50)
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
		t.Error("оригинал пропал из ленты")
	}
	if sawTwin {
		t.Error("двойник встал в ленту наравне с заметками людей")
	}
	// А на СТРАНИЦЕ он открывается как всякая заметка: ссылка с оригинала ведёт
	// именно туда, и отвечать на неё «такой страницы нет» было бы поломкой.
	if _, err := p.NoteViewByID(ctx, Viewer{}, twin); err != nil {
		t.Errorf("страница двойника: %v", err)
	}
}

// Двойники заводятся ПОДРЯД: потолок частоты защищает ленту от наплыва, а
// двойника в ленте нет вовсе. Иначе «заполнить старые заметки» упиралось бы в
// пять записей в сутки — в правило, охраняющее то, куда двойник не попадает.
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
