// Пакет dmbot — ЛС-бот РюмкинЪ: вход на сайт, публикация заметок,
// проверка сессии. Состояние диалога хранится в БД и переживает рестарт.
package dmbot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"lovegw/internal/love"
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
	b    *bot.Bot
	st   *store.Store
	site SiteAuth
	log  *slog.Logger
}

// New создаёт ЛС-бота. httpClient (может быть nil) задаёт соединение с
// Bot API через прокси.
func New(token string, st *store.Store, site SiteAuth, httpClient *http.Client, log *slog.Logger) (*Bot, error) {
	if log == nil {
		log = slog.Default()
	}
	d := &Bot{st: st, site: site, log: log}
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
	return d, nil
}

// Start запускает long polling; блокируется до отмены контекста.
func (d *Bot) Start(ctx context.Context) { d.b.Start(ctx) }

// Notify отправляет пользователю личное сообщение (используется мостом
// для уведомлений о протухшей сессии).
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
	chatID := msg.Chat.ID

	state, err := d.st.DialogState(ctx, chatID)
	if err != nil {
		d.log.Error("чтение состояния диалога", "user", chatID, "err", err)
		return
	}

	// Команда прерывает любой текущий диалог.
	if cmd := command(msg.Text); cmd != "" {
		if state != "" {
			_ = d.st.ClearDialogState(ctx, chatID)
		}
		d.handleCommand(ctx, chatID, cmd, msg)
		return
	}
	if state == "" {
		d.send(ctx, chatID, "Не понимаю. Наберите /start, чтобы увидеть команды")
		return
	}
	d.handleStateInput(ctx, chatID, state, msg)
}

func (d *Bot) handleCommand(ctx context.Context, chatID int64, cmd string, msg *models.Message) {
	switch cmd {
	case "/start":
		d.send(ctx, chatID, startMessage)
	case "/login":
		d.setState(ctx, chatID, stateAwaitCredentials)
		d.send(ctx, chatID, "Для входа на сайт отправьте логин и пароль через пробел")
	case "/add_note":
		d.setState(ctx, chatID, stateAwaitNote)
		d.send(ctx, chatID, "Отправьте текст заметки")
	case "/add_anonymous_note":
		d.setState(ctx, chatID, stateAwaitAnonNote)
		d.send(ctx, chatID, "Отправьте текст анонимной заметки")
	case "/status":
		d.handleStatus(ctx, chatID)
	default:
		d.send(ctx, chatID, "Неизвестная команда. Наберите /start")
	}
}

func (d *Bot) handleStateInput(ctx context.Context, chatID int64, state string, msg *models.Message) {
	switch state {
	case stateAwaitCredentials:
		d.tryLogin(ctx, chatID, msg)
	case stateAwaitNote:
		d.addNote(ctx, chatID, msg.Text, false)
		_ = d.st.ClearDialogState(ctx, chatID)
	case stateAwaitAnonNote:
		d.addNote(ctx, chatID, msg.Text, true)
		_ = d.st.ClearDialogState(ctx, chatID)
	}
}

// tryLogin входит на сайт и сохраняет куки. Сообщение с логином/паролем
// удаляется — в отличие от Python-версии, где пароль оставался в истории.
func (d *Bot) tryLogin(ctx context.Context, chatID int64, msg *models.Message) {
	d.deleteMessage(ctx, chatID, msg.ID)

	parts := strings.Fields(msg.Text)
	if len(parts) != 2 {
		d.send(ctx, chatID, "Нужны логин и пароль через пробел. Попробуйте ещё раз или /login")
		return
	}
	cookies, err := d.site.Login(ctx, parts[0], parts[1])
	if err != nil {
		var le *love.LoginError
		if errors.As(err, &le) {
			d.send(ctx, chatID, "Вход не выполнен: "+le.Errors)
		} else {
			d.log.Warn("ошибка входа", "user", chatID, "err", err)
			d.send(ctx, chatID, "Не удалось войти, попробуйте позже. Начать заново: /login")
		}
		return
	}
	cookiesJSON, err := love.CookiesToJSON(cookies, time.Now())
	if err != nil {
		d.log.Error("сериализация кук", "user", chatID, "err", err)
		d.send(ctx, chatID, msgInternalError)
		return
	}
	if err := d.st.UpsertSession(ctx, chatID, cookiesJSON, time.Now()); err != nil {
		d.log.Error("сохранение сессии", "user", chatID, "err", err)
		d.send(ctx, chatID, "Не удалось сохранить сессию, попробуйте позже")
		return
	}
	_ = d.st.ClearDialogState(ctx, chatID)
	d.send(ctx, chatID, "Успешный вход. Теперь ваши ответы в обсуждениях попадут на сайт")
}

func (d *Bot) addNote(ctx context.Context, chatID int64, text string, anonymous bool) {
	cookiesJSON, valid, err := d.st.SessionCookies(ctx, chatID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !valid) {
		d.send(ctx, chatID, "Сначала войдите на сайт: /login")
		return
	}
	if err != nil {
		d.log.Error("чтение сессии", "user", chatID, "err", err)
		d.send(ctx, chatID, msgInternalError)
		return
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil || len(cookies) == 0 {
		_ = d.st.SetSessionValid(ctx, chatID, false, time.Now())
		d.send(ctx, chatID, "Сессия истекла. Сделайте /login ещё раз")
		return
	}
	if err := d.site.PostNote(ctx, cookies, text, anonymous); err != nil {
		d.log.Warn("публикация заметки не удалась", "user", chatID, "err", err)
		d.send(ctx, chatID, "Не удалось опубликовать заметку, попробуйте позже")
		return
	}
	_ = d.st.SetSessionValid(ctx, chatID, true, time.Now())
	d.send(ctx, chatID, "Заметка отправлена на сайт")
}

func (d *Bot) handleStatus(ctx context.Context, chatID int64) {
	_, valid, err := d.st.SessionCookies(ctx, chatID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.send(ctx, chatID, "Сессия не найдена. Для отправки на сайт сделайте /login")
	case err != nil:
		d.log.Error("чтение сессии", "user", chatID, "err", err)
		d.send(ctx, chatID, msgInternalError)
	case !valid:
		d.send(ctx, chatID, "Сессия истекла. Сделайте /login ещё раз")
	default:
		d.send(ctx, chatID, "Сессия активна. Можно отправлять заметки и ответы")
	}
}

func (d *Bot) setState(ctx context.Context, chatID int64, state string) {
	if err := d.st.SetDialogState(ctx, chatID, state, time.Now()); err != nil {
		d.log.Error("сохранение состояния диалога", "user", chatID, "err", err)
	}
}

func (d *Bot) send(ctx context.Context, chatID int64, text string) {
	if _, err := d.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		d.log.Warn("отправка ЛС не удалась", "user", chatID, "err", err)
	}
}

func (d *Bot) deleteMessage(ctx context.Context, chatID int64, messageID int) {
	if _, err := d.b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: messageID}); err != nil {
		// Не критично: у бота может не быть права удалять, или сообщение старое.
		d.log.Debug("не удалось удалить сообщение с логином", "user", chatID, "err", err)
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

const startMessage = `Привет! Меня зовут РюмкинЪ. Я умею:
/login — войти на сайт НГС.Лав
/add_note — добавить заметку
/add_anonymous_note — добавить анонимную заметку
/status — проверить сессию сайта`
