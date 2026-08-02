package tgx

// Телеграм-сторона автораспознавания: ловим голосовые и кружки в группе
// обсуждения, скачиваем файл и ставим задачу в очередь. Сеть и провайдер живут
// в пакете asr — сюда транспорт отдаёт только скачивание и реплай, поэтому
// гейт MAX подключится к тому же сервису без дублирования.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"lovegw/internal/asr"
	"lovegw/internal/store"
)

// fileSizeLimit — потолок скачиваемого файла. Голосовое на 90 секунд весит
// сотни килобайт, минутный кружок — единицы мегабайт; запас взят с избытком.
const fileSizeLimit = 20 << 20

// voiceQueue — то, что хендлеру нужно от сервиса распознавания (шов для тестов).
type voiceQueue interface {
	Enqueue(job asr.Job) bool
}

// VoiceHandler ставит голосовые из группы обсуждения в очередь распознавания.
// Сам не ходит в сеть: хендлер апдейта не должен задерживать поллинг.
type VoiceHandler struct {
	m      *Mirror
	q      voiceQueue
	chatID int64
	log    *slog.Logger
}

// NewVoiceHandler создаёт хендлер для группы обсуждения discussionChatID.
func NewVoiceHandler(m *Mirror, q voiceQueue, discussionChatID int64, log *slog.Logger) *VoiceHandler {
	if log == nil {
		log = slog.Default()
	}
	return &VoiceHandler{m: m, q: q, chatID: discussionChatID, log: log}
}

// Handle разбирает обновление и при голосовом ставит задачу в очередь.
func (h *VoiceHandler) Handle(ctx context.Context, u *models.Update) {
	if u == nil || u.Message == nil {
		return
	}
	msg := u.Message
	if msg.Chat.ID != h.chatID {
		return
	}
	// Автофорвард — копия поста канала: посты каналов не распознаём, мессенджеры
	// показывают там собственную расшифровку по кнопке. Прочие сообщения от
	// ботов тоже не наши.
	if msg.IsAutomaticForward || msg.From == nil || msg.From.IsBot {
		return
	}
	fileID, fileKey, duration, ok := voiceFile(msg)
	if !ok {
		return
	}

	userID := msg.From.ID
	replyTo := msg.ID
	job := asr.Job{
		Messenger: store.MessengerTelegram,
		FileKey:   fileKey,
		Duration:  duration,
		UserID:    userID,
		Fetch: func(ctx context.Context) ([]byte, error) {
			return h.m.DownloadFile(ctx, fileID)
		},
		Reply: func(ctx context.Context, text string) error {
			return h.m.ReplyText(ctx, h.chatID, replyTo, text)
		},
	}
	if !h.q.Enqueue(job) {
		h.log.Warn("очередь распознавания переполнена, голосовое пропущено",
			"message", msg.ID, "duration", duration)
	}
}

// voiceFile достаёт из сообщения голосовое или кружок. Ключ кэша —
// file_unique_id: при пересылке file_id меняется, а он остаётся прежним,
// поэтому за повтор провайдеру платить не нужно.
func voiceFile(msg *models.Message) (fileID, fileKey string, duration int, ok bool) {
	switch {
	case msg.Voice != nil:
		return msg.Voice.FileID, msg.Voice.FileUniqueID, msg.Voice.Duration, true
	case msg.VideoNote != nil:
		return msg.VideoNote.FileID, msg.VideoNote.FileUniqueID, msg.VideoNote.Duration, true
	default:
		return "", "", 0, false
	}
}

// DownloadFile скачивает файл Bot API тем же клиентом, что и поллинг (Telegram
// с российского IP доступен только через прокси). Ошибки санируются: ссылка на
// файл содержит токен бота, ему не место в логах.
func (m *Mirror) DownloadFile(ctx context.Context, fileID string) ([]byte, error) {
	f, err := m.b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", sanitize(err))
	}
	if f.FileSize > fileSizeLimit {
		return nil, fmt.Errorf("файл %d байт больше лимита %d", f.FileSize, fileSizeLimit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.b.FileDownloadLink(f), nil)
	if err != nil {
		return nil, sanitize(err)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("скачивание файла: %w", sanitize(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("скачивание файла: статус %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, fileSizeLimit+1))
	if err != nil {
		return nil, fmt.Errorf("скачивание файла: %w", sanitize(err))
	}
	if len(data) > fileSizeLimit {
		return nil, fmt.Errorf("файл больше лимита %d байт", fileSizeLimit)
	}
	return data, nil
}

// sanitize убирает из ошибки URL с токеном бота: net/http вкладывает адрес
// запроса в *url.Error, а адрес файла Bot API — это /file/bot<токен>/…
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ReplyText отправляет текст реплаем на сообщение в том же треде. Без
// ParseMode: расшифровка — сырой текст пользователя, разметку в ней искать
// нечего, а символы < и & сломали бы HTML. Длинный текст уходит серией.
func (m *Mirror) ReplyText(ctx context.Context, chatID int64, replyToID int, text string) error {
	for _, chunk := range splitRunes(text, messageLimit) {
		_, err := send(ctx, m, chatID, func(ctx context.Context) (*models.Message, error) {
			return m.b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:             chatID,
				Text:               chunk,
				ReplyParameters:    &models.ReplyParameters{MessageID: replyToID},
				LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: bot.True()},
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// splitRunes режет текст на куски не длиннее limit рун, предпочитая границу
// пробела. Пустой текст даёт пустой список — отправлять нечего.
func splitRunes(text string, limit int) []string {
	r := []rune(text)
	if len(r) == 0 {
		return nil
	}
	var out []string
	for len(r) > limit {
		cut := limit
		if idx := strings.LastIndexAny(string(r[:limit]), " \n"); idx > 0 {
			cut = len([]rune(string(r[:limit])[:idx]))
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		r = r[cut:]
	}
	if s := strings.TrimSpace(string(r)); s != "" {
		out = append(out, s)
	}
	return out
}
