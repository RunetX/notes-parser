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

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// Интервалы отправки: группа обсуждения упирается в лимит Telegram
// «20 сообщений в минуту в группу», канал и ЛС — мягче.
const (
	channelSendInterval = 3 * time.Second
	groupSendInterval   = 3200 * time.Millisecond
	dmSendInterval      = time.Second
)

// linkSubscribe — подпись ссылки «Подписаться» в подвале поста канала (в MAX
// это кнопка с тем же текстом).
const linkSubscribe = "🔔 Подписаться"

// Mirror — телеграм-сторона зеркала: постинг заметок в канал, комментариев
// в группу обсуждения, уведомлений подписчикам в ЛС.
type Mirror struct {
	b                *bot.Bot
	channelID        int64
	discussionChatID int64
	signature        string
	baseURL          string
	log              *slog.Logger
	// hc — тот же клиент, что и у поллинга: файлы Bot API качаем через прокси,
	// иначе с российского IP они недоступны.
	hc *http.Client

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter

	// onVoice — необязательный хук распознавания голосовых (nil — фича
	// выключена). Ставится до старта поллинга, читается из его горутин.
	// subBot — юзернейм ЛС-бота для ссылки «Подписаться» в подвале поста
	// канала (пусто — ссылки нет). Ставится там же и тем же замком.
	vmu     sync.Mutex
	onVoice func(ctx context.Context, u *models.Update)
	subBot  string

	// mediaCache: (тип, URL медиа) → Telegram file_id. Одинаковые аватары (один
	// автор комментирует много раз) грузим в Telegram один раз, дальше — по
	// file_id. Тип в ключе обязателен: file_id привязан к типу вложения, и
	// присланный как фото аватар Telegram не примет документом
	// («can't use file of type Photo as Document») — а один и тот же URL
	// уходит и фотографией (аватар автора заметки в канал), и документом
	// (аватар в комментарии).
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
	hc := p.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	m := &Mirror{
		channelID:        p.ChannelID,
		discussionChatID: p.DiscussionChatID,
		signature:        p.Signature,
		baseURL:          strings.TrimSuffix(p.BaseURL, "/"),
		log:              log,
		hc:               hc,
		limiters:         make(map[int64]*rate.Limiter),
		mediaCache:       make(map[string]string),
	}
	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, u *models.Update) {
			onUpdate(ctx, u)
			if h := m.voiceHook(); h != nil {
				h(ctx, u)
			}
		}),
	}
	if p.HTTPClient != nil {
		opts = append(opts, bot.WithHTTPClient(pollTimeout, p.HTTPClient))
	}
	b, err := bot.New(p.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("создание бота: %w", err)
	}
	m.b = b
	return m, nil
}

// SetVoiceHandler подключает распознавание голосовых. Вызывается до Start;
// nil снимает хук.
func (m *Mirror) SetVoiceHandler(fn func(ctx context.Context, u *models.Update)) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	m.onVoice = fn
}

func (m *Mirror) voiceHook() func(ctx context.Context, u *models.Update) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	return m.onVoice
}

// SetSubscribeBot задаёт юзернейм ЛС-бота (без «@») для ссылки «Подписаться»
// в подвале постов канала. Вызывается до Start; пусто — ссылки нет.
func (m *Mirror) SetSubscribeBot(username string) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	m.subBot = username
}

// subscribeLink — deep-link «Подписаться» для подвала поста канала; пусто —
// подписаться из канала нельзя (юзернейм не снялся или id заметки не годится
// в payload).
//
// Именно ссылка в тексте, а не inline-кнопка: своя клавиатура у поста
// вытесняет родную кнопку «Комментарии» — Telegram рисует их в одном месте и
// показывает что-то одно, а вход в обсуждение нужнее.
//
// Deep-link (а не callback) потому, что постер-бот и РюмкинЪ — разные боты:
// нажатие пришло бы постеру, а он в ЛС писать не может. Заодно снимается
// главное ограничение Telegram — бот не пишет первым тому, кто его не
// запускал: переход открывает РюмкинЪ и сам стартует его.
func (m *Mirror) subscribeLink(n store.Note) string {
	m.vmu.Lock()
	bot := m.subBot
	m.vmu.Unlock()
	payload := kbd.StartSub(n.ID)
	if bot == "" || payload == "" {
		return ""
	}
	return "https://t.me/" + bot + "?start=" + payload
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
	subLink := m.subscribeLink(n)
	text := ComposeNoteMessage(m.baseURL, m.signature, n, subLink)

	if len(avatar) > 0 && visibleNoteLen(m.signature, subLink, n) <= captionLimit {
		msg, err := send(ctx, m, m.channelID, func(ctx context.Context) (*models.Message, error) {
			return m.b.SendPhoto(ctx, &bot.SendPhotoParams{
				ChatID:    m.channelID,
				Photo:     m.mediaInput(mediaPhoto, n.AuthorAvatarURL, avatar, "avatar.jpg"),
				Caption:   text,
				ParseMode: models.ParseModeHTML,
			})
		})
		if err == nil {
			m.rememberFileID(mediaPhoto, n.AuthorAvatarURL, photoFileID(msg))
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
// threadID — корень треда (id автофорварда) десятичной строкой, replyToID —
// сообщение адресата реплики (пусто — отвечаем корню, как было раньше).
func (m *Mirror) PostComment(ctx context.Context, n store.Note, threadID, replyToID string, c store.Comment, avatar []byte) (string, error) {
	root, err := parseMessageID(threadID)
	if err != nil {
		return "", fmt.Errorf("пост комментария %d: %w", c.ID, err)
	}
	target := m.replyTarget(root, replyToID)

	msgID, err := m.sendComment(ctx, c, target, avatar)
	if err != nil && target != root {
		// Сообщение адресата могли удалить — такой реплай Telegram отвергает, а
		// sendUnsent не перескакивает через неотправленный комментарий, и тред
		// заметки встал бы навсегда. Запасной заход на корень треда.
		m.log.Warn("реплай на адресата не прошёл, отвечаем корню треда",
			"comment", c.ID, "reply_to", target, "err", err)
		msgID, err = m.sendComment(ctx, c, root, avatar)
	}
	if err != nil {
		return "", fmt.Errorf("пост комментария %d в тред: %w", c.ID, err)
	}
	return msgID, nil
}

// sendComment — одна попытка отправки комментария реплаем на replyTo.
func (m *Mirror) sendComment(ctx context.Context, c store.Comment, replyTo int, avatar []byte) (string, error) {
	reply := &models.ReplyParameters{MessageID: replyTo}

	if len(avatar) > 0 && commentVisibleLen(c) <= captionLimit {
		msg, err := send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
			return m.b.SendDocument(ctx, &bot.SendDocumentParams{
				ChatID:          m.discussionChatID,
				Document:        m.mediaInput(mediaDocument, c.AvatarURL, avatar, "avatar.jpg"),
				Caption:         ComposeCommentCaption(c),
				ParseMode:       models.ParseModeHTML,
				ReplyParameters: reply,
			})
		})
		if err == nil {
			if msg.Document != nil {
				m.rememberFileID(mediaDocument, c.AvatarURL, msg.Document.FileID)
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
		return "", err
	}
	return strconv.Itoa(msg.ID), nil
}

// replyTarget — сообщение, на которое отвечаем: адресат реплики, если он
// известен, иначе корень треда. Тред от этого не рвётся — Telegram считает
// его по цепочке реплаев, а адресат сам сидит в этом же треде. Непонятный
// id адресата не валит пост: падаем на корень.
func (m *Mirror) replyTarget(root int, replyToID string) int {
	if replyToID == "" {
		return root
	}
	id, err := parseMessageID(replyToID)
	if err != nil {
		m.log.Warn("id адресата не разобран, отвечаем корню треда", "reply_to", replyToID, "err", err)
		return root
	}
	return id
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
			Photo:           m.mediaInput(mediaPhoto, imageURL, image, "image.jpg"),
			ReplyParameters: reply,
		})
	})
	if err == nil {
		m.rememberFileID(mediaPhoto, imageURL, photoFileID(msg))
		return strconv.Itoa(msg.ID), nil
	}
	m.log.Warn("иллюстрация как фото не отправлена, пробуем документом", "err", err)

	msg, err = send(ctx, m, m.discussionChatID, func(ctx context.Context) (*models.Message, error) {
		return m.b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:          m.discussionChatID,
			Document:        m.mediaInput(mediaDocument, imageURL, image, "image.jpg"),
			ReplyParameters: reply,
		})
	})
	if err != nil {
		return "", fmt.Errorf("пост иллюстрации в тред: %w", err)
	}
	if msg.Document != nil {
		m.rememberFileID(mediaDocument, imageURL, msg.Document.FileID)
	}
	return strconv.Itoa(msg.ID), nil
}

// Типы вложений в ключе mediaCache: file_id одного типа не годится для другого.
const (
	mediaPhoto    = "photo"
	mediaDocument = "document"
)

// mediaInput отдаёт готовый file_id, если этот URL уже грузили в Telegram тем
// же типом вложения, иначе — байты на загрузку. Так одинаковые аватары не
// грузятся повторно.
func (m *Mirror) mediaInput(kind, url string, data []byte, filename string) models.InputFile {
	if fid := m.cachedFileID(kind, url); fid != "" {
		return &models.InputFileString{Data: fid}
	}
	return &models.InputFileUpload{Filename: filename, Data: bytes.NewReader(data)}
}

func (m *Mirror) cachedFileID(kind, url string) string {
	if url == "" {
		return ""
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	return m.mediaCache[kind+"\x00"+url]
}

func (m *Mirror) rememberFileID(kind, url, fileID string) {
	if url == "" || fileID == "" {
		return
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	m.mediaCache[kind+"\x00"+url] = fileID
}

// photoFileID — file_id самого крупного варианта отправленного фото.
func photoFileID(msg *models.Message) string {
	if n := len(msg.Photo); n > 0 {
		return msg.Photo[n-1].FileID
	}
	return ""
}

// ParseMessageID разбирает телеграмный id сообщения из строкового вида
// message_targets — нужен снаружи, чтобы собрать deep-link уведомления.
func ParseMessageID(s string) (int, error) { return parseMessageID(s) }

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
		return 0, sanitize(err)
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, sanitize(err)
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
// Санировать ошибку здесь не нужно: go-telegram/bot сам подменяет токен на
// «***» в url.Error, а doctor печатает её на экран как есть.
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
// латентным багом (символы <, > и & ломали отправку). subLink — deep-link
// подписки для подвала (пусто — подвал только из подписи канала).
func ComposeNoteMessage(baseURL, signature string, n store.Note, subLink string) string {
	name := html.EscapeString(n.AuthorName)
	var b strings.Builder
	if n.AuthorID == "" || n.AuthorID == "0" {
		fmt.Fprintf(&b, "<b>%s:</b>\n", name)
	} else {
		fmt.Fprintf(&b, `<b><a href="%s">%s:</a></b>%s`,
			html.EscapeString(baseURL+"/profile/"+n.AuthorID), name, "\n")
	}
	b.WriteString(html.EscapeString(n.Text))
	sub := ""
	if subLink != "" {
		sub = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(subLink), linkSubscribe)
	}
	if foot := noteFooter(signature, sub); foot != "" {
		b.WriteString("\n\n")
		b.WriteString(foot)
	}
	return b.String()
}

// noteFooter — подвал поста: подпись канала и ссылка «Подписаться» одной
// строкой. sub — готовая ссылка с разметкой либо (при подсчёте видимой длины)
// один её текст; подпись приходит из конфига и уходит как есть.
func noteFooter(signature, sub string) string {
	switch {
	case sub == "":
		return signature
	case signature == "":
		return sub
	}
	return signature + " · " + sub
}

// visibleNoteLen — длина видимого текста поста заметки (без HTML-тегов), по
// которой решаем, влезает ли он в подпись к фото (лимит Telegram captionLimit).
// Ссылки/теги в лимит не считаются, поэтому оцениваем по именам и тексту.
func visibleNoteLen(signature, subLink string, n store.Note) int {
	l := len([]rune(n.AuthorName)) + len(":\n") + len([]rune(n.Text))
	sub := ""
	if subLink != "" {
		sub = linkSubscribe // в лимит идёт видимый текст ссылки, не URL
	}
	if foot := noteFooter(signature, sub); foot != "" {
		l += len("\n\n") + len([]rune(foot))
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
	head := fmt.Sprintf("%s, %s:", html.EscapeString(c.AuthorName), html.EscapeString(c.AuthorAge))
	if c.AuthorLink != "" {
		head = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(c.AuthorLink), head)
	}
	return "<b>" + head + "</b>\n" + html.EscapeString(text)
}

// ComposeCommentCaption — подпись к документу-аватару (лимит подписи).
func ComposeCommentCaption(c store.Comment) string { return composeComment(c, captionLimit) }

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
const (
	subNoticeCommentLimit = 400
	subNoticeNoteLimit    = 120
)

// ComposeSubNotice собирает HTML уведомления подписчика: повод (reason —
// готовая строка от mirror.SubEvent), кто написал (ссылкой на профиль автора
// комментария), под чьей заметкой и выдержка текста. Раньше уходил один голый
// URL — по нему нельзя было понять ни автора, ни повод.
// Нулевой комментарий (c.ID == 0) — повод «новая заметка автора»: цитировать
// нечего, показываем саму заметку.
func ComposeSubNotice(reason string, n store.Note, c store.Comment, link string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n\n", html.EscapeString(reason))

	if c.ID == 0 {
		fmt.Fprintf(&b, "<b>%s</b>:\n%s", html.EscapeString(n.AuthorName),
			html.EscapeString(truncateRunes(n.Text, subNoticeCommentLimit)))
		fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть заметку</a>", html.EscapeString(link))
		return b.String()
	}

	author := html.EscapeString(c.AuthorName)
	if c.AuthorAge != "" {
		author += ", " + html.EscapeString(c.AuthorAge)
	}
	if c.AuthorLink != "" {
		author = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(c.AuthorLink), author)
	}
	fmt.Fprintf(&b, "<b>%s</b> в заметке <i>%s</i>", author,
		html.EscapeString(truncateRunes(oneLine(n.AuthorName+": "+n.Text), subNoticeNoteLimit)))
	if !c.PublishedAt.IsZero() {
		fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("02.01 15:04"))
	}
	b.WriteString(":\n")
	b.WriteString(html.EscapeString(truncateRunes(c.Text, subNoticeCommentLimit)))
	fmt.Fprintf(&b, "\n\n<a href=\"%s\">Открыть в обсуждении</a>", html.EscapeString(link))
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
func DeepLink(discussionChatID, commentMsgID, threadRootID int64) string {
	return fmt.Sprintf("https://t.me/c/%s/%d?thread=%d",
		internalChatID(discussionChatID), commentMsgID, threadRootID)
}

// ChannelDeepLink — ссылка на пост канала: уведомление о новой заметке автора
// ведёт на сам пост, а не в тред (в Telegram тред к этому моменту ещё не
// пойман — его ловит bridge по автофорварду).
func ChannelDeepLink(channelID, msgID int64) string {
	return fmt.Sprintf("https://t.me/c/%s/%d", internalChatID(channelID), msgID)
}

// internalChatID — внутренний id чата: полный id без префикса -100
// (в Python-версии был захардкожен).
func internalChatID(chatID int64) string {
	return strings.TrimPrefix(strconv.FormatInt(chatID, 10), "-100")
}
