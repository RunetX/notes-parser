package platform

// Страница участника и жители в панели администратора — против настоящего
// Postgres.
//
// Подделкой это не проверить: половина правил здесь живёт в SELECT (аноним не
// попадает ни в счётчик, ни в список), а вторая — в UPDATE с условием
// `AND persona`, то есть в том, задело ли обновление строку.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// АНОНИМНАЯ ЗАМЕТКА НЕ ПОПАДАЕТ НА СТРАНИЦУ АВТОРА — ни числом, ни строкой.
// Иначе профиль деанонимизировал бы его соседством: «заметок 2» рядом с одной
// показанной означает вторую, и найти её в ленте того же дня несложно.
func TestПрофильНеВыдаётАнонима(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Полынь-Трава")

	if _, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "подписанная заметка"}); err != nil {
		t.Fatal(err)
	}
	// Заметка у автора одна в пять минут, а нам нужны две от ОДНОГО человека:
	// в этом вся проверка. Отодвигаем первую, как это делают соседние тесты.
	if _, err := p.pool.Exec(ctx,
		`UPDATE notes SET published_at = published_at - interval '6 minutes' WHERE author_id = $1`,
		author); err != nil {
		t.Fatal(err)
	}
	// Своя анонимная заметка хранит НАСТОЯЩЕГО автора — на том и держится
	// проверка: строка в базе есть, а на странице её быть не должно.
	if _, err := p.CreateNote(ctx, NewNote{
		AuthorID: author, Body: "тайна", Anonymous: true,
	}); err != nil {
		t.Fatal(err)
	}
	prof, err := p.UserProfile(ctx, author)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Notes != 1 {
		t.Errorf("заметок в карточке %d, ожидалась одна: анонимная сюда не идёт", prof.Notes)
	}
	notes, err := p.AuthorNotes(ctx, author, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || strings.Contains(notes[0].Excerpt, "тайна") {
		t.Errorf("в списке заметок аноним: %+v", notes)
	}
}

// Скрытое модератором на страницу не идёт тоже: она отвечает на вопрос «что
// человек сказал НА ВИДУ», а для скрытого есть очередь.
func TestПрофильНеПоказываетСкрытое(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Мурена")
	mod := mustAdmin(t, p, "Садовник")

	hidden, err := p.CreateNote(ctx, NewNote{AuthorID: author, Body: "скрытая заметка"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.HideSubject(ctx, Viewer{UserID: mod, Role: RoleAdmin},
		NoteSubject(hidden), CatProfanity, "брань"); err != nil {
		t.Fatal(err)
	}
	prof, err := p.UserProfile(ctx, author)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Notes != 0 {
		t.Errorf("скрытая заметка попала в счётчик: %d", prof.Notes)
	}
	notes, err := p.AuthorNotes(ctx, author, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("скрытая заметка в списке: %+v", notes)
	}
}

// Реплика приезжает вместе с началом ЗАМЕТКИ, в которой сказана: реплика вне
// разговора нечитаема, а один её номер не говорит человеку ничего.
func TestРепликиПрофиляНазываютРазговор(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	author := mustUser(t, p, "Ягода")
	other := mustUser(t, p, "Сосед")

	note, err := p.CreateNote(ctx, NewNote{AuthorID: other, Body: "про третье свидание"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateComment(ctx, NewComment{
		NoteID: note, AuthorID: author, Body: "а я бы не поехала",
	}); err != nil {
		t.Fatal(err)
	}
	list, err := p.AuthorComments(ctx, author, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("реплик %d, ожидалась одна", len(list))
	}
	if list[0].NoteID != note || !strings.Contains(list[0].Note, "третье свидание") {
		t.Errorf("реплика без разговора: %+v", list[0])
	}
	if prof, err := p.UserProfile(ctx, author); err != nil || prof.Comments != 1 {
		t.Errorf("реплик в карточке %d (%v), ожидалась одна", prof.Comments, err)
	}
}

// ------------------------------------------------------------------- жители

// Биография — свойство ЖИТЕЛЯ. Живому человеку её не пишут: поля «о себе»
// площадка не заводила нигде, а сочинённое под чужим именем было бы подделкой.
func TestБиографияТолькоЖителю(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	persona := mustPersona(t, p, "Механик Сева")
	human := mustUser(t, p, "Полынь-Трава")

	if err := p.SetPersonaBio(ctx, persona, "Слесарь, гараж в Первомайке."); err != nil {
		t.Fatalf("биография жителю: %v", err)
	}
	prof, err := p.UserProfile(ctx, persona)
	if err != nil {
		t.Fatal(err)
	}
	if !prof.Persona || prof.Bio != "Слесарь, гараж в Первомайке." {
		t.Errorf("карточка жителя: persona=%v, bio=%q", prof.Persona, prof.Bio)
	}
	if err := p.SetPersonaBio(ctx, human, "выдумка"); !errors.Is(err, ErrNotPersona) {
		t.Errorf("живому человеку сочинили биографию: %v", err)
	}
	// Повторный enroll обязан ДОНЕСТИ правку карточки, а не отказать.
	if err := p.SetPersonaBio(ctx, persona, "Слесарь, гараж в Первомайке. Кот."); err != nil {
		t.Fatalf("правка биографии: %v", err)
	}
}

// Фото жителю ставит АДМИНИСТРАТОР, и только жителю: у персонажа нет анкеты
// НГС, а человеку фото приносит его собственная — чужой рукой оно не ставится.
func TestФотоЖителяСтавитАдминистратор(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	persona := mustPersona(t, p, "Механик Сева")
	human := mustUser(t, p, "Полынь-Трава")

	media := mustShot(t, p, 300, 300)
	actor := Viewer{UserID: admin, Role: RoleAdmin}
	if err := p.SetPersonaAvatarAsAdmin(ctx, actor, persona, &media, "лицо жителя"); err != nil {
		t.Fatalf("фото жителю: %v", err)
	}
	prof, err := p.UserProfile(ctx, persona)
	if err != nil {
		t.Fatal(err)
	}
	if prof.AvatarURL == "" {
		t.Error("фото не встало")
	}

	// Модератор решает про СЛОВА: лицо жителя не его вопрос.
	if err := p.SetPersonaAvatarAsAdmin(ctx,
		Viewer{UserID: admin, Role: RoleModerator}, persona, &media, ""); !errors.Is(err, ErrNotAdmin) {
		t.Errorf("модератор поставил фото: %v", err)
	}
	if err := p.SetPersonaAvatarAsAdmin(ctx, actor, human, &media, ""); !errors.Is(err, ErrNotPersona) {
		t.Errorf("живому человеку поставили фото: %v", err)
	}

	// Снятие — тот же вызов с nil. Ссылка ngs_avatar_url остаётся пустой: по
	// ней `platform media` добирает байты, а у жителя её нет и быть не может.
	if err := p.SetPersonaAvatarAsAdmin(ctx, actor, persona, nil, "передумали"); err != nil {
		t.Fatalf("снятие фото: %v", err)
	}
	if prof, err := p.UserProfile(ctx, persona); err != nil || prof.AvatarURL != "" {
		t.Errorf("фото не снялось: %q (%v)", prof.AvatarURL, err)
	}

	// Оба решения — в журнале: «поставили, но в журнал не попало» через месяц
	// отвечается догадкой.
	log, err := p.AuditTail(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var on, off bool
	for _, e := range log {
		switch e.Action {
		case ActionAvatar:
			on = true
		case ActionAvatarOff:
			off = true
		}
	}
	if !on || !off {
		t.Errorf("в журнале нет решений о фото: поставили=%v сняли=%v", on, off)
	}
}

// Список жителей — то, чем их правят в панели администратора. Живых в нём нет.
func TestСписокЖителей(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	persona := mustPersona(t, p, "Механик Сева")
	mustUser(t, p, "Полынь-Трава")
	if err := p.SetPersonaBio(ctx, persona, "Слесарь."); err != nil {
		t.Fatal(err)
	}
	list, err := p.Personas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != persona || list[0].Bio != "Слесарь." {
		t.Errorf("список жителей: %+v", list)
	}
}

// Мордолента: кто попадает в полосу лиц и в каком порядке.
//
// Два правила, и оба видны только на живой базе. Без ФОТО жителя в полосе нет:
// мордолента есть лента лиц, и силуэт занял бы место, ничего не сказав. Порядок
// — по последнему сказанному слову: полоса с вечным порядком за неделю
// становится частью фона, а на НГС мордолента как раз двигалась.
func TestМордолентаБерётЛицаИДвижетсяПоРазговору(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	admin := mustAdmin(t, p, "Садовник")
	actor := Viewer{UserID: admin, Role: RoleAdmin}
	note := mustStageNote(t, p, admin)

	sova := mustPersona(t, p, "Сова")
	seva := mustPersona(t, p, "Механик Сева")
	nemoy := mustPersona(t, p, "Безлицый")
	mustUser(t, p, "Полынь-Трава") // живой человек в полосу не идёт вовсе

	media := mustShot(t, p, 300, 300)
	for _, id := range []int64{sova, seva} {
		if err := p.SetPersonaAvatarAsAdmin(ctx, actor, id, &media, "лицо"); err != nil {
			t.Fatalf("фото жителю %d: %v", id, err)
		}
	}

	// Сперва сказала Сова, потом Сева, — значит наверху полосы Сева.
	for _, id := range []int64{sova, seva, nemoy} {
		if _, err := p.CreateComment(ctx, NewComment{
			NoteID: note, AuthorID: id, Body: "и вот что я думаю",
		}); err != nil {
			t.Fatalf("реплика жителя %d: %v", id, err)
		}
	}

	faces, err := p.PersonaFaces(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 2 {
		t.Fatalf("в полосе %d лиц, ожидалось 2 (безлицый и живой в неё не идут): %+v", len(faces), faces)
	}
	if faces[0].ID != seva || faces[1].ID != sova {
		t.Errorf("порядок полосы %d, %d — ожидался «кто говорил последним»: %d, %d",
			faces[0].ID, faces[1].ID, seva, sova)
	}
	if faces[0].Nick != "Механик Сева" || faces[0].AvatarURL == "" {
		t.Errorf("лицо без имени или без картинки: %+v", faces[0])
	}

	// Ещё не заговоривший из полосы не пропадает — он здесь живёт, просто
	// молчит; уходит он в конец.
	if err := p.SetPersonaAvatarAsAdmin(ctx, actor, nemoy, &media, "лицо"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM comments WHERE author_id = $1`, nemoy); err != nil {
		t.Fatal(err)
	}
	faces, err = p.PersonaFaces(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(faces) != 3 || faces[2].ID != nemoy {
		t.Errorf("молчащий житель встал не в конец полосы: %+v", faces)
	}
}
