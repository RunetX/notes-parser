// Пакет maxx — MAX-сторона зеркала (mirror.Sink поверх официального SDK
// maxbot v2): постинг заметок в канал, «ручной автофорвард» (у каналов MAX
// нет нативных комментариев — копию заметки в чат обсуждения бот кладёт
// сам), комментарии в тред чата, пер-чатовые лимитеры и повтор после 429.
package maxx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"golang.org/x/time/rate"

	"lovegw/internal/store"
)

// Интервалы отправки: общий потолок API — 30 rps на домен, но канал и чат
// обсуждения держим в темпе телеграм-стороны, чтобы лента читалась.
const (
	channelSendInterval = 3 * time.Second
	chatSendInterval    = 3200 * time.Millisecond
	dmSendInterval      = time.Second
)

// retryAfter — пауза перед единственным повтором после 429. MAX не
// документирует retry_after; уточнить по живым логам (бриф, R2).
// Переменная — для подмены в тестах.
var retryAfter = 5 * time.Second

// longPollTimeout — серверное удержание GET /updates. Должно быть заметно
// меньше клиентского HTTP-таймаута (30 c в SDK и MintsifraClient), иначе
// пустые long-poll'ы обрываются клиентским таймаутом (видно на проде).
const longPollTimeout = 20 * time.Second

// Mirror — MAX-сторона зеркала.
type Mirror struct {
	api              *maxbot.Api
	channelID        int64
	discussionChatID int64
	signature        string
	baseURL          string
	log              *slog.Logger

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter

	// discussionLink — ссылка-приглашение чата обсуждения для кнопки
	// «Обсудить» на постах канала; снимается через GetChat лениво (один раз).
	// noteThreads — корни веток заметок (id заметки → mid копии в чате),
	// чтобы кнопка вела прямо в ветку. Заполняет StartThread; mirror зовёт
	// его до поста в канал.
	lmu            sync.Mutex
	discussionLink string
	noteThreads    map[string]string

	// talks — роутер личной переписки (может быть nil): реплай в диалоге
	// уходит на сайт. Ставится в runDaemon после сборки поллера (Ф5).
	talks TalkReplyRouter

	up *uploader
}

// Params — параметры MAX-бота. HTTPClient (может быть nil) задаёт транспорт
// с доверием к сертификату Минцифры (см. mintsifra.go); nil — транспорт SDK.
// APIBaseURL переопределяет адрес API (тесты против httptest); пусто —
// боевой platform-api2.max.ru.
type Params struct {
	Token            string
	ChannelID        int64
	DiscussionChatID int64
	Signature        string
	BaseURL          string // базовый URL сайта (ссылки на профили в постах)
	APIBaseURL       string
	HTTPClient       *http.Client
}

// NewMirror создаёт MAX-бота.
func NewMirror(p Params, log *slog.Logger) (*Mirror, error) {
	if log == nil {
		log = slog.Default()
	}
	opts := []maxbot.Opt{maxbot.WithPollingTimeout(longPollTimeout)}
	if p.HTTPClient != nil {
		opts = append(opts, maxbot.WithHTTPClient(p.HTTPClient))
	}
	if p.APIBaseURL != "" {
		opts = append(opts, maxbot.WithBaseURL(p.APIBaseURL))
	}
	api, err := maxbot.NewApi(p.Token, opts...)
	if err != nil {
		return nil, fmt.Errorf("создание MAX-бота: %w", err)
	}
	return &Mirror{
		api:              api,
		channelID:        p.ChannelID,
		discussionChatID: p.DiscussionChatID,
		signature:        p.Signature,
		baseURL:          strings.TrimSuffix(p.BaseURL, "/"),
		log:              log,
		limiters:         make(map[int64]*rate.Limiter),
		up:               newUploader(api),
	}, nil
}

// Name — имя мессенджера для message_targets.
func (m *Mirror) Name() string { return store.MessengerMax }

// Me возвращает данные бота (диагностика doctor).
func (m *Mirror) Me(ctx context.Context) (model.BotInfo, error) {
	return m.api.Bots.GetMyInfo(ctx)
}

// PostNote постит заметку в канал, возвращает mid сообщения. В отличие от
// Telegram, текст и вложение в MAX — поля одного сообщения (нет «подписи к
// фото» с лимитом), поэтому аватар всегда идёт вложением при полном тексте.
func (m *Mirror) PostNote(ctx context.Context, n store.Note, avatar []byte) (string, error) {
	msg := maxbot.NewMessage().
		SetChat(m.channelID).
		SetText(ComposeNoteMessage(m.baseURL, m.signature, n)).
		SetFormat(model.FormatHTML).
		SetDisableLinkPreview(true)
	m.attachImage(ctx, msg, n.AuthorAvatarURL, avatar, "аватар автора")
	m.attachDiscussButton(ctx, msg, n.ID)

	mid, err := m.send(ctx, m.channelID, msg)
	if err != nil {
		return "", fmt.Errorf("пост заметки %s в канал MAX: %w", n.ID, err)
	}
	return mid, nil
}

// attachDiscussButton добавляет к посту канала кнопку «Обсудить» (замена
// телеграмного автофорварда как точки входа в тред). Если корень ветки заметки
// уже известен — ссылка ведёт прямо в неё, иначе в чат целиком по ссылке-
// приглашению (она снимается через GetChat один раз; при ошибке пост уходит
// без кнопки — попробуем на следующем).
func (m *Mirror) attachDiscussButton(ctx context.Context, msg *maxbot.Message, noteID string) {
	if m.discussionChatID == 0 {
		return
	}
	// Корень нужен ровно один раз — на этот пост; дальше он живёт в
	// message_targets, а карта не должна расти вместе с лентой.
	m.lmu.Lock()
	link, thread := m.discussionLink, m.noteThreads[noteID]
	delete(m.noteThreads, noteID)
	m.lmu.Unlock()
	if deep := MessageLink(m.discussionChatID, thread); deep != "" {
		kb := model.NewKeyboard()
		kb.AddRow().AddLink("💬 Обсудить", deep)
		msg.AddKeyboard(kb)
		return
	}
	if link == "" {
		chat, err := m.api.Chats.GetChat(ctx, m.discussionChatID)
		if err != nil || chat.Link == "" {
			m.log.Warn("ссылка чата обсуждения не снята, пост без кнопки", "err", err)
			return
		}
		link = chat.Link
		m.lmu.Lock()
		m.discussionLink = link
		m.lmu.Unlock()
	}
	kb := model.NewKeyboard()
	kb.AddRow().AddLink("💬 Обсудить", link)
	msg.AddKeyboard(kb)
}

// StartThread — «ручной автофорвард»: копия заметки в чат обсуждения, её mid
// становится корнем треда (mirror.ThreadStarter). Mirror зовёт его до поста в
// канал — тогда кнопка «Обсудить» ведёт прямо в ветку заметки; если не
// удалось, вызов повторяется на каждом цикле опроса, а кнопка ведёт в чат.
func (m *Mirror) StartThread(ctx context.Context, n store.Note, _ string) (string, error) {
	if m.discussionChatID == 0 {
		return "", errors.New("чат обсуждения MAX не задан (discussion_chat_id)")
	}
	msg := maxbot.NewMessage().
		SetChat(m.discussionChatID).
		SetText(ComposeNoteMessage(m.baseURL, "", n)).
		SetFormat(model.FormatHTML).
		SetDisableLinkPreview(true)
	mid, err := m.send(ctx, m.discussionChatID, msg)
	if err != nil {
		return "", fmt.Errorf("копия заметки %s в чат обсуждения: %w", n.ID, err)
	}
	// Запоминаем корень: если пост в канал ещё впереди (mirror зовёт
	// StartThread до него), кнопка «Обсудить» поведёт прямо в эту ветку.
	m.lmu.Lock()
	if m.noteThreads == nil {
		m.noteThreads = make(map[string]string)
	}
	m.noteThreads[n.ID] = mid
	m.lmu.Unlock()
	return mid, nil
}

// PostComment постит комментарий в тред чата обсуждения — ответом на
// сообщение адресата реплики, а при неизвестном адресате (replyToID пуст) на
// корень треда, как было до слоя адресатов. Текст не режем: лимит длины
// сообщения MAX не задокументирован (бриф, R2).
func (m *Mirror) PostComment(ctx context.Context, n store.Note, threadID, replyToID string, c store.Comment, avatar []byte) (string, error) {
	send := func(replyTo string) (string, error) {
		msg := maxbot.NewMessage().
			SetChat(m.discussionChatID).
			SetReply(ComposeCommentMessage(c), replyTo).
			SetFormat(model.FormatHTML).
			SetDisableLinkPreview(true)
		m.attachImage(ctx, msg, c.AvatarURL, avatar, "аватар комментария")
		return m.send(ctx, m.discussionChatID, msg)
	}

	if replyToID != "" && replyToID != threadID {
		mid, err := send(replyToID)
		if err == nil {
			return mid, nil
		}
		// Сообщение адресата могли удалить — реплай на него не пройдёт, а
		// sendUnsent не перескакивает через неотправленный комментарий, и тред
		// заметки встал бы навсегда. Запасной заход на корень треда.
		m.log.Warn("реплай на адресата не прошёл, отвечаем корню треда",
			"comment", c.ID, "reply_to", replyToID, "err", err)
	}

	mid, err := send(threadID)
	if err != nil {
		return "", fmt.Errorf("пост комментария %d в тред MAX: %w", c.ID, err)
	}
	return mid, nil
}

// PostNoteImage постит иллюстрацию заметки ответом на корень треда.
func (m *Mirror) PostNoteImage(ctx context.Context, threadID, imageURL string, image []byte) (string, error) {
	token, err := m.up.token(ctx, imageURL, image)
	if err != nil {
		return "", fmt.Errorf("загрузка иллюстрации в MAX: %w", err)
	}
	msg := maxbot.NewMessage().
		SetChat(m.discussionChatID).
		SetReply("", threadID).
		AddAttachByToken(token, model.AttachImage)

	mid, err := m.send(ctx, m.discussionChatID, msg)
	if err != nil {
		return "", fmt.Errorf("пост иллюстрации в тред MAX: %w", err)
	}
	return mid, nil
}

// NotifySubscriber шлёт подписчику ЛС о комментарии с его ключевым словом:
// слово, автор, заметка и выдержка текста — плюс deep-link на сам комментарий
// в чате обсуждения. Запасной вариант, если mid непонятного вида или чата
// обсуждения нет, — ссылка на комментарий на сайте (анкер — anchor-<id>).
func (m *Mirror) NotifySubscriber(ctx context.Context, userID int64, keyword string, n store.Note, c store.Comment, _, commentMsgID string) error {
	link := m.chatMessageLink(commentMsgID)
	if link == "" {
		link = fmt.Sprintf("%s/notes/%s/#anchor-%d", m.baseURL, n.ID, c.ID)
	}
	msg := maxbot.NewMessage().
		SetUser(userID).
		SetText(composeSubNotice(keyword, n, c, link)).
		SetFormat(model.FormatHTML)
	if _, err := m.send(ctx, userID, msg); err != nil {
		return fmt.Errorf("уведомление подписчика %d: %w", userID, err)
	}
	return nil
}

// SendText шлёт обычное текстовое сообщение (алерты админу, doctor).
func (m *Mirror) SendText(ctx context.Context, userID int64, text string) error {
	_, err := m.send(ctx, userID, maxbot.NewMessage().SetUser(userID).SetText(text))
	return err
}

// PostChannelHTML постит произвольный HTML-текст в канал без превью ссылок
// (публикация дайджеста). Возвращает mid сообщения. Бюджет chantext (3500
// ВИДИМЫХ рун) для MAX недостаточен: сервер считает строку вместе с разметкой,
// а у выпуска с полусотней ссылок разметка весит больше пятисот знаков.
func (m *Mirror) PostChannelHTML(ctx context.Context, html string) (string, error) {
	text, cut := fitHTML(html)
	if cut {
		m.log.Warn("текст поста в канал MAX обрезан под предел сообщения",
			"было", apiLen(html), "стало", apiLen(text), "предел", messageLimit)
	}
	msg := maxbot.NewMessage().
		SetChat(m.channelID).
		SetText(text).
		SetFormat(model.FormatHTML).
		SetDisableLinkPreview(true)
	mid, err := m.send(ctx, m.channelID, msg)
	if err != nil {
		return "", fmt.Errorf("пост в канал MAX: %w", err)
	}
	return mid, nil
}

// ThreadLink — ссылка на ветку заметки в чате обсуждения (ссылки дайджеста);
// "" — mid непонятного вида или чат обсуждения не задан.
func (m *Mirror) ThreadLink(threadID string) string {
	return m.chatMessageLink(threadID)
}

// chatMessageLink — ссылка на сообщение чата обсуждения; "" — mid непонятного
// вида или чат обсуждения не задан (вызывающий откатывается на сайт).
func (m *Mirror) chatMessageLink(mid string) string {
	if m.discussionChatID == 0 {
		return ""
	}
	return MessageLink(m.discussionChatID, mid)
}

// attachImage прикладывает изображение к сообщению через upload-токен
// (с кэшем URL→токен). Ошибка загрузки не валит отправку: сообщение уйдёт
// без картинки, как и в телеграм-стороне.
func (m *Mirror) attachImage(ctx context.Context, msg *maxbot.Message, url string, data []byte, what string) {
	if len(data) == 0 {
		return
	}
	token, err := m.up.token(ctx, url, data)
	if err != nil {
		m.log.Warn(what+" не загружен в MAX, шлём без картинки", "err", err)
		return
	}
	msg.AddAttachByToken(token, model.AttachImage)
}

// send пропускает отправку через пер-чатовый лимитер и один раз повторяет
// после 429. Возвращает mid отправленного сообщения.
func (m *Mirror) send(ctx context.Context, limiterKey int64, msg *maxbot.Message) (string, error) {
	if err := m.limiterFor(limiterKey).Wait(ctx); err != nil {
		return "", err
	}
	res, err := m.api.Messages.Send(ctx, msg)
	if err != nil && isTooManyRequests(err) {
		m.log.Warn("MAX rate limit, ждём", "chat", limiterKey, "retry_after", retryAfter)
		select {
		case <-time.After(retryAfter):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		res, err = m.api.Messages.Send(ctx, msg)
	}
	if err != nil {
		return "", err
	}
	mid := res.Message.Body.Mid
	if mid == "" {
		return "", errors.New("MAX не вернул mid сообщения")
	}
	return mid, nil
}

// isTooManyRequests — эвристика 429: SDK разбирает тело ошибки в Error без
// HTTP-кода, точный code для rate limit не задокументирован. Уточнить по
// живым логам.
func isTooManyRequests(err error) bool {
	var apiErr *maxbot.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	all := strings.ToLower(apiErr.Code + " " + apiErr.Err + " " + apiErr.Message)
	return strings.Contains(all, "too.many") || strings.Contains(all, "too many") ||
		strings.Contains(all, "429")
}

// limiterFor — пер-чатовый лимитер. Ключи не пересекаются: id каналов и
// групповых чатов MAX отрицательные (снято на Ф0-спайке), id пользователей
// для ЛС — положительные.
func (m *Mirror) limiterFor(key int64) *rate.Limiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lim, ok := m.limiters[key]; ok {
		return lim
	}
	var interval time.Duration
	switch key {
	case m.channelID:
		interval = channelSendInterval
	case m.discussionChatID:
		interval = chatSendInterval
	default:
		interval = dmSendInterval
	}
	lim := rate.NewLimiter(rate.Every(interval), 1)
	m.limiters[key] = lim
	return lim
}
