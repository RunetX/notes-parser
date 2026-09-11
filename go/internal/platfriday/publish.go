package platfriday

// Публикация выпуска и служба, которая ведёт вечер.
//
// ВЕСЬ ПОРЯДОК ВЫВЕДЕН ИЗ БАЗЫ, и своего состояния служба не держит вовсе.
// Заметка выпуска ключуется неделей (`friday_issues`), число заданных вопросов
// считается по самим вопросам, а какой следующий — берётся из `Build`, который
// детерминирован той же неделей. Отсюда три свойства даром: рестарт посреди
// вечера ничего не теряет, второй прогон команды ничего не удваивает, а
// владелец, посмотревший черновик, увидит опубликованным ровно его.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lovegw/internal/platform"
)

// Poster — что рубрике нужно от площадки. Интерфейсом, чтобы публикация
// проверялась подделкой: настоящая пишет в Postgres и ходит через все гейты.
type Poster interface {
	EnsureSystemUser(ctx context.Context, nick string) (int64, error)
	CreateNote(ctx context.Context, in platform.NewNote) (int64, error)
	CreateComment(ctx context.Context, in platform.NewComment) (int64, error)
	FridayIssue(ctx context.Context, week string) (int64, error)
	StartFridayIssue(ctx context.Context, week string, noteID int64) (int64, error)
	FridayAsked(ctx context.Context, noteID int64) (int, error)
	QuizStats(ctx context.Context, noteID int64) ([]platform.QuizStat, error)
}

// Config — как рубрика ведёт вечер.
type Config struct {
	// Weekday и Hour — слот выпуска. Пятница 16:00 Нск по умолчанию, и час здесь
	// не «вечер» нарочно: замер КПН (1114 заметок, 650 тыс. реплик) говорит, что
	// дневная заметка набирает за шесть часов 55 % и живёт до полуночи, а
	// вечерняя выгорает за час-полтора — толпа приходит в 20–23 независимо от
	// того, когда выложено.
	Weekday time.Weekday
	Hour    int
	// Questions — сколько вопросов за вечер.
	Questions int
	// Gap — через сколько выходит следующий вопрос. Час: тред получает шесть
	// толчков за вечер вместо одного, и у разговора есть время случиться.
	Gap time.Duration
	// SiteNick — ник служебной анкеты площадки, под которой выходит рубрика.
	SiteNick string
	// Live — публиковать по-настоящему. Ложь означает сухой прогон: выпуск
	// собирается целиком, а на площадку не уходит ни строки, — тот же тумблер и
	// тот же довод, что у народа.
	Live bool
	Log  *slog.Logger
}

// Result — что сделал проход.
type Result struct {
	Week    string
	NoteID  int64
	Posted  int
	Asked   int
	Skipped string
}

// Step — один проход: завести выпуск, если пора, и задать очередной вопрос,
// если подошёл его час. Идемпотентен, безопасен при работающем демоне.
func Step(ctx context.Context, p Poster, src Source, cfg Config, now time.Time) (Result, error) {
	week := WeekOf(now)
	res := Result{Week: week}

	slot := slotOf(now, cfg)
	if now.Before(slot) {
		res.Skipped = "слот недели ещё не наступил"
		return res, nil
	}

	noteID, err := p.FridayIssue(ctx, week)
	if err != nil {
		return res, err
	}
	issue, err := Build(ctx, src, now, cfg.Questions)
	if err != nil {
		return res, err
	}

	if noteID == 0 {
		if !cfg.Live {
			res.Skipped = "сухой прогон: заметка не опубликована"
			return res, nil
		}
		noteID, err = publishIntro(ctx, p, cfg, issue)
		if err != nil {
			return res, err
		}
		res.Posted++
	}
	res.NoteID = noteID

	asked, err := p.FridayAsked(ctx, noteID)
	if err != nil {
		return res, err
	}
	res.Asked = asked
	if asked >= len(issue.Questions) {
		res.Skipped = "все вопросы вечера заданы"
		return res, nil
	}
	// Час следующего вопроса считается от ПОСЛЕДНЕГО заданного, а не от слота:
	// служба могла простоять, и догонять пропущенное залпом незачем — шесть
	// вопросов подряд это не вечер, а список.
	if asked > 0 {
		last, err := lastAsked(ctx, p, noteID)
		if err != nil {
			return res, err
		}
		if now.Sub(last) < cfg.Gap {
			res.Skipped = "следующий вопрос ещё не созрел"
			return res, nil
		}
	}
	if !cfg.Live {
		res.Skipped = "сухой прогон: вопрос не опубликован"
		return res, nil
	}
	if err := publishQuestion(ctx, p, cfg, noteID, issue.Questions[asked]); err != nil {
		return res, err
	}
	res.Posted++
	res.Asked = asked + 1
	return res, nil
}

func publishIntro(ctx context.Context, p Poster, cfg Config, issue Issue) (int64, error) {
	author, err := p.EnsureSystemUser(ctx, cfg.SiteNick)
	if err != nil {
		return 0, fmt.Errorf("служебная анкета: %w", err)
	}
	// KeepHere не нужен: служебная анкета не проходит вынос на НГС сама — там
	// спрашивается kind = KindMember (см. platngs). Рубрика остаётся здешней
	// СТРУКТУРНО, а не проверкой, которую однажды забудут.
	id, err := p.CreateNote(ctx, platform.NewNote{AuthorID: author, Body: issue.Intro})
	if err != nil {
		return 0, fmt.Errorf("вступительная заметка: %w", err)
	}
	got, err := p.StartFridayIssue(ctx, issue.Week, id)
	if err != nil {
		return 0, err
	}
	if got != id {
		// Неделя уже занята: другой проход успел раньше. Наша заметка при этом
		// опубликована — сказать об этом вслух честнее, чем молча продолжить.
		return got, fmt.Errorf("выпуск недели %s уже заведён заметкой %d, а мы опубликовали %d",
			issue.Week, got, id)
	}
	if cfg.Log != nil {
		cfg.Log.Info("пятничная рубрика: выпуск открыт", "неделя", issue.Week, "заметка", id)
	}
	return id, nil
}

func publishQuestion(ctx context.Context, p Poster, cfg Config, noteID int64, q Question) error {
	author, err := p.EnsureSystemUser(ctx, cfg.SiteNick)
	if err != nil {
		return fmt.Errorf("служебная анкета: %w", err)
	}
	quiz := q.Quiz
	id, err := p.CreateComment(ctx, platform.NewComment{
		NoteID: noteID, AuthorID: author, Body: q.Body, Quiz: &quiz,
	})
	if err != nil {
		return fmt.Errorf("вопрос (%s): %w", q.Kind, err)
	}
	if cfg.Log != nil {
		cfg.Log.Info("пятничная рубрика: вопрос задан",
			"вид", q.Kind, "комментарий", id, "источник", quiz.SourceNote)
	}
	return nil
}

func lastAsked(ctx context.Context, p Poster, noteID int64) (time.Time, error) {
	stats, err := p.QuizStats(ctx, noteID)
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, s := range stats {
		if s.Asked.After(last) {
			last = s.Asked
		}
	}
	return last, nil
}

// slotOf — момент слота на ТОЙ ЖЕ неделе, что и `now`.
//
// Именно на той же, а не «ближайший прошедший»: выпуск ключуется неделей
// (`weekOf`), и слот обязан считаться по тому же календарю. Иначе в среду
// службе видна пятница ПРОШЛОЙ недели — то есть слот, который давно наступил, —
// и она открыла бы выпуск на три дня раньше, записав его текущей неделей.
func slotOf(now time.Time, cfg Config) time.Time {
	local := now.In(nskLoc())
	slot := time.Date(local.Year(), local.Month(), local.Day(), cfg.Hour, 0, 0, 0, nskLoc())
	// Понедельник этой недели, затем — нужный день от него.
	back := (int(slot.Weekday()) + 6) % 7
	monday := slot.AddDate(0, 0, -back)
	return monday.AddDate(0, 0, (int(cfg.Weekday)+6)%7)
}

// Service — служба рубрики: тикает и ведёт вечер сама.
type Service struct {
	p    Poster
	src  Source
	cfg  Config
	tick time.Duration
}

// NewService — служба с тактом в пять минут: час между вопросами меряется
// грубо, а чаще ходить в базу незачем.
func NewService(p Poster, src Source, cfg Config) *Service {
	return &Service{p: p, src: src, cfg: cfg, tick: 5 * time.Minute}
}

// Run ведёт рубрику, пока жив контекст. Ошибка прохода не роняет службу: пустая
// неделя и отказ базы — не повод замолчать до перезапуска.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			res, err := Step(ctx, s.p, s.src, s.cfg, time.Now())
			switch {
			case errors.Is(err, ErrNoMaterial):
				if s.cfg.Log != nil {
					s.cfg.Log.Warn("пятничная рубрика: нет материала", "неделя", res.Week)
				}
			case err != nil:
				if s.cfg.Log != nil {
					s.cfg.Log.Error("пятничная рубрика", "ошибка", err)
				}
			}
		}
	}
}
