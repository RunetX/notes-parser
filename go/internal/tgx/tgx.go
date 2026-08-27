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

	"lovegw/internal/alerts"
	"lovegw/internal/chantext"
	"lovegw/internal/kbd"
	"lovegw/internal/msglimit"
	"lovegw/internal/store"
	"lovegw/internal/subnotice"
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
	log              *slog.Logger
	// hc — тот же клиент, что и у поллинга: файлы Bot API качаем через прокси,
	// иначе с российского IP они недоступны.
	hc *http.Client

	limiters *msglimit.Limiters

	// onVoice — необязательный хук распознавания голосовых (nil — фича
	// выключена). Ставится до старта поллинга, читается из его горутин.
	// subBot — юзернейм ЛС-бота для ссылки «Подписаться» в подвале поста
	// канала (пусто — ссылки нет). Ставится там же и тем же замком.
	// pollAlert — алерт о полосе сбоев поллинга (nil — уведомлять некому);
	// ставится в wire-фазе, читается обработчиком ошибок бота.
	vmu       sync.Mutex
	onVoice   func(ctx context.Context, u *models.Update)
	subBot    string
	pollAlert *alerts.PollWatch

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
		log:              log,
		hc:               hc,
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
		// Ошибки поллинга библиотека сама только ретраит с backoff'ом —
		// вечный 409 (второй процесс) или протухший токен оставили бы демон
		// молча полуживым. Логируем и считаем полосу сбоев для алерта.
		bot.WithErrorsHandler(func(err error) {
			m.log.Warn("telegram bot", "err", err)
			if w := m.pollWatch(); w != nil {
				w.Error(context.Background(), err)
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
	m.limiters = msglimit.New(m.sendInterval)
	return m, nil
}

// SetPollAlert подключает алерт о полосе сбоев поллинга. name различает ботов
// в тексте алерта. Как и все Set*, зовётся до Start (wire-фаза runDaemon).
func (m *Mirror) SetPollAlert(name string, send func(ctx context.Context, text string)) {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	m.pollAlert = alerts.NewPollWatch(name, send)
}

func (m *Mirror) pollWatch() *alerts.PollWatch {
	m.vmu.Lock()
	defer m.vmu.Unlock()
	return m.pollAlert
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
	text := ComposeNoteMessage(m.signature, n, subLink)

	if len(avatar) > 0 && chantext.VisibleUTF16Len(text) <= captionLimit {
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

// mediaCacheLimit — потолок числа записей кэша file_id. Сам file_id
// бессрочен (в отличие от токенов MAX), поэтому срока жизни у записей нет,
// но демон живёт месяцами: без потолка карта росла бы по числу уникальных
// URL аватаров. Промах стоит одной лишней загрузки картинки.
const mediaCacheLimit = 4096

func (m *Mirror) rememberFileID(kind, url, fileID string) {
	if url == "" || fileID == "" {
		return
	}
	m.cmu.Lock()
	defer m.cmu.Unlock()
	if len(m.mediaCache) >= mediaCacheLimit {
		m.mediaCache = make(map[string]string)
	}
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
	if err := m.limiters.For(chatID).Wait(ctx); err != nil {
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

// sendInterval — темп отправки по адресату: группа обсуждения упирается в
// лимит Telegram «20 сообщений в минуту в группу», канал и ЛС мягче.
func (m *Mirror) sendInterval(chatID int64) time.Duration {
	switch chatID {
	case m.channelID:
		return channelSendInterval
	case m.discussionChatID:
		return groupSendInterval
	default:
		return dmSendInterval
	}
}

// ComposeNoteMessage собирает HTML-текст поста заметки. Имя и текст
// экранируются: в Python-версии сырой текст в parse_mode=HTML был
// латентным багом (символы <, > и & ломали отправку). subLink — deep-link
// подписки для подвала (пусто — подвал только из подписи канала).
//
// Тело УКЛАДЫВАЕТСЯ в предел сообщения, и это не аккуратность, а защита
// очереди: Telegram отвечает на слишком длинный текст отказом, а приёмник идёт
// по заметкам строго по порядку и на отказе встаёт — то есть одна такая заметка
// останавливает канал насовсем (в MAX эта пробка уже случалась, 06.08.2026).
// У заметок НГС такой длины не бывает; у написанных ЗДЕСЬ потолок тела —
// 20 000 знаков, то есть впятеро больше сообщения.
func ComposeNoteMessage(signature string, n store.Note, subLink string) string {
	return composeNote(signature, n, subLink, messageLimit)
}

// composeNote собирает пост под заданный предел видимой длины. Режется только
// ТЕЛО: шапка с автором и подвал со ссылками — то, по чему читатель находит
// оригинал, и терять их ради лишней строки текста нельзя.
func composeNote(signature string, n store.Note, subLink string, limit int) string {
	// Имя автора — просто имя. Ссылкой на анкету НГС оно было до 27.08.2026;
	// ссылок на НГС проект не ставит нигде (решение владельца), а второго
	// адреса у зеркального автора нет: страницы участника площадка не заводит.
	head := fmt.Sprintf("<b>%s:</b>\n", html.EscapeString(n.AuthorName))
	sub := ""
	if subLink != "" {
		sub = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(subLink), linkSubscribe)
	}
	tail := ""
	if foot := noteFooter(signature, sub, n.SourceURL); foot != "" {
		tail = "\n\n" + foot
	}
	// Тело либо размечено отправителем (площадка), либо экранируется целиком,
	// как весь текст НГС.
	body := n.TextHTML
	if body == "" {
		body = html.EscapeString(n.Text)
	}
	body, _ = chantext.FitMeasured(body,
		limit-chantext.VisibleUTF16Len(head)-chantext.VisibleUTF16Len(tail),
		chantext.VisibleUTF16Len)
	return head + body + tail
}

// noteFooter — подвал поста одной строкой: ссылка на оригинал, подпись канала,
// «Подписаться». sub — готовая ссылка с разметкой; подпись приходит из конфига
// и уходит как есть.
//
// Ссылка на источник стоит ПЕРВОЙ и появилась вместе с заметками площадки: у
// заметки НГС адрес читатель и так найдёт через обсуждение, а у написанной
// здесь другого адреса нет вовсе — по нему её читают целиком и по нему же
// отвечают.
func noteFooter(signature, sub, sourceURL string) string {
	parts := make([]string, 0, 3)
	if sourceURL != "" {
		parts = append(parts, chantext.SourceLink(sourceURL))
	}
	if signature != "" {
		parts = append(parts, signature)
	}
	if sub != "" {
		parts = append(parts, sub)
	}
	return strings.Join(parts, " · ")
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

// commentHead — «Ник, возраст:» шапки комментария. Возраст пустой у реплики,
// написанной на площадке: анкетных полей она не заводит вовсе (решение эпика E,
// ст. 10 — нет поля, нет спора о спецкатегориях). Без этой ветки шапка вышла бы
// «Ник, :» — запятая на месте, а за ней пусто.
func commentHead(name, age string) string {
	if age == "" {
		return html.EscapeString(name) + ":"
	}
	return fmt.Sprintf("%s, %s:", html.EscapeString(name), html.EscapeString(age))
}

// composeComment собирает HTML комментария с заголовком автора, обрезая ТЕЛО
// под лимит видимой длины limit (Python резал сырой HTML по байтам и мог
// сломать разметку). Заголовок в лимит заложен и не режется.
//
// Заголовок был ССЫЛКОЙ на анкету НГС до 27.08.2026: ссылок на НГС проект не
// ставит нигде (решение владельца). Поле store.Comment.AuthorLink при этом
// живо — по нему дайджест зеркала опознаёт человека, числового id у
// комментария НГС нет вовсе.
//
// Тело либо размечено отправителем (реплика площадки со знаками НГС), либо
// экранируется целиком, как весь текст сайта.
func composeComment(c store.Comment, limit int) string {
	head := "<b>" + commentHead(c.AuthorName, c.AuthorAge) + "</b>\n"
	body := c.TextHTML
	if body == "" {
		body = html.EscapeString(c.Text)
	}
	body, _ = chantext.FitMeasured(body, limit-chantext.VisibleUTF16Len(head),
		chantext.VisibleUTF16Len)
	return head + body
}

// ComposeCommentCaption — подпись к документу-аватару (лимит подписи).
func ComposeCommentCaption(c store.Comment) string { return composeComment(c, captionLimit) }

// Выдержки в уведомлении подписчика: длиннее сайт всё равно покажет по ссылке.
// ComposeSubNotice собирает HTML уведомления подписчика (общий композер —
// subnotice.Compose; здесь только подпись ссылки: в Telegram комментарий
// открывается в ветке обсуждения).
func ComposeSubNotice(reason string, n store.Note, c store.Comment, link string) string {
	return subnotice.Compose(reason, n, c, link, "Открыть в обсуждении")
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
