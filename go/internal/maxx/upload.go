package maxx

// Загрузка медиа в MAX: POST /uploads → URL → multipart → токен вложения.
// Кэш URL→токен — как mediaCache в tgx (URL→file_id): один автор
// комментирует многократно, повторно грузить аватар не нужно.
//
// В отличие от telegram'ского file_id токен вложения MAX бессрочным НЕ
// объявлен, а протухший убивает не одно сообщение, а весь тред: комментарий
// с ним не уходит, очередь заметки встаёт и не двигается (см. compose.go).
// Поэтому у записей есть срок жизни: он заведомо короче любого разумного
// срока годности токена, а цена промаха — одна лишняя загрузка аватара.

import (
	"bytes"
	"context"
	"sync"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

// tokenTTL — срок жизни записи кэша. Реальный срок годности токена MAX не
// документирован, поэтому берём консервативно.
const tokenTTL = 30 * time.Minute

// tokenCacheLimit — потолок числа записей: демон живёт месяцами, и без
// вытеснения карта росла бы по числу уникальных URL аватаров.
const tokenCacheLimit = 2048

type cachedToken struct {
	token string
	at    time.Time
}

type uploader struct {
	api *maxbot.Api
	now func() time.Time // подменяется в тестах

	mu     sync.Mutex
	tokens map[string]cachedToken // URL медиа → токен вложения MAX
}

func newUploader(api *maxbot.Api) *uploader {
	return &uploader{api: api, now: time.Now, tokens: make(map[string]cachedToken)}
}

// token загружает изображение (уже скачанные байты) и возвращает токен
// вложения; повторная загрузка того же URL отдаёт кэшированный токен, пока он
// не просрочен.
func (u *uploader) token(ctx context.Context, url string, data []byte) (string, error) {
	if url != "" {
		if cached, ok := u.cached(url); ok {
			return cached, nil
		}
	}
	token, err := u.api.Upload.Upload(ctx, model.UploadImage,
		bytes.NewReader(data), "image.jpg", int64(len(data)))
	if err != nil {
		return "", err
	}
	if url != "" && token != "" {
		u.store(url, token)
	}
	return token, nil
}

func (u *uploader) cached(url string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c, ok := u.tokens[url]
	if !ok || c.token == "" {
		return "", false
	}
	if u.now().Sub(c.at) >= tokenTTL {
		delete(u.tokens, url)
		return "", false
	}
	return c.token, true
}

func (u *uploader) store(url, token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.tokens) >= tokenCacheLimit {
		u.evictLocked()
	}
	u.tokens[url] = cachedToken{token: token, at: u.now()}
}

// evictLocked чистит просроченные записи, а если чистить нечего — сбрасывает
// кэш целиком. Точность вытеснения здесь не нужна: промах стоит одной лишней
// загрузки аватара.
func (u *uploader) evictLocked() {
	now := u.now()
	for url, c := range u.tokens {
		if now.Sub(c.at) >= tokenTTL {
			delete(u.tokens, url)
		}
	}
	if len(u.tokens) >= tokenCacheLimit {
		u.tokens = make(map[string]cachedToken)
	}
}
