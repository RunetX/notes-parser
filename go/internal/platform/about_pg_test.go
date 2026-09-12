package platform

// Рассказ о себе и фотографии (эпик M) — против настоящего Postgres.
//
// Подделкой это не проверить ни в одной точке. Право писать живёт в EXISTS по
// таблице согласий; потолок в три снимка держат CHECK и UNIQUE, а не счёт в Go;
// уборка байтов — это три NOT EXISTS в одном DELETE, и «а не ссылается ли кто
// ещё» ровно тот вопрос, ради которого она и написана.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// aboutStore — хранилище, живущее весь тест. mustShot заводит своё на каждый
// вызов и тем самым переставляет p.media, а уборке нужен КАТАЛОГ, в котором
// файл и лежит.
func aboutStore(t *testing.T, p *Platform) *MediaStore {
	t.Helper()
	store, err := NewMediaStore(p, t.TempDir())
	if err != nil {
		t.Fatalf("хранилище: %v", err)
	}
	return store
}

func mustPhoto(t *testing.T, store *MediaStore, w, h int) Media {
	t.Helper()
	m, err := store.PutSized(context.Background(), testPNG(t, w, h), "", w, h)
	if err != nil {
		t.Fatalf("приём фотографии: %v", err)
	}
	return m
}

// БЕЗ ПОДПИСИ НЕЛЬЗЯ НИЧЕГО, и проверка стоит в ЯДРЕ, а не в форме: дорог сюда
// будет больше одной, и второй список правил рядом с этим однажды разойдётся.
func TestБезСогласияРассказатьОСебеНельзя(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	u := mustUser(t, p, "Рио")
	if err := p.RevokeConsent(ctx, u, ConsentProfile); err != nil {
		t.Fatal(err)
	}
	if err := p.SetAbout(ctx, u, About{City: "Бердск"}); !errors.Is(err, ErrNoProfileConsent) {
		t.Errorf("рассказ без согласия: %v, ожидался ErrNoProfileConsent", err)
	}
	m := mustPhoto(t, store, 800, 600)
	if _, err := p.AddProfilePhoto(ctx, u, &m); !errors.Is(err, ErrNoProfileConsent) {
		t.Errorf("фотография без согласия: %v, ожидался ErrNoProfileConsent", err)
	}
	if err := p.MayTellAbout(ctx, u); !errors.Is(err, ErrNoProfileConsent) {
		t.Errorf("MayTellAbout молчит там, где запись откажет: %v", err)
	}
}

// С подписью — пишется, и карточка ВСТАЁТ В ОЧЕРЕДЬ той же транзакцией:
// «опубликовано, но в очередь не попало» существовать не должно.
func TestРассказОСебеИдётКЧеловеку(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	u := mustUser(t, p, "Полынь-Трава")

	if err := p.SetAbout(ctx, u, About{
		Bio: "Гараж вместо кабинета.", City: "Бердск", Job: "слесарь",
	}); err != nil {
		t.Fatal(err)
	}
	prof, err := p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.City != "Бердск" || prof.Job != "слесарь" || prof.Bio != "Гараж вместо кабинета." {
		t.Errorf("карточка вышла %+v", prof)
	}
	queue, err := p.ReviewQueue(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range queue {
		if it.Subject == ProfileSubject(u) {
			found = true
			if !strings.Contains(it.Body, "Гараж") {
				t.Errorf("модератору не показали текст: %q", it.Body)
			}
		}
	}
	if !found {
		t.Error("карточка не попала в очередь: её смотрит человек и только он")
	}
	// А ПУСТАЯ карточка человека не беспокоит: стереть всё — то же самое, что и
	// не заполнять, и строка в очереди стоила бы его времени ни за что.
	if err := p.SetAbout(ctx, u, About{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Decide(ctx, Viewer{UserID: u, Role: RoleModerator}, ProfileSubject(u),
		DecisionKeep, ""); err != nil {
		t.Fatal(err)
	}
	queue, err = p.ReviewQueue(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range queue {
		if it.Subject == ProfileSubject(u) {
			t.Error("пустая карточка снова встала в очередь")
		}
	}
}

// Три места, и держит их БАЗА: двух одновременных загрузок проверка в Go не
// разводит. Снятое место освобождается и занимается заново — но НЕ
// перенумеровывает соседей: ссылка на место в карточке модератора начала бы
// указывать на другой снимок.
func TestТриФотографииИНиОднойБольше(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	u := mustUser(t, p, "Мурена")

	for i := 1; i <= PhotoLimit; i++ {
		m := mustPhoto(t, store, 100+i, 100)
		pos, err := p.AddProfilePhoto(ctx, u, &m)
		if err != nil {
			t.Fatalf("фотография %d: %v", i, err)
		}
		if pos != i {
			t.Errorf("фотография легла на место %d, ожидалось %d", pos, i)
		}
	}
	extra := mustPhoto(t, store, 999, 100)
	if _, err := p.AddProfilePhoto(ctx, u, &extra); !errors.Is(err, ErrPhotoLimit) {
		t.Errorf("четвёртая фотография: %v, ожидался ErrPhotoLimit", err)
	}
	if err := p.RemoveProfilePhoto(ctx, u, 2); err != nil {
		t.Fatal(err)
	}
	pos, err := p.AddProfilePhoto(ctx, u, &extra)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 2 {
		t.Errorf("новая фотография заняла место %d, а свободно было второе", pos)
	}
	photos, err := p.ProfilePhotos(ctx, u, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != PhotoLimit {
		t.Errorf("в альбоме %d фотографий, ожидалось %d", len(photos), PhotoLimit)
	}
}

// СНЯТАЯ ФОТОГРАФИЯ УНОСИТ БАЙТЫ — единственное место проекта, где хранилище
// чистится. И НЕ уносит, если на те же байты ссылается кто-то ещё: имя файла
// есть его содержимое, и двое вправе поставить одну картинку.
func TestСнятаяФотографияУноситБайты(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	one := mustUser(t, p, "Ягода")
	two := mustUser(t, p, "Лисёнок")

	m := mustPhoto(t, store, 640, 480)
	path := store.FilePath(m.SHA256, m.MIME)
	if _, err := p.AddProfilePhoto(ctx, one, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AddProfilePhoto(ctx, two, &m); err != nil {
		t.Fatal(err)
	}
	// Пока ссылается второй — файл на месте, и строка media тоже.
	if err := p.RemoveProfilePhoto(ctx, one, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл снесли, хотя на него ссылается чужая строка: %v", err)
	}
	// А когда не ссылается никто — уходит и файл.
	if err := p.RemoveProfilePhoto(ctx, two, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("файл остался лежать: %v", err)
	}
	var alive bool
	if err := p.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM media WHERE sha256 = $1)`, m.SHA256).Scan(&alive); err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Error("строка хранилища осталась без единой ссылки")
	}
}

// АВАТАР — тоже ссылка, и уборка обязана его видеть: «моя фотография» и «чужой
// аватар» бывают одним файлом.
func TestУборкаВидитАватар(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	u := mustUser(t, p, "Мавр")

	m := mustPhoto(t, store, 300, 300)
	path := store.FilePath(m.SHA256, m.MIME)
	if _, err := p.AddProfilePhoto(ctx, u, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := p.pool.Exec(ctx,
		`UPDATE users SET avatar_sha = $2 WHERE id = $1`, u, m.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveProfilePhoto(ctx, u, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("уборка снесла файл, который стои́т аватаром: %v", err)
	}
}

// ОТЗЫВ СОГЛАСИЯ ИСПОЛНЯЕТСЯ СРАЗУ и целиком — так обещает сам документ. Это
// единственное место площадки, где отзыв УДАЛЯЕТ, а не обезличивает: у заметок
// есть чужие ответы, ради которых их держат безымянными, а у фотографии нет.
func TestОтзывУбираетРассказИФотографии(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	u := mustUser(t, p, "Каза")

	if err := p.SetAbout(ctx, u, About{Bio: "про себя", City: "Обь", Job: "врач"}); err != nil {
		t.Fatal(err)
	}
	m := mustPhoto(t, store, 500, 500)
	path := store.FilePath(m.SHA256, m.MIME)
	if _, err := p.AddProfilePhoto(ctx, u, &m); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeConsent(ctx, u, ConsentProfile); err != nil {
		t.Fatal(err)
	}
	prof, err := p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Bio != "" || prof.City != "" || prof.Job != "" {
		t.Errorf("после отзыва осталось %+v", prof)
	}
	photos, err := p.ProfilePhotos(ctx, u, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 0 {
		t.Errorf("после отзыва осталось фотографий: %d", len(photos))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("файл пережил отзыв согласия: %v", err)
	}
	// Публикации при этом на месте: отзыв этого согласия про рассказ о себе, а
	// не про человека.
	if _, err := p.CreateNote(ctx, NewNote{AuthorID: u, Body: "а писать могу"}); err != nil {
		t.Errorf("отзыв необязательного согласия отнял право публиковать: %v", err)
	}
}

// Обезличивание делает то же самое, и это ВАЖНЕЕ вида страницы: строка,
// пережившая обезличивание, — обработка без основания.
func TestОбезличиваниеУбираетРассказИФотографии(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	admin := mustAdmin(t, p, "Гадёныш")
	u := mustUser(t, p, "Артемизия")

	if err := p.SetAbout(ctx, u, About{Bio: "про себя", City: "Обь"}); err != nil {
		t.Fatal(err)
	}
	m := mustPhoto(t, store, 400, 400)
	path := store.FilePath(m.SHA256, m.MIME)
	if _, err := p.AddProfilePhoto(ctx, u, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AnonymizeUser(ctx, Viewer{UserID: admin, Role: RoleAdmin}, u); err != nil {
		t.Fatal(err)
	}
	prof, err := p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Bio != "" || prof.City != "" {
		t.Errorf("после обезличивания осталось %+v", prof)
	}
	photos, err := p.ProfilePhotos(ctx, u, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 0 {
		t.Errorf("после обезличивания осталось фотографий: %d", len(photos))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("файл пережил обезличивание: %v", err)
	}
}

// Выгрузка субъекту отдаёт и рассказ, и список фотографий: это его данные, и
// «что вы обо мне знаете» без них было бы неполным ответом.
func TestВыгрузкаОтдаётРассказИФотографии(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	u := mustUser(t, p, "Пуша")

	if err := p.SetAbout(ctx, u, About{Bio: "про себя", City: "Анадырь", Job: "швея"}); err != nil {
		t.Fatal(err)
	}
	m := mustPhoto(t, store, 200, 200)
	if _, err := p.AddProfilePhoto(ctx, u, &m); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := p.ExportUser(ctx, u, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Анадырь", "швея", "про себя", "мои_фотографии", "/media/"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("в выгрузке нет %q", want)
		}
	}
}

// Модератор скрывает КАРТОЧКУ целиком и ОДНУ фотографию порознь: это разные
// решения, и вердикт «весь рассказ не годится» не должен быть единственным
// ответом на одну дурную фотографию из трёх.
func TestМодераторСкрываетКарточкуИОтдельныйСнимок(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	store := aboutStore(t, p)
	mod := mustAdmin(t, p, "Хатуль мадан")
	actor := Viewer{UserID: mod, Role: RoleModerator}
	u := mustUser(t, p, "Примус")

	if err := p.SetAbout(ctx, u, About{Bio: "про себя"}); err != nil {
		t.Fatal(err)
	}
	m := mustPhoto(t, store, 320, 240)
	if _, err := p.AddProfilePhoto(ctx, u, &m); err != nil {
		t.Fatal(err)
	}
	photos, err := p.ProfilePhotos(ctx, u, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.HidePhotoAsModerator(ctx, actor, photos[0].ID, true, "лишнее"); err != nil {
		t.Fatal(err)
	}
	seen, err := p.ProfilePhotos(ctx, u, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 0 {
		t.Error("скрытая фотография видна посторонним")
	}
	// А рассказ при этом на месте: снимок судили, а не его.
	prof, err := p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.AboutStatus != StatusVisible {
		t.Error("вместе с фотографией скрылась и карточка")
	}
	// Теперь вердикт очереди — и скрывается уже вся карточка.
	if err := p.HideSubject(ctx, actor, ProfileSubject(u), CatOther, "не годится"); err != nil {
		t.Fatal(err)
	}
	prof, err = p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.AboutStatus != StatusHiddenMod {
		t.Errorf("карточка не скрыта: статус %d", prof.AboutStatus)
	}
	if err := p.RestoreSubject(ctx, actor, ProfileSubject(u), "передумал"); err != nil {
		t.Fatal(err)
	}
	prof, err = p.UserProfile(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if prof.AboutStatus != StatusVisible {
		t.Error("карточка не вернулась")
	}
}

// Жалоба читателя на карточку принимается — тем же путём, что и на реплику: у
// вида объекта нет своего разбора, и рассказ о себе такой же текст.
func TestНаКарточкуМожноПожаловаться(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	u := mustUser(t, p, "Мрачный")
	reader := mustUser(t, p, "Лампочка")

	if err := p.SetAbout(ctx, u, About{Bio: "про себя"}); err != nil {
		t.Fatal(err)
	}
	if err := p.AddReport(ctx, reader, ProfileSubject(u), "непристойно"); err != nil {
		t.Fatalf("жалоба на карточку: %v", err)
	}
}
