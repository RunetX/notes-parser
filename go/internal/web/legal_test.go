package web

// Бумаги площадки: соглашения, политика конфиденциальности, отказ от
// ответственности — и переехавший к ним в соседи переключатель тем.
//
// Проверяется не текст (он живёт в файлах и правится человеком), а четыре
// свойства, без которых страницы бессмысленны: они открыты гостю, ссылка на них
// стоит на каждой странице, реквизиты оператора на них те же, что в согласиях,
// и соглашения показываются НАСТОЯЩИЕ — те самые, что подписывают при входе.

import (
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

var legalPages = []struct{ path, title string }{
	{"/consents", "Соглашения"},
	{"/privacy", "Политика конфиденциальности"},
	{"/disclaimer", "Отказ от ответственности"},
}

// Политику обработки оператор обязан опубликовать так, чтобы её прочёл ЛЮБОЙ
// (ч. 2 ст. 18.1): за входом ей не место. Отказ от ответственности — тому же
// гостю: он читает чужие тексты и вправе знать, чьи они, ДО того, как на них
// наткнётся.
func TestLegalPagesAreOpenToGuests(t *testing.T) {
	h := openServer(t, &fakeStore{})
	for _, p := range legalPages {
		w := do(h, guest(t, "GET", p.path))
		if w.Code != http.StatusOK {
			t.Errorf("%s: код %d, ожидался 200", p.path, w.Code)
			continue
		}
		if body := w.Body.String(); !strings.Contains(body, p.title) {
			t.Errorf("%s: на странице нет заголовка %q", p.path, p.title)
		}
	}
}

// Ссылка стоит в подвале, то есть на каждой странице: бумаги ищут внизу по
// привычке всего остального интернета, а политика, видная только из профиля,
// доступна ровно тем, кто уже согласился.
func TestLegalPagesAreLinkedFromEveryPage(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := openServer(t, st)
	for _, target := range []string{"/", "/n/312811", "/login"} {
		body := do(h, guest(t, "GET", target)).Body.String()
		for _, p := range legalPages {
			if !strings.Contains(body, `href="`+p.path+`"`) {
				t.Errorf("%s: в подвале нет ссылки на %s", target, p.path)
			}
		}
	}
}

// Соглашения на странице — те же самые, что показывает вход: страница берёт их
// у ядра, а не хранит свою копию. Копия документа, живущая своей жизнью, — это
// ровно тот случай, когда через год расходятся оригинал и то, что видят люди.
func TestConsentsPageShowsTheRealDocuments(t *testing.T) {
	op := platform.Operator{Name: "ИП Иванов И. И.", Contact: "help@t3h.ru"}
	h := newTestServer(t, &fakeStore{}, Config{Operator: op})
	body := do(h, guest(t, "GET", "/consents")).Body.String()

	docs, err := platform.CurrentConsentDocs(op)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("согласий %d, а их два", len(docs))
	}
	for _, d := range docs {
		if !strings.Contains(body, d.Title) {
			t.Errorf("на странице нет документа %q", d.Title)
		}
	}
	if !strings.Contains(body, op.Contact) {
		t.Error("реквизиты оператора в документ не подставлены")
	}
}

// Реквизиты — из конфига, а не написаны в тексте словами: разойдясь, страница
// стала бы вторым источником правды о том, кто обрабатывает данные. Пустой
// конфиг даёт ту же безличную подстановку, что и в согласиях, — на пилоте это
// правда, и врать ею не нужно.
func TestOwnDocsNameTheOperatorFromConfig(t *testing.T) {
	op := platform.Operator{Name: "ИП Иванов И. И.", Contact: "help@t3h.ru"}
	h := newTestServer(t, &fakeStore{}, Config{Operator: op})
	for _, path := range []string{"/privacy", "/disclaimer"} {
		body := do(h, guest(t, "GET", path)).Body.String()
		if !strings.Contains(body, op.Name) || !strings.Contains(body, op.Contact) {
			t.Errorf("%s: нет реквизитов оператора из конфига", path)
		}
	}

	bare := openServer(t, &fakeStore{})
	for _, path := range []string{"/privacy", "/disclaimer"} {
		body := do(bare, guest(t, "GET", path)).Body.String()
		if !strings.Contains(body, platform.Operator{}.Public().Name) {
			t.Errorf("%s: пустые реквизиты дали пустое место вместо безличной подстановки", path)
		}
	}
}

// Опечатка в имени поля обязана стать отказом, а не строкой «<no value>»
// посреди политики: документ, который врёт про оператора, хуже отсутствующего.
func TestOwnDocsFailLoudlyOnUnknownField(t *testing.T) {
	if _, _, err := ownDoc("нет-такого.txt", platform.Operator{}); err == nil {
		t.Error("несуществующий документ показался без ошибки")
	}
}

// Переключатель тем переехал из подвала в меню участника (решение владельца
// 23.08.2026): тема — настройка «про меня». Плата названа честно и стережётся
// здесь же: у гостя переключателя нет вовсе, до входа тему держит системная
// настройка браузера.
func TestThemeSwitcherLivesInTheAccountMenu(t *testing.T) {
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember,
	})
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, nil, Config{})

	member := do(h, as(guest(t, "GET", "/"), token)).Body.String()
	start := strings.Index(member, `<div class="acctmenu">`)
	if start < 0 {
		t.Fatal("у вошедшего нет меню участника")
	}
	menu, _, _ := strings.Cut(member[start:], "</header>")
	if !strings.Contains(menu, `action="/theme"`) {
		t.Error("переключателя тем нет в меню участника")
	}
	for _, th := range themes {
		if !strings.Contains(menu, `value="`+th.ID+`"`) {
			t.Errorf("в меню нет кнопки темы %q", th.Name)
		}
	}

	if guestBody := do(h, guest(t, "GET", "/")).Body.String(); strings.Contains(guestBody, `action="/theme"`) {
		t.Error("гостю показан переключатель тем: меню участника у него не рисуется вовсе")
	}
}
