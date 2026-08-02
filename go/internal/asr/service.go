package asr

// Сервис распознавания: очередь, воркер-пул, лимиты и защита от расходов.
// Хендлер апдейта только ставит задачу в очередь и возвращается — сеть и
// SQLite живут здесь, чтобы поллинг мессенджера не блокировался.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"lovegw/internal/alerts"
	"lovegw/internal/store"
)

// convertTimeout — потолок работы ffmpeg на одном файле.
const convertTimeout = 30 * time.Second

// jobTimeout — общий потолок обработки одной задачи (скачивание, конвертация,
// запрос к провайдеру с ретраями).
const jobTimeout = 5 * time.Minute

// transcriptPrefix — маркер расшифровки, чтобы в треде было видно, что это бот.
const transcriptPrefix = "🎙 "

// Job — одно голосовое из треда. Транспорт спрятан в замыкания: сервис не
// знает ни про Telegram, ни про MAX.
type Job struct {
	Messenger string // store.MessengerTelegram | store.MessengerMax
	FileKey   string // стабильный ключ кэша (telegram: file_unique_id)
	Duration  int    // длительность в секундах из метаданных сообщения
	UserID    int64  // на кого списывать суточную квоту
	// Fetch скачивает исходные байты (OGG/Opus, MP4).
	Fetch func(ctx context.Context) ([]byte, error)
	// Reply публикует текст реплаем на исходное сообщение.
	Reply func(ctx context.Context, text string) error
}

// Config — лимиты и защита от расходов.
type Config struct {
	MaxDurationSec    int // потолок длительности одного сообщения
	UserDailyLimitSec int // суточная квота на пользователя; 0 — без квоты
	Concurrency       int // воркеров распознавания
}

// Service — воркер-пул распознавания.
type Service struct {
	tr   Transcriber
	conv Converter
	st   *store.Store
	cfg  Config
	log  *slog.Logger

	jobs chan Job

	mu       sync.Mutex
	inflight map[string]bool  // ключи в работе: дедуп одновременных дублей
	alerter  *alerts.Alerter  // nil — админ не задан
	now      func() time.Time // шов для тестов (день квоты)
}

// alertKey — ключ троттлера алертов админу.
const alertKey = "ASR"

// New собирает сервис. Нулевые лимиты в cfg заменяются на разумные значения.
func New(tr Transcriber, conv Converter, st *store.Store, cfg Config, log *slog.Logger) *Service {
	if cfg.MaxDurationSec <= 0 {
		cfg.MaxDurationSec = 90
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	return &Service{
		tr:       tr,
		conv:     conv,
		st:       st,
		cfg:      cfg,
		log:      log,
		jobs:     make(chan Job, cfg.Concurrency*4),
		inflight: make(map[string]bool),
		now:      time.Now,
	}
}

// SetAlert подключает уведомления админу: о сбое ключа или исчерпанном балансе
// провайдера сообщаем один раз и один раз о восстановлении. nil — только лог.
func (s *Service) SetAlert(send func(ctx context.Context, text string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if send == nil {
		s.alerter = nil
		return
	}
	s.alerter = alerts.New(send, 1)
}

// Run поднимает воркеров и блокируется до отмены контекста.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(s.cfg.Concurrency)
	for i := 0; i < s.cfg.Concurrency; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-s.jobs:
					s.process(ctx, job)
					s.release(job)
				}
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

// Enqueue ставит задачу в очередь и не блокирует вызывающего. false — очередь
// переполнена, голосовое пропущено. Повтор уже обрабатываемого файла молча
// схлопывается: платить дважды за одно и то же не нужно.
func (s *Service) Enqueue(job Job) bool {
	key := inflightKey(job)
	s.mu.Lock()
	if s.inflight[key] {
		s.mu.Unlock()
		return true
	}
	s.inflight[key] = true
	s.mu.Unlock()

	select {
	case s.jobs <- job:
		return true
	default:
		s.release(job)
		return false
	}
}

func (s *Service) release(job Job) {
	s.mu.Lock()
	delete(s.inflight, inflightKey(job))
	s.mu.Unlock()
}

// inflightKey — ключ дедупликации: id файла уникален внутри мессенджера.
func inflightKey(job Job) string { return job.Messenger + "/" + job.FileKey }

// process выполняет задачу. Тред не засоряем: при любом сбое — только лог.
// Единственное исключение — превышенный потолок длительности: человек должен
// понять, почему ответа нет.
func (s *Service) process(ctx context.Context, job Job) {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	log := s.log.With("messenger", job.Messenger, "file", job.FileKey, "duration", job.Duration)

	if job.Duration > s.cfg.MaxDurationSec {
		log.Info("голосовое длиннее лимита, не распознаём")
		s.reply(ctx, job, fmt.Sprintf("Голосовое длиннее %d сек — не расшифровываю.", s.cfg.MaxDurationSec), log)
		return
	}

	if text, ok, err := s.st.Transcript(ctx, job.Messenger, job.FileKey); err != nil {
		log.Warn("кэш расшифровок недоступен", "err", err)
	} else if ok {
		log.Info("расшифровка из кэша, запрос к провайдеру не нужен")
		s.reply(ctx, job, transcriptPrefix+text, log)
		return
	}

	if s.cfg.UserDailyLimitSec > 0 {
		day := s.now().UTC().Format(time.DateOnly)
		ok, err := s.st.TryReserveASR(ctx, job.Messenger, job.UserID, day,
			job.Duration, s.cfg.UserDailyLimitSec)
		if err != nil {
			log.Warn("квота ASR недоступна", "err", err)
			return
		}
		if !ok {
			log.Info("суточная квота исчерпана", "user", job.UserID, "limit_sec", s.cfg.UserDailyLimitSec)
			return
		}
	}

	audio, err := job.Fetch(ctx)
	if err != nil {
		log.Warn("не скачали голосовое", "err", err)
		return
	}

	convCtx, convCancel := context.WithTimeout(ctx, convertTimeout)
	wav, err := s.conv.ToWAV(convCtx, audio)
	convCancel()
	if err != nil {
		log.Warn("конвертация не удалась", "err", err)
		return
	}

	text, err := s.tr.Transcribe(ctx, bytes.NewReader(wav))
	if err != nil {
		log.Warn("распознавание не удалось", "err", err)
		if errors.Is(err, ErrAuth) {
			s.alertFail(ctx, "провайдер отверг ключ или исчерпан баланс — голосовые не распознаются")
		}
		return
	}
	s.alertOK(ctx)
	if strings.TrimSpace(text) == "" {
		log.Info("пустая расшифровка, в тред не пишем")
		return
	}

	// Кэш пишем до реплая: сбой отправки не должен приводить к повторной оплате.
	if err := s.st.SaveTranscript(ctx, job.Messenger, job.FileKey, text, job.Duration); err != nil {
		log.Warn("расшифровка не закэширована", "err", err)
	}
	s.reply(ctx, job, transcriptPrefix+text, log)
}

func (s *Service) reply(ctx context.Context, job Job, text string, log *slog.Logger) {
	if err := job.Reply(ctx, text); err != nil {
		log.Warn("расшифровка не отправлена", "err", err)
	}
}

func (s *Service) alertFail(ctx context.Context, detail string) {
	if a := s.currentAlerter(); a != nil {
		a.Fail(ctx, alertKey, detail)
	}
}

func (s *Service) alertOK(ctx context.Context) {
	if a := s.currentAlerter(); a != nil {
		a.OK(ctx, alertKey)
	}
}

func (s *Service) currentAlerter() *alerts.Alerter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alerter
}
