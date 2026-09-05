package platform

import (
	"context"
	"testing"
)

// Служебная анкета площадки — та, под которой выходит недельный выпуск.
// Идемпотентность здесь не удобство: `platform migrate` гоняют при каждой
// выкатке схемы, и вторая анкета означала бы, что «под кем выходит выпуск» —
// вопрос с двумя ответами.
func TestСлужебнаяАнкетаЗаводитсяОдинРаз(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	first, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatalf("первая анкета: %v", err)
	}
	second, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatalf("повтор: %v", err)
	}
	if first != second {
		t.Fatalf("повторный вызов завёл вторую анкету: %d и %d", first, second)
	}
	if id, err := p.SystemUserID(ctx); err != nil || id != first {
		t.Fatalf("поиск служебной анкеты: %d, %v (ожидалось %d)", id, err, first)
	}
	// Ник — latest-wins: имя площадки живёт одной константой в коде, и
	// переименование обязано доезжать до подписи прошлых выпусков само.
	if _, err := p.EnsureSystemUser(ctx, "Другое имя"); err != nil {
		t.Fatalf("переименование: %v", err)
	}
	u, err := p.UserByID(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if u.Nick != "Другое имя" {
		t.Fatalf("ник после переименования %q", u.Nick)
	}
	if u.Kind != KindService {
		t.Fatalf("вид анкеты %d, ожидался служебный", u.Kind)
	}
	// Роль модераторская, и ровно по двум делам: закрепить свой выпуск наверху
	// ленты и не встать самой себе в очередь автомата. Администратором её
	// делать незачем.
	if u.Role != RoleModerator {
		t.Fatalf("роль служебной анкеты %d, ожидалась модераторская", u.Role)
	}
}

// Второй служебной анкеты не бывает, и держит это БАЗА, а не порядок вызовов:
// два `migrate` разом или накат дампа поверх живой базы проверку в коде не
// разводят. Тот же довод, что у двойника заметки (notes_synth_of).
func TestВторойСлужебнойАнкетыНеБывает(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	if _, err := p.EnsureSystemUser(ctx, "Зазеркалье"); err != nil {
		t.Fatal(err)
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO users (id, nick, kind) VALUES (nextval('users_native_seq'), $1, $2)`,
		"Зазеркалье-2", KindService)
	if err == nil {
		t.Fatal("база приняла вторую служебную анкету")
	}
}

// ГЛАВНЫЙ ТЕСТ ЭТОЙ ПРАВКИ, и стоит он НА ПУТИ ДАННЫХ: проверяется не формула,
// а то, что легло в очередь выноса.
//
// 05.09.2026 недельный выпуск уехал заметкой на love.ngs.ru (313176) под именем
// владельца: сводку подписывал живой человек, а у человека стои́т галочка
// «отправлять мои записи на НГС» — она про АВТОРА, а не про текст, и отличить
// сводку площадки от его собственной заметки было нечем.
//
// Галочка здесь взводится СЛУЖЕБНОЙ АНКЕТЕ нарочно, прямым UPDATE в обход
// SetNGSSend: так тест доказывает, что выпуск остаётся дома СТРУКТУРНО (у
// площадки нет анкеты на НГС, и enqueueNGS спрашивает `kind = KindMember`), а
// не потому, что признак случайно оказался выключен.
//
// Заодно он же держит вторую половину правила: согласий служебная анкета не
// подписывает вовсе, и публикация проходит без единой строки в `consents`.
func TestВыпускПлощадкиНеУходитНаНГС(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	sys, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE users SET ngs_send = true WHERE id = $1`, sys); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: sys, Body: "Дайджест недели"}); err != nil {
		t.Fatalf("выпуск: %v", err)
	}
	if n := outboxCount(t, p); n != 0 {
		t.Fatalf("выпуск площадки встал в очередь на НГС: %d строк", n)
	}

	// Контроль: у живого участника с той же галочкой заметка уходит. Без него
	// тест зеленел бы и на сломанной очереди, которая не берёт вообще ничего.
	member := mustNGSMember(t, p, 1493279, "Рио")
	if err := p.SetNGSSend(ctx, member, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: member, Body: "заметка человека"}); err != nil {
		t.Fatalf("заметка участника: %v", err)
	}
	if n := outboxCount(t, p); n != 1 {
		t.Fatalf("в очереди %d строк, ожидалась одна — заметка участника", n)
	}
}

// Площадка не ставит саму себя в очередь автомата модерации: строка очереди —
// платный запрос к модели, а проверять собственную сводку значит проверять то,
// что оператор и написал. Даётся это ролью, а не отдельным условием.
func TestВыпускНеВстаётВОчередьПроверки(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	sys, err := p.EnsureSystemUser(ctx, "Зазеркалье")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: sys, Body: "Дайджест недели"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM moderation_queue`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("выпуск площадки встал в очередь проверки: %d строк", n)
	}
}
