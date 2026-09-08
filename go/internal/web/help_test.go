package web

// Справка и правила.
//
// Тут проверяется не текст (он живёт и меняется в шаблонах), а свойства, без
// которых страницы бессмысленны: каждая тема открыта не вошедшему, оглавление
// ведёт на все темы и никуда больше, реквизиты оператора те же, что в
// согласиях, а числа взяты из ядра, а не написаны словами.
//
// Последнее — не педантизм: до 08.09.2026 в справке стояло «30 комментариев в
// час» при настоящих девяноста, и разъехались они молча.

import (
	"net/http"
	"strings"
	"testing"

	"lovegw/internal/platform"
)

func helpBody(t *testing.T, path string, cfg Config) string {
	t.Helper()
	h := newTestServer(t, &fakeStore{}, cfg)
	w := do(h, guest(t, "GET", path))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: код %d, ожидался 200", path, w.Code)
	}
	return w.Body.String()
}

// withMessengers — площадка, у которой мессенджеры настроены. Без них тема
// «Telegram и MAX» не показывается вовсе, и половина проверок ниже искала бы
// страницу, которой в этой сборке нет.
func withMessengers() Config {
	return Config{Contacts: Contacts{
		Telegram: "https://t.me/group", MAX: "https://max.ru/group",
		BotTelegram: "https://t.me/bot", BotMAX: "https://max.ru/bot",
	}}
}

// Правила, которые видно только изнутри, — это не правила, а сюрприз. Открыта
// каждая тема, а не одна лишь первая страница.
func TestHelpIsOpenToGuests(t *testing.T) {
	cfg := withMessengers()
	h := newTestServer(t, &fakeStore{}, cfg)
	for _, topic := range helpTopics {
		w := do(h, guest(t, "GET", "/help/"+topic.Slug))
		if w.Code != http.StatusOK {
			t.Errorf("/help/%s: код %d, ожидался 200", topic.Slug, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), topic.Title) {
			t.Errorf("/help/%s: на странице нет её собственного заголовка %q", topic.Slug, topic.Title)
		}
	}
}

// Оглавление ведёт на все темы: страница, до которой не дойти нажатием, для
// читателя не существует.
func TestHelpIndexLinksEveryTopic(t *testing.T) {
	body := helpBody(t, "/help", withMessengers())
	for _, topic := range helpTopics {
		if !strings.Contains(body, `href="/help/`+topic.Slug+`"`) {
			t.Errorf("оглавление не ведёт на /help/%s", topic.Slug)
		}
		if !strings.Contains(body, topic.Lead) {
			t.Errorf("у темы %q нет строки, чем она отличается от соседней", topic.Slug)
		}
	}
}

// Тему видно с любой её соседки: возвращаться в оглавление ради перехода в
// соседний раздел человек не должен.
func TestHelpTopicLinksItsNeighbours(t *testing.T) {
	body := helpBody(t, "/help/mod", withMessengers())
	if !strings.Contains(body, `href="/help/rules"`) {
		t.Error("со страницы темы не видно соседних разделов")
	}
	if strings.Contains(body, `href="/help/mod"`) {
		t.Error("тема ссылается сама на себя: это кнопка, ничего не делающая")
	}
	if !strings.Contains(body, `href="/help"`) {
		t.Error("со страницы темы нет дороги в оглавление")
	}
}

// Выдуманный адрес отвечает «нет такой страницы», а не пустой справкой.
func TestHelpUnknownTopicIs404(t *testing.T) {
	h := newTestServer(t, &fakeStore{}, withMessengers())
	if w := do(h, guest(t, "GET", "/help/kakoy-to-razdel")); w.Code != http.StatusNotFound {
		t.Errorf("код %d, ожидался 404", w.Code)
	}
}

// Ссылка на справку стоит в шапке, то есть доступна с любой страницы: искать
// правила по памяти адреса человек не должен.
func TestHelpIsLinkedFromEveryPage(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := openServer(t, st)
	for _, target := range []string{"/", "/n/312811", "/login"} {
		if body := do(h, guest(t, "GET", target)).Body.String(); !strings.Contains(body, `href="/help"`) {
			t.Errorf("%s: нет ссылки на справку", target)
		}
	}
}

// /help/narod назван в форме ответа под песочницей, на странице жителя и в
// мордоленте. Слуг темы — часть этого обещания: ссылка обязана открывать
// страницу, а не 404.
func TestHelpNarodTopicExists(t *testing.T) {
	body := helpBody(t, "/help/narod", Config{})
	if !strings.Contains(body, "машина") {
		t.Error("раздел о жителях не говорит, что реплики пишет машина")
	}
}

// Реквизиты оператора — те же, что подставлены в тексты согласий. Разойдясь,
// справка стала бы вторым источником правды о том, кто обрабатывает данные.
func TestHelpShowsTheSameOperatorAsConsents(t *testing.T) {
	op := platform.Operator{Name: "ИП Иванов И. И.", Contact: "help@t3h.ru"}
	for _, path := range []string{"/help", "/help/data"} {
		body := helpBody(t, path, Config{Operator: op})
		if !strings.Contains(body, op.Name) || !strings.Contains(body, op.Contact) {
			t.Errorf("%s: нет реквизитов оператора из конфига", path)
		}
	}

	// Пустые реквизиты дают ту же безличную подстановку, что и в согласиях, а
	// не пустое место: на пилоте это правда, и врать ею не нужно.
	if blank := helpBody(t, "/help", Config{}); !strings.Contains(blank, platform.Operator{}.Public().Name) {
		t.Error("без реквизитов справка молчит об операторе")
	}
}

// Числа в справке приезжают из ядра. Справка, разошедшаяся с поведением кнопки,
// хуже отсутствующей: человек поверит написанному и решит, что площадка
// сломана. Спрашивается каждое там, где оно стоит.
func TestHelpNumbersComeFromTheCore(t *testing.T) {
	for _, c := range []struct{ path, want, what string }{
		{"/help/write", "10 минут", "окно правки (platform.EditWindow)"},
		{"/help/origin", "10 минут", "окно правки на странице про значки"},
		{"/help/read", "больше 5", "потолок закреплённых (platform.MaxPinned)"},
		{"/help/write", "5 минут", "окно частоты заметок (platform.NoteWindow)"},
		{"/help/write", "5 в сутки", "суточный потолок заметок (platform.NotesPerDay)"},
		{"/help/write", "10 секунд", "окно частоты реплик (platform.CommentWindow)"},
		{"/help/write", "90 в час", "часовой потолок реплик (platform.CommentsPerHour)"},
	} {
		if !strings.Contains(helpBody(t, c.path, Config{}), c.want) {
			t.Errorf("%s: %s не совпадает с ядром (нет %q)", c.path, c.what, c.want)
		}
	}
}

// Контакты показываются, только если их настроили, и пустое поле пропускается
// поштучно. Заголовок над пустотой — тот же дефект, что «ЧТО ТЕБЯ ЦЕПЛЯЕТ» без
// единой темы в брифе жителя: раздел есть, сказать ему нечего.
func TestКонтактыПоказываютсяТолькоНастроенные(t *testing.T) {
	if body := helpBody(t, "/help", Config{}); strings.Contains(body, "Как связаться") {
		t.Error("ненастроенные контакты дали пустой раздел")
	}

	body := helpBody(t, "/help", Config{Contacts: Contacts{
		ProfileID: 1493279,
		Telegram:  "https://t.me/zazerkalje",
	}})
	if !strings.Contains(body, "Как связаться") {
		t.Fatal("настроенные контакты не показаны")
	}
	if !strings.Contains(body, `href="/u/1493279"`) {
		t.Error("нет ссылки на страницу владельца ЗДЕСЬ")
	}
	if strings.Contains(body, "love.ngs.ru/profile") {
		t.Error("контакты увели на НГС: ссылок туда площадка не ставит нигде")
	}
	if !strings.Contains(body, "https://t.me/zazerkalje") {
		t.Error("нет ссылки на группу в Telegram")
	}
	// MAX не задан — строки о нём быть не должно.
	if strings.Contains(body, "Группа в MAX") {
		t.Error("показана группа MAX, которой в настройках нет")
	}
}

// Про мессенджер площадка рассказывает, только если знает его адрес: «нажмите
// „Обсудить"» там, где этого мессенджера нет, отправляет человека в никуда.
func TestHelpMessengersFollowTheSettings(t *testing.T) {
	// Не настроено ничего — темы нет ни в оглавлении, ни по своему адресу.
	h := newTestServer(t, &fakeStore{}, Config{})
	if body := do(h, guest(t, "GET", "/help")).Body.String(); strings.Contains(body, "/help/messengers") {
		t.Error("оглавление зовёт в мессенджеры, которых у площадки нет")
	}
	if w := do(h, guest(t, "GET", "/help/messengers")); w.Code != http.StatusNotFound {
		t.Errorf("тема без единого адреса ответила %d, ожидался 404", w.Code)
	}

	// Настроен один — про него и рассказываем.
	body := helpBody(t, "/help/messengers", Config{Contacts: Contacts{BotTelegram: "https://t.me/bot"}})
	if !strings.Contains(body, "https://t.me/bot") {
		t.Error("нет адреса настроенного бота")
	}
	if strings.Contains(body, "<h2>MAX</h2>") {
		t.Error("рассказали про MAX, которого у площадки нет")
	}
}
