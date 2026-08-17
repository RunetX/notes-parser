// Команда apiprobe — разовая разведка JSON-RPC API сайта под живой сессией.
// Зовёт только ЧИТАЮЩИЕ методы и меряет присутствие анкеты (last_activity) до и
// после, чтобы понять, светит ли сам вызов API человека в сети. Ничего не
// отправляет, ничего не помечает прочитанным: getMessagesHistory, readMessages
// и pingOnline здесь намеренно отсутствуют.
//
// Куки берутся из боевой БД через store.SessionCookies (он же расшифрует) и не
// печатаются ни при каких флагах.
//
// Временный инструмент разведки (бриф briefs/love-ajax-api.md), не часть демона.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lovegw/internal/acct"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// siteBase — базовый URL сайта; ставится из флага в main.
var siteBase string

const (
	uaDesktop = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	uaMobile  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

// call — читающий вызов, который мы согласны сделать под чужой живой сессией.
type call struct {
	gateway string // "m" — мобильный шлюз, "d" — десктопный
	method  string
	params  []any
}

// A/B на одном и том же логическом методе: он есть в обоих каталогах, только с
// разными сигнатурами. Если десктопный отвечает, а мобильный падает — дело не в
// правах, не в параметрах и не в сессии, а в самом мобильном сервисе.
var reads = []call{
	{"d", "autocompleteRegions", []any{"city", "Новоси"}},
	{"m", "autocompleteRegions", []any{"Новоси"}},
	{"d", "getTagsCloud", []any{}},
	{"m", "getNewMessagesCount", []any{}},
}

func main() {
	dbPath := flag.String("db", "data/lovegw.db", "путь к боевой БД")
	messenger := flag.String("messenger", store.MessengerTelegram, "мессенджер сессии")
	userID := flag.Int64("user", 0, "user_id владельца сессии")
	base := flag.String("base", "https://love.ngs.ru", "базовый URL сайта")
	excerpt := flag.Int("excerpt", 160, "сколько байт ответа показывать")
	page := flag.String("page", "", "вместо вызовов: снять страницу мобильной версии под сессией и показать её скрипты")
	talks := flag.Bool("talks", false, "опыт: узнать о новом ЛС, не открывая его (нужна служебная учётка)")
	acctDB := flag.String("accounts", "data/accounts.db", "база служебных аккаунтов")
	acctName := flag.String("account", "reserve", "имя служебного аккаунта-отправителя")
	buddies := flag.String("buddies", "", "показать в списке диалогов запись этого паспорта (свой служебный!)")
	asAccount := flag.Bool("as", false, "для -buddies: смотреть глазами служебной учётки, а не владельца")
	methods := flag.Bool("methods", false, "опыт: дают ли getTalksList/getNewMessages данные без пометки прочитанным")
	activity := flag.Int64("activity", 0, "анонимно показать last_activity этой анкеты и выйти")
	acts := flag.String("acts", "list,history,get", "какие действия проверять в -presence: list,history,get")
	presence := flag.Bool("presence", false, "опыт: светит ли обход человека «в сети» (долгий, ~30-60 мин)")
	flag.Parse()
	if *userID == 0 {
		fmt.Println("нужен -user")
		return
	}

	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	siteBase = *base

	st, err := store.Open(ctx, *dbPath)
	if err != nil {
		fmt.Println("БД:", err)
		return
	}
	defer st.Close()

	cookiesJSON, valid, err := st.SessionCookies(ctx, *messenger, *userID)
	if err != nil {
		fmt.Println("сессия:", err)
		return
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		fmt.Println("разбор кук:", err)
		return
	}
	fmt.Printf("сессия %s/%d: valid=%v, живых кук %d\n", *messenger, *userID, valid, len(cookies))

	mbase, err := love.MobileBaseURL(*base)
	if err != nil {
		fmt.Println("мобильная база:", err)
		return
	}
	desktop := love.New(*base, uaDesktop, 2*time.Second, quiet)
	mobile := love.New(mbase, uaMobile, 2*time.Second, quiet)

	if *page != "" {
		dumpPage(ctx, mbase+*page, cookies)
		return
	}

	// 1. Кто мы. Это авторизованный GET страницы — он и сам может двигать
	// присутствие, поэтому замер ниже честен только как «после первого GET».
	profileID, passportID, nick, err := desktop.SiteIdentity(ctx, cookies)
	if err != nil {
		fmt.Println("identity:", err)
		return
	}
	fmt.Printf("анкета %s, паспорт %s, ник %q\n\n", profileID, passportID, unescape(nick))
	pid, _ := strconv.ParseInt(profileID, 10, 64)

	if *talks {
		runTalksExperiment(ctx, desktop, mbase, cookies, passportID, *acctDB, *acctName)
		return
	}
	if *buddies != "" {
		who := cookies
		// -as: смотреть глазами служебной учётки, а не владельца. Нужно, чтобы
		// проверить обратное направление — как владелец выглядит для других.
		if *asAccount {
			as, err := acct.Open(ctx, *acctDB)
			if err != nil {
				fmt.Println("база аккаунтов:", err)
				return
			}
			defer as.Close()
			_, j, err := as.Get(ctx, *acctName)
			if err != nil {
				fmt.Println("аккаунт:", err)
				return
			}
			if who, err = love.CookiesFromJSON([]byte(j), time.Now()); err != nil {
				fmt.Println("куки аккаунта:", err)
				return
			}
			fmt.Printf("(смотрим глазами служебной учётки %s)\n", *acctName)
		}
		dumpBuddy(ctx, who, *buddies)
		return
	}
	if *methods {
		runMethodsExperiment(ctx, desktop, cookies, profileID, passportID, *acctDB, *acctName)
		return
	}
	if *presence {
		runPresenceExperiment(ctx, desktop, cookies, passportID, *acctDB, *acctName, *acts)
		return
	}
	// Анонимный замер присутствия чужой анкеты: ползёт ли отметка сама.
	if *activity != 0 {
		fmt.Printf("анкета %d: %s (сейчас Нск %s)\n", *activity, snapshot(ctx, mobile, *activity),
			time.Now().UTC().Add(7*time.Hour).Format("15:04"))
		return
	}

	// 2. Присутствие ДО вызовов — читаем анонимно, без кук.
	before := snapshot(ctx, mobile, pid)
	fmt.Printf("last_activity до вызовов: %s\n\n", before)

	// 3. Читающие вызовы.
	hc := &http.Client{Timeout: 30 * time.Second}
	for _, c := range reads {
		url := mbase + "/ajax/"
		origin := mbase
		ua := uaMobile
		if c.gateway == "d" {
			url, origin, ua = *base+"/ajax/", *base, uaDesktop
		}
		status, body, err := rpc(ctx, hc, url, origin, ua, cookies, c.method, c.params)
		tag := c.method
		if c.gateway == "d" {
			tag += " (десктоп)"
		}
		switch {
		case err != nil:
			fmt.Printf("%-34s ошибка: %v\n", tag, err)
		default:
			fmt.Printf("%-34s %d  %s\n", tag, status, describe(body, *excerpt))
		}
		time.Sleep(2 * time.Second)
	}

	// 4. Присутствие ПОСЛЕ.
	after := snapshot(ctx, mobile, pid)
	fmt.Printf("\nlast_activity после вызовов: %s\n", after)
	if before == after {
		fmt.Println("=> не сдвинулось")
	} else {
		fmt.Println("=> СДВИНУЛОСЬ")
	}
	fmt.Printf("сейчас (Нск): %s\n", time.Now().UTC().Add(7*time.Hour).Format("02.01.2006 15:04"))
}

// runTalksExperiment отвечает на вопрос: можно ли УЗНАТЬ о новом ЛС, не открывая
// его. Служебная учётка шлёт владельцу сообщение, дальше под сессией владельца
// считаем непрочитанное, смотрим список диалогов и считаем снова: если список
// не гасит счётчик, значит уведомление можно доставлять, ничего не помечая.
// Последним шагом — история, чтобы убедиться, что гасит именно она.
//
// Собеседник здесь — наша же служебная анкета, поэтому «просмотрено» не увидит
// никто посторонний.
func runTalksExperiment(ctx context.Context, site *love.Client, mbase string, owner []*http.Cookie, ownerPassport, acctPath, acctName string) {
	as, err := acct.Open(ctx, acctPath)
	if err != nil {
		fmt.Println("база аккаунтов:", err)
		return
	}
	defer as.Close()
	account, reserveJSON, err := as.Get(ctx, acctName)
	if err != nil {
		fmt.Println("аккаунт:", err)
		return
	}
	reserve, err := love.CookiesFromJSON([]byte(reserveJSON), time.Now())
	if err != nil {
		fmt.Println("куки аккаунта:", err)
		return
	}
	fmt.Printf("отправитель: %s (анкета %s), живых кук %d\n\n", unescape(account.Nick), account.ProfileID, len(reserve))

	hc := &http.Client{Timeout: 30 * time.Second}
	count := func(label string) {
		status, body, err := rpc(ctx, hc, siteBase+"/ajax/", siteBase, uaDesktop, owner, "countNewMessages", []any{})
		if err != nil {
			fmt.Printf("  %-42s ошибка: %v\n", label, err)
			return
		}
		fmt.Printf("  %-42s %d %s\n", label, status, describe(body, 40))
	}

	// Иголка — по ней потом ищем, попадает ли текст сообщения в список диалогов.
	needle := fmt.Sprintf("apiprobe-%d", time.Now().Unix()%100000)

	count("0. непрочитанных до отправки")
	sent, err := site.TalksSend(ctx, reserve, ownerPassport, "проверка доставки "+needle)
	if err != nil {
		fmt.Println("  отправка ЛС:", err)
		return
	}
	fmt.Printf("  1. отправлено служебной учёткой, id %s\n", sent.SiteMsgID)
	time.Sleep(2 * time.Second)

	count("2. непрочитанных после отправки")

	// 3. Список диалогов — сырой, чтобы проверить наличие текста, и разобранный,
	// чтобы увидеть счётчик непрочитанного.
	raw, err := getRaw(ctx, hc, fmt.Sprintf(siteBase+"/ajax?request=loadBuddiesList&before=0&limit=%d&anticache=%d", 10, time.Now().UnixMilli()), owner)
	if err != nil {
		fmt.Println("  список диалогов (сырой):", err)
	} else {
		fmt.Printf("  3. список диалогов: %d байт, текст сообщения внутри: %v\n",
			len(raw), bytes.Contains(raw, []byte(needle)))
	}
	dialogs, err := site.TalksDialogs(ctx, owner, 10)
	if err != nil {
		fmt.Println("  список диалогов (разбор):", err)
	} else {
		fmt.Printf("     всего диалогов %d; из них с непрочитанным:\n", len(dialogs))
		for _, d := range dialogs {
			if d.Unread > 0 {
				fmt.Printf("       паспорт %s, непрочитанных %d, последнее %s\n", d.PassportID, d.Unread, d.LastMsgID)
			}
		}
	}

	count("4. непрочитанных ПОСЛЕ списка диалогов")

	// 5. И только теперь история — шаг, который, по гипотезе, и помечает.
	msgs, err := site.TalksHistory(ctx, owner, account.PassportID, "", 0)
	if err != nil {
		fmt.Println("  история:", err)
	} else {
		fmt.Printf("  5. история прочитана: %d сообщений\n", len(msgs))
	}
	count("6. непрочитанных ПОСЛЕ истории")

	// Заодно: не ожил ли мобильный шлюз.
	status, body, err := rpc(ctx, hc, mbase+"/ajax/", mbase, uaMobile, owner, "getNewMessagesCount", []any{})
	if err != nil {
		fmt.Printf("\nмобильный getNewMessagesCount: ошибка %v\n", err)
	} else {
		fmt.Printf("\nмобильный getNewMessagesCount: %d %s\n", status, describe(body, 40))
	}
}

// runMethodsExperiment проверяет два неиспользуемых десктопных метода:
// getTalksList (обещает список диалогов массивом вместо HTML) и getNewMessages
// (обещает сами новые сообщения). Ключевых вопроса два: есть ли в ответе ТЕКСТ
// и гасит ли вызов непрочитанное. Поэтому счётчик спрашивается после каждого
// шага, а getMessagesHistory здесь не зовётся вовсе — иначе он погасит счётчик
// и остальные измерения потеряют смысл.
func runMethodsExperiment(ctx context.Context, site *love.Client, owner []*http.Cookie, ownerProfile, ownerPassport, acctPath, acctName string) {
	as, err := acct.Open(ctx, acctPath)
	if err != nil {
		fmt.Println("база аккаунтов:", err)
		return
	}
	defer as.Close()
	account, reserveJSON, err := as.Get(ctx, acctName)
	if err != nil {
		fmt.Println("аккаунт:", err)
		return
	}
	reserve, err := love.CookiesFromJSON([]byte(reserveJSON), time.Now())
	if err != nil {
		fmt.Println("куки аккаунта:", err)
		return
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	needle := fmt.Sprintf("apiprobe-%d", time.Now().Unix()%100000)

	count := func(label string) {
		_, body, err := rpc(ctx, hc, siteBase+"/ajax/", siteBase, uaDesktop, owner, "countNewMessages", []any{})
		if err != nil {
			fmt.Printf("      счётчик %-34s ошибка: %v\n", label, err)
			return
		}
		fmt.Printf("      счётчик %-34s %s\n", label, describe(body, 20))
	}
	try := func(method string, params ...any) {
		status, body, err := rpc(ctx, hc, siteBase+"/ajax/", siteBase, uaDesktop, owner, method, params)
		if err != nil {
			fmt.Printf("  %s%v: ошибка %v\n", method, params, err)
			return
		}
		fmt.Printf("  %s%v → %d %s\n", method, params, status, describe(body, 200))
		fmt.Printf("      текст сообщения внутри: %v\n", bytes.Contains(body, []byte(needle)))
		count("после " + method)
	}

	if _, err := site.TalksSend(ctx, reserve, ownerPassport, "проверка методов "+needle); err != nil {
		fmt.Println("отправка ЛС служебной учёткой:", err)
		return
	}
	fmt.Printf("служебная учётка %s отправила ЛС с меткой %s\n\n", unescape(account.Nick), needle)
	time.Sleep(2 * time.Second)
	count("до всего")

	// userId у getNewMessages объявлен int — строку сайт может не пережить.
	profileNum, _ := strconv.ParseInt(ownerProfile, 10, 64)
	passportNum, _ := strconv.ParseInt(ownerPassport, 10, 64)

	// Свои id дали {html:"",count:0} — значит параметр это СОБЕСЕДНИК, а не мы.
	peerProfile, _ := strconv.ParseInt(account.ProfileID, 10, 64)
	peerPassport, _ := strconv.ParseInt(account.PassportID, 10, 64)
	_, _ = profileNum, passportNum

	try("getNewMessages", peerProfile)
	try("getNewMessages", peerPassport)
	fmt.Println("\ngetMessagesHistory здесь НЕ звался — непрочитанное осталось у вас.")
}

// runPresenceExperiment отвечает на последний открытый вопрос: делает ли обход
// человека видимо «в сети» для собеседника.
//
// Устройство. Наблюдаем СЛУЖЕБНУЮ анкету глазами владельца: в списке диалогов
// сайт печатает присутствие собеседника («Online» либо «был(а) в сети …»). Это
// ровно то, что видит живой человек, — в отличие от last_activity, которое лишь
// косвенный признак. Служебную анкету при этом не трогает никто: боевой демон её
// не обходит, в браузере она не открыта. Запросы владельца светят владельца, а
// не её, поэтому наблюдение не портит наблюдаемое.
//
// Шаги: дождаться, пока служебная погаснет (заодно узнаем время затухания) →
// сделать ею ОДИН вызов → посмотреть, зажглась ли. И так для каждого вызова.
func runPresenceExperiment(ctx context.Context, site *love.Client, owner []*http.Cookie, ownerPassport, acctPath, acctName, acts string) {
	as, err := acct.Open(ctx, acctPath)
	if err != nil {
		fmt.Println("база аккаунтов:", err)
		return
	}
	defer as.Close()
	account, reserveJSON, err := as.Get(ctx, acctName)
	if err != nil {
		fmt.Println("аккаунт:", err)
		return
	}
	reserve, err := love.CookiesFromJSON([]byte(reserveJSON), time.Now())
	if err != nil {
		fmt.Println("куки аккаунта:", err)
		return
	}
	fmt.Printf("наблюдаем служебную анкету %s (паспорт %s) глазами владельца\n\n",
		unescape(account.Nick), account.PassportID)

	// Действия, каждое — ОДИН запрос служебной сессией. Каждое стоит получаса
	// ожидания затухания, поэтому набор выбирается флагом -acts.
	all := []struct {
		key  string
		name string
		do   func() error
	}{
		{"list", "loadBuddiesList (список диалогов)", func() error {
			_, err := site.TalksDialogs(ctx, reserve, 10)
			return err
		}},
		{"history", "getMessagesHistory (история диалога)", func() error {
			_, err := site.TalksHistory(ctx, reserve, ownerPassport, "", 0)
			return err
		}},
		{"get", "GET / (обычная страница)", func() error {
			_, _, _, err := site.SiteIdentity(ctx, reserve)
			return err
		}},
	}
	wanted := strings.Split(acts, ",")
	var todo []struct {
		key  string
		name string
		do   func() error
	}
	for _, a := range all {
		for _, w := range wanted {
			if strings.TrimSpace(w) == a.key {
				todo = append(todo, a)
			}
		}
	}
	if len(todo) == 0 {
		fmt.Printf("в -acts нечего делать: %q\n", acts)
		return
	}

	for _, a := range todo {
		if !waitUntilOffline(ctx, owner, account.PassportID) {
			fmt.Println("не дождались затухания — опыт прерван")
			return
		}
		if err := a.do(); err != nil {
			fmt.Printf("  %s: ошибка %v\n", a.name, err)
			continue
		}
		time.Sleep(20 * time.Second)
		state := presenceOf(ctx, owner, account.PassportID)
		fmt.Printf("  ПОСЛЕ «%s» → %q\n\n", a.name, state)
	}
	fmt.Println("готово")
}

// waitUntilOffline опрашивает присутствие раз в две минуты, пока служебная
// анкета не погаснет. Возвращает false, если за час не погасла.
func waitUntilOffline(ctx context.Context, owner []*http.Cookie, passport string) bool {
	start := time.Now()
	for i := 0; i < 30; i++ {
		state := presenceOf(ctx, owner, passport)
		if !strings.Contains(strings.ToLower(state), "online") {
			fmt.Printf("  погасла через %s после последнего действия → %q\n", time.Since(start).Round(time.Minute), state)
			return true
		}
		fmt.Printf("  [%s] ещё горит: %q\n", time.Now().UTC().Add(7*time.Hour).Format("15:04"), state)
		time.Sleep(2 * time.Minute)
	}
	return false
}

// presenceOf достаёт строку присутствия собеседника из списка диалогов владельца.
func presenceOf(ctx context.Context, owner []*http.Cookie, passport string) string {
	hc := &http.Client{Timeout: 30 * time.Second}
	url := fmt.Sprintf(siteBase+"/ajax?request=loadBuddiesList&before=0&limit=%d&anticache=%d", 10, time.Now().UnixMilli())
	raw, err := getRaw(ctx, hc, url, owner)
	if err != nil {
		return "ошибка: " + err.Error()
	}
	i := bytes.Index(raw, []byte(passport))
	if i < 0 {
		return "диалога нет в списке"
	}
	m := rePresence.FindSubmatch(raw[i:])
	if m == nil {
		return "строки присутствия нет"
	}
	return strings.TrimSpace(unescape(string(m[1])))
}

var rePresence = regexp.MustCompile(`account-activity-info[^>]*>([^<]*)<`)

// dumpBuddy показывает запись одного диалога из списка — чтобы понять, есть ли
// в списке текст последнего сообщения. Паспорт задаётся руками и должен быть
// СВОИМ служебным: в чужих записях лежит чужая переписка.
func dumpBuddy(ctx context.Context, cookies []*http.Cookie, passport string) {
	hc := &http.Client{Timeout: 30 * time.Second}
	url := fmt.Sprintf(siteBase+"/ajax?request=loadBuddiesList&before=0&limit=%d&anticache=%d", 10, time.Now().UnixMilli())
	raw, err := getRaw(ctx, hc, url, cookies)
	if err != nil {
		fmt.Println("список диалогов:", err)
		return
	}
	fmt.Printf("список диалогов: %d байт\n\nмаркеры:\n", len(raw))
	for _, needle := range []string{"проверка", "доставки", "unread", "msg", "text", "preview", "last"} {
		fmt.Printf("  %-10s %d\n", needle, bytes.Count(raw, []byte(needle)))
	}
	i := bytes.Index(raw, []byte(passport))
	if i < 0 {
		fmt.Printf("\nпаспорт %s в списке не найден\n", passport)
		return
	}
	from, to := max(0, i-100), min(len(raw), i+2600)
	fmt.Printf("\n--- запись паспорта %s ---\n%s\n", passport, unescape(string(raw[from:to])))
}

func getRaw(ctx context.Context, hc *http.Client, url string, cookies []*http.Cookie) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", uaDesktop)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// unescape разворачивает \uXXXX: и SiteIdentity, и acct хранят ник как есть,
// а сайт отдаёт его экранированным внутри JS-объекта страницы.
func unescape(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+strings.ReplaceAll(s, `"`, `\"`)+`"`), &out); err != nil {
		return s
	}
	return out
}

// dumpPage снимает страницу под сессией и показывает её JS-обвязку: какие
// бандлы грузятся и есть ли на ней маркеры RPC. Значение token печатается
// длиной, а не текстом: он привязан к сессии.
func dumpPage(ctx context.Context, url string, cookies []*http.Cookie) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", uaMobile)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println("страница:", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	fmt.Printf("%s → %d, %d байт\n\n", url, resp.StatusCode, len(body))

	fmt.Println("скрипты:")
	for _, m := range reScriptSrc.FindAllSubmatch(body, -1) {
		fmt.Println("  ", string(m[1]))
	}
	fmt.Println("\nмаркеры:")
	for _, marker := range []string{"LoveRun", "LoveSimpleRPC", "/ajax", "Love.user", "MobileNotesLimit"} {
		fmt.Printf("  %-18s %d\n", marker, bytes.Count(body, []byte(marker)))
	}
	for _, m := range reToken.FindAllSubmatch(body, -1) {
		fmt.Printf("  token             найден, длина значения %d\n", len(m[1]))
	}
}

var (
	reScriptSrc = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	reToken     = regexp.MustCompile(`token"?\s*[:=]\s*"([^"]*)"`)
)

func snapshot(ctx context.Context, mobile *love.Client, pid int64) string {
	a, err := mobile.FetchActivity(ctx, nil, pid)
	if err != nil {
		return "ошибка: " + err.Error()
	}
	if a.Missing {
		return "анкеты нет"
	}
	return fmt.Sprintf("%q (hide_me=%v)", a.Raw, a.HideMe)
}

func rpc(ctx context.Context, hc *http.Client, url, origin, ua string, cookies []*http.Cookie, method string, params []any) (int, []byte, error) {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params, "id": 1,
	})
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// describe сжимает ответ до формы: размер, тип result и короткая выдержка.
// Выдержка обрезается намеренно: в ответах ходят ники и куски чужих реплик.
func describe(body []byte, n int) string {
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("<")) {
		return fmt.Sprintf("%db HTML (не JSON) %s", len(body), clip(body, 60))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Sprintf("%db не разобрать: %s", len(body), clip(body, 60))
	}
	if env.Error != nil && env.Error.Message != "" {
		return fmt.Sprintf("%db ОШИБКА code=%d %q", len(body), env.Error.Code, env.Error.Message)
	}
	return fmt.Sprintf("%db result=%s %s", len(body), shape(env.Result), clip(env.Result, n))
}

func shape(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	switch {
	case len(t) == 0:
		return "нет"
	case t[0] == '[':
		var a []json.RawMessage
		if json.Unmarshal(t, &a) == nil {
			return fmt.Sprintf("массив[%d]", len(a))
		}
	case t[0] == '{':
		var m map[string]json.RawMessage
		if json.Unmarshal(t, &m) == nil {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			return "объект{" + strings.Join(keys, ",") + "}"
		}
	}
	return "скаляр"
}

func clip(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
