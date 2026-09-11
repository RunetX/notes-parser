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

// withLinks — площадка, у которой настроено всё необязательное: и мессенджеры, и
// сбор пожертвований. Без первых нет темы «Telegram и MAX», без второго — темы
// «Поддержать площадку», и проверки ниже искали бы страницы, которых в такой
// сборке нет вовсе.
func withLinks() Config {
	return Config{
		Contacts: Contacts{
			Telegram: "https://t.me/group", MAX: "https://max.ru/group",
			BotTelegram: "https://t.me/bot", BotMAX: "https://max.ru/bot",
		},
		Support: Support{
			URL: "https://pay.example.org/p/test", InfraRub: 1200, ModelsRub: 800,
			AsOf: "сентябрь 2026", Payee: "Иван",
		},
	}
}

// Правила, которые видно только изнутри, — это не правила, а сюрприз. Открыта
// каждая тема, а не одна лишь первая страница.
func TestHelpIsOpenToGuests(t *testing.T) {
	cfg := withLinks()
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
	body := helpBody(t, "/help", withLinks())
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
	body := helpBody(t, "/help/mod", withLinks())
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
	h := newTestServer(t, &fakeStore{}, withLinks())
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

// ── Иллюстрации ───────────────────────────────────────────────────────────

// Значок в справке обязан быть ТЕМ САМЫМ, что стоит на странице, а не его
// портретом: справка, показавшая непохожий знак, учит искать несуществующее.
// Держится это общим шаблоном (parts/icons.gohtml), и вот проверка, что оба
// места берут его оттуда, — иначе первая же правка значка разведёт их молча.
func TestHelpDrawsTheSameIconsAsPages(t *testing.T) {
	st := &fakeStore{total: 1, notes: []platform.NoteView{sampleNote()}, note: sampleNote()}
	h := openServer(t, st)

	// Переключатель вида: страница заметки и раздел «Как читать».
	note := do(h, guest(t, "GET", "/n/312811")).Body.String()
	read := do(h, guest(t, "GET", "/help/read")).Body.String()
	for _, want := range []string{
		`<rect x="5" y="6.8" width="10" height="2.4" rx="1.2"/>`, // лесенка — дерево
		`<rect x="1" y="6.8" width="14" height="2.4" rx="1.2"/>`, // вровень — линейный
	} {
		if !strings.Contains(note, want) || !strings.Contains(read, want) {
			t.Errorf("значок переключателя разошёлся между страницей и справкой: %s", want)
		}
	}

	// Значок реплики: липкая шапка треда и та же страница справки.
	const speech = `<path d="M3 4.6A1.6 1.6 0 0 1 4.6 3h10.8A1.6 1.6`
	if !strings.Contains(note, speech) || !strings.Contains(read, speech) {
		t.Error("значок реплики в справке не тот, что в шапке")
	}
}

// Демо-карточка ленты — ЖИВОЙ фрагмент, и врать значком ей нельзя. Номер у неё
// из нативной полосы: с нулём она объявляла себя копией с НГС, то есть
// противоречила соседней теме, которая этот значок и объясняет.
func TestHelpDemoCardIsNative(t *testing.T) {
	body := helpBody(t, "/help/read", Config{})
	if !strings.Contains(body, `class="demo"`) {
		t.Fatal("на странице нет живого снимка карточки")
	}
	if got := demoNote(); !platform.IsNative(got.ID) {
		t.Errorf("демо-заметка №%d лежит вне нативной полосы: значок объявит её копией с НГС", got.ID)
	}
	// Нажимать в снимке нечего: ссылка ведёт в заметку, которой нет.
	if !strings.Contains(body, "<div class=\"demo\" inert>") {
		t.Error("снимок не помечен inert: ссылки внутри него живые")
	}
}

// Набор кнопок реакций приезжает из ядра — как окно правки и пороги частоты.
// Второй такой же список, набранный в шаблоне руками, разошёлся бы с настоящим.
func TestHelpReactionsComeFromTheCore(t *testing.T) {
	body := helpBody(t, "/help/signs", Config{})
	for _, code := range platform.ReactionCodes {
		if !strings.Contains(body, "/assets/smile/"+code+".") {
			t.Errorf("в справке нет кнопки реакции %q", code)
		}
	}
}

// Про сбор площадка говорит, только если знает его адрес, — то же правило, что
// у мессенджеров, и здесь оно нужнее: страница «поддержите» с пустой ссылкой
// просит денег и не говорит куда.
//
// Проверяется при НАСТРОЕННЫХ мессенджерах намеренно: гейты независимы, и
// прежняя форма helpTopics («настроены мессенджеры → верни весь список
// целиком») пропустила бы тему без адреса именно в этом сочетании.
func TestHelpSupportFollowsTheSettings(t *testing.T) {
	cfg := withLinks()
	cfg.Support = Support{}
	h := newTestServer(t, &fakeStore{}, cfg)
	if body := do(h, guest(t, "GET", "/help")).Body.String(); strings.Contains(body, "/help/support") {
		t.Error("оглавление зовёт поддержать площадку, у которой нет адреса сбора")
	}
	if w := do(h, guest(t, "GET", "/help/support")); w.Code != http.StatusNotFound {
		t.Errorf("тема без адреса сбора ответила %d, ожидался 404", w.Code)
	}

	body := helpBody(t, "/help/support", withLinks())
	if !strings.Contains(body, `>https://pay.example.org/p/test</a>`) {
		t.Error("адрес сбора показан не целиком: куда ведёт ссылка наружу, должно быть видно до нажатия")
	}
	if !strings.Contains(body, `rel="noopener nofollow"`) {
		t.Error("у ссылки наружу нет rel")
	}
	// Имя получателя называется ДО перехода: незнакомое имя на чужой платёжной
	// странице пугает сильнее, чем названное заранее. Не задано — молчим.
	if !strings.Contains(body, "Иван") {
		t.Error("получатель не назван, хотя имя в настройках есть")
	}
	noName := withLinks()
	noName.Support.Payee = ""
	if strings.Contains(helpBody(t, "/help/support", noName), "Получатель —") {
		t.Error("страница говорит о получателе, которого не назвали")
	}
}

// Цена месяца приезжает из настроек, а не написана в шаблоне словами, — то же
// правило, что у порогов частоты выше. И печатается она ТОЛЬКО вместе с датой
// замера: число без неё через полгода врёт молча.
//
// ИТОГ при этом считается, а не берётся третьим полем: рукой заполняемый, он
// однажды разошёлся бы с парой, и разошёлся бы молча.
func TestSupportCostComesFromConfig(t *testing.T) {
	cfg := withLinks()
	cfg.Support.InfraRub, cfg.Support.ModelsRub = 3141, 2718
	cfg.Support.AsOf = "март 2027"
	body := helpBody(t, "/help/support", cfg)
	for _, want := range []string{rub(3141), rub(2718), rub(5859), "март 2027"} {
		if !strings.Contains(body, want) {
			t.Errorf("на странице нет %q: числа месяца берутся из настроек, а итог считается по ним", want)
		}
	}

	cfg.Support.AsOf = ""
	if body := helpBody(t, "/help/support", cfg); strings.Contains(body, rub(3141)) {
		t.Error("сумма напечатана без даты замера: такая молча стареет")
	}
}

// Строка про статью расходов не печатается, пока её не посчитали: ноль рублей
// рядом с «вызовы моделей» читался бы как замер, а это незаполненное поле.
func TestSupportSkipsTheCostNobodyCounted(t *testing.T) {
	cfg := withLinks()
	cfg.Support.InfraRub, cfg.Support.ModelsRub = 1200, 0
	body := helpBody(t, "/help/support", cfg)
	if !strings.Contains(body, rub(1200)) {
		t.Fatal("посчитанная статья расходов не показана")
	}
	if strings.Contains(body, "Вместе около") {
		t.Error("напечатан итог по одной статье: складывать не с чем")
	}
	if strings.Contains(body, "вызовы языковых") {
		t.Error("непосчитанная статья расходов показана строкой")
	}
}

// Кривой адрес сбора в href не попадает — по тем же доводам, что у контактов
// (см. TestКриваяСсылкаНаГруппуГаснет), и правило это намеренно одно на двоих:
// две его копии разошлись бы как раз на той ссылке, где ошибка стоит чужих
// денег. Погашенный адрес снимает тему целиком, так что молчание честнее.
func TestКриваяСсылкаНаСборГаснет(t *testing.T) {
	for _, bad := range []string{"pay.example.org/p/x", "http://pay.example.org/p/x", "javascript:alert(1)"} {
		got := checkSupport(Support{URL: bad, InfraRub: 1200, AsOf: "сентябрь 2026"}, quietLog())
		if got.URL != "" {
			t.Errorf("ссылка %q уцелела", bad)
		}
		if got.InfraRub != 1200 || got.AsOf != "сентябрь 2026" {
			t.Error("проверка ссылки не должна трогать цену месяца")
		}
	}
	if got := checkSupport(Support{URL: "https://pay.example.org/p/x"}, quietLog()); got.URL == "" {
		t.Error("годная ссылка потерялась")
	}
}

// Страница обязана говорить, что взамен не будет НИЧЕГО, и это не стилистика.
// Встречное предоставление превращает пожертвование в оплату услуги: неправдой
// становится «платных услуг здесь нет» в «Отказе от ответственности», а знание
// о том, кто заплатил, — новой категорией персональных данных, то есть новой
// редакцией согласия и переподпиской ВСЕМИ. Сторож стоит против будущей
// «маленькой» правки, которая сделает это тихо.
func TestSupportPagePromisesNothingInReturn(t *testing.T) {
	body := helpBody(t, "/help/support", withLinks())
	if !strings.Contains(body, "Ничего.") {
		t.Error("страница не говорит прямо, что пожертвование не даёт ничего")
	}
	for _, w := range []string{"привилег", "премиум", "подписчик", "ранний доступ", "спасибо всем, кто"} {
		if strings.Contains(body, w) {
			t.Errorf("на странице сбора появилось встречное предоставление: %q", w)
		}
	}
}

// Ссылка — и только ссылка. Первый же виджет, бейдж или QR с чужого хоста ломает
// обещание политики («стороннего кода на страницах нет, и это держится не
// обещанием: CSP запрещает…») и в бою упрётся в img-src/script-src молча.
func TestSupportPageLoadsNothingForeign(t *testing.T) {
	body := helpBody(t, "/help/support", withLinks())
	for _, bad := range []string{`src="http`, `src='http`, "<iframe", "<form"} {
		if strings.Contains(body, bad) {
			t.Errorf("на странице сбора появилось чужое или платёжная форма: %q", bad)
		}
	}
}
