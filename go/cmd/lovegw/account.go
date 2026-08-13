package main

// Сервисные аккаунты сайта: технические сессии для обходов под авторизацией и
// редких ручных действий (второй профиль как резерв доступа). Живут в своей
// базе (пакет acct) и намеренно невидимы ботам — см. шапку пакета.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"lovegw/internal/acct"
	"lovegw/internal/config"
	"lovegw/internal/love"
)

var accountSubcommands = map[string]bool{
	"login": true, "list": true, "check": true, "forget": true,
	"cookie": true, "say": true,
}

func cmdAccount(ctx context.Context, args []string) error {
	sub, rest := splitSubcommand(args, accountSubcommands)
	switch sub {
	case "login":
		return accountLogin(ctx, rest)
	case "list":
		return accountList(ctx, rest)
	case "check":
		return accountCheck(ctx, rest)
	case "forget":
		return accountForget(ctx, rest)
	case "cookie":
		return accountCookie(ctx, rest)
	case "say":
		return accountSay(ctx, rest)
	default:
		usage()
		return fmt.Errorf("account: нужна подкоманда (login|list|check|forget|cookie|say)")
	}
}

// accountFlags — общие флаги подкоманд: конфиг и путь к базе аккаунтов.
type accountFlags struct {
	cfgPath   *string
	accounts  *string
	name      *string
	valueArgs map[string]bool
}

func accountFlagSet(name string, extra map[string]bool) (*flag.FlagSet, accountFlags) {
	fs := flag.NewFlagSet("account "+name, flag.ExitOnError)
	f := accountFlags{
		cfgPath:   fs.String("config", defaultConfigPath, configFlagUsage),
		accounts:  fs.String("accounts", "", accountsFlagUsage),
		name:      fs.String("name", "", "имя аккаунта (например reserve)"),
		valueArgs: map[string]bool{"config": true, "accounts": true, "name": true},
	}
	for k, v := range extra {
		f.valueArgs[k] = v
	}
	return fs, f
}

// accountLogin — вход на сайт и сохранение сессии.
//
// Логин и пароль читаются со stdin: во флаге они осели бы в истории оболочки и
// светились в `ps` у всех на машине.
func accountLogin(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("login", map[string]bool{"purpose": true})
	// Не -note: в этом CLI -note всюду означает id заметки на сайте.
	purpose := fs.String("purpose", "", "зачем этот аккаунт (свободный текст, попадёт в account list)")
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	if *f.name == "" {
		return fmt.Errorf("account login: нужен -name")
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	login, password, err := readCredentials()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
	cookies, err := client.Login(ctx, login, password)
	if err != nil {
		return fmt.Errorf("вход на сайт: %w", err)
	}
	cookiesJSON, err := love.CookiesToJSON(cookies, time.Now())
	if err != nil {
		return err
	}
	// Кем оказалась сессия, спрашиваем сразу: без этого в списке будет одно
	// голое имя, и перепутать резерв с основной анкетой проще простого.
	profileID, passportID, nick, err := client.SiteIdentity(ctx, cookies)
	if err != nil {
		log.Warn("идентичность аккаунта не снята (сессия сохранена)", "err", err)
	}

	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()
	a := acct.Account{
		Name: *f.name, ProfileID: profileID, PassportID: passportID,
		Nick: nick, Note: *purpose,
	}
	if err := db.Put(ctx, a, cookiesJSON, time.Now()); err != nil {
		return err
	}
	fmt.Printf("аккаунт сохранён: %s\n", a.Title())
	return nil
}

// readCredentials читает логин и пароль двумя строками stdin. Работает и с
// человеком в терминале, и с пайпом (`printf '%s\n%s\n' "$L" "$P" | lovegw …`).
func readCredentials() (login, password string, err error) {
	fmt.Fprintln(os.Stderr, "Логин и пароль вводятся ДВУМЯ строками (пароль виден на экране —")
	fmt.Fprintln(os.Stderr, "во флаге он попал бы в историю оболочки, что хуже).")
	r := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "логин: ")
	if login, err = readLine(r); err != nil {
		return "", "", err
	}
	fmt.Fprint(os.Stderr, "пароль: ")
	if password, err = readLine(r); err != nil {
		return "", "", err
	}
	if login == "" || password == "" {
		return "", "", fmt.Errorf("пустой логин или пароль")
	}
	return login, password, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

func accountList(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("list", nil)
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()
	list, err := db.List(ctx)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("аккаунтов нет: заведите первый — lovegw account login -name reserve")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "имя\tник\tанкета\tсессия\tвход\tпоследний успех\tзаметка")
	for _, a := range list {
		state := "валидна"
		if !a.Valid {
			state = "НЕВАЛИДНА"
		}
		fmt.Fprintf(tw, "%s\t%s\tu%s\t%s\t%s\t%s\t%s\n",
			a.Name, dash(a.Nick), dash(a.ProfileID), state,
			fmtTime(a.UpdatedAt), fmtTimeOrDash(a.LastOKAt), dash(a.Note))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fmtTimeOrDash(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return fmtTime(t)
}

// accountCheck — жива ли сессия. Заодно ловит смену ника и бан: сайт под
// мёртвой сессией отвечает как гостю.
func accountCheck(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("check", nil)
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()

	names := []string{*f.name}
	if *f.name == "" {
		list, err := db.List(ctx)
		if err != nil {
			return err
		}
		names = names[:0]
		for _, a := range list {
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		fmt.Println("аккаунтов нет")
		return nil
	}
	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
	for _, name := range names {
		if err := checkAccount(ctx, db, client, name); err != nil {
			return err
		}
	}
	return nil
}

func checkAccount(ctx context.Context, db *acct.Store, client *love.Client, name string) error {
	a, cookiesJSON, err := db.Get(ctx, name)
	if err != nil {
		return err
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		return err
	}
	profileID, passportID, nick, err := client.SiteIdentity(ctx, cookies)
	if err != nil {
		fmt.Printf("%s: сессия не работает (%v)\n", name, err)
		return db.SetValid(ctx, name, false, time.Now())
	}
	if nick != a.Nick && a.Nick != "" {
		fmt.Printf("%s: ник сменился, было %q\n", name, a.Nick)
	}
	if err := db.SetIdentity(ctx, name, profileID, passportID, nick); err != nil {
		return err
	}
	if err := db.SetValid(ctx, name, true, time.Now()); err != nil {
		return err
	}
	fmt.Printf("%s: жива — %s, u%s\n", name, nick, profileID)
	return nil
}

func accountForget(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("forget", nil)
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	if *f.name == "" {
		return fmt.Errorf("account forget: нужен -name")
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()
	ok, err := db.Forget(ctx, *f.name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("аккаунта %q нет", *f.name)
	}
	fmt.Printf("аккаунт %s удалён вместе с куками\n", *f.name)
	return nil
}

// accountCookie отдаёт заголовок Cookie локальным скриптам (обход анкет для
// досье написан на Python, а расшифровать AES стандартной библиотекой он не
// может). Пишем ТОЛЬКО в пайп: печать сессии на экран — прямой путь к тому,
// чтобы она осела в скроллбеке или в чьём-нибудь скриншоте.
func accountCookie(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("cookie", nil)
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	if *f.name == "" {
		return fmt.Errorf("account cookie: нужен -name")
	}
	if isTerminal(os.Stdout) {
		return fmt.Errorf("account cookie пишет только в пайп или файл: " +
			"куки не должны попадать на экран (пример: lovegw account cookie -name reserve | скрипт)")
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()
	_, cookiesJSON, err := db.Get(ctx, *f.name)
	if err != nil {
		return err
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		return err
	}
	if len(cookies) == 0 {
		return fmt.Errorf("у аккаунта %q не осталось живых кук — нужен account login", *f.name)
	}
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	fmt.Println(strings.Join(parts, "; "))
	return nil
}

// isTerminal — вывод идёт на экран, а не в пайп/файл.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// siteCookies — под какой сессией идёт сервисный обход.
//
// accounts — имена сервисных аккаунтов через запятую: берётся первый валидный,
// в этом и состоит резерв (основную анкету заблокировали — обход продолжается
// со следующей). Помеченный невалидным аккаунт пропускается: оживить его
// заново — дело `account check` или `account login`. Пусто — прежнее поведение:
// сессия админа из боевой БД (там флаг прощается, см. adminCookies).
// Вторым значением возвращается, КТО пошёл на сайт: это пишется в лог, чтобы
// потом не гадать, под кем ушли запросы.
func siteCookies(ctx context.Context, cfg *config.Config, accounts, messenger string, userID int64, allowInvalid bool) ([]*http.Cookie, string, error) {
	if strings.TrimSpace(accounts) == "" {
		cookies, err := adminCookies(ctx, cfg, messenger, userID, allowInvalid)
		return cookies, "сессия админа из боевой БД", err
	}
	db, err := openAccounts(ctx, cfg, "")
	if err != nil {
		return nil, "", err
	}
	defer db.Close()

	var problems []string
	for _, name := range strings.Split(accounts, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		cookies, title, why, err := accountCookies(ctx, db, name)
		if err != nil {
			return nil, "", err
		}
		if why != "" {
			problems = append(problems, name+": "+why)
			continue
		}
		return cookies, title, nil
	}
	return nil, "", fmt.Errorf("ни один аккаунт из %q не годится: %s", accounts, strings.Join(problems, "; "))
}

// accountSay — комментарий на сайт от имени сервисного аккаунта: инструмент
// проверки гипотез (как площадка отвечает на такое-то действие), а не способ
// вести переписку. Отсюда подтверждение перед отправкой и печать id своей
// реплики — по нему потом сверяют, что с ней стало.
func accountSay(ctx context.Context, args []string) error {
	fs, f := accountFlagSet("say", map[string]bool{"note": true, "reply": true})
	noteID := fs.String("note", "", "id заметки")
	replyTo := fs.Int64("reply", 0, "id комментария, которому отвечаем (0 — в корень заметки)")
	noPrefix := fs.Bool("no-prefix", false, "не подставлять обращение «Ник, …»")
	yes := fs.Bool("yes", false, "не спрашивать подтверждения")
	if err := fs.Parse(reorderArgs(args, f.valueArgs)); err != nil {
		return err
	}
	if *f.name == "" || *noteID == "" {
		return fmt.Errorf("account say: нужны -name и -note")
	}
	text, err := sayText(fs.Args())
	if err != nil {
		return err
	}
	cfg, err := config.Load(*f.cfgPath)
	if err != nil {
		return err
	}
	db, err := openAccounts(ctx, cfg, *f.accounts)
	if err != nil {
		return err
	}
	defer db.Close()
	cookies, title, why, err := accountCookies(ctx, db, *f.name)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("аккаунт %s: %s", *f.name, why)
	}

	log := newLogger(cfg.LogLevel)
	client := love.New(cfg.Site.BaseURL, cfg.Site.UserAgent,
		time.Duration(cfg.Site.RequestIntervalMS)*time.Millisecond, log)
	page, err := client.FetchCommentsPage(ctx, *noteID)
	if err != nil {
		return fmt.Errorf("заметка %s: %w", *noteID, err)
	}
	if !*noPrefix {
		text = withAddressPrefix(page, *replyTo, text)
	}
	if err := confirmSay(title, *noteID, *replyTo, text, page, *yes); err != nil {
		return err
	}

	comAPIID := ""
	if *replyTo != 0 {
		comAPIID = fmt.Sprint(*replyTo)
	}
	if err := client.PostComment(ctx, cookies, *noteID, comAPIID, text); err != nil {
		return fmt.Errorf("комментарий не ушёл: %w", err)
	}
	if err := db.SetValid(ctx, *f.name, true, time.Now()); err != nil {
		return err
	}
	fmt.Println("отправлено")
	return reportPostedComment(ctx, client, cfg.Site.BaseURL, *noteID, page)
}

// confirmSay показывает, что именно и от кого уйдёт, и ждёт слова «да».
func confirmSay(title, noteID string, replyTo int64, text string, page love.CommentsPage, yes bool) error {
	fmt.Printf("от кого: %s\n", title)
	fmt.Printf("куда:    заметка %s", noteID)
	if page.Note != nil {
		fmt.Printf(" — %s: %s", page.Note.AuthorName, headline(page.Note.Text))
	}
	fmt.Println()
	if replyTo != 0 {
		fmt.Printf("ответ:   комментарию %d\n", replyTo)
	}
	fmt.Printf("текст:   %s\n", text)
	fmt.Println("\nКомментарий появится на сайте и уйдёт в каналы зеркалом; отозвать его нечем.")
	if yes {
		return nil
	}
	fmt.Print("отправляем? (да/нет): ")
	answer, err := readLine(bufio.NewReader(os.Stdin))
	if err != nil {
		return err
	}
	if strings.ToLower(strings.TrimSpace(answer)) != "да" {
		return fmt.Errorf("отменено")
	}
	return nil
}

// reportPostedComment ищет свою свежую реплику и печатает её id: это готовый
// якорь, чтобы потом проверить, удалили её или нет.
func reportPostedComment(ctx context.Context, client *love.Client, baseURL, noteID string, before love.CommentsPage) error {
	seen := make(map[int64]bool, len(before.Comments))
	for _, c := range before.Comments {
		seen[c.ID] = true
	}
	after, err := client.FetchCommentsPage(ctx, noteID)
	if err != nil {
		fmt.Println("id реплики не установлен: тред не перечитался —", err)
		return nil
	}
	for _, c := range after.Comments {
		if !seen[c.ID] {
			fmt.Printf("реплика %d: %s/notes/comments/%s#anchor-%d\n",
				c.ID, strings.TrimRight(baseURL, "/"), noteID, c.ID)
			return nil
		}
	}
	fmt.Println("новой реплики в треде не видно: сайт мог отправить её на премодерацию")
	return nil
}

// withAddressPrefix ставит обращение «Ник, …» так же, как это делает мост:
// адресата на сайте узнают по префиксу, а не по дереву — дерево показывает лишь
// корень ветки (см. love.AddressPrefix).
func withAddressPrefix(page love.CommentsPage, replyTo int64, text string) string {
	if replyTo == 0 || love.AddressPrefix(text) != "" {
		return text
	}
	for _, c := range page.Comments {
		if c.ID == replyTo && c.AuthorName != "" {
			return c.AuthorName + ", " + text
		}
	}
	return text
}

// sayText берёт текст комментария из аргументов или, если их нет, со stdin —
// так удобнее вставлять многострочное.
func sayText(args []string) (string, error) {
	text := strings.TrimSpace(strings.Join(args, " "))
	if text == "" {
		if isTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "текст комментария (закончить Ctrl+Z / Ctrl+D):")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return "", fmt.Errorf("account say: пустой текст")
	}
	return text, nil
}

// headline — первая строка текста, обрезанная под одну строку экрана.
func headline(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	r := []rune(text)
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return text
}

// readStdinText читает текст комментария со stdin (когда он не задан
// аргументом): так удобнее вставлять многострочное.
func readStdinText() (string, error) {
	if isTerminal(os.Stdin) {
		fmt.Fprintln(os.Stderr, "текст комментария (закончить Ctrl+Z / Ctrl+D):")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// accountCookies — годится ли аккаунт для обхода. why != "" — не годится, но
// это не ошибка: следующий в списке может подойти, в этом смысл резерва.
func accountCookies(ctx context.Context, db *acct.Store, name string) (cookies []*http.Cookie, title, why string, err error) {
	a, cookiesJSON, err := db.Get(ctx, name)
	if errors.Is(err, acct.ErrNotFound) {
		return nil, "", "нет такого аккаунта", nil
	}
	if err != nil {
		return nil, "", "", err
	}
	if !a.Valid {
		return nil, "", "сессия помечена невалидной", nil
	}
	if cookies, err = love.CookiesFromJSON([]byte(cookiesJSON), time.Now()); err != nil {
		return nil, "", "", err
	}
	if len(cookies) == 0 {
		return nil, "", "куки истекли", nil
	}
	return cookies, a.Title(), "", nil
}
