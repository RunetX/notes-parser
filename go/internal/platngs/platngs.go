package platngs

// Вынос написанного на площадке обратно на love.ngs.ru.
//
// Живёт у ДЕМОНА, а не у веб-морды, и это не вкус: ключ шифрования кук
// (LOVEGW_SECRET_KEY) есть только здесь — в docker-compose записано прямо, что
// конфиг морды отдельный, потому что она смотрит в интернет. Морда лишь ставит
// строку в ngs_outbox той же транзакцией, что и публикацию; ходит на сайт эта
// служба. Тот же расклад, что у platout, который носит заметки в мессенджеры.
//
// Отправляем ОТ ИМЕНИ ЧЕЛОВЕКА, его собственной сессией НГС из sessions: это не
// наша публикация и подписывать её собой мы не вправе. Нет живой сессии — строка
// не неудача, а skipped: человек мог просто не входить в бота, и жечь на нём
// попытки незачем.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"lovegw/internal/love"
	"lovegw/internal/platform"
	"lovegw/internal/sitetext"
	"lovegw/internal/store"
)

// Site — что службе нужно от клиента сайта.
type Site interface {
	PostNote(ctx context.Context, cookies []*http.Cookie, text string, anonymous bool) error
	PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error
}

// Config — темп и порции.
type Config struct {
	// Interval — как часто заглядываем в очередь. Полминуты: человек, нажавший
	// «отправить», ждёт появления своих слов на сайте, но не секунда в секунду, а
	// частый холостой запрос к Postgres отбирает ядро у зеркала.
	Interval time.Duration
	// Batch — сколько строк за проход. Немного: каждая — это POST на чужой сайт
	// под чужой сессией, и торопиться тут нечем.
	Batch int
}

// Service — обход очереди.
type Service struct {
	p    *platform.Platform
	st   *store.Store
	site Site
	cfg  Config
	log  *slog.Logger
}

func New(p *platform.Platform, st *store.Store, site Site, cfg Config, log *slog.Logger) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 5
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{p: p, st: st, site: site, cfg: cfg, log: log}
}

// Run крутит обход до отмены контекста.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.pass(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("вынос на НГС", "err", err)
			}
		}
	}
}

// pass — один проход по очереди.
func (s *Service) pass(ctx context.Context) error {
	jobs, err := s.p.NextNGSJobs(ctx, s.cfg.Batch)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return nil
		}
		s.one(ctx, j)
	}
	return nil
}

// one уносит одну публикацию.
func (s *Service) one(ctx context.Context, j platform.NGSJob) {
	// Пустое тело значит, что публикацию скрыли или снесли, пока строка ждала.
	// Это не ошибка: модератор решил про слова, и уносить их на сайт после этого
	// тем более незачем.
	if j.Body == "" {
		s.skip(ctx, j, "публикации больше нет")
		return
	}
	cookies, ok := s.cookies(ctx, j)
	if !ok {
		return
	}
	// Тот же приведённый вид, что у амвона и утренней заметки: сайт не отвечает
	// отказом на длинное тире, он просто печатает его в нашем тексте, — а
	// типографика выдаёт машину вернее содержания.
	text := sitetext.Normalize(j.Body)
	var err error
	switch j.Kind {
	case platform.NGSNote:
		err = s.site.PostNote(ctx, cookies, text, false)
	case platform.NGSComment:
		replyTo := ""
		if j.ReplyToNGSID != 0 {
			replyTo = strconv.FormatInt(j.ReplyToNGSID, 10)
		}
		err = s.site.PostComment(ctx, cookies,
			strconv.FormatInt(j.NoteID, 10), replyTo, text)
	default:
		s.skip(ctx, j, "неизвестный вид: "+j.Kind)
		return
	}
	if ferr := s.p.FinishNGSJob(ctx, j.ID, "", err); ferr != nil {
		s.log.Error("запись исхода выноса", "строка", j.ID, "err", ferr)
	}
	if err != nil {
		// Сайт отвечает 500 и на ПРИНЯТЫЙ комментарий (замер 17.08.2026), поэтому
		// неудача здесь не доказывает, что текста там нет. Отсюда потолок в три
		// попытки: повторять вечно значит однажды поставить дубль в чужой тред.
		s.log.Warn("вынос на НГС не удался",
			"строка", j.ID, "вид", j.Kind, "объект", j.ObjectID,
			"попытка", j.Attempts, "err", err)
		return
	}
	s.log.Info("вынесено на НГС", "вид", j.Kind, "объект", j.ObjectID, "автор", j.AuthorID)
}

// cookies — живая сессия НГС автора. false означает «строка уже закрыта».
func (s *Service) cookies(ctx context.Context, j platform.NGSJob) ([]*http.Cookie, bool) {
	// id участника площадки РАВЕН номеру анкеты — на этом равенстве держится всё
	// зеркало, и здесь оно избавляет от таблицы соответствий.
	messenger, userID, err := s.st.SessionForProfile(ctx, strconv.FormatInt(j.AuthorID, 10))
	if errors.Is(err, store.ErrNotFound) {
		// Галочка стоит, а сессии нет: человек её не заводил или она протухла.
		// Это не сбой и не его вина — пропускаем, не тратя попыток.
		s.skip(ctx, j, "нет живой сессии сайта")
		return nil, false
	}
	if err != nil {
		s.log.Error("поиск сессии для выноса", "строка", j.ID, "err", err)
		return nil, false
	}
	json, valid, err := s.st.SessionCookies(ctx, messenger, userID)
	if err != nil || !valid {
		s.skip(ctx, j, "сессия сайта недействительна")
		return nil, false
	}
	cookies, err := love.CookiesFromJSON([]byte(json), time.Now())
	if err != nil || len(cookies) == 0 {
		s.skip(ctx, j, "сессия сайта не читается")
		return nil, false
	}
	return cookies, true
}

func (s *Service) skip(ctx context.Context, j platform.NGSJob, why string) {
	if err := s.p.SkipNGSJob(ctx, j.ID, why); err != nil {
		s.log.Error("снятие строки выноса", "строка", j.ID, "err", err)
		return
	}
	s.log.Info("вынос на НГС пропущен", "вид", j.Kind, "объект", j.ObjectID, "почему", why)
}
