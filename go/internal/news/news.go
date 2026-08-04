// Пакет news — внутренние новости проекта: произвольный текст админа уходит
// постом в каналы мессенджеров, минуя сайт. Заметки в love.ngs.ru при этом не
// появляется, в notes ничего не пишется — единственный след публикации это
// строки message_targets вида (мессенджер, news, <id новости>).
//
// Ввод — команда /news в ЛС командному боту (dmbot); тред обсуждения новость
// не заводит: в Telegram комментарии под ней всё равно появятся автофорвардом
// канала, но на сайт они не уйдут — bridge не опознаёт такое сообщение ни как
// заметку, ни как комментарий и молча его пропускает.
package news

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

// Publisher — канал-приёмник новости. Реализуют tgx.Mirror и maxx.Mirror
// (тот же метод, что публикует дайджест).
type Publisher interface {
	Name() string // store.MessengerTelegram / store.MessengerMax
	PostChannelHTML(ctx context.Context, html string) (msgID string, err error)
}

// Service публикует новости во все включённые каналы.
type Service struct {
	st   *store.Store
	pubs []Publisher
	log  *slog.Logger
}

func New(st *store.Store, pubs []Publisher, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, pubs: pubs, log: log}
}

// Ready — есть ли куда публиковать.
func (s *Service) Ready() bool { return s != nil && len(s.pubs) > 0 }

// Prepare готовит текст админа к публикации: обрезает пробелы, проверяет
// HTML-подмножество каналов и потолок длины. Ошибка написана так, чтобы её
// можно было показать админу как есть.
func Prepare(text string) (string, error) {
	html := strings.TrimSpace(text)
	if html == "" {
		return "", errors.New("пустой текст")
	}
	if err := chantext.ValidateHTML(html); err != nil {
		return "", err
	}
	// Новость — один пост, серию не собираем: длинный текст лучше разбить на
	// две новости самому, чем получить сюрприз в канале.
	if n := chantext.VisibleLen(html); n > chantext.MessageBudget {
		return "", fmt.Errorf("новость длиннее %d знаков (сейчас %d) — сократите",
			chantext.MessageBudget, n)
	}
	return html, nil
}

// NewID — идентификатор новости для message_targets: метка времени публикации
// в UTC. По нему повторная публикация узнаёт уже отправленные каналы.
func NewID(t time.Time) string { return t.UTC().Format("20060102-150405") }

// Result — итог публикации в один мессенджер.
type Result struct {
	Messenger string
	Sent      bool // отправлено этим вызовом; false без Err — уже было опубликовано
	Err       error
}

// Publish постит новость во все каналы. Идемпотентно по (мессенджер, news,
// id): повторный вызов с тем же id досылает только те каналы, куда новость не
// ушла. Сбой одного мессенджера не отменяет остальные — он едет в Result,
// и повтор докатит недостающее.
func (s *Service) Publish(ctx context.Context, id, html string) []Result {
	results := make([]Result, 0, len(s.pubs))
	for _, p := range s.pubs {
		r := Result{Messenger: p.Name()}
		switch _, _, found, err := s.st.Target(ctx, p.Name(), store.TargetNews, id); {
		case err != nil:
			r.Err = err
		case found:
			// Уже опубликовано (повтор после частичного сбоя).
		default:
			if r.Err = s.post(ctx, p, id, html); r.Err == nil {
				r.Sent = true
			}
		}
		if r.Err != nil {
			s.log.Error("новость не опубликована", "messenger", p.Name(), "news", id, "err", r.Err)
		} else if r.Sent {
			s.log.Info("новость опубликована", "messenger", p.Name(), "news", id)
		}
		results = append(results, r)
	}
	return results
}

func (s *Service) post(ctx context.Context, p Publisher, id, html string) error {
	msgID, err := p.PostChannelHTML(ctx, html)
	if err != nil {
		return err
	}
	return s.st.SetTarget(ctx, p.Name(), store.TargetNews, id, msgID, "")
}

// Report — отчёт о публикации для админа одной строкой на мессенджер.
func Report(results []Result) string {
	lines := make([]string, 0, len(results))
	for _, r := range results {
		switch {
		case r.Err != nil:
			lines = append(lines, r.Messenger+": ошибка — "+r.Err.Error())
		case r.Sent:
			lines = append(lines, r.Messenger+": опубликовано")
		default:
			lines = append(lines, r.Messenger+": уже было опубликовано")
		}
	}
	return strings.Join(lines, "\n")
}

// Failed — остался ли мессенджер, куда новость не ушла (повтор имеет смысл).
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Err != nil {
			return true
		}
	}
	return false
}
