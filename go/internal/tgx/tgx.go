// Пакет tgx — обёртка над go-telegram/bot: пер-чатовые лимитеры,
// повтор после 429 (retry_after), композиция HTML-сообщений.
package tgx

import (
	"bytes"
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

	// mediaCache: URL медиа → Telegram file_id. Одинаковые аватары (один автор
	// комментирует много раз) грузим в Telegram один раз, дальше — по file_id.
	cmu        sync.Mutex
	mediaCache map[string]string
}

// Params — параметры постер-бота. HTTPClient (может быть nil) задаёт
// соединение с Bot API через прокси; nil — прямое соединение.
type Params struct {
	Token            string
	ChannelID        int64
	DiscussionChatID int64
	Signature        string
	BaseURL          string
	HTTPClient       *http.Client
}

// pollTimeout — таймаут long polling обновлений.
const pollTimeout = 30 * time.Second

// NewMirror создаёт бота. onUpdate вызывается для каждого входящего
// обновления (захват автофорварда, мост ответов).
func NewMirror(p Params, log *slog.Logger, onUpdate func(ctx context.Context, u *models.Update)) (*Mirror, error) {
	if log == nil {
		log = slog.Default()
	}
	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, u *models.Update) {
			onUpdate(ctx, u)
		}),
	}
	if p.HTTPClient != nil {
		opts = append(opts, bot.WithHTTPClient(pollTimeout, p.HTTPClient))
	}
	b, err := bot.New(p.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("создание бота: %w", err)
	}
	return &Mirror{
		b:                b,
		channelID:        p.ChannelID,
		discussionChatID: p.DiscussionChatID,
		signature:        p.Signature,
		baseURL:          strings.TrimSuffix(p.BaseURL, "/"),
		log:              log,
		limiters:         make(map[int64]*rate.Limiter),
		mediaCache:       make(map[string]string),
	}, nil
}

// Start запускает long polling; блокируется до отмены контекста.
func (m *Mirror) Start(ctx context.Context) { m.b.Start(ctx) }

// Name — имя мессенджера для message_targets.
func (m *Mirror) Name() string { return store.MessengerTelegram }

// Me возвращает данные бота (диагностика).
func (m *Mirror) Me(ctx context.Context) (*models.User, error) { return m.b.GetMe(ctx) }

// PostNote постит заметку в канал, возвращает id сообщения. avatar — байты
// аватара живого автора (nil для анонимов/силуэтов). Если аватар есть и весь
// текст влезает в подпись к фото — постим фото с подписью; иначе полным
// текстом без аватара. Контент заметки не режем ни при каких условиях.
func (m *Mirror) PostNote(ctx context.Context, n store.Note, avatar []byte) (string, error) {
	text := ComposeNoteMessage(m.baseURL, m.signature, n)

	if len(avatar) > 0 && visibleNoteLen(m.signature, n) <= captionLimit {
		msg, err := send(ctx, m, m.channelID, func(ctx context.Context) (*models.Message, error) {
			return m.b.SendPhoto(ctx, &bot.SendPhotoParams{
				ChatID:    m.channelID,
				Photo:     m.mediaInput(n.AuthorAvatarURL, avatar, "avatar.jpg"),
				Caption:   text,
				ParseMode: models.ParseModeHTML,
			})
		})
		if err == nil {
			m.rememberFileID(n.AuthorAvatarURL, photoFileID(msg))
			return strconv.Itoa(msg.ID), nil
		}
		m.log.Warn("заметка с фото не отправлена, шлём текстом", "note", n.ID, "err", err)
	}

	msg, err := send(ctx, m, m.channelID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:             m.channelID,
			Text:               text,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		})
	})
	if err != nil {
		return "", fmt.Errorf("пост заметки %s в канал: %w", n.ID, err)
	}
	return strconv.Itoa(msg.ID), nil
}

// PostComment постит комментарий в тред группы обсуждения. Короткий (влезает
// в подпись, ≤1024) и с аватаром — документом с аватаром и HTML-подписью.
// Длинный — обычным текстовым сообщением (до 4096), чтобы не резать текст:
// подпись к медиа ограничена 1024, а на сайте комментарий целиком. avatar —
// уже скачанные байты (Telegram не может забрать медиа love.ngs.ru по URL).
// threadID — корень треда (id автофорварда) десятичной строкой.
func (m *Mirror) PostComment(ctx context.Context, n store.Note, threadID string, c store.Comment, avatar []byte) (string, error) {
	root, err := parseMessageID(threadID)
	if err != nil {
		return "", fmt.Errorf("пост комментария %d: %w", c.ID, err)
	}
	reply := &models.ReplyParameters{MessageID: root}

	if len(avatar) > 0 && commentVisibleLen(c) <= captionLimit {
		msg, err := send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
			return m.b.SendDocument(ctx, &bot.SendDocumentParams{
				ChatID:          m.discussionChatID,
				Document:        m.mediaInput(c.AvatarURL, avatar, "avatar.jpg"),
				Caption:         ComposeCommentCaption(c),
				ParseMode:       models.ParseModeHTML,
				ReplyParameters: reply,
			})
		})
		if err == nil {
			if msg.Document != nil {
				m.rememberFileID(c.AvatarURL, msg.Document.FileID)
			}
			return strconv.Itoa(msg.ID), nil
		}
		m.log.Warn("аватар-документ не отправлен, шлём комментарий текстом", "comment", c.ID, "err", err)
	}

	msg, err := send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:             m.discussionChatID,
			Text:               composeComment(c, messageLimit),
			ParseMode:          models.ParseModeHTML,
			ReplyParameters:    reply,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		})
	})
	if err != nil {
		return "", fmt.Errorf("пост комментария %d в тред: %w", c.ID, err)
	}
	return strconv.Itoa(msg.ID), nil
}

// PostNoteImage постит иллюстрацию заметки первым сообщением в треде. Пробуем
// как фото; если Telegram отверг (крупный размер/пропорции) — как документ,
// чтобы иллюстрация всё же дошла.
func (m *Mirror) PostNoteImage(ctx context.Context, threadID, imageURL string, image []byte) (string, error) {
	root, err := parseMessageID(threadID)
	if err != nil {
		return "", fmt.Errorf("пост иллюстрации: %w", err)
	}
	reply := &models.ReplyParameters{MessageID: root}

	msg, err := send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendPhoto(ctx, &bot.SendPhotoParams{
			ChatID:          m.discussionChatID,
			Photo:           m.mediaInput(imageURL, image, "image.jpg"),
			ReplyParameters: reply,
		})
	})
	if err == nil {
		m.rememberFileID(imageURL, photoFileID(msg))
		return strconv.Itoa(msg.ID), nil
	}
	m.log.Warn("иллюстрация как фото не отправлена, пробуем документом", "err", err)

	msg, err = send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:          m.discussionChatID,
			Document:        m.mediaInput(imageURL, image, "image.jpg"),
			ReplyParameters: reply,
		})
	})
	if err != nil {
		return "", fmt.Errorf("пост иллюстрации в тред: %w", err)
	}
	if msg.Document != nil {
		m.rememberFileID(imageURL, msg.Document.FileID)
	}
	return strconv.Itoa(msg.ID), nil
}

// mediaInput отдаёт готовый file_id, если этот URL уже грузили в Telegram,
// иначе — байты на загрузку. Так одинаковые аватары не грузятся повторно.
func (m *Mirror) mediaInput(url string, data []byte, filename string) models.InputFile {
	if fid := m.cachedFileID(url); fid != "" {
		return &models.InputFileString{Data: fid}
	}
	return &models.InputFileUpload{Filename: filename, Data: bytes.NewReader(data)}
}

func (m *Mirror) cachedFileID(url string) string {
	if url == "" {
		return ""
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	return m.mediaCache[url]
}

func (m *Mirror) rememberFileID(url, fileID string) {
	if url == "" || fileID == "" {
		return
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	m.mediaCache[url] = fileID
}

// photoFileID — file_id самого крупного варианта отправленного фото.
func photoFileID(msg *models.Message) string {
	if n := len(msg.Photo); n > 0 {
		return msg.Photo[n-1].FileID
	}
	return ""
}

// NotifySubscriber шлёт подписчику ЛС о комментарии с его ключевым словом:
// сработавшее слово, автор комментария, под какой заметкой и выдержка текста
// — плюс deep-link на сам комментарий в треде.
// threadID/commentMsgID — id корня треда и сообщения комментария (строками).
func (m *Mirror) NotifySubscriber(ctx context.Context, userID int64, keyword string, n store.Note, c store.Comment, threadID, commentMsgID string) error {
	root, err := parseMessageID(threadID)
	if err != nil {
		return fmt.Errorf("уведомление подписчика %d: %w", userID, err)
	}
	msgID, err := parseMessageID(commentMsgID)
	if err != nil {
		return fmt.Errorf("уведомление подписчика %d: %w", userID, err)
	}
	text := ComposeSubNotice(keyword, n, c,
		DeepLink(m.discussionChatID, int64(msgID), int64(root)))
	_, err = send(ctx, m, userID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:             userID,
			Text:               text,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		})
	})
	if err != nil {
		return fmt.Errorf("уведомление подписчика %d: %w", userID, err)
	}
	return nil
}

// parseMessageID разбирает телеграмный id сообщения из строкового вида
// message_targets (id мессенджеров хранятся непрозрачными строками).
func parseMessageID(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("не телеграмный id сообщения %q: %w", s, err)
	}
	return id, nil
}

// ErrPollingConflict — бота уже слушает другой процесс (HTTP 409):
// например, всё ещё работает старая Python-версия.
var ErrPollingConflict = errors.New("бота уже слушает другой процесс (409 Conflict)")

// ProbePendingUpdates возвращает число неподтверждённых обновлений, не
// подтверждая их (offset не передаётся — очередь не трогается). Библиотека
// не экспортирует getUpdates, поэтому прямой вызов Bot API. httpClient
// может быть nil (прямое соединение).
func ProbePendingUpdates(ctx context.Context, token string, httpClient *http.Client) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.telegram.org/bot"+token+"/getUpdates?timeout=0", nil)
	if err != nil {
		return 0, err
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
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
// httpClient может быть nil (прямое соединение).
func CheckToken(ctx context.Context, token string, httpClient *http.Client) (*models.User, error) {
	opts := []bot.Option{bot.WithSkipGetMe()}
	if httpClient != nil {
		opts = append(opts, bot.WithHTTPClient(pollTimeout, httpClient))
	}
	b, err := bot.New(token, opts...)
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

// SendText шлёт обычное сообщение (с уведомлением) через лимитер чата.
// Используется для алертов админу.
func (m *Mirror) SendText(ctx context.Context, chatID int64, text string) error {
	_, err := send(ctx, m, chatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text})
	})
	return err
}

// PostChannelHTML постит произвольный HTML-текст в канал без превью ссылок
// (публикация дайджеста). Возвращает id сообщения строкой.
func (m *Mirror) PostChannelHTML(ctx context.Context, html string) (string, error) {
	msg, err := send(ctx, m, m.channelID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:             m.channelID,
			Text:               html,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
		})
	})
	if err != nil {
		return "", fmt.Errorf("пост в канал: %w", err)
	}
	return strconv.Itoa(msg.ID), nil
}

// ThreadLink — ссылка на корень треда обсуждения (ссылки дайджеста);
// "" — threadID не телеграмный.
func (m *Mirror) ThreadLink(threadID string) string {
	return ThreadDeepLink(m.discussionChatID, threadID)
}

// ThreadDeepLink — ссылка на корень треда по строковому id из
// message_targets (для превью дайджеста без создания бота).
func ThreadDeepLink(discussionChatID int64, threadID string) string {
	root, err := parseMessageID(threadID)
	if err != nil {
		return ""
	}
	return DeepLink(discussionChatID, int64(root), int64(root))
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

// visibleNoteLen — длина видимого текста поста заметки (без HTML-тегов), по
// которой решаем, влезает ли он в подпись к фото (лимит Telegram captionLimit).
// Ссылки/теги в лимит не считаются, поэтому оцениваем по именам и тексту.
func visibleNoteLen(signature string, n store.Note) int {
	l := len([]rune(n.AuthorName)) + len(":\n") + len([]rune(n.Text))
	if signature != "" {
		l += len("\n\n") + len([]rune(signature))
	}
	return l
}

// Лимиты Telegram на видимый текст: подпись к медиа и обычное сообщение.
const (
	captionLimit = 1024
	messageLimit = 4096
)

// commentVisibleLen — длина видимого текста комментария (заголовок автора +
// текст), по ней решаем, влезает ли он в подпись к документу.
func commentVisibleLen(c store.Comment) int {
	return len([]rune(c.AuthorName)) + len([]rune(c.AuthorAge)) + len(", :\n") + len([]rune(c.Text))
}

// composeComment собирает HTML комментария с заголовком-ссылкой автора,
// обрезая ТЕКСТ по границе руны под лимит видимой длины limit (Python резал
// сырой HTML по байтам и мог сломать разметку). Заголовок в лимит заложен.
func composeComment(c store.Comment, limit int) string {
	visibleHead := len([]rune(c.AuthorName)) + len([]rune(c.AuthorAge)) + len(", :\n")
	// Запас на случай расхождения рун и UTF-16 (эмодзи считаются за два).
	budget := limit - visibleHead - 8
	if budget < 0 {
		budget = 0
	}
	text := c.Text
	if r := []rune(text); len(r) > budget {
		text = string(r[:budget]) + "…"
	}
	return fmt.Sprintf(`<b><a href="%s">%s, %s:</a></b>%s%s`,
		c.AuthorLink, html.EscapeString(c.AuthorName), html.EscapeString(c.AuthorAge),
		"\n", html.EscapeString(text))
}

// ComposeCommentCaption — подпись к документу-аватару (лимит подписи).
func ComposeCommentCaption(c store.Comment) string { return composeComment(c, captionLimit) }

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
const (
	subNoticeCommentLimit = 400
	subNoticeNoteLimit    = 120
)

// ComposeSubNotice собирает HTML уведомления подписчика: сработавшее слово,
// кто написал (ссылкой на профиль автора комментария), под чьей заметкой и
// выдержка текста. Раньше уходил один голый URL — по нему нельзя было понять
// ни автора, ни повод.
func ComposeSubNotice(keyword string, n store.Note, c store.Comment, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔔 Ключевое слово <b>%s</b>\n\n", html.EscapeString(keyword))

	author := html.EscapeString(c.AuthorName)
	if c.AuthorAge != "" {
		author += ", " + html.EscapeString(c.AuthorAge)
	}
	if c.AuthorLink != "" {
		author = fmt.Sprintf(`<a href="%s">%s</a>`, c.AuthorLink, author)
	}
	fmt.Fprintf(&b, "<b>%s</b> в заметке <i>%s</i>", author,
		html.EscapeString(truncateRunes(oneLine(n.AuthorName+": "+n.Text), subNoticeNoteLimit)))
	if !c.PublishedAt.IsZero() {
		fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("02.01 15:04"))
	}
	b.WriteString(":\n")
	b.WriteString(html.EscapeString(truncateRunes(c.Text, subNoticeCommentLimit)))
	fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть в обсуждении</a>", link)
	return b.String()
}

// oneLine сводит текст в одну строку: заметка упоминается одной строкой-курсивом.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncateRunes режет текст по границе руны, добавляя многоточие.
func truncateRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit])) + "…"
	}
	return s
}

// DeepLink строит ссылку t.me/c/... на комментарий в треде группы обсуждения.
// Внутренний id чата выводится из полного id отбрасыванием префикса -100
// (в Python-версии был захардкожен).
func DeepLink(discussionChatID, commentMsgID, threadRootID int64) string {
	internal := strings.TrimPrefix(strconv.FormatInt(discussionChatID, 10), "-100")
	return fmt.Sprintf("https://t.me/c/%s/%d?thread=%d", internal, commentMsgID, threadRootID)
}
