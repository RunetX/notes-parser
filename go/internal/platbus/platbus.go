// Пакет platbus — тикер шины событий площадки: раздаёт поводы, разбирает
// реакции и убирает состарившееся.
//
// Почему это отдельная служба, а не работа внутри ядра. Ядро горутин не крутит
// вовсе — это его свойство, а не случайность: `platform` зовут и демон, и
// веб-морда, и разовые команды, и служба, поднявшаяся у каждого из них, делала
// бы одну и ту же работу вчетвером. Здесь же тикер один, живёт под общим
// errgroup ДЕМОНА и знает про ядро ровно четыре метода.
//
// Почему раздача вообще идёт фоном, а не в транзакции публикации, написано в
// шапке platform/events.go. Коротко: правил адресации больше одного, они
// дорожают, и платить их временем пишущего значит замедлять разговор ради
// удобства читающих.
//
// Три дела с разными тактами, и разница между ними не в важности, а в природе:
//
//   - РАЗДАЧА — секунды. Повод, приехавший через минуту, читается как поломка:
//     человек уже видел ответ на странице и ждёт, что колокольчик знает о нём.
//   - РЕАКЦИИ — минуты. Нажатие стоит одного движения, и событие на каждое
//     превратило бы страницу событий в счётчик. Медленный такт — это и есть то,
//     что схлопывает десять нажатий в одну строку.
//   - УБОРКА — сутки. Сроки хранения меряются месяцами (platform.KeepRead и
//     соседи), и чаще раза в день это работа впустую.
//
// Отказ прохода не роняет демона: он логируется и всё. Шина — удобство поверх
// разговора, а зеркало и мост — сам разговор, и падать вместе с первой второму
// незачем.
package platbus

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"lovegw/internal/platform"
)

// Store — что тикеру нужно от ядра. Интерфейсом, а не *platform.Platform:
// расписание проверяется без Postgres, а правила адресации живут в SQL и
// проверяются против настоящей базы (events_pg_test.go).
type Store interface {
	FanOut(ctx context.Context, limit int) (int, error)
	NoticeReactions(ctx context.Context, limit int) (int, error)
	PruneBus(ctx context.Context, limit int) (platform.BusPruned, error)
}

// Дефолты службы.
const (
	// defaultInterval — такт раздачи.
	defaultInterval = 5 * time.Second
	// defaultBatch — сколько фактов раздаём за проход. Ядро всё равно урезает
	// сотней (clampLimit), и это правильный потолок: пачка идёт одной
	// транзакцией, а держать её длиннее незачем — следующий проход через пять
	// секунд.
	defaultBatch = 100
	// reactionsEvery — как часто разбираем нажатия. См. шапку: медленный такт
	// здесь не экономия, а способ схлопнуть поток в одну строку.
	reactionsEvery = 5 * time.Minute
	// reactionsBatch — сколько нажатий за проход.
	reactionsBatch = 100
	// pruneEvery — как часто убираем состарившееся.
	pruneEvery = 24 * time.Hour
	// pruneBatch и pruneRounds — уборка идёт порциями и не бесконечно: порция
	// держит короткую транзакцию, а потолок кругов не даёт одному проходу
	// зачистить полмиллиона строк, заняв базу на всё это время. Не успевшее
	// уйти сегодня уйдёт завтра — сроки хранения меряются месяцами, и сутки
	// опоздания им безразличны.
	pruneBatch  = 100
	pruneRounds = 50
)

// Config — параметры службы. Нулевые значения дополняются дефолтами: пакет
// обязан подниматься и с пустой секцией конфига.
type Config struct {
	Interval time.Duration
	Batch    int
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Batch <= 0 {
		c.Batch = defaultBatch
	}
	return c
}

// Service — тикер шины.
type Service struct {
	cfg Config
	st  Store
	log *slog.Logger

	// reacted и pruned — когда в последний раз делали редкие дела. В памяти, а
	// не в базе: это расписание, а не состояние данных. Рестарт сдвигает его на
	// такт и ничего не портит — оба дела идемпотентны.
	reacted time.Time
	pruned  time.Time
}

// New собирает службу.
func New(cfg Config, st Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg.withDefaults(), st: st, log: log}
}

// Run крутит службу до отмены контекста.
func (s *Service) Run(ctx context.Context) error {
	s.log.Info("шина событий запущена", "интервал", s.cfg.Interval, "пачка", s.cfg.Batch)
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// Pass — что сделал один проход. Возвращается разовой команде: у службы,
// работающей молча, это единственный способ показать, что она работает.
type Pass struct {
	Fanned    int
	Reactions int
	Pruned    platform.BusPruned
}

// Once делает всё, что делает такт, но не глядя на расписание редких дел, и
// возвращает сделанное. Разовая команда `lovegw platform events` — это про
// «покажи, что шина жива», а не «подожди пять минут до следующего такта».
func (s *Service) Once(ctx context.Context) (Pass, error) {
	var p Pass
	var err error
	// Реакции — ПЕРЕД раздачей: их разбор сам заводит события, и сделав его
	// вторым, мы бы оставляли свежие поводы нерозданными до следующего прохода.
	// Для разовой команды это ещё и вопрос честности вывода: «ждёт раздачи 1»
	// сразу после прохода читается как поломка.
	if p.Reactions, err = s.st.NoticeReactions(ctx, reactionsBatch); err != nil {
		return p, err
	}
	if p.Fanned, err = s.st.FanOut(ctx, s.cfg.Batch); err != nil {
		return p, err
	}
	p.Pruned, err = s.prune(ctx)
	return p, err
}

// tick — один такт. Раздача каждый раз, редкие дела по своим срокам.
//
// Порядок не случаен: разбор реакций стоит ПЕРЕД раздачей, потому что сам
// заводит события, — иначе они ждали бы следующего такта на ровном месте.
func (s *Service) tick(ctx context.Context) {
	s.rare(&s.reacted, reactionsEvery, func() error {
		n, err := s.st.NoticeReactions(ctx, reactionsBatch)
		if err == nil && n > 0 {
			s.log.Debug("разобраны реакции", "объектов", n)
		}
		return err
	}, "разбор реакций")
	if _, err := s.st.FanOut(ctx, s.cfg.Batch); err != nil {
		s.warn("раздача поводов", err)
	}
	s.rare(&s.pruned, pruneEvery, func() error {
		p, err := s.prune(ctx)
		if err == nil && p.Any() {
			s.log.Info("уборка шины",
				"прочитанных", p.Read, "непрочитанных", p.Unread, "фактов", p.Events)
		}
		return err
	}, "уборка шины")
}

// rare выполняет дело не чаще, чем раз в every. Отметка ставится ДО работы:
// иначе отказавшее дело повторялось бы каждый такт, а оно на то и редкое, что
// подождёт до следующего срока.
func (s *Service) rare(last *time.Time, every time.Duration, do func() error, what string) {
	if time.Since(*last) < every {
		return
	}
	*last = time.Now()
	if err := do(); err != nil {
		s.warn(what, err)
	}
}

// prune убирает состарившееся порциями, пока не кончится или пока не упрётся в
// потолок кругов.
func (s *Service) prune(ctx context.Context) (platform.BusPruned, error) {
	var total platform.BusPruned
	for range pruneRounds {
		p, err := s.st.PruneBus(ctx, pruneBatch)
		if err != nil {
			return total, err
		}
		total.Read += p.Read
		total.Unread += p.Unread
		total.Events += p.Events
		if !p.Any() {
			break
		}
	}
	return total, nil
}

// warn — отказ прохода. Отмена контекста отказом не считается: это штатное
// завершение демона.
func (s *Service) warn(what string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.log.Warn(what, "err", err)
}
