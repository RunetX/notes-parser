package web

// Проверки страницы событий и колокольчика (эпик F).
//
// Хранилище поддельное — как и везде в этом пакете: правила адресации живут в
// SQL и проверяются против настоящего Postgres (platform/events_pg_test.go), а
// здесь речь о том, что видит человек.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// fakeEvents — шина, которой всё равно, кто спрашивает: тесты проверяют показ,
// а не адресацию.
type fakeEvents struct {
	list   []platform.NotificationView
	unread int
	// markedUpto — с какой границей пришла отметка прочитанного. Именно она и
	// проверяется: кнопка обязана гасить то, что человек ВИДЕЛ.
	markedUpto int64
	marks      int
}

func (f *fakeEvents) Notifications(_ context.Context, _ int64, offset, limit int) ([]platform.NotificationView, error) {
	if offset >= len(f.list) {
		return nil, nil
	}
	return f.list[offset:min(offset+limit, len(f.list))], nil
}

func (f *fakeEvents) CountNotifications(context.Context, int64) (int, error) { return len(f.list), nil }
func (f *fakeEvents) Unread(context.Context, int64) (int, error)             { return f.unread, nil }

func (f *fakeEvents) MarkRead(_ context.Context, _ int64, upto int64) error {
	f.markedUpto, f.marks = upto, f.marks+1
	return nil
}

// busServer — вошедший участник и подключённая шина.
func busServer(t *testing.T, ev Events) (http.Handler, string) {
	t.Helper()
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{BaseURL: "http://127.0.0.1", Log: quietLog()},
		&fakeStore{}, auth, nil, nil, nil)
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetEvents(ev)
	return srv.routes(), token
}

func sampleNotices() []platform.NotificationView {
	at := time.Date(2026, 8, 22, 19, 30, 0, 0, time.UTC)
	return []platform.NotificationView{
		{EventID: 40, Kind: platform.EventComment, Reason: platform.ReasonReplyToComment,
			At: at, ActorNick: "Мавр", NoteID: 312811, CommentID: 77, Excerpt: "да я не о том"},
		{EventID: 39, Kind: platform.EventReaction, Reason: platform.ReasonReaction,
			At: at, NoteID: 312811, CommentID: 70, Code: "popcorn", Count: 3},
		{EventID: 38, Kind: platform.EventHidden, Reason: platform.ReasonAboutYou,
			At: at, NoteID: 312811, CommentID: 65, Read: true, Hidden: true,
			Detail: "личные данные третьих лиц"},
	}
}

// Гостю событий не показывают: это его собственные поводы, а собственных у него
// нет. Отправляем на вход, а не отказываем, — приглашение уместнее преграды.
func TestEventsSendGuestToLogin(t *testing.T) {
	h, _ := busServer(t, &fakeEvents{})
	w := do(h, guest(t, "GET", "/events"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("гостю ответили %d, Location %q", w.Code, w.Header().Get("Location"))
	}
}

// Без шины страницы нет вовсе — ровно как /mod без модерации. Обещать страницу,
// которой у этого человека не будет, незачем.
func TestEventsAbsentWithoutBus(t *testing.T) {
	auth := newFakeAuth()
	auth.users[testProfileID] = platform.User{ID: testProfileID, Nick: testNick, Kind: platform.KindMember}
	token, _, err := auth.CreateSession(context.Background(), testProfileID, "")
	if err != nil {
		t.Fatal(err)
	}
	h := newFullServer(t, &fakeStore{}, auth, nil, nil, nil, Config{})
	if w := do(h, as(guest(t, "GET", "/events"), token)); w.Code != http.StatusNotFound {
		t.Fatalf("без шины страница событий ответила %d, ожидалось 404", w.Code)
	}
	// И колокольчика в шапке тоже нет: значок, ведущий в никуда, хуже его
	// отсутствия.
	if body := do(h, as(guest(t, "GET", "/"), token)).Body.String(); strings.Contains(body, `class="bell`) {
		t.Error("колокольчик нарисован без шины")
	}
}

func TestEventsListShowsReasons(t *testing.T) {
	h, token := busServer(t, &fakeEvents{list: sampleNotices(), unread: 2})
	body := do(h, as(guest(t, "GET", "/events"), token)).Body.String()

	for _, want := range []string{
		"Ответ на вашу реплику", "Вашу запись отметили", "Ваша публикация скрыта",
		"/n/312811#c77", // ссылка ведёт в место треда
		"да я не о том", // выдержка из открытого текста
		"личные данные третьих лиц", // причина модератора — она же и есть объяснение
	} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице событий нет %q:\n%s", want, body)
		}
	}
	// У скрытой реплики якоря нет: страница до неё не долистает, потому что её
	// там больше нет.
	if strings.Contains(body, "#c65") {
		t.Error("ссылка ведёт на скрытую реплику")
	}
}

// Правило Ш5г буквально: кто нажал реакцию, не показывается никому и нигде.
// Стережёт это тест, а не память, — поле, которого нет в ядре, легко завести
// заново на показе.
func TestReactionNoticeHasNoName(t *testing.T) {
	notices := sampleNotices()
	h, token := busServer(t, &fakeEvents{list: notices[1:2]})
	body := do(h, as(guest(t, "GET", "/events"), token)).Body.String()

	if !strings.Contains(body, "Вашу запись отметили") {
		t.Fatalf("повод о реакции не показан:\n%s", body)
	}
	if strings.Contains(body, "Мавр") {
		t.Error("в поводе о реакции названо имя")
	}
	if !strings.Contains(body, "×3") {
		t.Errorf("число реакций не показано:\n%s", body)
	}
}

// Колокольчик показывает число, а выше потолка — «99+»: точное число там уже не
// считалось (platform.UnreadCap).
func TestBellShowsCount(t *testing.T) {
	h, token := busServer(t, &fakeEvents{unread: 4})
	if body := do(h, as(guest(t, "GET", "/"), token)).Body.String(); !strings.Contains(body, ">4</span>") {
		t.Errorf("счётчик не нарисован:\n%s", body)
	}

	h, token = busServer(t, &fakeEvents{unread: platform.UnreadCap})
	if body := do(h, as(guest(t, "GET", "/"), token)).Body.String(); !strings.Contains(body, "99+") {
		t.Errorf("потолок счётчика не показан как 99+:\n%s", body)
	}
}

// Отметка прочитанного гасит то, что человек ВИДЕЛ: границей служит самый свежий
// повод показанной страницы, а не «всё вообще». Иначе кнопка на пятой странице
// гасила бы непрочитанное, до которого он не дошёл.
func TestMarkReadUsesPageTop(t *testing.T) {
	ev := &fakeEvents{list: sampleNotices(), unread: 2}
	h, token := busServer(t, ev)

	body := do(h, as(guest(t, "GET", "/events"), token)).Body.String()
	if !strings.Contains(body, `name="upto" value="40"`) {
		t.Fatalf("граница отметки не проставлена:\n%s", body)
	}
	w := do(h, postAs(t, "/events/read", url.Values{"upto": {"40"}, "back": {"/events?page=2"}}, token))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("отметка ответила %d", w.Code)
	}
	// Возврат — туда, откуда нажали, вместе с номером страницы: человек, отметив
	// прочитанное на второй странице, не должен оказаться в начале списка.
	if got := w.Header().Get("Location"); got != "/events?page=2" {
		t.Errorf("возврат после отметки: %q", got)
	}
	if ev.marks != 1 || ev.markedUpto != 40 {
		t.Errorf("отметок %d, граница %d — ожидалась одна до 40", ev.marks, ev.markedUpto)
	}
}

// Форма отметки — это запись, значит скрытое поле обязательно: без него площадка
// принимала бы её с чужой страницы.
func TestMarkReadNeedsCSRF(t *testing.T) {
	ev := &fakeEvents{list: sampleNotices()}
	h, token := busServer(t, ev)

	w := do(h, as(post(t, "/events/read", url.Values{"upto": {"40"}}), token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("форма без токена принята с кодом %d", w.Code)
	}
	if ev.marks != 0 {
		t.Error("отметка выполнена без токена")
	}
}

// Пустой список — не ошибка и не пустая страница: человеку говорят, что здесь
// вообще бывает, и отсылают в справку. Своя метка обязана объясняться (Ш5з).
func TestEmptyEventsExplainThemselves(t *testing.T) {
	h, token := busServer(t, &fakeEvents{})
	body := do(h, as(guest(t, "GET", "/events"), token)).Body.String()
	if !strings.Contains(body, "Пока ничего не происходило") {
		t.Fatalf("пустая страница событий не объяснилась:\n%s", body)
	}
	if strings.Contains(body, "Отметить прочитанным") {
		t.Error("кнопка отметки показана, когда отмечать нечего")
	}
}
