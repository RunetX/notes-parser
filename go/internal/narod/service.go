package narod

// Оркестратор: тот, кто крутит мир в реальном времени.
//
// Устроен он ровно как реплей, только часы настоящие, а сцена — площадка вместо
// архивного треда. Это не совпадение и не экономия: калибровка обязана мерить ТУ
// ЖЕ механику, которая потом работает, — поэтому кубик, память, настроение и
// летописец здесь те же самые объекты, а различаются только Clock и Stage.
//
// Такта два, и разведены они не ради параллелизма, а потому что отвечают на
// разные вопросы:
//
//	СМОТР (scan) — «что нового на сцене»: заметки-песочницы и чужие реплики.
//	  На каждое новое событие каждый житель бросает монетку РОВНО ОДИН РАЗ
//	  (ключ dice), и выпавшее «прийти» превращается в план с отложенным сроком.
//	РАБОТА (work) — «у кого срок настал»: план берётся CAS'ом, генерируется
//	  текст, ставится реплика.
//
// Слей их в один — и служба либо ждала бы генерации, не читая сцену (а значит,
// пропускала бы точки решения), либо перечитывала бы сцену на каждую реплику.
//
// РЕЖИМОВ ДВА. В live реплика уходит на площадку; в dry-run — никуда, но МИР
// ДВИЖЕТСЯ: журнал, знакомство, отношения и расход считаются так же. Это и есть
// смысл сухого прогона — посмотреть на поведение целиком, не публикуя ни строки;
// shadow-режима из брифа при этом нет вовсе, его заменила сама песочница.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// Режимы службы.
const (
	ModeDryRun = "dry-run"
	ModeLive   = "live"
)

// Ключи курсоров.
const (
	// Курсора по id здесь БОЛЬШЕ НЕТ, и это не упрощение. Он читал песочницы
	// «после номера N», то есть держался на том, что номер растёт со временем,
	// — верно, пока песочницами были только НАТИВНЫЕ заметки. С 30.08.2026 ею
	// становится и зеркальная, а её номер лежит в полосе НГС, то есть ВСЕГДА
	// ниже любого нативного: курсор, ушедший к 100000000028, не мог увидеть
	// заметку 313128 никогда. Ровно та же грабля, что у живого добора на
	// смешанном треде (platform.FreshAfter) и у линейного вида: порядок по id
	// верен ВНУТРИ полосы и неверен между ними.
	//
	// Взамен спрашивается то, что и требовалось: знает ли МИР этот тред. Мир и
	// есть память службы о виденном, а песочниц единицы — их заводят по одной
	// рукой администратора.
	stageNoteCap = 200
)

// Сорта событий, на которые бросается монетка.
const (
	eventNote  = "note"
	eventReply = "reply"
)

// ErrOffStage — заметки на сцене больше нет: администратор снял признак
// песочницы либо её скрыли.
//
// Ошибкой она называется по месту в сигнатуре, а событием является ШТАТНЫМ:
// песочницу заводят и закрывают руками, и служба обязана это пережить, закрыв
// разговор, — а не остановить смотр всем остальным песочницам. Отдельный тип
// нужен ровно затем, чтобы отличать её от настоящего отказа базы, где остановка
// как раз правильна.
var ErrOffStage = errors.New("заметки нет на сцене")

// Config — настройки службы. Все умолчания названы в Defaults вместе с доводом:
// число без довода через месяц никто не сможет ни поднять, ни опустить.
type Config struct {
	Mode      string
	ScanEvery time.Duration
	WorkEvery time.Duration

	// Потолки СВОИ, консервативнее платформенных: у площадки они защищают от
	// шторма, здесь — от каскада «житель отвечает жителю», который умеет
	// разгоняться сам и тратит деньги на каждом шаге.
	PerPersonaHour int
	PerPersonaDay  int
	PerThread      int

	// ThreadCloseAfter — сколько тишины означает, что разговор кончен. После
	// этого тред закрывается и его читает летописец.
	ThreadCloseAfter time.Duration
	// PlanCap — сколько живёт неисполненное намерение. Всё, что старше,
	// снимается: ответ через двое суток это уже не ответ.
	PlanCap time.Duration
	// AskRate — доля реплик с вопросом. ЗАМЕР по 111 тыс. реплик архива: 19,0 %
	// (живые треды на ту же тему дают 20,7 % и 21,2 %). Число КОРПУСНОЕ, как
	// соседние: личного замера в карточке пока нет.
	AskRate float64
	// OneThoughtRate — доля реплик, написанных ОДНОЙ фразой. ЗАМЕР по 111 тыс.
	// реплик архива: 75,6 % (у живого треда на ту же тему — 78 %). У нас без этого
	// жребия выходило 15,6 %: модель строит каждую реплику как сетап и панч, и
	// двухчастность узнаётся раньше содержания. Число КОРПУСНОЕ, а не донорское —
	// в карточке замера пока нет, и сказано это здесь, а не подразумевается.
	OneThoughtRate float64
	// EllipsisRate — доля реплик с многоточием. Тоже корпусный замер (22,5 %; у
	// живого треда на ту же тему 28,9 %), у нас было ноль.
	EllipsisRate float64
	// GeneralizeRate — доля реплик, в которых случай выносится на ВЕСЬ ПОЛ
	// («все бабы такие»). ЗАМЕР по 10,8 млн реплик архива: прямое обобщение стоит
	// в 0,198 % реплик вообще и в 0,48 % там, где оно в треде звучит хоть раз
	// (таких тредов 30 % от всех длиннее двадцати). Умолчание — условная доля:
	// песочницу засевают материалом про отношения, то есть ровно тем, где живые
	// обобщают. Невод замера ловит ЗАУЧЕННУЮ ФОРМУЛУ, а не поведение, поэтому
	// число это ПОЛ, а не потолок; поднимать его выше замера — авторское решение,
	// и оно должно называться таковым.
	GeneralizeRate float64
	// LatencyScale — множитель ЗАДЕРЖКИ ответа, и это единственное место службы,
	// которое сознательно ОТСТУПАЕТ от замера. Единица — человеческий темп из
	// карточки (Latency.ToReplySec): у него длинный хвост, кто-то отвечает через
	// минуту, а кто-то через девять часов, — и ровно этот хвост не даёт принять
	// жителей за ботов. Он же мертвит СТЕНД: садовник, глядящий на песочницу
	// вживую, видит две реплики и тишину до утра. Значение меньше единицы сжимает
	// время; названо оно отступлением, а не настройкой, потому что задержка —
	// замер, и подкручивая её, мы меняем не число, а правдоподобие.
	LatencyScale float64
	// DayCalls — потолок обращений к модели за сутки. Ноль — без потолка, и это
	// состояние для стенда, а не для боя.
	DayCalls int
}

// Defaults — умолчания с доводами.
func Defaults() Config {
	return Config{
		Mode: ModeDryRun,
		// Смотр раз в полминуты: задержка ответа у жителей берётся из замера и
		// меряется минутами (Latency.ToReplySec, медиана 2–20 мин у доноров), то
		// есть полминуты теряются внутри неё целиком.
		ScanEvery: 30 * time.Second,
		// Работа чаще смотра: план, созревший между тактами, обязан уйти близко
		// к своему сроку — иначе замеренная задержка превращается в замеренную
		// задержку плюс случайный хвост такта.
		WorkEvery: 10 * time.Second,
		// Два в час и восемь в сутки: у самого разговорчивого донора замер даёт
		// 22 реплики на тред, но тред у него занимает часы, а не минуты.
		PerPersonaHour: 2,
		PerPersonaDay:  8,
		// Шесть на тред — не про характер (характер меряет Dice.MaxPerThread из
		// карточки), а про деньги: в песочнице жителей десятки, и без общего
		// потолка один тред способен выбрать суточный бюджет целиком.
		PerThread: 6,
		// Двенадцать часов тишины: замер затухания (archive.MineDecay) говорит,
		// что после двенадцати разговор продолжается в пяти процентах случаев, —
		// то есть дальше ждать уже нечего.
		ThreadCloseAfter: 12 * time.Hour,
		PlanCap:          48 * time.Hour,
		AskRate:          0.19,
		OneThoughtRate:   0.756,
		EllipsisRate:     0.225,
		GeneralizeRate:   0.0048,
		// Единица — темп живых. Сжимать его можно только осознанно и только на
		// стенде, поэтому умолчание не «поудобнее», а «как у людей».
		LatencyScale: 1,
		DayCalls:     100,
	}
}

// Validate — проверка настроек. Отказ на СБОРКЕ, а не на первом такте: служба,
// молча не работающая из-за опечатки в режиме, выглядит как выключенная.
func (c Config) Validate() error {
	if c.Mode != ModeDryRun && c.Mode != ModeLive {
		return fmt.Errorf("режим %q: бывает %s или %s", c.Mode, ModeDryRun, ModeLive)
	}
	if c.GeneralizeRate < 0 || c.GeneralizeRate > 1 {
		return fmt.Errorf("доля вбросов %v: бывает от нуля до единицы", c.GeneralizeRate)
	}
	if c.LatencyScale <= 0 {
		return fmt.Errorf("множитель задержки %v: должен быть больше нуля", c.LatencyScale)
	}
	if c.ScanEvery <= 0 || c.WorkEvery <= 0 {
		return errors.New("такты службы должны быть положительными")
	}
	return nil
}

// Player — житель на сцене: карточка плюс анкета, под которой он публикуется.
type Player struct {
	Card   *Card
	UserID int64 // анкета на площадке; 0 — житель ещё не заведён (narod enroll)
}

// Service — служба народа.
type Service struct {
	cfg     Config
	world   *World
	stage   Stage
	gen     JSONGenerator
	chron   JSONGenerator // летописец: свой срок, тред читается целиком
	players []Player
	clock   Clock
	seed    uint64
	log     *slog.Logger

	// enabled — рантайм-тумблер. Живёт он НЕ в конфиге и не в мире, а в боевой
	// базе демона: выключатель обязан работать и тогда, когда мир не открылся, а
	// конфиг в контейнер монтируется файлом снаружи и правкой не переключается.
	enabled func(context.Context) bool
	// model — как назвать модель в журнале. Служба не спрашивает её у клиента:
	// у llm и rullm это разные вещи, а в gen_runs должна стоять одна строка.
	model    string
	provider string
}

// NewService собирает службу. Карточки ПРОВЕРЯЮТСЯ здесь же: в live выходят
// только композиты, и это второе из двух мест, где записано правило (первое —
// сборка конфигурации). Дублирование намеренное: цена ошибки не поломка, а
// публикация под манерой письма живого человека.
func NewService(cfg Config, w *World, stage Stage, gen JSONGenerator,
	players []Player, log *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if w == nil {
		return nil, errors.New("народ: мир не открыт")
	}
	cards := make([]Card, 0, len(players))
	for _, p := range players {
		if p.Card == nil {
			return nil, errors.New("народ: житель без карточки")
		}
		cards = append(cards, *p.Card)
	}
	if cfg.Mode == ModeLive {
		if stage == nil {
			return nil, errors.New("народ: в живом режиме нужна сцена")
		}
		if err := CheckLive(cards); err != nil {
			return nil, err
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg: cfg, world: w, stage: stage, gen: gen, players: players,
		clock: RealClock{}, seed: 1, log: log,
		enabled: func(context.Context) bool { return true },
	}, nil
}

// SetClock подменяет часы (реплей и тесты).
func (s *Service) SetClock(c Clock) { s.clock = c }

// SetSeed задаёт зерно броска. Одно на службу: монетка выводится из (зерно,
// житель, событие), поэтому перезапуск с тем же зерном повторяет решения — это
// нужно не тестам, а разбору жалоб.
func (s *Service) SetSeed(seed uint64) { s.seed = seed }

// SetGate вешает рантайм-тумблер.
func (s *Service) SetGate(f func(context.Context) bool) {
	if f != nil {
		s.enabled = f
	}
}

// SetChronicler даёт летописцу СВОЙ клиент модели. Нужен он ровно из-за срока:
// реплику пишут по двадцати последним строкам, а летопись читает ТРЕД ЦЕЛИКОМ —
// на первой боевой песочнице это 112 реплик, — и один срок на двоих означает
// либо слишком долгое ожидание у реплики, либо обрыв у летописи. Первая
// песочница закрылась с нулём сдвинутых отношений при 68 посчитанных знакомствах,
// и в отказах того же прогона лежит «context deadline exceeded» на реплике,
// которая в разы короче. Пусто — летопись идёт тем же клиентом, что и реплики.
func (s *Service) SetChronicler(gen JSONGenerator) { s.chron = gen }

// SetModel называет модель и провайдера для журнала.
func (s *Service) SetModel(provider, model string) { s.provider, s.model = provider, model }

// Run крутит оба такта до отмены. Ошибка одного такта службу не роняет: народ —
// служба некритическая, и уронить ею зеркало значило бы поменять местами цену
// вопроса. В лог она при этом идёт всегда.
func (s *Service) Run(ctx context.Context) error {
	scan := time.NewTicker(s.cfg.ScanEvery)
	work := time.NewTicker(s.cfg.WorkEvery)
	defer scan.Stop()
	defer work.Stop()

	s.log.Info("народ запущен", "режим", s.cfg.Mode, "жителей", len(s.players))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-scan.C:
			if !s.enabled(ctx) {
				continue
			}
			if err := s.Scan(ctx); err != nil {
				s.log.Error("народ: смотр", "err", err)
			}
		case <-work.C:
			if !s.enabled(ctx) {
				continue
			}
			if err := s.Work(ctx); err != nil {
				s.log.Error("народ: работа", "err", err)
			}
		}
	}
}

// Scan — один проход смотра: новые заметки, новые реплики, затихшие треды.
func (s *Service) Scan(ctx context.Context) error {
	if err := s.scanNotes(ctx); err != nil {
		return err
	}
	if err := s.scanThreads(ctx); err != nil {
		return err
	}
	return s.closeStale(ctx)
}

// scanNotes — заметки-песочницы, которых служба ещё не видела.
func (s *Service) scanNotes(ctx context.Context) error {
	notes, err := s.stage.StageNotesSince(ctx, 0, stageNoteCap)
	if err != nil {
		return err
	}
	if len(notes) == stageNoteCap {
		// Молча упереться в потолок нельзя: заметки идут по возрастанию
		// номера, и за потолком осталась бы новая песочница, а не старая.
		s.log.Warn("народ: песочниц больше потолка", "потолок", stageNoteCap)
	}
	now := s.clock.Now()
	for _, n := range notes {
		// Виденную песочницу пропускаем: за её тредом ходит scanThreads.
		known, err := s.world.KnownThread(ctx, n.ID)
		if err != nil {
			return err
		}
		if known {
			continue
		}
		// Часы жизни треда заводятся от того мгновения, когда песочница стала
		// ВИДНА жителям, а не от времени написания текста. Разница неважна у
		// свежей `narod seed` (там это одно и то же) и решает всё у заметки,
		// переведённой в песочницу из ленты: её текст может быть недельной
		// давности, а `ThreadCloseAfter` — двенадцать часов, и тред умирал бы
		// раньше, чем первый житель успеет открыть рот (поймано на первой же
		// боевой песочнице 29.08.2026: заметка суточной давности была объявлена
		// затихшей через минуту после запуска службы).
		if _, err := s.world.TouchThread(ctx, n.ID, false, now); err != nil {
			return err
		}
		for _, p := range s.players {
			// Автор заметки монетку не бросает: «прийти в новую заметку» — это
			// про чужую, а под своей человек появляется, отвечая пришедшим.
			if p.UserID == n.AuthorID {
				continue
			}
			pt := DecisionPoint{
				Now: now, Actor: p.UserID, NoteID: n.ID,
				TriggerID: 0, Trigger: n.Body,
				Author: n.AuthorID, Nick: n.AuthorNick,
			}
			if err := s.roll(ctx, p, eventKey(eventNote, n.ID), pt, n.ID, 0, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanThreads — новые реплики в живых тредах.
//
// Тред читается ЦЕЛИКОМ, а не с курсора, и это не расточительство: кубику нужен
// НОМЕР реплики по порядку (замер отклика снят по позиции в треде), а знать его
// можно только видя тред от начала. Дороговизны здесь нет — песочниц единицы, а
// перебрасывать монетку по уже виденным репликам не даёт ключ dice.
func (s *Service) scanThreads(ctx context.Context) error {
	live, err := s.world.StaleThreads(ctx, s.clock.Now().Add(time.Hour), 50)
	if err != nil {
		return err
	}
	for _, th := range live {
		if err := s.scanThread(ctx, th.NoteID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) scanThread(ctx context.Context, noteID int64) error {
	note, thread, err := s.look(ctx, noteID)
	if errors.Is(err, ErrOffStage) {
		// Заметка ушла со сцены — админ снял признак либо её скрыли. Это
		// ШТАТНОЕ событие, а не сбой: жителям там больше нечего делать, и
		// разговор закрывается ровно как у запертого треда.
		//
		// Раньше отсюда возвращалась ошибка, и она роняла ВЕСЬ смотр: одна
		// заметка, снятая со сцены неделю назад, останавливала службу для
		// всех остальных песочниц (поймано боем 30.08.2026 — смотр падал
		// каждые тридцать секунд на заметке 100000000028 и до новой не
		// доходил вовсе).
		return s.world.CloseThread(ctx, noteID)
	}
	if err != nil {
		return err
	}
	if note.Locked {
		return s.world.CloseThread(ctx, noteID)
	}
	mine, err := s.world.SpokeInThread(ctx, noteID)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	for i, c := range thread {
		for _, p := range s.players {
			// Сам себе точкой решения реплика не бывает — ни своя, ни чужая,
			// сказанная за него службой.
			if c.AuthorID == p.UserID || mine[c.ID] == p.Card.ID {
				continue
			}
			pt := DecisionPoint{
				Now: now, Actor: p.UserID, NoteID: noteID,
				TriggerID: c.ID, Trigger: c.Body,
				Author: c.AuthorID, Nick: c.AuthorNick,
				Addressed: c.ReplyTo != 0 && s.authorOf(thread, c.ReplyTo) == p.UserID,
				Gender:    c.Gender,
				Seen:      i + 1,
			}
			said, err := s.world.SaidInThread(ctx, p.Card.ID, noteID)
			if err != nil {
				return err
			}
			pt.Said = said
			if err := s.roll(ctx, p, eventKey(eventReply, c.ID), pt, noteID, c.ID, c.AuthorNick); err != nil {
				return err
			}
		}
	}
	return nil
}

// authorOf — чья это реплика. Линейным поиском намеренно: тред песочницы
// короткий, а карта на каждый такт стоила бы больше самого поиска.
func (s *Service) authorOf(thread []StageReply, id int64) int64 {
	for _, c := range thread {
		if c.ID == id {
			return c.AuthorID
		}
	}
	return 0
}

// roll — бросок монетки и, если выпало говорить, намерение в очередь.
//
// Монетка записывается ДО того, как что-то произойдёт, и ключом (житель,
// событие): пятнадцать процентов, спрошенные десять раз за десять тактов,
// превращаются в восемьдесят — урок оплачен амвоном. Здесь он важнее вдвое:
// такт смотра перечитывает тред целиком каждые полминуты.
func (s *Service) roll(ctx context.Context, p Player, event string, pt DecisionPoint,
	noteID, replyTo int64, target string) error {
	if p.UserID == 0 {
		return nil // житель ещё не заведён на площадке: играть ему нечем
	}
	if _, err := s.world.DiceOf(ctx, p.Card.ID, event); err == nil {
		return nil // уже бросали
	}
	d := &CardDecider{Card: *p.Card, Seed: s.seed}
	got, err := d.Decide(ctx, pt)
	if err != nil {
		return err
	}
	verdict := DiceSkip
	if got.Speak {
		verdict = DiceCome
	}
	if _, fresh, err := s.world.Roll(ctx, Dice{
		ActorID: p.Card.ID, EventID: event, Verdict: verdict, At: pt.Now,
	}); err != nil || !fresh {
		return err
	}
	if !got.Speak {
		return nil
	}
	// Корневая реплика адресата не имеет: житель вернулся в заметку, а не
	// ответил тому, кто его сюда позвал.
	if got.Root {
		replyTo, target = 0, ""
	}
	_, _, err = s.world.PlanReply(ctx, Plan{
		ActorID: p.Card.ID, EventID: event, NoteID: noteID,
		ReplyTo: replyTo, Target: target,
		DueAt: pt.Now.Add(s.slowdown(got.After)),
	}, pt.Now)
	return err
}

// Work — один проход работы: созревшие планы.
func (s *Service) Work(ctx context.Context) error {
	now := s.clock.Now()
	plans, err := s.world.DuePlans(ctx, now, 5)
	if err != nil {
		return err
	}
	for _, pl := range plans {
		if now.Sub(pl.CreatedAt) > s.cfg.PlanCap {
			// Протухшее намерение снимается молча: ответ через двое суток это
			// уже не ответ, а археология.
			if err := s.world.FinishPlan(ctx, pl.ID, PlanDropped, "намерение протухло"); err != nil {
				return err
			}
			continue
		}
		if err := s.execute(ctx, pl); err != nil {
			s.log.Error("народ: реплика", "житель", pl.ActorID, "заметка", pl.NoteID, "err", err)
			if err := s.world.FinishPlan(ctx, pl.ID, PlanDropped, err.Error()); err != nil {
				return err
			}
		}
	}
	return nil
}

// execute доводит один план до реплики.
func (s *Service) execute(ctx context.Context, pl Plan) error {
	// CAS ДО генерации и отправки: строка, застрявшая в posting, не
	// переотправляется никогда — площадка могла реплику и принять, ответив
	// ошибкой, а вторая копия под тем же именем хуже пропавшей.
	if err := s.world.TakePlan(ctx, pl.ID); err != nil {
		if errors.Is(err, ErrPlanTaken) {
			return nil
		}
		return err
	}
	p, ok := s.playerOf(pl.ActorID)
	if !ok {
		return s.world.FinishPlan(ctx, pl.ID, PlanDropped, "жителя нет в каталоге")
	}
	if why, err := s.overCap(ctx, p, pl); err != nil {
		return err
	} else if why != "" {
		return s.drop(ctx, pl, GenSkipped, why)
	}

	note, thread, err := s.look(ctx, pl.NoteID)
	if err != nil {
		return err
	}
	if note.Locked {
		return s.drop(ctx, pl, GenSkipped, "тред заперт")
	}
	point, err := s.compose(ctx, p, pl, note, thread)
	if err != nil {
		return err
	}
	if s.gen == nil {
		return s.drop(ctx, pl, GenSkipped, "модель не подключена")
	}
	draft, err := Write(ctx, s.gen, point, s.seed^uint64(pl.ID))
	if _, spendErr := s.world.AddSpend(ctx, s.clock.Now(), max(draft.Attempts, 1), 0, 0); spendErr != nil {
		return spendErr
	}
	if err != nil {
		return err // сеть или модель: это авария, и она пишется в журнал выше
	}
	if draft.Skip {
		if _, err := s.world.RecordGenRun(ctx, s.genRun(pl, draft, GenSkipped, draft.Reason)); err != nil {
			return err
		}
		return s.world.FinishPlan(ctx, pl.ID, PlanDone, "промолчал: "+draft.Reason)
	}
	return s.publish(ctx, p, pl, draft)
}

// publish кладёт реплику на сцену и в память жителя.
//
// В ЖУРНАЛ ПИШЕТСЯ ДО ПУБЛИКАЦИИ, и порядок этот обратный привычному: реплика,
// ушедшая на площадку и не попавшая в журнал, делает жителя противоречащим
// самому себе, а починить это потом нечем — текст уже стоит. Обратная цена
// (запись в журнале о реплике, которой на странице нет) дешевле: она сделает
// жителя молчаливее, а не безумнее.
func (s *Service) publish(ctx context.Context, p Player, pl Plan, d Draft) error {
	now := s.clock.Now()
	entry := JournalEntry{
		ActorID: p.Card.ID, At: now, Kind: JournalComment,
		NoteID: pl.NoteID, Text: d.Text,
	}
	jid, err := s.world.Remember(ctx, entry)
	if err != nil {
		return err
	}
	if s.cfg.Mode == ModeDryRun {
		if _, err := s.world.RecordGenRun(ctx, s.genRun(pl, d, GenSkipped, "сухой прогон: на площадку не ушло")); err != nil {
			return err
		}
		return s.world.FinishPlan(ctx, pl.ID, PlanDone, "сухой прогон")
	}
	id, err := s.stage.StagePost(ctx, p.UserID, pl.NoteID, pl.ReplyTo, d.Text)
	if err != nil {
		return err
	}
	if err := s.world.SetJournalComment(ctx, jid, id); err != nil {
		return err
	}
	if _, err := s.world.TouchThread(ctx, pl.NoteID, true, now); err != nil {
		return err
	}
	if _, err := s.world.RecordGenRun(ctx, s.genRun(pl, d, GenPosted, "")); err != nil {
		return err
	}
	return s.world.FinishPlan(ctx, pl.ID, PlanDone, "")
}

// compose собирает всё, из чего пишется реплика: память, настроение, жребии.
func (s *Service) compose(ctx context.Context, p Player, pl Plan,
	note StageNote, thread []StageReply) (WritePoint, error) {
	point := WritePoint{Card: p.Card, Note: note, Thread: thread}
	for _, c := range thread {
		if c.ID == pl.ReplyTo {
			point.ReplyTo = c
		}
	}
	peers := s.peersOf(ctx, note, thread)
	memory, err := WriteMemory(ctx, s.world, p.Card.ID, peers)
	if err != nil {
		return WritePoint{}, err
	}
	point.Memory = memory

	// Жребии бросаются ЗДЕСЬ, до сборки настроения: длина, эмодзи, приём
	// задевания и вброс — величины, живущие МЕЖДУ репликами, и модель, увидевшая
	// их списком или долей, решает за весь прогон разом (оплачено дважды: длиной
	// 28.08.2026 и комплиментом-суффиксом 29.08.2026).
	rng := rand.New(rand.NewPCG(s.seed^uint64(pl.ID), uint64(pl.NoteID)+1))
	mood := MoodPoint{
		Card:       *p.Card,
		Heat:       heatOf(thread, p.UserID, point.ReplyTo.AuthorID),
		Jab:        rng.IntN(len(jabWays)),
		Generalize: rng.Float64() < s.cfg.GeneralizeRate,
	}
	if point.ReplyTo.ID != 0 {
		mood.Peer = point.ReplyTo.AuthorNick
		// Пол собеседника берётся СО СЦЕНЫ, а карточка остаётся запасным
		// путём. Прежде было наоборот, и оттого пола не было вовсе у всех, кто
		// не житель: у администратора, заведшего песочницу, и у любого
		// человека, когда песочницу откроют людям, — а подтекст живёт ровно у
		// разнополой пары (subtextLine).
		mood.PeerGender = point.ReplyTo.Gender
		if mood.PeerGender == "" {
			if peer, ok := s.playerByUser(point.ReplyTo.AuthorID); ok {
				mood.PeerGender = peer.Card.Persona.Gender
			}
		}
		if a, ok, err := s.world.ActorByPlatformUser(ctx, point.ReplyTo.AuthorID); err == nil && ok {
			e, err := s.world.EdgeOf(ctx, p.Card.ID, a.ID)
			if err != nil {
				return WritePoint{}, err
			}
			mood.Tone = e.Tone()
		}
	}
	point.Mood = WriteMood(mood)

	point.TargetRunes = int(QuantileAt(p.Card.Register.Runes, rng.Float64()) + 0.5)
	point.OneThought = rng.Float64() < s.cfg.OneThoughtRate
	point.Asks = rng.Float64() < s.cfg.AskRate
	point.Ellipsis = s.cfg.EllipsisRate
	if r := p.Card.Register.EmojiRate; r > 0 {
		want := rng.Float64() < r
		point.Emoji = &want
	}
	// СОДЕРЖАНИЕ реплики — те же жребии, что длина и эмодзи, и по тому же
	// доводу: доля живёт МЕЖДУ репликами, а модель видит каждую поодиночке и
	// исполняет названную долю в каждой.
	point.Digit = rng.Float64() < p.Card.Register.DigitRate
	if r := p.Card.Register.GeneralRate; r > 0 {
		// Узкий рычаг настроения (вброс на весь пол, 0,198 % по замеру) и этот
		// широкий говорят об одном, поэтому противоречить друг другу им нельзя:
		// выпал вброс — обобщение разрешено, а не запрещено соседней строкой.
		want := mood.Generalize || rng.Float64() < r
		point.Broad = &want
	}
	return point, nil
}

// peersOf — про кого житель вспоминает. Только те, кто в разговоре УЖЕ говорил:
// память про человека, ещё не сказавшего ни слова, — это подсказка о том, кто
// сейчас придёт, и реплика начала бы отвечать на несказанное.
func (s *Service) peersOf(ctx context.Context, note StageNote, thread []StageReply) []MemoryPeer {
	seen := map[int64]bool{}
	var out []MemoryPeer
	add := func(userID int64, nick string) {
		if userID == 0 || seen[userID] {
			return
		}
		seen[userID] = true
		a, ok, err := s.world.ActorByPlatformUser(ctx, userID)
		if err != nil || !ok {
			return
		}
		out = append(out, MemoryPeer{ActorID: a.ID, Nick: nick})
	}
	add(note.AuthorID, note.AuthorNick)
	for _, c := range thread {
		add(c.AuthorID, c.AuthorNick)
	}
	return out
}

// heatOf — сколько раз пара уже обменялась репликами в этом треде.
func heatOf(thread []StageReply, me, peer int64) int {
	if me == 0 || peer == 0 {
		return 0
	}
	byID := make(map[int64]int64, len(thread))
	for _, c := range thread {
		byID[c.ID] = c.AuthorID
	}
	n := 0
	for _, c := range thread {
		to := byID[c.ReplyTo]
		if (c.AuthorID == me && to == peer) || (c.AuthorID == peer && to == me) {
			n++
		}
	}
	return n
}

// overCap — упёрся ли житель в потолок. Возвращает ПРИЧИНУ словами: она едет и
// в журнал, и в отчёт, и «промолчал» без причины через месяц не объяснить.
func (s *Service) overCap(ctx context.Context, p Player, pl Plan) (string, error) {
	now := s.clock.Now()
	if n, err := s.world.SaidSince(ctx, p.Card.ID, now.Add(-time.Hour)); err != nil {
		return "", err
	} else if s.cfg.PerPersonaHour > 0 && n >= s.cfg.PerPersonaHour {
		return fmt.Sprintf("потолок часа: уже %d реплик", n), nil
	}
	if n, err := s.world.SaidSince(ctx, p.Card.ID, now.Add(-24*time.Hour)); err != nil {
		return "", err
	} else if s.cfg.PerPersonaDay > 0 && n >= s.cfg.PerPersonaDay {
		return fmt.Sprintf("потолок суток: уже %d реплик", n), nil
	}
	th, err := s.world.ThreadOf(ctx, pl.NoteID)
	if err != nil {
		return "", err
	}
	if s.cfg.PerThread > 0 && th.PersonaN >= s.cfg.PerThread {
		return fmt.Sprintf("потолок треда: народ сказал уже %d реплик", th.PersonaN), nil
	}
	if s.cfg.DayCalls > 0 {
		spent, err := s.world.SpentOn(ctx, now)
		if err != nil {
			return "", err
		}
		if spent >= s.cfg.DayCalls {
			return fmt.Sprintf("суточный бюджет модели выбран (%d вызовов)", spent), nil
		}
	}
	return "", nil
}

// closeStale закрывает затихшие треды.
func (s *Service) closeStale(ctx context.Context) error {
	stale, err := s.world.StaleThreads(ctx, s.clock.Now().Add(-s.cfg.ThreadCloseAfter), 20)
	if err != nil {
		return err
	}
	for _, th := range stale {
		res, err := s.chronicle(ctx, th.NoteID)
		if err != nil {
			// Летопись — не публикация: сорвавшийся разбор не повод оставлять
			// тред открытым навсегда. Закрываем и говорим в лог; знакомство
			// при этом уже посчитано, оно идёт первым и без модели.
			s.log.Error("народ: летопись", "заметка", th.NoteID, "err", err)
		}
		if err := s.world.CloseThread(ctx, th.NoteID); err != nil {
			return err
		}
		moved, episodes := 0, 0
		if res != nil {
			moved, episodes = len(res.Edges), len(res.Episodes)
		}
		s.log.Info("народ: тред затих", "заметка", th.NoteID,
			"реплик народа", th.PersonaN, "рёбер", moved, "эпизодов", episodes)
	}
	return nil
}

// chronicle — разбор закончившегося треда: что он изменил между людьми.
//
// Зовётся ОДИН раз на тред, в момент закрытия, и это единственное место эпика,
// где модель судит не о ТЕКСТЕ, а о ЛЮДЯХ. Границы её власти держит код
// (narod.Chronicle): дельта с потолком, закрытый список видов эпизода, имена по
// участникам треда; модели остаются знак и повод.
//
// Знакомство при этом считается ПО ТРЕДУ и потому копится даже в сухом прогоне,
// где слов нет вовсе, — то же деление, что у голоса: бесплатное меряет кубик,
// платное двигает мир.
func (s *Service) chronicle(ctx context.Context, noteID int64) (*ChronicleResult, error) {
	note, thread, err := s.look(ctx, noteID)
	if err != nil {
		return nil, err
	}
	th := ChronicleThread{
		NoteID: noteID, NoteText: note.Body, NoteBy: note.AuthorNick,
		At: s.clock.Now(),
	}
	byUser := map[int64]string{}
	for _, p := range s.players {
		if p.UserID != 0 {
			byUser[p.UserID] = p.Card.ID
		}
	}
	for _, c := range thread {
		r := ChronicleReply{
			ID: c.ID, ActorID: byUser[c.AuthorID], Nick: c.AuthorNick,
			Text: c.Body, ReplyTo: c.ReplyTo,
		}
		if c.ReplyTo != 0 {
			r.Target = byUser[s.authorOf(thread, c.ReplyTo)]
		}
		th.Replies = append(th.Replies, r)
	}
	gen := s.chron
	if gen == nil {
		gen = s.gen
	}
	return Chronicle(ctx, s.world, gen, th)
}

// look — заметка и её тред одним вопросом: их всегда спрашивают вместе.
func (s *Service) look(ctx context.Context, noteID int64) (StageNote, []StageReply, error) {
	notes, err := s.stage.StageNotesSince(ctx, noteID-1, 1)
	if err != nil {
		return StageNote{}, nil, err
	}
	if len(notes) == 0 || notes[0].ID != noteID {
		return StageNote{}, nil, fmt.Errorf("заметка %d: %w", noteID, ErrOffStage)
	}
	thread, err := s.stage.StageThread(ctx, noteID)
	return notes[0], thread, err
}

func (s *Service) playerOf(cardID string) (Player, bool) {
	for _, p := range s.players {
		if p.Card.ID == cardID {
			return p, true
		}
	}
	return Player{}, false
}

func (s *Service) playerByUser(userID int64) (Player, bool) {
	for _, p := range s.players {
		if p.UserID == userID && userID != 0 {
			return p, true
		}
	}
	return Player{}, false
}

func (s *Service) genRun(pl Plan, d Draft, verdict, reason string) GenRun {
	return GenRun{
		PlanID: pl.ID, ActorID: pl.ActorID, At: s.clock.Now(),
		Provider: s.provider, Model: s.model, Drafts: d.Attempts,
		Verdict: verdict, Reason: reason, Text: d.Text, Rejects: d.Rejects,
	}
}

// drop — «не сказал, и вот почему»: одной записью в оба журнала.
func (s *Service) drop(ctx context.Context, pl Plan, verdict, reason string) error {
	if _, err := s.world.RecordGenRun(ctx, s.genRun(pl, Draft{}, verdict, reason)); err != nil {
		return err
	}
	return s.world.FinishPlan(ctx, pl.ID, PlanDone, reason)
}

// slowdown растягивает или сжимает замеренную задержку ответа. Отдельной
// функцией, а не выражением на месте, чтобы отступление от замера было видно
// поимённо: искать по имени — значит найти все места, где темп неправдив.
func (s *Service) slowdown(d time.Duration) time.Duration {
	if s.cfg.LatencyScale <= 0 || s.cfg.LatencyScale == 1 {
		return d
	}
	return time.Duration(float64(d) * s.cfg.LatencyScale)
}
