package web

// Рассказ о себе и фотографии (эпик M) со стороны морды.
//
// Главное, что здесь проверяется, — ПОРЯДОК: документ прежде формы. Форма,
// показанная до подписи, не «чуть удобнее», а сбор данных без основания, и
// заметить это по виду страницы нельзя — она выглядит работающей.

import (
	"net/http"
	"strconv"
	"net/url"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

// aboutServer — вошедший участник и морда со всеми способностями.
func aboutServer(t *testing.T, signed bool) (http.Handler, *fakeAuth, *fakeWriter, *fakeStore, string) {
	t.Helper()
	st := profileStore()
	// Смотрим на СВОЮ страницу: «Рассказ о себе» показывает уже написанное,
	// и карточка ему нужна своя.
	st.profile.ID = testProfileID
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember,
	})
	// Обязательные — иначе своя страница уводит на экран входа, и проверять
	// формы будет негде.
	grantConsents(t, auth, testProfileID)
	if signed {
		doc := currentDoc(t, platform.ConsentProfile)
		if err := auth.GrantConsent(t.Context(), testProfileID, doc.Kind, doc.Version, ""); err != nil {
			t.Fatal(err)
		}
	}
	wr := &fakeWriter{}
	// Перекодировщик подключён: без него поля файла на форме нет вовсе (кнопка,
	// отвечающая отказом, хуже её отсутствия), и половину эпика было бы не
	// проверить.
	srv := newServerFor(t, st, auth, wr, newFakeMod(), nil, Config{})
	srv.SetShots(newShots())
	return srv.routes(), auth, wr, st, token
}

func currentDoc(t *testing.T, kind string) platform.ConsentDoc {
	t.Helper()
	doc, err := platform.ConsentDocOf(platform.Operator{}, kind)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// Документ ПРЕЖДЕ формы, и формы до подписи нет вовсе — не «есть, но отвечает
// отказом». Подпись, данная мимоходом под полем ввода, подписью не является;
// тем же порядком устроены экраны привязки мессенджера и переписки.
func TestРассказОСебеСперваДокумент(t *testing.T) {
	h, _, _, _, token := aboutServer(t, false)
	body := do(h, as(guest(t, "GET", "/me/about"), token)).Body.String()

	if !strings.Contains(body, "Согласие на рассказ о себе") {
		t.Errorf("документа на экране нет:\n%s", tailOf(body))
	}
	for _, forbidden := range []string{`name="bio"`, `name="city"`, `type="file"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("форма показана до подписи: %s", forbidden)
		}
	}
	// И обещание документа проверяется по его же тексту: он обязан сослаться на
	// действующие согласия, иначе закрытый перечень обрабатываемого молча
	// перестанет быть закрытым.
	if !strings.Contains(body, "ДОПОЛНЕНИЕ") {
		t.Error("документ не говорит, что дополняет прежние согласия")
	}
}

// А после подписи экрана документа больше нет: формы стоят на «Моей странице»,
// там же, где виден их результат, — и /me/about уводит туда же.
//
// Развести показ и правку по разным адресам значит однажды показать одно, а
// править другое; 12.09.2026 это и случилось — владелец, глядя на свою
// страницу, спросил, как вообще пользователь загружает свои три фотографии.
func TestПослеПодписиФормыНаСвоейСтранице(t *testing.T) {
	h, _, _, _, token := aboutServer(t, true)

	if got := do(h, as(guest(t, "GET", "/me/about"), token)).Header().Get("Location"); got != "/me" {
		t.Errorf("подписавшего увели на %q, а форм там больше нет", got)
	}
	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()
	for _, want := range []string{`name="bio"`, `name="city"`, `name="job"`, `type="file"`} {
		if !strings.Contains(body, want) {
			t.Errorf("на своей странице нет %s:\n%s", want, tailOf(body))
		}
	}
	// Потолок назван числом ИЗ ЯДРА: написанное словом разошлось бы с
	// проверкой базы молча.
	if !strings.Contains(body, "из 3") {
		t.Errorf("потолок фотографий не назван числом из ядра:\n%s", tailOf(body))
	}
}

// Подпись ставится своей кнопкой и возвращает на ту же страницу — уже с формой.
func TestПодписьВключаетРассказ(t *testing.T) {
	h, auth, _, _, token := aboutServer(t, false)
	w := do(h, postAs(t, "/me/about/consent", url.Values{}, token))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	have := auth.consents[testProfileID]
	if !have.Has(platform.ConsentProfile, currentDoc(t, platform.ConsentProfile).Version) {
		t.Error("согласие не записано")
	}
}

// Написанное доходит до ядра как есть: морда полей не чистит и не дополняет —
// потолки длины и право писать живут в одном месте, и второй их список здесь
// однажды разошёлся бы с настоящим.
func TestРассказДоходитДоЯдра(t *testing.T) {
	h, _, wr, _, token := aboutServer(t, true)
	form := url.Values{"city": {"Бердск"}, "job": {"слесарь"}, "bio": {"Гараж вместо кабинета."}}
	if w := do(h, postAs(t, "/me/about", form, token)); w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	want := platform.About{Bio: "Гараж вместо кабинета.", City: "Бердск", Job: "слесарь"}
	if wr.about != want {
		t.Errorf("до ядра дошло %+v, ожидалось %+v", wr.about, want)
	}
}

// Фотография снимается по МЕСТУ, а «такой уже нет» — не ошибка: это нажатие
// дважды или возврат по истории, и страницей отказа на него отвечать не на что.
func TestФотографияСнимаетсяПоМесту(t *testing.T) {
	h, _, wr, _, token := aboutServer(t, true)
	form := url.Values{"position": {"2"}}
	if w := do(h, postAs(t, "/me/photo/drop", form, token)); w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if wr.photoDropped != 2 {
		t.Errorf("сняли место %d, а просили второе", wr.photoDropped)
	}

	wr.aboutFail = platform.ErrNoPhoto
	if w := do(h, postAs(t, "/me/photo/drop", form, token)); w.Code != http.StatusSeeOther {
		t.Errorf("«такой фотографии нет» ответило кодом %d, а это не ошибка", w.Code)
	}
}

// Гость сюда не попадает вовсе — ни на страницу, ни на её формы.
func TestРассказТолькоВошедшим(t *testing.T) {
	h, _, _, _, _ := aboutServer(t, true)
	if got := do(h, guest(t, "GET", "/me/about")).Header().Get("Location"); got != "/login" {
		t.Errorf("гостя ведёт на %q, ожидался /login", got)
	}
	if w := do(h, post(t, "/me/about", url.Values{"bio": {"я"}})); w.Code == http.StatusOK {
		t.Error("гость записал рассказ о себе")
	}
}

// Страница участника показывает город и занятие СТРОКАМИ справочной колонки, а
// рассказ — плашкой. Пустые поля пропускаются поштучно: строка «Город: —» хуже
// отсутствующей.
func TestКарточкаНаСтраницеУчастника(t *testing.T) {
	st := profileStore()
	st.profile.City = "Бердск"
	st.profile.Job = "слесарь-ремонтник"
	st.profile.Bio = "Гараж вместо кабинета."
	h, _, token := profileServer(t, st, platform.RoleUser)
	body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()

	for _, want := range []string{"Город", "Бердск", "Занятие", "слесарь-ремонтник", "Гараж вместо кабинета."} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
	// А предупреждения про машину у ЖИВОГО человека быть не должно: это его
	// собственные слова.
	if strings.Contains(body, "пишет машина") {
		t.Error("живого человека объявили жителем площадки")
	}
}

// Альбом виден на странице, а скрытая модератором фотография — только своему
// хозяину и модератору.
func TestСкрытуюФотографиюВидятДвое(t *testing.T) {
	st := profileStore()
	st.photos = []platform.Photo{
		{ID: 1, Position: 1, URL: "/media/aa/one.webp"},
		{ID: 2, Position: 2, URL: "/media/bb/two.webp", Status: platform.StatusHiddenMod},
	}
	for _, tc := range []struct {
		имя     string
		роль    platform.Role
		скрытую bool
	}{
		{"посторонний участник", platform.RoleUser, false},
		{"модератор", platform.RoleModerator, true},
	} {
		t.Run(tc.имя, func(t *testing.T) {
			h, _, token := profileServer(t, st, tc.роль)
			body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()
			if !strings.Contains(body, "one.webp") {
				t.Error("видимой фотографии нет на странице")
			}
			if got := strings.Contains(body, "two.webp"); got != tc.скрытую {
				t.Errorf("скрытая фотография показана=%v, ожидалось %v", got, tc.скрытую)
			}
		})
	}
}

// Карточку, скрытую модератором, посторонний не видит ЦЕЛИКОМ — ни рассказа,
// ни города, ни занятия, ни снимков. Решается это одним вопросом в Go: три
// условия в шаблоне однажды показали бы половину скрытого.
func TestСкрытаяКарточкаНеВиднаПостороннему(t *testing.T) {
	st := profileStore()
	st.profile.City = "Бердск"
	st.profile.Bio = "Гараж вместо кабинета."
	st.profile.AboutStatus = platform.StatusHiddenMod
	st.photos = []platform.Photo{{ID: 1, Position: 1, URL: "/media/aa/one.webp"}}

	h, _, token := profileServer(t, st, platform.RoleUser)
	body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()
	for _, hidden := range []string{"Бердск", "Гараж вместо кабинета.", "one.webp"} {
		if strings.Contains(body, hidden) {
			t.Errorf("скрытая карточка показала %q", hidden)
		}
	}
	// Публикации при этом на месте: скрыт рассказ о себе, а не человек.
	if !strings.Contains(body, "про третье свидание") {
		t.Error("вместе с карточкой скрылись и публикации")
	}

	h, _, token = profileServer(t, st, platform.RoleModerator)
	body = do(h, as(guest(t, "GET", profilePath()), token)).Body.String()
	if !strings.Contains(body, "Бердск") || !strings.Contains(body, "Карточка скрыта модератором") {
		t.Errorf("модератор не видит скрытую карточку или не знает, что она скрыта:\n%s", tailOf(body))
	}
}

// Приём фотографии — это ПРИЁМ ФАЙЛА, и путь обязан стоять в общем списке:
// у Caddy свой потолок тела, и путь, известный одному из двух списков,
// получает потолок другого. Разъезжались они уже дважды.
func TestПутьФотографииСчитаетсяПриёмомФайла(t *testing.T) {
	r := post(t, "/me/photo", url.Values{})
	if !isUpload(r) {
		t.Error("/me/photo не считается приёмом файла: тело порежет потолок текстовой формы")
	}
}

// Аватар берётся из СВОЕЙ фотографии — третья дорога к лицу рядом с «Обновить
// аватар» (из анкеты НГС) и «Убрать». Нужна она потому, что первая умирает
// вместе с анкетой: у вошедшего по приглашению её нет вовсе, а комментариев
// сайт месяц не принимал.
//
// Проверяется ПУТЬ ДАННЫХ, а не факт ответа: у какой фотографии спросили байты,
// что ушло в перекодировщик и с какой ссылкой лёг аватар. Пустая ссылка —
// условие, а не мелочь: по ней `platform media` отличает «байты ещё не забрали»
// от «фото своё», и непустая вернула бы человеку снимок из анкеты следующим же
// обходом.
func TestАватарИзСвоейФотографии(t *testing.T) {
	h, _, wr, _, token := aboutServer(t, true)
	wr.photoBytes = []byte("снимок")

	w := do(h, postAs(t, "/me/avatar/photo", url.Values{"position": {"2"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("код %d, ожидался 303", w.Code)
	}
	if wr.photoTaken != 2 {
		t.Errorf("байты спросили у места %d, а просили второе", wr.photoTaken)
	}
	if wr.avatar.url != "" {
		t.Errorf("аватар лёг со ссылкой %q — по непустой его перепишет добор из анкеты", wr.avatar.url)
	}
	if len(wr.avatar.data) == 0 {
		t.Error("аватар лёг пустым")
	}
}

// Уменьшать обязательно: в ленте двадцать заметок, и двадцать снимков по 1600
// точек ради двадцати пятаков — та же арифметика, по которой фото жителя
// перекодируется в 300.
func TestАватарИзФотографииУменьшается(t *testing.T) {
	st := profileStore()
	st.profile.ID = testProfileID
	auth, token := signedInAs(t, platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember})
	grantConsents(t, auth, testProfileID)
	doc := currentDoc(t, platform.ConsentProfile)
	if err := auth.GrantConsent(t.Context(), testProfileID, doc.Kind, doc.Version, ""); err != nil {
		t.Fatal(err)
	}
	wr := &fakeWriter{photoBytes: []byte("снимок")}
	srv := newServerFor(t, st, auth, wr, newFakeMod(), nil, Config{})
	shots := newShots()
	srv.SetShots(shots)

	do(srv.routes(), postAs(t, "/me/avatar/photo", url.Values{"position": {"1"}}, token))
	if shots.side != avatarSide {
		t.Errorf("перекодировали до %d точек, а аватару положено %d", shots.side, avatarSide)
	}
	if string(shots.seen) != "снимок" {
		t.Errorf("в перекодировщик ушло %q, а не байты фотографии", shots.seen)
	}
}

// Фотографию убрали или скрыли, пока страница висела открытой: это не поломка,
// а разошедшийся с жизнью экран, и человеку говорят словами.
func TestАватарИзПропавшейФотографии(t *testing.T) {
	h, _, wr, _, token := aboutServer(t, true)
	wr.photoBytes = nil // ядро отвечает ErrNoPhoto

	w := do(h, postAs(t, "/me/avatar/photo", url.Values{"position": {"3"}}, token))
	if w.Code >= 500 {
		t.Fatalf("код %d: пропавшая фотография это не поломка площадки", w.Code)
	}
	if !strings.Contains(w.Body.String(), "больше нет") {
		t.Error("страница не объясняет, почему аватар не поставился")
	}
}

// У СКРЫТОЙ модератором кнопки нет вовсе: ядро её в аватар не отдаёт, а кнопка,
// отвечающая отказом, хуже отсутствующей.
func TestСкрытуюФотографиюВАватарНеПредлагают(t *testing.T) {
	st := profileStore()
	st.profile.ID = testProfileID
	st.photos = []platform.Photo{
		{ID: 1, Position: 1, URL: "/media/a.webp", Status: platform.StatusHiddenMod},
	}
	auth, token := signedInAs(t, platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember})
	grantConsents(t, auth, testProfileID)
	doc := currentDoc(t, platform.ConsentProfile)
	if err := auth.GrantConsent(t.Context(), testProfileID, doc.Kind, doc.Version, ""); err != nil {
		t.Fatal(err)
	}
	srv := newServerFor(t, st, auth, &fakeWriter{}, newFakeMod(), nil, Config{})
	srv.SetShots(newShots())
	body := do(srv.routes(), as(guest(t, "GET", "/me"), token)).Body.String()

	if strings.Contains(body, "/me/avatar/photo") {
		t.Error("скрытую модератором фотографию предлагают поставить лицом в ленту")
	}
	if !strings.Contains(body, "Убрать") {
		t.Error("«Убрать» у скрытой фотографии пропало — убирать её человек вправе")
	}
}

// Сторона, до которой снимок всё равно уменьшат, ПЕЧАТАЕТСЯ В РАЗМЕТКУ: по ней
// браузер уменьшает фотографию ДО отправки (assets/app.js), и без неё скрипт
// молча ничего не делает — а «молча ничего» тут значит три минуты закачки по
// каналу в 38 КБ/с.
//
// Тест на ПУТИ ДАННЫХ, а не на формуле: число в Go и число в разметке — разные
// места, и разъезжались такие пары в этом проекте не раз.
func TestСторонаСнимкаЕдетВРазметку(t *testing.T) {
	h, _, _, _, token := aboutServer(t, true)
	body := do(h, as(guest(t, "GET", "/me"), token)).Body.String()

	if !strings.Contains(body, "data-shrink") {
		t.Error("форма фотографии не помечена для уменьшения в браузере")
	}
	want := `data-maxside="` + strconv.Itoa(photoSide) + `"`
	if !strings.Contains(body, want) {
		t.Errorf("в разметке нет %s — скрипту неоткуда узнать сторону", want)
	}
}
