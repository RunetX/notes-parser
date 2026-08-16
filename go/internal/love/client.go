package love

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
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

	// pageSizeLimit — потолок страницы сайта. Ответ читается целиком в память,
	// и без потолка сорвавшийся (или враждебный) ответ раздул бы демон.
	// Переполнение — явная ошибка, а не молчаливая обрезка: обрезанный HTML
	// выглядел бы дрейфом вёрстки и поднял бы ложную тревогу.
	pageSizeLimit = 16 << 20

	// maxRedirects — потолок цепочки перенаправлений (у net/http дефолт 10).
	maxRedirects = 5
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

// ErrNotFound — сайт ответил 404. Для массовых обходов это рабочий случай
// (удалённая анкета, снесённая заметка), а не сбой: обход не должен вставать.
var ErrNotFound = errors.New("сайт вернул 404")

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
	// Политика перенаправлений своя, если вызывающий не задал свою: за медиа мы
	// ходим по ссылке с чужой страницы, и цепочка редиректов — та же ссылка,
	// только не видная заранее. Куки на чужой домен net/http не отдаёт и сам.
	if hc.CheckRedirect == nil {
		hc.CheckRedirect = checkRedirect
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		ua:      userAgent,
		hc:      hc,
		limiter: rate.NewLimiter(rate.Every(requestInterval), 2),
		log:     log,
	}
}

// StrictPacing убирает стартовый «залп» лимитера (burst 2): мгновенно уходит
// не больше одного запроса, дальше строго по интервалу. Для массовых обходов
// профилей — DDoS-Guard режет два подряд мгновенных запроса как бота.
func (c *Client) StrictPacing() {
	c.limiter.SetBurst(1)
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
// CommentsPage — страница комментариев вместе с шапкой самой заметки.
// Note == nil — шапка не разобралась: её дрейф не валит комментарии
// (зеркалу шапка нужна только для необязательных обновлений — свежих
// иллюстраций и признака «комментарии запрещены»).
type CommentsPage struct {
	Comments []Comment
	Note     *Note
}

// FetchCommentsPage загружает страницу комментариев целиком: комментарии и
// шапку заметки одним запросом.
func (c *Client) FetchCommentsPage(ctx context.Context, noteID string) (CommentsPage, error) {
	body, err := c.RawComments(ctx, noteID)
	if err != nil {
		return CommentsPage{}, err
	}
	comments, err := ParseComments(strings.NewReader(string(body)), c.baseURL)
	if err != nil {
		return CommentsPage{}, err
	}
	page := CommentsPage{Comments: comments}
	if n, err := ParseNoteFromCommentsPage(strings.NewReader(string(body)), c.baseURL); err == nil {
		page.Note = &n
	}
	return page, nil
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
	if err := checkMediaURL(ctx, rawURL); err != nil {
		return nil, err
	}
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

// checkMediaURL решает, пойдём ли мы по ссылке за картинкой. Ссылка приезжает
// атрибутом чужой страницы, а демон живёт на VPS: без проверки он сходил бы по
// ней и во внутреннюю сеть — к соседям по хосту и к метаданным облака.
func checkMediaURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("медиа %q: ссылка не разбирается", rawURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("медиа %q: допустимы только http и https", rawURL)
	}
	if internalHost(ctx, u.Hostname()) {
		return fmt.Errorf("медиа %q: адрес во внутренней сети", rawURL)
	}
	return nil
}

// checkRedirect — та же проверка на каждом шаге цепочки: перенаправление уводит
// туда же, куда и прямая ссылка, только незаметно.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("перенаправлений больше %d", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("перенаправление на схему %q", req.URL.Scheme)
	}
	if internalHost(req.Context(), req.URL.Hostname()) {
		return fmt.Errorf("перенаправление на внутренний адрес %q", req.URL.Host)
	}
	return nil
}

// internalHost — адрес не из публичного интернета: литерал внутреннего адреса
// или имя, которое в такой адрес разрешается. Имя проверяется по ВСЕМ его
// адресам: одного публичного в ответе мало, ходить будут по любому из них.
// Неразрешимое имя считаем внутренним — идти всё равно некуда.
func internalHost(ctx context.Context, host string) bool {
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsGlobalUnicast() || ip.IsPrivate()
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return true
	}
	for _, a := range ips {
		if !a.IP.IsGlobalUnicast() || a.IP.IsPrivate() {
			return true
		}
	}
	return false
}

// get выполняет GET с ретраями (сеть/5xx) и экспоненциальным backoff.
// 4xx не ретраится: это не временный сбой. cookies (необязательно) — сессия
// пользователя для страниц, требующих авторизации (например, обход профилей).
func (c *Client) get(ctx context.Context, path string, cookies ...*http.Cookie) ([]byte, error) {
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
		body, retriable, err := c.getOnce(ctx, path, cookies)
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

func (c *Client) getOnce(ctx context.Context, path string, cookies []*http.Cookie) (body []byte, retriable bool, err error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.ua)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(resp.Body, pageSizeLimit+1))
		if err != nil {
			return nil, true, err
		}
		if len(b) > pageSizeLimit {
			return nil, false, fmt.Errorf("GET %s: страница больше %d байт", path, pageSizeLimit)
		}
		return b, true, nil
	case resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("GET %s: %w", path, ErrForbidden)
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, fmt.Errorf("GET %s: %w", path, ErrNotFound)
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
