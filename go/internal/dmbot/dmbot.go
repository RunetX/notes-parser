// Пакет dmbot — ЛС-бот РюмкинЪ: вход на сайт, публикация заметок,
// проверка сессии, подписки. Диалоговое ядро мессенджер-агностично
// (logic.go, состояния в dialog_states переживают рестарт); здесь —
// телеграм-обёртка над go-telegram/bot.
package dmbot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"lovegw/internal/alerts"
	"lovegw/internal/kbd"
	"lovegw/internal/news"
	"lovegw/internal/store"
)

// Состояния диалога (хранятся в dialog_states).
const (
	stateAwaitCredentials = "await_credentials"
	stateAwaitNote        = "await_note"
	stateAwaitAnonNote    = "await_anon_note"
	stateAwaitNews        = "await_news"
	// stateAwaitNoteKind — заметка ждёт выбора авторства кнопкой. Отдельное
	// состояние, а не пустота между командой и нажатием: иначе набранный вместо
	// нажатия текст получил бы «Не понимаю».
	stateAwaitNoteKind = "await_note_kind"
	// stateAwaitSubscription — ждём ключевое слово подписки (кнопка «Добавить»
	// или /subscribe без аргумента).
	stateAwaitSubscription = "await_subscription"
	// statePMPrefix + <peer_id> — залипание на диалоге talks (/talk): текст без
	// команды уходит выбранному собеседнику.
	statePMPrefix = "pm:"
	// stateNewsPrefix + <id новости> + "\n" + <html> — новость админа ждёт
	// подтверждения. Текст лежит прямо в состоянии, поэтому черновик
	// переживает рестарт демона, а id — повторную попытку после сбоя одного
	// из каналов (уже отправленные не задваиваются).
	stateNewsPrefix = "news:"
)

const (
	msgInternalError  = "Внутренняя ошибка, попробуйте позже"
	msgUnknownCommand = "Неизвестная команда. Наберите /start"
	msgTalksOff       = "Личная переписка сайта не подключена."
	msgNoteGone       = "Не нашёл эту заметку — возможно, она уже ушла из ленты."
	msgSubLimit       = "Больше " + subLimitText + " подписок я не держу. Снимите лишние: /mysubs"
)

// subLimitText — предел числа подписок для текста отказа: держим его строкой,
// чтобы сообщение оставалось константой (значение — store.SubscriptionLimit,
// сходство стережёт тест).
const subLimitText = "50"

// Приглашения к вводу. Вынесены в константы: к ним ведут две дороги — команда
// и нажатие кнопки, и текст должен быть один.
const (
	msgAskCredentials  = "Для входа на сайт отправьте логин и пароль через пробел"
	msgAskNote         = "Отправьте текст заметки"
	msgAskAnonNote     = "Отправьте текст анонимной заметки"
	msgAskNoteKind     = "Заметка от своего имени или анонимно?"
	msgAskSubKind      = "На что подписать?"
	msgAskSubscription = "Пришлите слово: уведомлю, когда оно встретится в комментарии.\n" +
		"На автора или на конкретную заметку подписывает кнопка «🔔 Подписаться» под постом в канале."
)

// SiteAuth — то, что боту нужно от клиента сайта.
type SiteAuth interface {
	Login(ctx context.Context, login, password string) ([]*http.Cookie, error)
	PostNote(ctx context.Context, cookies []*http.Cookie, text string, anonymous bool) error
}

type Bot struct {
	b     *bot.Bot
	logic *Logic
	talks TalkRouter
	log   *slog.Logger

	// pollAlert — алерт о полосе сбоев поллинга (nil — уведомлять некому);
	// ставится в wire-фазе runDaemon, читается обработчиком ошибок бота.
	pmu       sync.Mutex
	pollAlert *alerts.PollWatch
}

// TalkRouter — личная переписка сайта (talks): маршрутизация ответа-реплая на
// сайт и отправка в залипший диалог (/talk). Реализуется talks.Watcher; может
// быть nil (переписка выключена). Ставится в runDaemon после сборки поллера.
type TalkRouter interface {
	HandleReply(ctx context.Context, messenger, replyMsgID string, userID int64, replyToID, text string) bool
	SendToDialog(ctx context.Context, messenger string, userID, peerID int64, ackID, text string) bool
}

// New создаёт ЛС-бота команд (РюмкинЪ). httpClient (может быть nil) задаёт
// соединение с Bot API через прокси.
func New(token string, st *store.Store, site SiteAuth, httpClient *http.Client, log *slog.Logger) (*Bot, error) {
	return newBot(token, httpClient, log, func(b *bot.Bot, log *slog.Logger) *Logic {
		return NewLogic(st, site, tgTransport{b: b, log: log}, store.MessengerTelegram, log)
	})
}

// NewTalks создаёт бота личной переписки: только /talks, /talk и доставка ЛС
// сайта. Сайт ему не нужен — вход и заметки живут у бота команд.
func NewTalks(token string, st *store.Store, httpClient *http.Client, log *slog.Logger) (*Bot, error) {
	return newBot(token, httpClient, log, func(b *bot.Bot, log *slog.Logger) *Logic {
		return NewTalksLogic(st, tgTransport{b: b, log: log}, store.MessengerTelegram, log)
	})
}

func newBot(token string, httpClient *http.Client, log *slog.Logger,
	newLogic func(*bot.Bot, *slog.Logger) *Logic) (*Bot, error) {
	if log == nil {
		log = slog.Default()
	}
	d := &Bot{log: log}
	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, u *models.Update) {
			d.handle(ctx, u)
		}),
		// Ошибки поллинга библиотека сама только ретраит — вечный 409 или
		// протухший токен оставили бы бота молча мёртвым. Считаем полосу
		// сбоев для алерта (см. tgx: та же схема у постера).
		bot.WithErrorsHandler(func(err error) {
			log.Warn("dm bot", "err", err)
			if w := d.pollWatch(); w != nil {
				w.Error(context.Background(), err)
			}
		}),
	}
	if httpClient != nil {
		opts = append(opts, bot.WithHTTPClient(30*time.Second, httpClient))
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, err
	}
	d.b = b
	d.logic = newLogic(b, log)
	return d, nil
}

// Start запускает long polling; блокируется до отмены контекста.
func (d *Bot) Start(ctx context.Context) { d.b.Start(ctx) }

// SetPollAlert подключает алерт о полосе сбоев поллинга. name различает ботов
// в тексте алерта. Как и все Set*, зовётся до Start (wire-фаза runDaemon).
func (d *Bot) SetPollAlert(name string, send func(ctx context.Context, text string)) {
	d.pmu.Lock()
	defer d.pmu.Unlock()
	d.pollAlert = alerts.NewPollWatch(name, send)
}

func (d *Bot) pollWatch() *alerts.PollWatch {
	d.pmu.Lock()
	defer d.pmu.Unlock()
	return d.pollAlert
}

// SetTalkRouter подключает роутер личной переписки целиком: и маршрутизацию
// реплаев, и команды /talks, /talk (в runDaemon после сборки поллера talks).
func (d *Bot) SetTalkRouter(r TalkRouter) {
	d.talks = r
	d.logic.talks = r
}

// SetNews подключает публикацию новостей проекта админом (/news).
func (d *Bot) SetNews(svc *news.Service, adminID int64) { d.logic.SetNews(svc, adminID) }

// SetPulpit подключает ручку амвона админом (/pulpit).
func (d *Bot) SetPulpit(svc PulpitControl, adminID int64) { d.logic.SetPulpit(svc, adminID) }

// AskTalksScan спрашивает согласия читать личную переписку (зовёт поллер talks,
// увидев сайт-аккаунт без согласия).
func (d *Bot) AskTalksScan(ctx context.Context, userID int64, alsoElsewhere bool) {
	d.logic.AskTalksScan(ctx, userID, alsoElsewhere)
}

// PublishCommands публикует меню команд бота. Зовётся после SetTalkRouter:
// от него зависит, попадут ли в список /talks и /talk.
func (d *Bot) PublishCommands(ctx context.Context) { d.logic.PublishCommands(ctx) }

// SetReplyRouter подключает только маршрутизацию реплаев, без команд. Нужен
// боту команд, когда переписку ведёт отдельный бот: ЛС, доставленные раньше,
// привязаны в message_targets к сообщениям бота команд, и ответ на них
// прилетит именно ему. В /start при этом /talks не появляется.
func (d *Bot) SetReplyRouter(r TalkRouter) { d.talks = r }

// Name — имя мессенджера (talks.PMTransport).
func (d *Bot) Name() string { return store.MessengerTelegram }

// SendPM доставляет входящее ЛС talks в личку пользователя (talks.PMTransport).
// Возвращает id сообщения — по нему message_targets свяжет реплай с диалогом.
func (d *Bot) SendPM(ctx context.Context, userID int64, html string) (string, error) {
	disable := true
	m, err := d.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             userID,
		Text:               html,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: &disable},
	})
	if err != nil {
		return "", err
	}
	return strconv.Itoa(m.ID), nil
}

// Confirm подтверждает отправку реакцией на сообщение пользователя
// (👍 — ушло на сайт, 👎 — не удалось). Лучшая попытка: ошибку реакции не
// эскалируем.
func (d *Bot) Confirm(ctx context.Context, userID int64, msgID string, ok bool) {
	id, err := strconv.Atoi(msgID)
	if err != nil {
		return
	}
	emoji := "👍"
	if !ok {
		emoji = "👎"
	}
	if _, err := d.b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    userID,
		MessageID: id,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
		}},
	}); err != nil {
		d.log.Debug("реакция-подтверждение не поставлена", "user", userID, "err", err)
	}
}

// Username — юзернейм бота (getMe), без «@». Нужен постеру канала: под каждой
// заметкой он вешает deep-link в этот ЛС. Ошибка не фатальна — просто не будет
// кнопки.
func (d *Bot) Username(ctx context.Context) (string, error) {
	me, err := d.b.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("getMe ЛС-бота: %w", err)
	}
	return me.Username, nil
}

// Notify отправляет пользователю личное сообщение (используется мостом
// для уведомлений о протухшей сессии и подписок).
func (d *Bot) Notify(ctx context.Context, tgUserID int64, text string) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: tgUserID, Text: text}); err != nil {
		d.log.Warn("уведомление пользователя не отправлено", "user", tgUserID, "err", err)
	}
}

// NotifyHTML отправляет личное сообщение с HTML-разметкой и необязательными
// кнопками (уведомление подписчика: имя автора ссылкой, выдержка комментария и
// «Отписаться»). Превью ссылок выключено — иначе телеграм подтягивает карточку
// сайта под каждым уведомлением.
func (d *Bot) NotifyHTML(ctx context.Context, tgUserID int64, html string, kb *kbd.Keyboard) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             tgUserID,
		Text:               html,
		ParseMode:          models.ParseModeHTML,
		ReplyMarkup:        tgMarkup(kb),
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}); err != nil {
		d.log.Warn("уведомление пользователя не отправлено", "user", tgUserID, "err", err)
	}
}

// handle обрабатывает входящее ЛС.
func (d *Bot) handle(ctx context.Context, u *models.Update) {
	// Нажатие кнопки — до проверки на сообщение: у callback_query его нет.
	if u.CallbackQuery != nil {
		d.handleCallback(ctx, u.CallbackQuery)
		return
	}
	msg := u.Message
	if msg == nil || msg.Chat.Type != models.ChatTypePrivate || msg.Text == "" {
		return
	}
	// Реплай на доставленное ЛС talks → на сайт (до диалоговой логики команд).
	if d.talks != nil && msg.ReplyToMessage != nil &&
		d.talks.HandleReply(ctx, store.MessengerTelegram, strconv.Itoa(msg.ID),
			msg.Chat.ID, strconv.Itoa(msg.ReplyToMessage.ID), msg.Text) {
		return
	}
	d.logic.HandleText(ctx, msg.Chat.ID, strconv.Itoa(msg.ID), msg.Text)
}

// handleCallback обрабатывает нажатие кнопки. Сообщение с клавиатурой приходит
// как MaybeInaccessibleMessage: у слишком старого доступен только id, и тогда
// правку текста пропускаем — роутер отвечает нажавшему в любом случае.
func (d *Bot) handleCallback(ctx context.Context, cq *models.CallbackQuery) {
	var msgID string
	if m := cq.Message.Message; m != nil {
		msgID = strconv.Itoa(m.ID)
	}
	d.logic.HandleCallback(ctx, cq.From.ID, kbd.Callback{
		AnswerID:  cq.ID,
		MessageID: msgID,
		Payload:   cq.Data,
	})
}

// tgTransport — телеграм-транспорт диалогового ядра.
type tgTransport struct {
	b   *bot.Bot
	log *slog.Logger
}

func (t tgTransport) Send(ctx context.Context, userID int64, text string) {
	if _, err := t.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: userID, Text: text}); err != nil {
		t.log.Warn("отправка ЛС не удалась", "user", userID, "err", err)
	}
}

func (t tgTransport) DeleteMessage(ctx context.Context, userID int64, messageID string) {
	id, err := strconv.Atoi(messageID)
	if err != nil {
		t.log.Error("не телеграмный id сообщения", "id", messageID)
		return
	}
	if _, err := t.b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: userID, MessageID: id}); err != nil {
		// Не критично: у бота может не быть права удалять, или сообщение старое.
		t.log.Debug("не удалось удалить сообщение с логином", "user", userID, "err", err)
	}
}

func (t tgTransport) SendKeyboard(ctx context.Context, userID int64, text string, kb *kbd.Keyboard) {
	if _, err := t.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      userID,
		Text:        text,
		ReplyMarkup: tgMarkup(kb),
	}); err != nil {
		t.log.Warn("отправка ЛС с кнопками не удалась", "user", userID, "err", err)
	}
}

func (t tgTransport) AnswerCallback(ctx context.Context, cb kbd.Callback, toast string) {
	if _, err := t.b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.AnswerID,
		Text:            toast,
	}); err != nil {
		// Нажатие живёт недолго: протухший callback_query — обычное дело.
		t.log.Debug("ответ на нажатие не прошёл", "err", err)
	}
}

func (t tgTransport) EditMessage(ctx context.Context, userID int64, messageID, text string, kb *kbd.Keyboard) {
	id, err := strconv.Atoi(messageID)
	if err != nil {
		t.log.Error("не телеграмный id сообщения", "id", messageID)
		return
	}
	if _, err := t.b.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:      userID,
		MessageID:   id,
		Text:        text,
		ReplyMarkup: tgMarkup(kb),
	}); err != nil {
		// Штатные отказы: «message is not modified» на повторном нажатии той же
		// кнопки и слишком старое сообщение. Обоим место в debug.
		t.log.Debug("правка сообщения не прошла", "user", userID, "msg", messageID, "err", err)
	}
}

func (t tgTransport) SetCommands(ctx context.Context, cmds []kbd.Command) {
	list := make([]models.BotCommand, 0, len(cmds))
	for _, c := range cmds {
		list = append(list, models.BotCommand{Command: c.Name, Description: c.Description})
	}
	if _, err := t.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: list}); err != nil {
		// Не фатально: команды просто не появятся в меню клиента.
		t.log.Warn("меню команд не опубликовано", "err", err)
	}
}

// tgMarkup переводит общую клавиатуру в телеграмную. Пустая — нетипизированный
// nil: reply_markup с omitempty пропадёт из запроса, и Telegram снимет кнопки.
func tgMarkup(kb *kbd.Keyboard) models.ReplyMarkup {
	if kb.Empty() {
		return nil
	}
	rows := make([][]models.InlineKeyboardButton, 0, len(kb.Rows))
	for _, row := range kb.Rows {
		btns := make([]models.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			btns = append(btns, models.InlineKeyboardButton{Text: b.Text, CallbackData: b.Payload})
		}
		rows = append(rows, btns)
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// command вычленяет команду из начала сообщения (учитывает форму /cmd@botname).
func command(text string) string {
	f := strings.Fields(text)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return ""
	}
	cmd := f[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	return cmd
}

// commandArg возвращает аргумент команды — всё, что идёт после первого слова
// (ключевое слово подписки может содержать пробелы).
func commandArg(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, " \t\n"); i >= 0 {
		return strings.TrimSpace(text[i+1:])
	}
	return ""
}

// startMessage собирает приветствие под роль бота: у бота переписки свой
// короткий список, у бота команд строки про диалоги появляются только когда
// переписку обслуживает он же (talks-роутер подключён).
func startMessage(talksOnly, withTalks, withProfile bool) string {
	if talksOnly {
		return `Привет! Я бот личной переписки НГС.Лав. Я умею:
/talks — мои диалоги на сайте
/talk <номер> — писать в выбранный диалог
/delivery — личные сообщения: читать ли их и куда слать
/cancel — выйти из диалога
Ответить на входящее ЛС можно просто реплаем.
Вход на сайт, заметки и подписки — у основного бота (там же /login).
То же самое — кнопками ниже.`
	}
	msg := `Привет! Меня зовут РюмкинЪ. Я умею:
/login — войти на сайт НГС.Лав
/add_note — добавить заметку
/add_anonymous_note — добавить анонимную заметку
/status — проверить сессию сайта
/subscribe <слово> — уведомлять о комментариях с этим словом
/unsubscribe <слово> — отписаться от слова
/mysubs — мои подписки`
	if withProfile {
		msg += "\n/profile — моя анкета на сайте: заблокировать или вернуть"
	}
	if withTalks {
		msg += "\n/talks — мои личные диалоги на сайте\n/talk <номер> — писать в выбранный диалог" +
			"\n/delivery — личные сообщения: читать ли их и куда слать"
	}
	return msg + "\nТо же самое — кнопками ниже."
}
