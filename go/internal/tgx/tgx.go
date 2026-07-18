// Пакет tgx — обёртка над go-telegram/bot: пер-чатовые лимитеры,
// повтор после 429 (retry_after), композиция HTML-сообщений.
package tgx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"golang.org/x/time/rate"

	"lovegw/internal/store"
)

// Интервалы отправки: группа обсуждения упирается в лимит Telegram
// «20 сообщений в минуту в группу», канал и ЛС — мягче.
const (
	channelSendInterval = 3 * time.Second
	groupSendInterval   = 3200 * time.Millisecond
	dmSendInterval      = time.Second
)

// Mirror — телеграм-сторона зеркала: постинг заметок в канал, комментариев
// в группу обсуждения, уведомлений подписчикам в ЛС.
type Mirror struct {
	b                *bot.Bot
	channelID        int64
	discussionChatID int64
	signature        string
	baseURL          string
	log              *slog.Logger

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
}

// NewMirror создаёт бота. onUpdate вызывается для каждого входящего
// обновления (захват автофорварда, мост ответов).
func NewMirror(token string, channelID, discussionChatID int64, signature, baseURL string,
	log *slog.Logger, onUpdate func(ctx context.Context, u *models.Update)) (*Mirror, error) {
	if log == nil {
		log = slog.Default()
	}
	b, err := bot.New(token,
		bot.WithSkipGetMe(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, u *models.Update) {
			onUpdate(ctx, u)
		}))
	if err != nil {
		return nil, fmt.Errorf("создание бота: %w", err)
	}
	return &Mirror{
		b:                b,
		channelID:        channelID,
		discussionChatID: discussionChatID,
		signature:        signature,
		baseURL:          strings.TrimSuffix(baseURL, "/"),
		log:              log,
		limiters:         make(map[int64]*rate.Limiter),
	}, nil
}

// Start запускает long polling; блокируется до отмены контекста.
func (m *Mirror) Start(ctx context.Context) { m.b.Start(ctx) }

// Me возвращает данные бота (диагностика).
func (m *Mirror) Me(ctx context.Context) (*models.User, error) { return m.b.GetMe(ctx) }

// PostNote постит заметку в канал, возвращает id сообщения.
func (m *Mirror) PostNote(ctx context.Context, n store.Note) (int64, error) {
	msg, err := send(ctx, m, m.channelID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:             m.channelID,
			Text:               ComposeNoteMessage(m.baseURL, m.signature, n),
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		})
	})
	if err != nil {
		return 0, fmt.Errorf("пост заметки %s в канал: %w", n.ID, err)
	}
	return int64(msg.ID), nil
}

// PostComment постит комментарий в тред группы обсуждения — как документ
// с аватаром и HTML-подписью (паритет с Python-версией).
func (m *Mirror) PostComment(ctx context.Context, n store.Note, c store.Comment) (int64, error) {
	msg, err := send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:          m.discussionChatID,
			Document:        &models.InputFileString{Data: c.AvatarURL},
			Caption:         ComposeCommentCaption(c),
			ParseMode:       models.ParseModeHTML,
			ReplyParameters: &models.ReplyParameters{MessageID: int(n.TGThreadID)},
		})
	})
	if err != nil {
		return 0, fmt.Errorf("пост комментария %d в тред: %w", c.ID, err)
	}
	return int64(msg.ID), nil
}

// NotifySubscriber шлёт подписчику ЛС с deep-link на комментарий.
func (m *Mirror) NotifySubscriber(ctx context.Context, userID int64, n store.Note, c store.Comment) error {
	_, err := send(ctx, m, userID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: userID,
			Text:   DeepLink(m.discussionChatID, c.TGMessageID, n.TGThreadID),
		})
	})
	if err != nil {
		return fmt.Errorf("уведомление подписчика %d: %w", userID, err)
	}
	return nil
}

// ErrPollingConflict — бота уже слушает другой процесс (HTTP 409):
// например, всё ещё работает старая Python-версия.
var ErrPollingConflict = errors.New("бота уже слушает другой процесс (409 Conflict)")

// ProbePendingUpdates возвращает число неподтверждённых обновлений, не
// подтверждая их (offset не передаётся — очередь не трогается). Библиотека
// не экспортирует getUpdates, поэтому прямой вызов Bot API.
func ProbePendingUpdates(ctx context.Context, token string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.telegram.org/bot"+token+"/getUpdates?timeout=0", nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return 0, ErrPollingConflict
	}
	var body struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("разбор ответа getUpdates: %w", err)
	}
	if !body.OK {
		return 0, fmt.Errorf("getUpdates: статус %d", resp.StatusCode)
	}
	return len(body.Result), nil
}

// CheckToken проверяет валидность токена бота через getMe.
func CheckToken(ctx context.Context, token string) (*models.User, error) {
	b, err := bot.New(token, bot.WithSkipGetMe())
	if err != nil {
		return nil, err
	}
	return b.GetMe(ctx)
}

// DeleteMessage удаляет сообщение (диагностика doctor).
func (m *Mirror) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	_, err := m.b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: messageID})
	return err
}

// SendSilent шлёт тихое сообщение без уведомления подписчиков (doctor).
func (m *Mirror) SendSilent(ctx context.Context, chatID int64, text string) (int, error) {
	msg, err := send(ctx, m, chatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:              chatID,
			Text:                text,
			DisableNotification: true,
		})
	})
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

// send пропускает вызов через пер-чатовый лимитер и один раз повторяет
// после 429, выждав retry_after.
func send(ctx context.Context, m *Mirror, chatID int64, fn func(ctx context.Context) (*models.Message, error)) (*models.Message, error) {
	if err := m.limiterFor(chatID).Wait(ctx); err != nil {
		return nil, err
	}
	msg, err := fn(ctx)
	if err == nil || !bot.IsTooManyRequestsError(err) {
		return msg, err
	}
	var tmr *bot.TooManyRequestsError
	retryAfter := 5 * time.Second
	if errors.As(err, &tmr) && tmr.RetryAfter > 0 {
		retryAfter = time.Duration(tmr.RetryAfter) * time.Second
	}
	m.log.Warn("telegram flood control, ждём", "chat", chatID, "retry_after", retryAfter)
	select {
	case <-time.After(retryAfter):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return fn(ctx)
}

func (m *Mirror) limiterFor(chatID int64) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lim, ok := m.limiters[chatID]; ok {
		return lim
	}
	var interval time.Duration
	switch chatID {
	case m.channelID:
		interval = channelSendInterval
	case m.discussionChatID:
		interval = groupSendInterval
	default:
		interval = dmSendInterval
	}
	lim := rate.NewLimiter(rate.Every(interval), 1)
	m.limiters[chatID] = lim
	return lim
}

// ComposeNoteMessage собирает HTML-текст поста заметки. Имя и текст
// экранируются: в Python-версии сырой текст в parse_mode=HTML был
// латентным багом (символы <, > и & ломали отправку).
func ComposeNoteMessage(baseURL, signature string, n store.Note) string {
	name := html.EscapeString(n.AuthorName)
	var b strings.Builder
	if n.AuthorID == "" || n.AuthorID == "0" {
		fmt.Fprintf(&b, "<b>%s:</b>\n", name)
	} else {
		fmt.Fprintf(&b, `<b><a href="%s/profile/%s">%s:</a></b>%s`, baseURL, n.AuthorID, name, "\n")
	}
	b.WriteString(html.EscapeString(n.Text))
	if signature != "" {
		b.WriteString("\n\n")
		b.WriteString(signature)
	}
	return b.String()
}

// captionLimit — лимит Telegram на подпись к документу (видимый текст).
const captionLimit = 1024

// ComposeCommentCaption собирает HTML-подпись комментария, обрезая текст
// по границе руны так, чтобы видимая часть уложилась в лимит (Python резал
// сырой HTML по 1024 байтам и мог сломать разметку — сообщение терялось).
func ComposeCommentCaption(c store.Comment) string {
	visibleHead := fmt.Sprintf("%s,%s:\n", c.AuthorName, c.AuthorAge)
	// Запас на случай расхождения рун и UTF-16 (эмодзи считаются за два).
	budget := captionLimit - len([]rune(visibleHead)) - 8
	text := c.Text
	if r := []rune(text); len(r) > budget {
		text = string(r[:budget]) + "…"
	}
	return fmt.Sprintf(`<b><a href="%s">%s,%s:</a></b>%s%s`,
		c.AuthorLink, html.EscapeString(c.AuthorName), html.EscapeString(c.AuthorAge),
		"\n", html.EscapeString(text))
}

// DeepLink строит ссылку t.me/c/... на комментарий в треде группы обсуждения.
// Внутренний id чата выводится из полного id отбрасыванием префикса -100
// (в Python-версии был захардкожен).
func DeepLink(discussionChatID, commentMsgID, threadRootID int64) string {
	internal := strings.TrimPrefix(strconv.FormatInt(discussionChatID, 10), "-100")
	return fmt.Sprintf("https://t.me/c/%s/%d?thread=%d", internal, commentMsgID, threadRootID)
}
