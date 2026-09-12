package web

// «Моя страница»: настройки, которые человек меняет о себе.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// Галочка выноса на НГС: показывается только тому, у кого есть анкета НГС, и
// переключается формой с CSRF. Это не настройка экрана, а согласие на
// публикацию своих слов на чужом сайте, поэтому подделанное нажатие обязано
// отлетать — в отличие от соседней «проматывать к новым».
func TestГалочкаОтправкиНаНГС(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String()
	if !strings.Contains(body, "Отправлять мои записи на НГС") {
		t.Fatal("галочки нет у участника с анкетой НГС")
	}
	if !strings.Contains(body, "сейчас выключено") {
		t.Error("умолчание должно быть выключено: публикуя здесь, человек соглашался на публикацию здесь")
	}

	// Включаем.
	if w := do(h, postAs(t, "/me/ngssend", url.Values{"on": {"1"}}, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("включение: код %d", w.Code)
	}
	if !wr.ngsSend[ngsID] {
		t.Fatal("ядро не узнало о включении")
	}

	// Без CSRF — отлуп: это согласие, а не прокрутка.
	r2 := as(post(t, "/me/ngssend", url.Values{"on": {"0"}}), token)
	if w := do(h, r2); w.Code == http.StatusSeeOther {
		t.Error("нажатие без CSRF прошло")
	}
	if !wr.ngsSend[ngsID] {
		t.Error("подделанное нажатие всё-таки выключило отправку")
	}
}

// ГАЛОЧКА, КОТОРАЯ МОЛЧА НИЧЕГО НЕ ДЕЛАЕТ, ХУЖЕ ОТСУТСТВУЮЩЕЙ (02.09.2026).
//
// Живой случай: у участницы семь ответов подряд легли в skipped «нет живой
// сессии сайта» — она включила отправку, а входа в РюмкинЪ у нас для неё нет, —
// и узнать об этом ей было неоткуда. Сессия живёт в SQLite демона, морда её не
// видит вовсе; зато видит СЛЕД, оставленный им в очереди.
//
// Проверяется и обратное: при нуле застрявших предупреждения нет. Строка обязана
// гаснуть сама, иначе однажды не ушедшая заметка попрекала бы человека год
// спустя, когда всё давно наладилось.
func TestОстановленнаяОтправкаНаНГСОбъясняетсяНаСтранице(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{
		ngsSend:    map[int64]bool{ngsID: true},
		ngsStuck:   map[int64]int{ngsID: 7},
		ngsStuckAt: time.Date(2026, 9, 2, 7, 55, 0, 0, time.UTC),
	}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String()
	if !strings.Contains(body, "На НГС не ушло 7 записей") {
		t.Error("страница молчит о том, что записи не уходят")
	}
	if !strings.Contains(body, "/login") {
		t.Error("сказано о беде и не сказано, что делать")
	}

	wr.ngsStuck = map[int64]int{ngsID: 0}
	if body := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String(); strings.Contains(body, "На НГС не ушло") {
		t.Error("предупреждение осталось после того, как отправка наладилась")
	}
}

// ЗАМЕТКА ЧЕЛОВЕКА С ГАЛОЧКОЙ ЗДЕСЬ НЕ ЗАВОДИТСЯ ВОВСЕ (решение владельца
// 02.09.2026: «если галка отправки стоит, то свою не создаём, отправляем на НГС,
// а с НГС забираем как обычно»).
//
// Проверяется НЕ «черновик завёлся», а то, что ядро НЕ звали публиковать здесь:
// прежде заметка выходила ДВАЖДЫ, и весь смысл правки в том, что второй копии
// больше нет. Плюс адрес перехода — в ЛЕНТУ, а не на заметку: заметки ещё нет, и
// её номер станет известен, только когда зеркало принесёт её с сайта.
func TestЗаметкаСГалочкойУходитНаНГСВместоПлощадки(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{ngsSend: map[int64]bool{ngsID: true}}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	w := do(h, postAs(t, "/new", url.Values{"body": {"пойдёт на сайт"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("публикация: код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/?ngs=1" {
		t.Errorf("после отправки повели на %q, а заметки здесь ещё нет", got)
	}
	if wr.note.Body != "" {
		t.Error("заметка всё-таки заведена здесь — она выйдет дважды")
	}
	if wr.ngsDraft == nil || wr.ngsDraft.Body != "пойдёт на сайт" {
		t.Fatalf("на сайт не отдали ничего: %+v", wr.ngsDraft)
	}
	// Лента говорит, что заметка в пути: полторы минуты тишины читаются как
	// пропажа текста.
	if body := do(h, as(guest(t, "GET", "/?ngs=1"), token)).Body.String(); !strings.Contains(body, "появится здесь сама") {
		t.Error("лента молчит о том, что заметка ушла на сайт")
	}
}

// БЕЗ ГАЛОЧКИ ВСЁ КАК БЫЛО: заметка заводится здесь, и переход ведёт на неё.
// Тест парный к предыдущему — порознь они не значат ничего: первый один прошёл
// бы и на сломанном условии, отправляющем на сайт вообще всё.
func TestБезГалочкиЗаметкаОстаётсяЗдесь(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{nextID: 100000000500}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	w := do(h, postAs(t, "/new", url.Values{"body": {"останется тут"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("публикация: код %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/n/100000000500" {
		t.Errorf("после публикации повели на %q, а заметка здесь", got)
	}
	if wr.ngsDraft != nil {
		t.Error("заметка ушла на сайт без галочки")
	}
	if wr.note.Body != "останется тут" {
		t.Errorf("ядро не завело заметку: %q", wr.note.Body)
	}
}

// ЗАМЕТКА В ПУТИ НАЗЫВАЕТСЯ НА «МОЕЙ СТРАНИЦЕ». Её нет ни в ленте, ни у автора —
// и без этой строки минута ожидания выглядит как потерянный текст.
func TestЗаметкаВПутиВиднаНаСвоейСтранице(t *testing.T) {
	auth := newFakeAuth()
	const ngsID = testProfileID
	auth.users[ngsID] = platform.User{ID: ngsID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), ngsID, "")
	if err != nil {
		t.Fatal(err)
	}
	grantConsents(t, auth, ngsID)
	wr := &fakeWriter{
		ngsSend:    map[int64]bool{ngsID: true},
		ngsPending: map[int64]int{ngsID: 1},
	}
	h := newFullServer(t, &fakeStore{}, auth, wr, nil, nil, Config{})

	body := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String()
	if !strings.Contains(body, "ещё не вернулась сюда") {
		t.Error("страница молчит о заметке, которая в пути")
	}
	wr.ngsPending = map[int64]int{ngsID: 0}
	if body := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String(); strings.Contains(body, "ещё не вернулась сюда") {
		t.Error("строка осталась после того, как заметка доехала")
	}
}

// СВОЯ СТРАНИЦА И ЧУЖАЯ — ОДИН И ТОТ ЖЕ ПРОФИЛЬ.
//
// 12.09.2026 владелец спросил: «к чему отдельно „Мой профиль" и „Моя
// страница"». До того дня своя страница показывала список тумблеров, а
// карточка с рассказом о себе жила по другому адресу — и человек, искавший
// свой профиль, попадал в настройки. Теперь показ ОДИН (parts/profile.gohtml),
// и тест смотрит именно на это: на своей странице стои́т ровно то же, что видят
// другие.
func TestСвояСтраницаЭтоТотЖеПрофиль(t *testing.T) {
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember,
	})
	grantConsents(t, auth, testProfileID)
	st := profileStore()
	st.profile.ID = testProfileID
	st.profile.City, st.profile.Job = "Бердск", "слесарь"
	st.photos = []platform.Photo{{ID: 1, Position: 1, URL: "/media/aa/one.webp"}}
	h := newFullServer(t, st, auth, &fakeWriter{}, newFakeMod(), nil, Config{})

	mine := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	theirs := do(h, as(guest(t, "GET", "/u/"+itoa64(testProfileID)), token)).Body.String()
	for _, want := range []string{
		"Бердск", "слесарь", "one.webp",
		// Записи — та же пара списков, что и у постороннего.
		"про третье свидание", `class="ucols"`,
	} {
		if !strings.Contains(mine, want) {
			t.Errorf("на своей странице нет %q:\n%s", want, tailOf(mine))
		}
		if !strings.Contains(theirs, want) {
			t.Errorf("на чужой странице нет %q — показы разошлись", want)
		}
	}
	// И вкладки называют страницу тем самым именем, которым её зовут пять
	// опубликованных согласий: «отозвать можно на „Моей странице"».
	if !strings.Contains(mine, "Моя страница") || !strings.Contains(mine, `href="/me/settings"`) {
		t.Errorf("нет полоски вкладок с именем страницы:\n%s", tailOf(mine))
	}
}

// ПРАВКА СТОИТ ТАМ ЖЕ, ГДЕ ПОКАЗ, и это главное требование владельца:
// «возможность редактирования сразу из страницы».
//
// Формы приходят с СЕРВЕРА и раскрываются разметкой (details/summary): строгий
// CSP не пускает inline-скриптов, и правка, доступная только при работающем
// JS, однажды перестала бы быть доступной вовсе.
func TestПравкаСтоитТамЖеГдеПоказ(t *testing.T) {
	h, _, _, _, token := aboutServer(t, true)
	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()

	for _, want := range []string{
		`name="bio"`, `name="city"`, `name="job"`, `type="file"`,
		`action="/me/about"`, `action="/me/photo"`, `action="/me/nick"`,
		`<details class="edit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на своей странице нет %q:\n%s", want, tailOf(body))
		}
	}
}

// А ЧУЖОЙ странице формы не достаются — ни одному постороннему, ни модератору.
// Показ общий, правка — нет, и держит это один нулевой указатель (.Edit), а не
// пять условий в шаблоне.
func TestНаЧужойСтраницеФормНет(t *testing.T) {
	st := profileStore()
	h, _, token := profileServer(t, st, platform.RoleAdmin)
	body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()

	for _, forbidden := range []string{`name="bio"`, `action="/me/photo"`, `action="/me/nick"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("на чужой странице завелось %q", forbidden)
		}
	}
}

// Вкладки не путаются местами: тумблеры — на настройках, профиль — на профиле.
// Их и разводили затем, чтобы каждая отвечала на свой вопрос.
func TestВкладкиНеПутаютсяМестами(t *testing.T) {
	h, _, _, _, token := aboutServer(t, true)

	prof := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	sett := do(h, as(guest(t, "GET", "/me/settings"), token)).Body.String()

	if strings.Contains(prof, "Проматывать к новым") {
		t.Error("тумблер остался на профиле")
	}
	if strings.Contains(sett, `name="bio"`) {
		t.Error("форма рассказа о себе оказалась в настройках")
	}
	if !strings.Contains(sett, "Проматывать к новым") {
		t.Errorf("тумблера нет и в настройках:\n%s", tailOf(sett))
	}
	// Согласия отзываются на «Моей странице» — так обещают пять выпущенных
	// документов, и вкладка обязана называть её тем же именем.
	if !strings.Contains(sett, "Моя страница") {
		t.Error("настройки не называют страницу так, как её зовут согласия")
	}
}

// Пока рассказано ЧТО-ТО — форма свёрнута; пока не рассказано ничего — она
// раскрыта сразу. Раздел, о котором говорит одна свёрнутая строка, человек не
// находит: 12.09.2026 владелец, глядя на свою страницу, спросил, как вообще
// пользователь загружает свои три фотографии.
func TestПустойРассказРаскрытСразу(t *testing.T) {
	h, _, _, st, token := aboutServer(t, true)

	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if !strings.Contains(body, `<details class="edit" open>`) {
		t.Errorf("пустой рассказ не позвал заполнить себя:\n%s", tailOf(body))
	}
	if !strings.Contains(body, "Рассказать о себе") {
		t.Error("нет приглашения рассказать о себе")
	}

	st.profile.Bio = "Гараж вместо кабинета."
	body = do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	if strings.Contains(body, `<details class="edit" open>`) {
		t.Error("заполненный рассказ держит форму раскрытой и отодвигает страницу")
	}
	if !strings.Contains(body, "Изменить рассказ о себе") {
		t.Error("заполненный рассказ нечем изменить")
	}
}

// Не подписан документ — форм нет ВОВСЕ, а есть ссылка на экран, где стои́т
// текст. Подпись, данная мимоходом под полем ввода, подписью не является.
func TestБезПодписиНаСвоейСтраницеФормНет(t *testing.T) {
	h, _, _, _, token := aboutServer(t, false)
	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()

	if strings.Contains(body, `name="bio"`) || strings.Contains(body, `type="file"`) {
		t.Errorf("формы показаны до подписи:\n%s", tailOf(body))
	}
	if !strings.Contains(body, `href="/me/about"`) {
		t.Error("не сказано, где прочитать документ")
	}
}

// ЭКРАН ОТЗЫВА НЕ ПУГАЕТ ТЕМ, ЧЕГО НЕ СЛУЧИТСЯ.
//
// Список последствий выбирается по виду документа. До 12.09.2026 ветки было
// две — «привязка» и «всё остальное», — и отзыв согласия на переписку попадал
// во вторую: человеку обещали, что его имя уйдёт со всех его заметок, а отзыв
// не трогает их вовсе. Экран, соврав однажды, перестаёт значить что-либо.
func TestЭкранОтзываГоворитПроСвойДокумент(t *testing.T) {
	h, _, _, _, token := aboutServer(t, true)
	ask := func(kind string) string {
		t.Helper()
		form := url.Values{"kind": {kind}, "action": {"revoke"}}
		return do(h, postAs(t, "/me/consent", form, token)).Body.String()
	}

	if body := ask(platform.ConsentTalks); strings.Contains(body, "уйдёт со всех ваших заметок") {
		t.Errorf("отзыву переписки обещали обезличивание заметок:\n%s", tailOf(body))
	} else if !strings.Contains(body, "Входящие письма закроются") {
		t.Errorf("не сказано главное последствие отзыва переписки:\n%s", tailOf(body))
	}

	// А у рассказа о себе последствие как раз НЕОБРАТИМОЕ, и молчать о нём
	// нельзя: файлы сносятся с диска.
	if body := ask(platform.ConsentProfile); !strings.Contains(body, "УДАЛЯЮТСЯ") {
		t.Errorf("не сказано, что фотографии сносятся с диска:\n%s", tailOf(body))
	}

	// Обязательное согласие свой страшный список сохраняет.
	if body := ask(platform.ConsentDistribution); !strings.Contains(body, "уйдёт со всех ваших заметок") {
		t.Errorf("отзыв распространения перестал объяснять обезличивание:\n%s", tailOf(body))
	}
}
