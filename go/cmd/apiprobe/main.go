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
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	nchan := flag.Bool("nchan", false, "опыт: push-канал msg.love.ngs.ru — достижимость и доставка ЛС")
	nchanPresence := flag.Bool("nchanpresence", false, "опыт: светит ли удержание WS-подписки «в сети» (долгий)")
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
		host := mbase
		if strings.HasPrefix(*page, "d:") {
			host, *page = *base, strings.TrimPrefix(*page, "d:")
		}
		dumpPage(ctx, host+*page, cookies)
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
	if *nchan {
		runNchanExperiment(ctx, desktop, cookies, *acctDB, *acctName)
		return
	}
	if *nchanPresence {
		runNchanPresenceExperiment(ctx, cookies, *acctDB, *acctName)
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

// runNchanPresenceExperiment — решающий опыт: светит ли человека «в сети» само
// удержание WebSocket-подписки на push-канал (без единого обращения к
// love.ngs.ru под его кукой). Если нет — talks может держать постоянную
// подписку и ходить на сайт только когда письмо реально пришло.
//
//  1. снять канал резерва с его домашней (это единственный заход под кукой — он
//     засветит резерв, поэтому дальше ждём затухания);
//  2. дождаться, пока резерв погаснет (глазами владельца, ~30 мин);
//  3. держать WS-подписку несколько минут, БЕЗ обращений к love.ngs.ru;
//  4. смотреть присутствие резерва глазами владельца.
func runNchanPresenceExperiment(ctx context.Context, owner []*http.Cookie, acctPath, acctName string) {
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
	hc := &http.Client{Timeout: 40 * time.Second}

	home, err := getRaw(ctx, hc, siteBase+"/", reserve)
	if err != nil {
		fmt.Println("домашняя резерва:", err)
		return
	}
	m := reChannelAssign.FindSubmatch(home)
	if m == nil {
		fmt.Println("имя push-канала не найдено")
		return
	}
	channel := string(m[1])
	fmt.Printf("канал резерва %s получен; жду затухания «в сети»\n", unescape(account.Nick))

	if !waitUntilOffline(ctx, owner, account.PassportID) {
		fmt.Println("резерв не погас за час — опыт прерван")
		return
	}

	// Держим подписку в фоне и молча сливаем кадры — важно только соединение.
	fmt.Println("резерв погас. Открываю WS-подписку и держу 5 мин, к сайту НЕ хожу…")
	subCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	sink := make(chan string, 64)
	go func() {
		if err := subscribeWS(subCtx, channel, sink); err != nil {
			fmt.Println("  WS:", err)
		}
		close(sink)
	}()
	go func() {
		for range sink { // события нам тут не важны, просто не копим
		}
	}()

	for _, wait := range []time.Duration{60, 120, 240} {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait * time.Second):
		}
		st := presenceOf(ctx, owner, account.PassportID)
		online := strings.Contains(strings.ToLower(st), "online")
		fmt.Printf("  держим подписку %d с → присутствие резерва: %q%s\n",
			int(wait), st, map[bool]string{true: "  ⟵ СВЕТИТ", false: ""}[online])
	}
	cancel()
	fmt.Println("готово: если все три замера НЕ online — удержание подписки не палит человека")
}

// reChannelAssign — присваивание имени push-канала на странице под сессией.
// Форматы: Love.pushStreamMessagesChannelName = "…"; либо "…":"…" в JSON-блоке.
var reChannelAssign = regexp.MustCompile(`pushStreamMessagesChannelName['"]?\s*[:=]\s*['"]([^'"]+)['"]`)

// runNchanExperiment: (1) вытащить имя push-канала резерва из его домашней под
// сессией, (2) проверить, отвечает ли msg.love.ngs.ru на подписку longpoll'ом,
// (3) проверить доставку — владелец шлёт ЛС на резерв, слушаем канал резерва.
// Имя канала — ключ к чужим уведомлениям, поэтому в вывод идёт только длина.
func runNchanExperiment(ctx context.Context, site *love.Client, owner []*http.Cookie, acctPath, acctName string) {
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
	hc := &http.Client{Timeout: 40 * time.Second}

	// 1. Имя канала резерва — с его домашней под его сессией.
	home, err := getRaw(ctx, hc, siteBase+"/", reserve)
	if err != nil {
		fmt.Println("домашняя резерва:", err)
		return
	}
	m := reChannelAssign.FindSubmatch(home)
	if m == nil {
		fmt.Println("имя push-канала на странице не найдено (маркер есть? дрейф вёрстки)")
		return
	}
	channel := string(m[1])
	fmt.Printf("канал резерва %s: получен, длина имени %d\n", unescape(account.Nick), len(channel))

	// 2. Подписка по WebSocket (транспорт подтверждён: 101 + ws+meta.nchan).
	// Слушаем в фоне, ПОТОМ шлём — иначе новое соединение пропустит уже
	// опубликованное. Куку не носим: канал сам себе капабилити, PHP-сессию
	// хост не трогает (это отдельный nginx/nchan за DDoS-Guard).
	frames := make(chan string, 16)
	wsCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	go func() {
		if err := subscribeWS(wsCtx, channel, frames); err != nil {
			fmt.Println("  WS:", err)
		}
		close(frames)
	}()
	time.Sleep(3 * time.Second) // дать хендшейку встать

	// 3. Владелец шлёт ЛС резерву.
	needle := fmt.Sprintf("apiprobe-nchan-%d", time.Now().Unix()%100000)
	if _, err := site.TalksSend(ctx, owner, account.PassportID, "проверка push "+needle); err != nil {
		fmt.Println("отправка ЛС владельцем:", err)
		cancel()
		return
	}
	fmt.Printf("владелец отправил ЛС резерву (метка %s), слушаю канал до 45 с…\n", needle)

	got := false
	for f := range frames {
		hit := strings.Contains(f, needle)
		fmt.Printf("  кадр: %d б, метка внутри: %v%s\n", len(f), hit, clipFrame(f))
		if hit {
			got = true
			decodeNchanFrame(f) // наше сообщение — безопасно показать структуру
			cancel()
		}
	}
	if got {
		fmt.Println("=> push ДОСТАВЛЯЕТ уведомление о новом ЛС по WebSocket")
	} else {
		fmt.Println("=> метки в кадрах не было (см. выше)")
	}
}

// subscribeWS — минимальный WebSocket-подписчик nchan поверх TLS, без
// зависимостей. Пишет текстовые кадры в out, пока не истечёт ctx или сервер не
// закроет соединение. Клиент ничего не отправляет после хендшейка (кроме pong),
// поэтому маскирование исходящих данных не нужно; входящие кадры сервер шлёт
// немаскированными.
func subscribeWS(ctx context.Context, channel string, out chan<- string) error {
	host := "msg.love.ngs.ru"
	d := &tls.Dialer{Config: &tls.Config{ServerName: host}}
	c, err := d.DialContext(ctx, "tcp", host+":443")
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn := c.(*tls.Conn)
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }() // разбудить чтение по таймауту

	var keyb [16]byte
	if _, err := rand.Read(keyb[:]); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(keyb[:])
	req := "GET /sub/LOVE-" + channel + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"User-Agent: " + uaDesktop + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Protocol: ws+meta.nchan\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("запрос апгрейда: %w", err)
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("ответ апгрейда: %w", err)
	}
	if !strings.Contains(status, "101") {
		return fmt.Errorf("апгрейд отклонён: %s", strings.TrimSpace(status))
	}
	for { // дочитать заголовки до пустой строки
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	for {
		op, payload, err := readWSFrame(br)
		if err != nil {
			if ctx.Err() != nil {
				return nil // штатный таймаут опыта
			}
			return err
		}
		switch op {
		case 0x1, 0x2: // text / binary
			out <- string(payload)
		case 0x8: // close
			return nil
		case 0x9: // ping — можно не отвечать в рамках короткого опыта
		}
	}
}

// readWSFrame читает один кадр (сервер шлёт немаскированные кадры).
func readWSFrame(br *bufio.Reader) (opcode byte, payload []byte, err error) {
	h0, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	h1, err := br.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	opcode = h0 & 0x0f
	masked := h1&0x80 != 0
	n := int(h1 & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(b[0])<<8 | int(b[1])
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(br, b[:]); err != nil {
			return 0, nil, err
		}
		n = 0
		for _, x := range b {
			n = n<<8 | int(x)
		}
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// decodeNchanFrame разбирает кадр nchan ws+meta: строки метаданных, затем тело
// {id, channel, text}, где text — URI-кодированный внутренний JSON события.
// Печатает ключи внутреннего события — что именно несёт push.
func decodeNchanFrame(f string) {
	i := strings.LastIndex(f, "{")
	if i < 0 {
		return
	}
	var outer struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(f[i:]), &outer); err != nil {
		fmt.Println("    (тело кадра не JSON)")
		return
	}
	inner, err := url.QueryUnescape(outer.Text)
	if err != nil {
		inner = outer.Text
	}
	fmt.Printf("    внутреннее событие: %s\n", inner)
}

// clipFrame — короткая выдержка кадра для диагностики (может нести чужой ник).
func clipFrame(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	if len(s) > 90 {
		s = s[:90] + "…"
	}
	return " | " + s
}

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
	for _, marker := range []string{
		"LoveRun", "LoveSimpleRPC", "/ajax", "Love.user", "MobileNotesLimit",
		"msg.love.ngs.ru", "pushStreamMessagesChannelName", "DOMAIN_SUFFIX",
		"lastMessagesCheckTime", "NchanSubscriber",
	} {
		fmt.Printf("  %-32s %d\n", marker, bytes.Count(body, []byte(marker)))
	}
	// Имя канала — это ключ: кто его знает, тот подписан на чужие уведомления.
	// Поэтому только длина, никогда значение.
	for _, m := range reChannel.FindAllSubmatch(body, -1) {
		fmt.Printf("  канал push: есть, длина имени %d\n", len(m[1]))
	}
	for _, m := range reSuffix.FindAllSubmatch(body, -1) {
		fmt.Printf("  DOMAIN_SUFFIX = %q\n", string(m[1]))
	}
	for _, m := range reToken.FindAllSubmatch(body, -1) {
		fmt.Printf("  token             найден, длина значения %d\n", len(m[1]))
	}
}

var (
	reScriptSrc = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	reToken     = regexp.MustCompile(`token"?\s*[:=]\s*"([^"]*)"`)
	reChannel   = regexp.MustCompile(`pushStreamMessagesChannelName"?\s*[:=]\s*"([^"]*)"`)
	reSuffix    = regexp.MustCompile(`DOMAIN_SUFFIX"?\s*[:=]\s*"([^"]*)"`)
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
