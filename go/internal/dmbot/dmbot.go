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
	log   *slog.Logger
}

// New создаёт ЛС-бота. httpClient (может быть nil) задаёт соединение с
// Bot API через прокси.
func New(token string, st *store.Store, site SiteAuth, httpClient *http.Client, log *slog.Logger) (*Bot, error) {
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
	d.logic = NewLogic(st, site, tgTransport{b: b, log: log}, store.MessengerTelegram, log)
	return d, nil
}

// Start запускает long polling; блокируется до отмены контекста.
func (d *Bot) Start(ctx context.Context) { d.b.Start(ctx) }

// Notify отправляет пользователю личное сообщение (используется мостом
// для уведомлений о протухшей сессии и подписок).
func (d *Bot) Notify(ctx context.Context, tgUserID int64, text string) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: tgUserID, Text: text}); err != nil {
		d.log.Warn("уведомление пользователя не отправлено", "user", tgUserID, "err", err)
	}
}

// handle обрабатывает входящее ЛС.
func (d *Bot) handle(ctx context.Context, u *models.Update) {
	msg := u.Message
	if msg == nil || msg.Chat.Type != models.ChatTypePrivate || msg.Text == "" {
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

const startMessage = `Привет! Меня зовут РюмкинЪ. Я умею:
/login — войти на сайт НГС.Лав
/add_note — добавить заметку
/add_anonymous_note — добавить анонимную заметку
/status — проверить сессию сайта
/subscribe <слово> — уведомлять о комментариях с этим словом
/unsubscribe <слово> — отписаться от слова
/mysubs — мои подписки`
