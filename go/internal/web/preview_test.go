//go:build preview

package web

// Стенд «посмотреть глазами».
//
// Поднять морду как команду нельзя: `lovegw web` требует живого Postgres и
// совпадения версии схемы, а на рабочей машине её нет. Заводить ради этого
// второй путь сборки морды (`--fake`) тем более нельзя — он однажды разойдётся
// с настоящим, и разойдётся молча. Зато настоящий routes() уже собирается в
// тестах поверх тест-дублей, и этого довольно: отдаются и страницы, и стили, и
// шрифты, то есть тему видно целиком.
//
// Метка сборки, а не обычный тест: висящий сервер в общем прогоне — это
// прогон, который никогда не кончается.
//
//	cd go && go test ./internal/web -tags preview -run Предпросмотр -timeout 0 -v
//
// Правило: всё, что здесь показано, — тест-дубли. Ни одной боевой строки, ни
// одного адреса боевой базы.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// previewAddr — порт постоянный намеренно: адрес со случайным номером
// приходится вычитывать из лога при каждом запуске, а смотрят сюда по многу
// раз подряд и с открытой вкладкой.
const previewAddr = "127.0.0.1:8791"

func TestПредпросмотр(t *testing.T) {
	st := profileStore()
	// Смотрим на СВОЮ страницу: тест-дубль отдаёт карточку только по точному
	// номеру, а «Рассказ о себе» читает карточку вошедшего.
	st.profile.ID = testProfileID
	st.profile.Comments = 412
	st.profile.Bio = "Слесарь-ремонтник на заводе, руки в мазуте, гараж вместо кабинета."
	st.profile.City = "Новосибирск"
	st.profile.Job = "слесарь-ремонтник"
	// Снимки — силуэты из своей же статики: боевых фотографий в стенде быть не
	// должно, а картинка нужна только чтобы увидеть сетку альбома.
	//
	// Их ДВА, а не три: при полном альбоме формы «Добавить фотографию» на
	// экране нет вовсе (кнопка, которая заведомо откажет, хуже отсутствующей),
	// и посмотреть на главное — как человек кладёт снимок — было бы негде.
	st.photos = []platform.Photo{
		{ID: 1, Position: 1, URL: assetURL("profile/male300px.png")},
		{ID: 2, Position: 2, URL: assetURL("profile/female300px.png")},
	}
	auth, token := signedInAs(t, platform.User{
		ID: testProfileID, Nick: testNick, Kind: platform.KindMember, Role: platform.RoleAdmin,
	})
	// Необязательные согласия тоже подписаны: иначе половина экранов стенда
	// показывает документ вместо того, ради чего на них смотрят.
	for _, kind := range []string{platform.ConsentProfile, platform.ConsentBinding} {
		doc, err := platform.ConsentDocOf(platform.Operator{}, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.GrantConsent(t.Context(), testProfileID, doc.Kind, doc.Version, ""); err != nil {
			t.Fatal(err)
		}
	}
	stand := newServerFor(t, st, auth, &fakeWriter{}, newFakeMod(), nil, Config{})
	stand.SetShots(newShots())
	// Переписка — отдельно: её в дизайн-хендофф не включали вовсе, и посмотреть
	// на неё под новой палитрой надо именно поэтому.
	mail := newFakeMail()
	if _, err := mail.SendMessage(t.Context(), peerID, testProfileID, "Здравствуйте! Видела вашу заметку про гараж."); err != nil {
		t.Fatal(err)
	}
	if _, err := mail.SendMessage(t.Context(), testProfileID, peerID, "И вам не хворать."); err != nil {
		t.Fatal(err)
	}
	stand.SetMail(mail)
	h := stand.routes()

	// Вошедшим считается всякий, кто не попросил обратного: кука сессии
	// HttpOnly, скриптом её не поставить, а вводить руками в отладчике при
	// каждом заходе — это и есть та возня, из-за которой глазами смотрят реже,
	// чем надо. Гостя показывает ?guest=1 — вид у него другой, и посмотреть на
	// него тоже надо.
	signed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.Query().Has("guest") {
			r.AddCookie(&http.Cookie{Name: sessCookie, Value: token})
		}
		// ?theme=grimoire — посмотреть другую палитру, не нажимая кнопку:
		// снимок экрана делается одним запуском браузера, и нажать в нём нечем.
		if th := r.URL.Query().Get("theme"); th != "" {
			r.AddCookie(&http.Cookie{Name: themeCookie, Value: th})
		}
		h.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", previewAddr)
	if err != nil {
		t.Fatalf("порт %s занят: %v", previewAddr, err)
	}
	srv := httptest.NewUnstartedServer(signed)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	t.Logf("вошедшим показывается всё; гость — добавить ?guest=1")
	for _, p := range []string{"/", "/n/312811", "/u/" + itoa64(testProfileID), "/me", "/me/about", "/mail", "/mail/1", "/help", "/help/read", "/login", "/new"} {
		t.Logf("%s%s", srv.URL, p)
	}
	t.Log("Ctrl+C, когда насмотритесь")
	for {
		time.Sleep(time.Hour)
	}
}
