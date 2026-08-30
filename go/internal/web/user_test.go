package web

// Страница участника: кому видна, что на ней стоит и кто вправе решать.

import (
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

const profUserID = platform.NativeIDBase + 77

func profileStore() *fakeStore {
	st := noteStore()
	st.profile = platform.Profile{
		ID: profUserID, Nick: "Механик Сева", Gender: platform.GenderMale,
		Kind: platform.KindMember, CreatedAt: time.Now().Add(-72 * time.Hour),
		Notes: 2, Comments: 41,
	}
	st.pubNotes = []platform.PubNote{{ID: 312811, At: time.Now(), Exact: true, Excerpt: "про третье свидание"}}
	st.pubComs = []platform.PubComment{{
		ID: 63238879, NoteID: 312811, At: time.Now(),
		Excerpt: "а я бы не поехал", Note: "про третье свидание",
	}}
	return st
}

func profileServer(t *testing.T, st *fakeStore, role platform.Role) (http.Handler, *fakeMod, string) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: role,
	})
	mod := newFakeMod()
	return newFullServer(t, st, auth, &fakeWriter{}, mod, nil, Config{}), mod, token
}

func profilePath() string { return "/u/" + itoa64(profUserID) }

// Страницы участников закрыты от гостя и от поисковика (решение владельца
// 30.08.2026): заметки и реплики открыты поиску по отдельности, а страница
// собирает их в одно место. Гостю — вход, а не «нет такой страницы»: страница
// есть, и войдя, он её увидит.
func TestСтраницаУчастникаТолькоВошедшим(t *testing.T) {
	h, _, token := profileServer(t, profileStore(), platform.RoleUser)

	w := do(h, guest(t, "GET", profilePath()))
	if got := w.Header().Get("Location"); got != "/login" {
		t.Errorf("гостя ведёт на %q, ожидался /login", got)
	}
	if w := do(h, as(guest(t, "GET", profilePath()), token)); w.Code != http.StatusOK {
		t.Fatalf("вошедшему код %d", w.Code)
	}
	if body := do(h, guest(t, "GET", "/robots.txt")).Body.String(); !strings.Contains(body, "Disallow: /u") {
		t.Errorf("в robots.txt нет /u:\n%s", body)
	}
	if got := do(h, as(guest(t, "GET", profilePath()), token)).Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("страница участника открыта роботам: %q", got)
	}
}

// Карточка и то, что человек написал: счётчики и обе ленты со ссылками на
// разговор, а не на голый номер.
func TestСтраницаУчастникаПоказываетНаписанное(t *testing.T) {
	h, _, token := profileServer(t, profileStore(), platform.RoleUser)
	body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()

	for _, want := range []string{
		"Механик Сева",
		"Заметок 2",
		"реплик 41",
		`href="/n/312811"`,
		"про третье свидание",
		`href="/n/312811#c63238879"`,
		"а я бы не поехал",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
}

// Житель называет себя вслух: читатель вправе знать, что реплики этого автора
// пишет машина. Биография рядом — она и есть то, ради чего страница заводилась.
func TestСтраницаЖителяНазываетСебя(t *testing.T) {
	st := profileStore()
	st.profile.Persona = true
	st.profile.Bio = "Слесарь, гараж в Первомайке, двое взрослых детей."
	h, _, token := profileServer(t, st, platform.RoleUser)

	body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()
	if !strings.Contains(body, "житель площадки") || !strings.Contains(body, "пишет машина") {
		t.Errorf("житель не назвал себя:\n%s", tailOf(body))
	}
	if !strings.Contains(body, "гараж в Первомайке") {
		t.Error("биографии жителя нет на его странице")
	}
	if !strings.Contains(body, "/help#narod") {
		t.Error("нет ссылки на объяснение в справке")
	}
}

// А страница ЖИТЕЛЯ открыта гостю (просьба владельца 30.08.2026 вслед за
// мордолентой): довод, закрывший живых, здесь не работает вовсе — собирать в
// одно место нечего, персональных данных у персонажа нет, а биографию оператор
// и написал для показа. От поисковика она закрыта по-прежнему: profile
// выдуманного человека в выдаче наравне с живыми — не то, чего мы хотим.
func TestСтраницаЖителяОткрытаГостю(t *testing.T) {
	st := profileStore()
	st.profile.Persona = true
	h, _, _ := profileServer(t, st, platform.RoleUser)

	w := do(h, guest(t, "GET", profilePath()))
	if w.Code != http.StatusOK {
		t.Fatalf("гостю на странице жителя код %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "житель площадки") {
		t.Error("гостю не сказано, кто перед ним")
	}
	if got := w.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Errorf("страница жителя открыта роботам: %q", got)
	}
}

// «Такого нет» и «есть, но живой» отвечаются гостю ОДИНАКОВО. Разведи мы эти
// два ответа — и перебор номеров сказал бы постороннему, какие анкеты на
// площадке заведены; сегодня этого не знает никто, кроме вошедших.
func TestГостюНеВидноКакиеАнкетыЕсть(t *testing.T) {
	st := profileStore()
	st.profile.Persona = true
	h, _, _ := profileServer(t, st, platform.RoleUser)

	// Номер, которого в базе нет вовсе.
	if got := do(h, guest(t, "GET", "/u/"+itoa64(profUserID+999))).Header().Get("Location"); got != "/login" {
		t.Errorf("на несуществующей анкете гостя ведёт на %q, ожидался /login", got)
	}
	// Тот же номер, но анкета живого человека.
	st.profile.Persona = false
	if got := do(h, guest(t, "GET", profilePath())).Header().Get("Location"); got != "/login" {
		t.Errorf("на анкете живого человека гостя ведёт на %q, ожидался /login", got)
	}
}

// Имя ЖИТЕЛЯ — ссылка и у гостя, там же, где имя живого остаётся текстом:
// мордолента ведёт ровно на ту же страницу, и расходиться этим двум дорогам
// незачем.
func TestИмяЖителяСсылкаИУГостя(t *testing.T) {
	st := profileStore()
	st.notes[0].Author.Persona = true
	h, _, _ := profileServer(t, st, platform.RoleUser)

	want := `href="/u/` + itoa64(st.notes[0].Author.ID) + `"`
	if body := do(h, guest(t, "GET", "/")).Body.String(); !strings.Contains(body, want) {
		t.Errorf("гостю имя жителя показано текстом, нет %q:\n%s", want, tailOf(body))
	}
	// А в треде — то же самое: правило про АВТОРА, а не про место показа.
	st.thread[0].Author.Persona = true
	if body := do(h, guest(t, "GET", "/n/312811")).Body.String(); !strings.Contains(body, `href="/u/`) {
		t.Errorf("в треде имя жителя не ссылка:\n%s", tailOf(body))
	}
}

// Запрет писать стоит на СТРАНИЦЕ ЧЕЛОВЕКА — там, где на него и смотрят.
// Роли раздаёт только администратор: право скрывать чужие слова не должно
// размножаться само.
func TestРешенияНаСтраницеУчастника(t *testing.T) {
	for _, c := range []struct {
		name       string
		role       platform.Role
		ban, roles bool
	}{
		{"участник", platform.RoleUser, false, false},
		{"модератор", platform.RoleModerator, true, false},
		{"администратор", platform.RoleAdmin, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, token := profileServer(t, profileStore(), c.role)
			body := do(h, as(guest(t, "GET", profilePath()), token)).Body.String()
			if got := strings.Contains(body, `value="ban"`); got != c.ban {
				t.Errorf("форма запрета: %v, ожидалось %v", got, c.ban)
			}
			if got := strings.Contains(body, `value="role"`); got != c.roles {
				t.Errorf("форма ролей: %v, ожидалось %v", got, c.roles)
			}
		})
	}
}

// Себе запрет не предлагают: кнопка, которой никто никогда не нажмёт осознанно,
// на странице только мешает.
func TestСебеЗапретНеПредлагают(t *testing.T) {
	st := profileStore()
	st.profile.ID = testProfileID
	h, _, token := profileServer(t, st, platform.RoleAdmin)

	body := do(h, as(guest(t, "GET", "/u/"+itoa64(testProfileID)), token)).Body.String()
	if strings.Contains(body, `value="ban"`) {
		t.Error("администратору предложили забанить самого себя")
	}
	if !strings.Contains(body, `href="/me"`) {
		t.Error("на своей странице нет дороги к «Моей странице»")
	}
}

// Карточки под /mod больше нет: она стала переходом на страницу человека.
// Постороннему двери по-прежнему не существует — существование закрытой двери
// само по себе сведения.
func TestКарточкаМодератораВедётНаСтраницуУчастника(t *testing.T) {
	h, _, token := profileServer(t, profileStore(), platform.RoleModerator)
	w := do(h, as(guest(t, "GET", "/mod/u/"+itoa64(profUserID)), token))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != profilePath() {
		t.Errorf("модератора увело на %q (код %d)", w.Header().Get("Location"), w.Code)
	}

	h2, _, token2 := profileServer(t, profileStore(), platform.RoleUser)
	if w := do(h2, as(guest(t, "GET", "/mod/u/"+itoa64(profUserID)), token2)); w.Code != http.StatusNotFound {
		t.Errorf("участнику на карточке модератора код %d, ожидался 404", w.Code)
	}
}

// Имя в ленте ведёт на страницу человека — у вошедшего. У гостя оно остаётся
// текстом: ссылка, ведущая к отказу, хуже её отсутствия.
func TestИмяАвтораВедётНаЕгоСтраницу(t *testing.T) {
	st := profileStore()
	h, _, token := profileServer(t, st, platform.RoleUser)

	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	want := `href="/u/` + itoa64(st.notes[0].Author.ID) + `"`
	if !strings.Contains(body, want) {
		t.Errorf("в ленте нет %q:\n%s", want, tailOf(body))
	}
	if guestBody := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(guestBody, `href="/u/`) {
		t.Error("гостю имя автора показано ссылкой")
	}
}

// У анонимной заметки автора нет вовсе, и имя ссылкой не становится: маскировку
// стоило бы сломать ровно одним таким переходом.
func TestАнонимНеСтановитсяСсылкой(t *testing.T) {
	st := profileStore()
	st.notes[0].Anonymous = true
	h, _, token := profileServer(t, st, platform.RoleUser)

	body := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	if strings.Contains(body, `href="/u/`) {
		t.Errorf("имя анонима стало ссылкой:\n%s", tailOf(body))
	}
}

// Дописанная живым добором строка обязана выглядеть ТАК ЖЕ, как нарисованная
// обновлением, — включая имя-ссылку. Второе место, где страница превращается в
// разметку, у площадки не заводится, но контекст ей приходится собирать заново,
// и забытое поле видно только здесь.
func TestДоборСохраняетИмяСсылкой(t *testing.T) {
	st := profileStore()
	st.fresh = st.thread
	h, _, token := profileServer(t, st, platform.RoleUser)

	body := do(h, as(guest(t, "GET", "/n/312811/fresh?after=0,0"), token)).Body.String()
	if !strings.Contains(body, `href="/u/`) {
		t.Errorf("в доборе имя автора не ссылка:\n%s", tailOf(body))
	}
	if guestBody := do(h, guest(t, "GET", "/n/312811/fresh?after=0,0")).Body.String(); strings.Contains(guestBody, `href="/u/`) {
		t.Error("гостю в доборе имя показано ссылкой")
	}
}

// ------------------------------------------------------------- жители в панели

func adminWithPersonas(t *testing.T) (http.Handler, *fakeMod, *fakeShots, string) {
	t.Helper()
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: platform.RoleAdmin,
	})
	mod := newFakeMod()
	mod.personas = []platform.PersonaRow{{
		ID: profUserID, Nick: "Механик Сева", Gender: platform.GenderMale,
		Bio: "Слесарь, гараж в Первомайке.",
	}}
	conv := newShots()
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()}, profileStore(), auth, &fakeWriter{}, mod, nil)
	srv.SetShots(conv)
	t.Cleanup(func() { _ = srv.Close() })
	return srv.routes(), mod, conv, token
}

// Жители видны в панели администратора — там же, где роли и приглашения: это
// решения о том, КТО здесь говорит.
func TestЖителиВидныАдминистратору(t *testing.T) {
	h, _, _, token := adminWithPersonas(t)
	body := do(h, as(guest(t, "GET", "/mod/admin"), token)).Body.String()

	for _, want := range []string{"Жители", "Механик Сева", "гараж в Первомайке", `value="avatar"`} {
		if !strings.Contains(body, want) {
			t.Errorf("в панели нет %q", want)
		}
	}
}

// Фото доходит до ядра, и доходит перекодированным СВОИМ потолком: на странице
// это квадрат 100×100, а не колонка заметки.
func TestФотоЖителяДоходитДоЯдра(t *testing.T) {
	h, mod, conv, token := adminWithPersonas(t)

	r := uploadTo(t, "/mod/admin/avatar", token, "", []byte("байты"), func(mw *multipart.Writer) {
		_ = mw.WriteField("id", itoa64(profUserID))
		_ = mw.WriteField("do", "avatar")
		_ = mw.WriteField("reason", "лицо жителя")
	})
	if w := do(h, r); w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	if mod.avatarFor != profUserID || mod.avatarShot == nil {
		t.Fatalf("до ядра дошло %d, картинка %v", mod.avatarFor, mod.avatarShot)
	}
	if mod.avatarWhy != "лицо жителя" {
		t.Errorf("причина не дошла: %q", mod.avatarWhy)
	}
	if conv.side != avatarSide {
		t.Errorf("потолок стороны %d, ожидался %d", conv.side, avatarSide)
	}
}

// «Убрать фото» шлёт в ядро nil — снятие, а не пустую картинку.
func TestФотоЖителяСнимается(t *testing.T) {
	h, mod, _, token := adminWithPersonas(t)

	r := uploadTo(t, "/mod/admin/avatar", token, "", nil, func(mw *multipart.Writer) {
		_ = mw.WriteField("id", itoa64(profUserID))
		_ = mw.WriteField("do", "avatar_off")
	})
	if w := do(h, r); w.Code != http.StatusOK {
		t.Fatalf("код %d", w.Code)
	}
	if mod.avatarFor != profUserID || mod.avatarShot != nil {
		t.Errorf("до ядра дошло %d, картинка %v — ожидалось снятие", mod.avatarFor, mod.avatarShot)
	}
}

// Живому человеку фото так не ставят, и отказ объясняется словами: фото ему
// приносит его же анкета НГС, а не рука администратора.
func TestФотоЖивомуЧеловекуОтказ(t *testing.T) {
	h, mod, _, token := adminWithPersonas(t)
	mod.avatarErr = platform.ErrNotPersona

	r := uploadTo(t, "/mod/admin/avatar", token, "", []byte("байты"), func(mw *multipart.Writer) {
		_ = mw.WriteField("id", itoa64(profUserID))
		_ = mw.WriteField("do", "avatar")
	})
	w := do(h, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("код %d, ожидался честный отказ", w.Code)
	}
	if !strings.Contains(w.Body.String(), "живого человека") {
		t.Errorf("отказ не объяснён:\n%s", tailOf(w.Body.String()))
	}
}

// Модератору раздела не существует: жители — это про то, кто говорит, а он
// решает про слова.
func TestЖителиЗакрытыОтМодератора(t *testing.T) {
	h, _, token := profileServer(t, profileStore(), platform.RoleModerator)
	if w := do(h, as(guest(t, "GET", "/mod/admin"), token)); w.Code != http.StatusNotFound {
		t.Errorf("модератору в панели код %d, ожидался 404", w.Code)
	}
	form := url.Values{"id": {itoa64(profUserID)}, "do": {"avatar_off"}}
	if w := do(h, postAs(t, "/mod/admin/avatar", form, token)); w.Code != http.StatusNotFound {
		t.Errorf("модератор снял фото: код %d", w.Code)
	}
}
