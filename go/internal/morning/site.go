package morning

// Адаптер клиента сайта.
//
// Клиент свой, а не общий с зеркалом, по той же причине, что у амвона, но с
// обратным знаком: амвону нужен БЫСТРЫЙ клиент (он спешит быть первым), а нам —
// чтобы не мешать зеркалу. Ходим мы редко: ленту читаем раз в такт и только
// когда есть работа, а заметку публикуем раз в сутки, — поэтому лимитер здесь
// нарочито медленный.

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"lovegw/internal/love"
)

// siteInterval — интервал собственного лимитера: 0,5 rps.
const siteInterval = 2 * time.Second

type siteAdapter struct{ c *love.Client }

// NewSite поднимает отдельный клиент сайта для утренней заметки.
func NewSite(baseURL, userAgent string, log *slog.Logger) Site {
	return siteAdapter{c: love.New(baseURL, userAgent, siteInterval, log)}
}

func (s siteAdapter) FetchNotes(ctx context.Context) ([]love.Note, error) {
	return s.c.FetchNotes(ctx)
}

func (s siteAdapter) PostNote(ctx context.Context, cookies []*http.Cookie, text string, anonymous bool) error {
	return s.c.PostNote(ctx, cookies, text, anonymous)
}

func (s siteAdapter) ProfileControl(ctx context.Context, cookies []*http.Cookie) (love.ProfileControl, error) {
	return s.c.ProfileControl(ctx, cookies)
}
