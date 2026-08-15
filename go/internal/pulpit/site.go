package pulpit

// Адаптер клиента сайта. Живёт здесь, а не в cmd, по одной причине: древовидный
// вид отдаётся сырым HTML (RawCommentsView), и разбирать его должен тот, кому
// нужно дерево, а не место сборки демона.
//
// Клиент амвону нужен СВОЙ: у love.Client лимитер общий на всё, и зеркало
// делит его между обходом ленты, опросом комментариев всех живых заметок и
// скачиванием аватаров. Заметка добиралась до нас за p90 = 619 с при медианных
// 164 с до первого чужого комментария — быть первым с таким лимитером нельзя.
// Свой клиент держим строго медленным (см. NewSite): три-четыре запроса в
// минуту против нынешних двадцати с лишним.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"time"

	"lovegw/internal/love"
)

// siteInterval — интервал собственного лимитера: 0,2 rps.
const siteInterval = 5 * time.Second

// siteAdapter — *love.Client в терминах амвона.
type siteAdapter struct {
	c       *love.Client
	baseURL string
}

// NewSite поднимает отдельный клиент сайта для амвона.
func NewSite(baseURL, userAgent string, log *slog.Logger) Site {
	return siteAdapter{c: love.New(baseURL, userAgent, siteInterval, log), baseURL: baseURL}
}

// NewSiteWith оборачивает готовый клиент (CLI-черновик обходится одним).
func NewSiteWith(c *love.Client, baseURL string) Site {
	return siteAdapter{c: c, baseURL: baseURL}
}

func (s siteAdapter) FetchNotes(ctx context.Context) ([]love.Note, error) {
	return s.c.FetchNotes(ctx)
}

func (s siteAdapter) FetchCommentsPage(ctx context.Context, noteID string) (love.CommentsPage, error) {
	return s.c.FetchCommentsPage(ctx, noteID)
}

// TreeComments — тред в древовидном виде: только он проставляет ParentID.
func (s siteAdapter) TreeComments(ctx context.Context, noteID string) ([]love.Comment, error) {
	raw, err := s.c.RawCommentsView(ctx, noteID, 1, love.ViewTree)
	if err != nil {
		return nil, err
	}
	return love.ParseComments(bytes.NewReader(raw), s.baseURL)
}

func (s siteAdapter) PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error {
	return s.c.PostComment(ctx, cookies, noteID, comAPIID, text)
}

func (s siteAdapter) ProfileControl(ctx context.Context, cookies []*http.Cookie) (love.ProfileControl, error) {
	return s.c.ProfileControl(ctx, cookies)
}

func (s siteAdapter) SiteIdentity(ctx context.Context, cookies []*http.Cookie) (string, string, string, error) {
	return s.c.SiteIdentity(ctx, cookies)
}
