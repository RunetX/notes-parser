// Пакет dmbot — ЛС-бот РюмкинЪ: вход на сайт, публикация заметок,
// проверка сессии, подписки. Диалоговое ядро мессенджер-агностично
// (logic.go, состояния в dialog_states переживают рестарт); здесь —
// телеграм-обёртка над go-telegram/bot.
package dmbot

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"lovegw/internal/store"
)

// Состояния диалога (хранятся в dialog_states).
const (
	stateAwaitCredentials = "await_credentials"
	stateAwaitNote        = "await_note"
	stateAwaitAnonNote    = "await_anon_note"
	// statePMPrefix + <peer_id> — залипание на диалоге talks (/talk): текст без
	// команды уходит выбранному собеседнику.
	statePMPrefix = "pm:"
)

const msgInternalError = "Внутренняя ошибка, попробуйте позже"

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

// SetTalkRouter подключает роутер личной переписки целиком: и маршрутизацию
// реплаев, и команды /talks, /talk (в runDaemon после сборки поллера talks).
func (d *Bot) SetTalkRouter(r TalkRouter) {
	d.talks = r
	d.logic.talks = r
}

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

// Notify отправляет пользователю личное сообщение (используется мостом
// для уведомлений о протухшей сессии и подписок).
func (d *Bot) Notify(ctx context.Context, tgUserID int64, text string) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: tgUserID, Text: text}); err != nil {
		d.log.Warn("уведомление пользователя не отправлено", "user", tgUserID, "err", err)
	}
}

// NotifyHTML отправляет личное сообщение с HTML-разметкой (уведомление
// подписчика: имя автора ссылкой, выдержка комментария). Превью ссылок
// выключено — иначе телеграм подтягивает карточку сайта под каждым уведомлением.
func (d *Bot) NotifyHTML(ctx context.Context, tgUserID int64, html string) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:             tgUserID,
		Text:               html,
		ParseMode:          models.ParseModeHTML,
		LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
	}); err != nil {
		d.log.Warn("уведомление пользователя не отправлено", "user", tgUserID, "err", err)
	}
}

// handle обрабатывает входящее ЛС.
func (d *Bot) handle(ctx context.Context, u *models.Update) {
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
func startMessage(talksOnly, withTalks bool) string {
	if talksOnly {
		return `Привет! Я бот личной переписки НГС.Лав. Я умею:
/talks — мои диалоги на сайте
/talk <номер> — писать в выбранный диалог
/cancel — выйти из диалога
Ответить на входящее ЛС можно просто реплаем.
Вход на сайт, заметки и подписки — у основного бота (там же /login).`
	}
	msg := `Привет! Меня зовут РюмкинЪ. Я умею:
/login — войти на сайт НГС.Лав
/add_note — добавить заметку
/add_anonymous_note — добавить анонимную заметку
/status — проверить сессию сайта
/subscribe <слово> — уведомлять о комментариях с этим словом
/unsubscribe <слово> — отписаться от слова
/mysubs — мои подписки`
	if withTalks {
		msg += "\n/talks — мои личные диалоги на сайте\n/talk <номер> — писать в выбранный диалог"
	}
	return msg
}
