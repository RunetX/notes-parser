package love

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

const (
	notesPath       = "/notes/limit~5/"
	commentsPathFmt = "/notes/comments/%s/desc/limit~30/?view=linear"

	requestTimeout = 15 * time.Second
	getRetries     = 3
)

// Виды отображения комментариев на сайте. Древовидный проставляет числовой
// data-parent-comment-id у каждого комментария (родитель), линейный — нет.
const (
	ViewTree   = "tree"
	ViewLinear = "linear"
)

// ErrForbidden — сайт ответил 403: геоблок DDoS-Guard или бан.
// Ретраи бессмысленны, нужен разрешённый (российский) IP.
var ErrForbidden = errors.New("сайт вернул 403 (геоблок/бан IP)")

// Client — HTTP-клиент сайта с общим лимитером запросов: сколько бы
// воркеров ни работало, к сайту уходит не больше одного запроса за интервал.
type Client struct {
	baseURL string
	ua      string
	hc      *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
}

func New(baseURL, userAgent string, requestInterval time.Duration, log *slog.Logger) *Client {
	return NewWithClient(baseURL, userAgent, requestInterval, nil, log)
}

// NewWithClient — как New, но с заданным *http.Client (например, маршрутизирующим
// сайт через прокси). hc == nil → клиент по умолчанию с таймаутом requestTimeout.
// Прокси для сайта нужен при массовой выгрузке, чтобы поберечь основной IP от
// блока при параллельном чтении: запросы уходят с IP прокси-хоста.
func NewWithClient(baseURL, userAgent string, requestInterval time.Duration, hc *http.Client, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		ua:      userAgent,
		hc:      hc,
		limiter: rate.NewLimiter(rate.Every(requestInterval), 2),
		log:     log,
	}
}

// FetchNotes скачивает и разбирает ленту заметок.
func (c *Client) FetchNotes(ctx context.Context) ([]Note, error) {
	body, err := c.RawNotes(ctx)
	if err != nil {
		return nil, err
	}
	return ParseNotes(strings.NewReader(string(body)))
}

// FetchComments скачивает и разбирает комментарии заметки.
func (c *Client) FetchComments(ctx context.Context, noteID string) ([]Comment, error) {
	body, err := c.RawComments(ctx, noteID)
	if err != nil {
		return nil, err
	}
	return ParseComments(strings.NewReader(string(body)), c.baseURL)
}

// RawNotes возвращает сырой HTML ленты (для crawl --save-html и фикстур).
func (c *Client) RawNotes(ctx context.Context) ([]byte, error) {
	return c.get(ctx, notesPath)
}

// RawComments возвращает сырой HTML страницы комментариев.
func (c *Client) RawComments(ctx context.Context, noteID string) ([]byte, error) {
	return c.get(ctx, fmt.Sprintf(commentsPathFmt, noteID))
}

// RawCommentsView возвращает сырой HTML страницы комментариев в заданном виде
// (ViewTree/ViewLinear) и с заданным номером страницы (page ≥ 1). Используется
// разовым граббером для полного обхода треда в древовидном виде. view форсится
// в каждом запросе намеренно: пейджер сайта отдаёт next-ссылку без ?view, и без
// него страница вернулась бы линейной — потерялось бы дерево ответов.
func (c *Client) RawCommentsView(ctx context.Context, noteID string, page int, view string) ([]byte, error) {
	var path string
	switch {
	case view == ViewTree:
		// Древовидный отдаёт весь тред одной страницей; каноничный URL без
		// desc/limit~30 не даёт 302-редиректа (лишний хоп на каждый запрос при
		// массовой выгрузке).
		if page <= 1 {
			path = fmt.Sprintf("/notes/comments/%s/?view=tree", noteID)
		} else {
			path = fmt.Sprintf("/notes/comments/%s/page~%d/?view=tree", noteID, page)
		}
	default:
		if page <= 1 {
			path = fmt.Sprintf("/notes/comments/%s/desc/limit~30/?view=%s", noteID, view)
		} else {
			path = fmt.Sprintf("/notes/comments/%s/page~%d/limit~30/?view=%s", noteID, page, view)
		}
	}
	return c.get(ctx, path)
}

// RawNotesPage возвращает сырой HTML страницы ленты (30 заметок) для перечисления
// живых id при массовой выгрузке. page 1 — /notes/limit~30/, дальше page~N.
func (c *Client) RawNotesPage(ctx context.Context, page int) ([]byte, error) {
	if page <= 1 {
		return c.get(ctx, "/notes/limit~30/")
	}
	return c.get(ctx, fmt.Sprintf("/notes/page~%d/limit~30/", page))
}

// mediaSizeLimit — предел размера скачиваемого медиа (аватар/иллюстрация).
const mediaSizeLimit = 10 << 20

// FetchMedia скачивает медиа (аватар или иллюстрацию заметки) по абсолютному
// URL напрямую (с RU-IP). Нужно потому, что Telegram забирает медиа по URL со
// своих зарубежных серверов, а инфраструктура love.ngs.ru (включая CDN
// hsmedia.ru) отдаёт им не картинку — отсюда «wrong type of the web page
// content». Байты качаем сами и грузим в Telegram как файл.
func (c *Client) FetchMedia(ctx context.Context, rawURL string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerUserAgent, c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("аватар %s: статус %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, mediaSizeLimit))
}

// get выполняет GET с ретраями (сеть/5xx) и экспоненциальным backoff.
// 4xx не ретраится: это не временный сбой.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < getRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			backoff += time.Duration(rand.Int64N(int64(backoff / 2)))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, retriable, err := c.getOnce(ctx, path)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retriable {
			return nil, err
		}
		c.log.Warn("повтор запроса к сайту", "path", path, "attempt", attempt+1, "err", err)
	}
	return nil, fmt.Errorf("GET %s: попытки исчерпаны: %w", path, lastErr)
}

func (c *Client) getOnce(ctx context.Context, path string) (body []byte, retriable bool, err error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.ua)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(resp.Body)
		return b, true, err
	case resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("GET %s: %w", path, ErrForbidden)
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("GET %s: статус %d", path, resp.StatusCode)
	default:
		return nil, false, fmt.Errorf("GET %s: статус %d", path, resp.StatusCode)
	}
}

// postForm выполняет POST формы, при необходимости с куками пользователя.
// Без ретраев: повтор POST может задвоить комментарий или заметку.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, cookies []*http.Cookie) (*http.Response, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	return c.hc.Do(req)
}
