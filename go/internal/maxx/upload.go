package maxx

// Загрузка медиа в MAX: POST /uploads → URL → multipart → токен вложения.
// Кэш URL→токен — как mediaCache в tgx (URL→file_id): один автор
// комментирует многократно, повторно грузить аватар не нужно.

import (
	"bytes"
	"context"
	"sync"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type uploader struct {
	api *maxbot.Api

	mu     sync.Mutex
	tokens map[string]string // URL медиа → токен вложения MAX
}

func newUploader(api *maxbot.Api) *uploader {
	return &uploader{api: api, tokens: make(map[string]string)}
}

// token загружает изображение (уже скачанные байты) и возвращает токен
// вложения; повторная загрузка того же URL отдаёт кэшированный токен.
func (u *uploader) token(ctx context.Context, url string, data []byte) (string, error) {
	if url != "" {
		u.mu.Lock()
		cached := u.tokens[url]
		u.mu.Unlock()
		if cached != "" {
			return cached, nil
		}
	}
	token, err := u.api.Upload.Upload(ctx, model.UploadImage,
		bytes.NewReader(data), "image.jpg", int64(len(data)))
	if err != nil {
		return "", err
	}
	if url != "" && token != "" {
		u.mu.Lock()
		u.tokens[url] = token
		u.mu.Unlock()
	}
	return token, nil
}
