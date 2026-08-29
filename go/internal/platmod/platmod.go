// Пакет platmod — автомат модерации площадки: он снимает ОБЪЁМ, а решения о
// людях оставляет человеку.
//
// Зачем он есть. Владелец сказал прямо (18.08.2026): следить за тредами он не
// сможет. Площадка при этом — единственное место, где сообщество теперь
// разговаривает, а у оператора персональных данных обязанности наступают по
// факту публикации, а не по факту прочтения. Значит между «никто не смотрит» и
// «премодерация, убивающая разговор» нужно третье, и вот оно.
//
// Рамка, в которой это безопасно, записана целиком:
//
//   - ПОСТ-модерация. Публикуем сразу, проверяем следом. Задержка в пару секунд
//     убивает разговор, а закон премодерации не требует.
//   - Автомат вправе ТОЛЬКО СКРЫТЬ и только по закрытому списку (ядро,
//     platform.AutoHideable). Ни банов, ни правок, ни удалений.
//   - Ссора и колкость — жанр раздела, а не нарушение (см. prompt.go).
//   - У ложного срабатывания есть дорога назад: автору видно причину и кнопка
//     «на пересмотр» (platform.Appeal).
//   - Каждое решение пишется с моделью, отпечатком промпта и цитатой — иначе
//     через месяц «за что скрыли» отвечается догадкой.
//
// Служба живёт под общим errgroup демона и ничего не знает ни про мессенджеры,
// ни про веб: её вход — очередь в Postgres, выход — вердикты там же.
package platmod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lovegw/internal/alerts"
	"lovegw/internal/platform"
	"lovegw/internal/profanity"
)

// Store — что автомату нужно от ядра. Интерфейсом, а не *platform.Platform:
// проверять политику и разбор ответа модели против настоящего Postgres было бы
// дорого и незачем — правила живут в Go.
type Store interface {
	PendingChecks(ctx context.Context, limit, maxAttempts int) ([]platform.Pending, error)
	BumpAttempts(ctx context.Context, subs []platform.Subject) error
	RecordVerdict(ctx context.Context, s platform.Subject, v platform.VerdictRecord) error
	SameBodyCount(ctx context.Context, authorID int64, body string, window time.Duration) (int, error)
}

// JSONGenerator — онлайн-LLM, отвечающий строго по JSON-схеме (тот же контракт,
// что у дайджеста и амвона; реализация — llm.Client).
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// Дефолты службы.
const (
	// defaultInterval — как часто заглядываем в очередь. Пост-модерация, поэтому
	// секунды здесь не нужны: реплика уже опубликована, и разница между «через
	// 20 секунд» и «через минуту» ни на что не влияет.
	defaultInterval = 30 * time.Second
	// defaultBatch — сколько публикаций в одном запросе к модели. Пачкой, а не
	// по одной, ради арифметики: правила занимают около тысячи токенов, реплика
	// — сотню, и поштучный запрос платил бы за правила каждый раз. Годовой темп
	// НГС (830 тыс. комментариев) это ~2300 в сутки; десятками — 230 запросов
	// в день вместо 2300.
	defaultBatch = 10
	// defaultMaxAttempts — сколько раз пробуем строку, на которой модель
	// спотыкается. Больше трёх значит «она не ответит никогда», а очередь при
	// этом стоит.
	defaultMaxAttempts = 3
	// defaultTimeout — потолок одного запроса. Триаж короткий; минуты здесь
	// означали бы, что что-то не так с сетью, а не с текстом.
	defaultTimeout = 90 * time.Second
	// defaultDailyRequests — потолок запросов к модели в сутки. Считаем
	// ЗАПРОСЫ, а не публикации: это прямая единица счёта денег. При пачке в
	// десять штук 500 запросов это пять тысяч публикаций — вдвое больше
	// суточного темпа полностью переехавшего сообщества.
	defaultDailyRequests = 500
	// Шторм одинаковых сообщений: три повтора в час — это уже не человек,
	// пишущий одно и то же, а поток.
	defaultFloodWindow = time.Hour
	defaultFloodMax    = 3
	// alertThreshold — сколько подряд неудачных обращений к модели терпим,
	// прежде чем сказать админу.
	alertThreshold = 3
)

// alertKey — ключ троттлера. Новость у автомата ровно одна: «перестал
// проверять».
const alertKey = "модерация"

// Config — параметры службы. Нулевые значения дополняются дефолтами: пакет
// обязан подниматься и с полупустой секцией конфига.
type Config struct {
	// Model — имя модели для отчёта и для карточки проверки. Пусто — как решит
	// клиент llm.
	Model string
	// Interval, Batch, MaxAttempts, Timeout, DailyRequests — см. дефолты выше.
	Interval      time.Duration
	Batch         int
	MaxAttempts   int
	Timeout       time.Duration
	DailyRequests int
	FloodWindow   time.Duration
	FloodMax      int
	// AlertSend (может быть nil) — ЛС админу: автомат перестал отвечать.
	AlertSend func(ctx context.Context, text string)
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Batch <= 0 {
		c.Batch = defaultBatch
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.DailyRequests <= 0 {
		c.DailyRequests = defaultDailyRequests
	}
	if c.FloodWindow <= 0 {
		c.FloodWindow = defaultFloodWindow
	}
	if c.FloodMax <= 0 {
		c.FloodMax = defaultFloodMax
	}
	return c
}

// Service — автомат модерации.
type Service struct {
	cfg   Config
	st    Store
	gen   JSONGenerator
	log   *slog.Logger
	alert *alerts.Alerter

	// spent и day — потолок запросов к модели за сутки. В памяти, а не в базе:
	// это защита от неожиданного счёта, а не учёт, и пережить рестарт ей
	// незачем — рестарт как раз тот момент, когда счёт стоит начать заново.
	spent int
	day   string
}

// New собирает службу. gen = nil означает «классификатора нет»: очередь при
// этом просто копится, а модератор читает её глазами — состояние рабочее, а не
// аварийное.
func New(cfg Config, st Store, gen JSONGenerator, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:   cfg.withDefaults(),
		st:    st,
		gen:   gen,
		log:   log,
		alert: alerts.New(cfg.AlertSend, alertThreshold),
	}
}

// Run крутит службу до отмены контекста.
func (s *Service) Run(ctx context.Context) error {
	s.log.Info("автомат модерации запущен",
		"model", s.cfg.Model, "интервал", s.cfg.Interval, "пачка", s.cfg.Batch,
		"запросов_в_сутки", s.cfg.DailyRequests, "классификатор", s.gen != nil)
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

// tick — один такт: проверка очереди.
func (s *Service) tick(ctx context.Context) {
	if err := s.checkBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Warn("проверка публикаций", "err", err)
	}
}

// applyMat накладывает на ответ модели словарь мата — единственное, что автомат
// гасит по СВОЕМУ доказательству.
//
// Порядок такой: модель судит первой, и если она нашла что-то СЕРЬЁЗНЕЕ (угроза,
// наркотики, чужие данные), её вердикт остаётся — иначе тред, где выругались в
// адрес угрозы, ушёл бы в очередь как брань, и модератор не увидел бы главного.
// Мат ставится там, где модель не нашла ничего, — в том числе когда она
// промолчала вовсе: решение по нему от неё не зависит.
//
// Живёт отдельной функцией, потому что её зовут ДВОЕ: боевой такт и стенд. Пока
// правило стояло прямо в checkBatch, стенд показывал не то, что сделает бой, —
// и первый же прогон после того, как мнение модели о брани отправили человеку,
// «потерял» настоящий мат, который в бою был бы скрыт.
func applyMat(items []platform.Pending, verdicts []*platform.VerdictRecord) {
	for i, v := range verdicts {
		// Своё мнение о брани модель высказывает, но последнее слово по мату не
		// за ней: её profanity — это «человеку», и без этой оговорки словарь
		// НЕ смог бы сделать своё именно там, где мат настоящий и виден обоим.
		// Замер поймал: «Нахуя банить за флуд» съехало из скрытия в очередь
		// ровно потому, что модель тоже назвала его бранью.
		if v != nil && v.Verdict != platform.VerdictClean && v.Category != platform.CatProfanity {
			continue
		}
		quote := profanity.FindMat(items[i].Body)
		if quote == "" {
			continue
		}
		verdicts[i] = &platform.VerdictRecord{
			Verdict: platform.VerdictHidden, Category: platform.CatProfanity,
			Reason: "нецензурная брань запрещена правилами площадки (пункт 11)",
			Quote:  quote, Model: "", PromptSHA: promptSHA,
		}
	}
}

// escalateExhausted отдаёт человеку то, что автомат проверить не смог.
//
// Отказ приходит на ВСЮ пачку, и причина бывает не в нашей строке вовсе:
// провайдер умеет отказаться читать входной текст целиком из-за одной реплики.
// Попытки при этом сгорают у всех десяти, а исчерпавшая их строка выпадает из
// PendingChecks навсегда — то есть не проверена ни машиной, ни человеком.
// Замер 23.08.2026: две пачки из семидесяти четырёх, двадцать строк.
//
// «Не смогли проверить» — это не «проверено». Поэтому последняя попытка не
// теряет строку, а ставит её в очередь модератору; категория «на усмотрение»,
// цитаты нет — её и неоткуда взять.
func (s *Service) escalateExhausted(ctx context.Context, items []platform.Pending) {
	for _, it := range items {
		if it.Attempts+1 < s.cfg.MaxAttempts {
			continue
		}
		if err := s.st.RecordVerdict(ctx, it.Subject, platform.VerdictRecord{
			Verdict:  platform.VerdictReview,
			Category: platform.CatOther,
			Reason:   "автомат не смог проверить эту публикацию",
			Model:    s.cfg.Model,
		}); err != nil {
			s.log.Error("не удалось передать человеку непроверенное",
				"объект", it.Subject.String(), "ошибка", err)
			return
		}
		s.log.Info("непроверенное передано человеку", "объект", it.Subject.String())
	}
}

// checkBatch берёт порцию очереди и выносит по ней мнение.
func (s *Service) checkBatch(ctx context.Context) error {
	items, err := s.st.PendingChecks(ctx, s.cfg.Batch, s.cfg.MaxAttempts)
	if err != nil || len(items) == 0 {
		return err
	}
	// Шторм ловится ДО модели и без неё: нарушение здесь состоит в повторе, а
	// модель видит одну реплику и увидеть повтор не может в принципе. Заодно это
	// самый дешёвый способ погасить самый частый вид атаки.
	rest := items[:0]
	for _, it := range items {
		flood, err := s.isFlood(ctx, it)
		if err != nil {
			return err
		}
		if !flood {
			rest = append(rest, it)
			continue
		}
		if err := s.st.RecordVerdict(ctx, it.Subject, platform.VerdictRecord{
			Verdict:  platform.VerdictHidden,
			Category: platform.CatFlood,
			Reason:   "одно и то же сообщение подряд",
			Model:    "",
		}); err != nil {
			return err
		}
		s.log.Info("скрыт повтор", "объект", it.Subject.String(), "автор", it.AuthorID)
	}
	items = rest
	if len(items) == 0 || s.gen == nil {
		return nil
	}
	if !s.takeBudget() {
		return nil
	}
	// Попытка засчитывается ДО запроса: строка, на которой модель падает
	// воспроизводимо, иначе попадала бы в каждую пачку вечно.
	subs := make([]platform.Subject, len(items))
	for i, it := range items {
		subs[i] = it.Subject
	}
	if err := s.st.BumpAttempts(ctx, subs); err != nil {
		return err
	}
	verdicts, err := s.classify(ctx, items)
	if err != nil {
		s.alert.Fail(ctx, alertKey, "классификатор не отвечает: "+err.Error())
		s.escalateExhausted(ctx, items)
		return err
	}
	s.alert.OK(ctx, alertKey)
	applyMat(items, verdicts)
	for i, v := range verdicts {
		if v == nil {
			continue // модель промолчала про этот номер — попробуем в следующий раз
		}
		if err := s.st.RecordVerdict(ctx, items[i].Subject, *v); err != nil {
			return err
		}
		if v.Verdict != platform.VerdictClean {
			s.log.Info("мнение автомата", "объект", items[i].Subject.String(),
				"вердикт", int(v.Verdict), "категория", v.Category)
		}
	}
	return nil
}

// isFlood — тот же текст от того же автора уже несколько раз за окно.
func (s *Service) isFlood(ctx context.Context, it platform.Pending) (bool, error) {
	if it.AuthorID == 0 || it.Subject.IsNote() {
		// У заметок свой потолок частоты (одна в пять минут), и повтор текста
		// там — это скорее правка через новую публикацию, чем шторм.
		return false, nil
	}
	n, err := s.st.SameBodyCount(ctx, it.AuthorID, it.Body, s.cfg.FloodWindow)
	if err != nil {
		return false, err
	}
	return n > s.cfg.FloodMax, nil
}

// classify спрашивает модель про всю пачку разом. Возвращает срез той же длины,
// что и вход; nil на месте означает «модель про этот номер не ответила».
func (s *Service) classify(ctx context.Context, items []platform.Pending) ([]*platform.VerdictRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	raw, err := s.gen.GenerateJSON(ctx, systemPrompt, buildPrompt(items), verdictSchema())
	if err != nil {
		return nil, fmt.Errorf("классификатор: %w", err)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("ответ классификатора: %w", err)
	}
	out := make([]*platform.VerdictRecord, len(items))
	for _, it := range a.Items {
		i := it.N - 1
		if i < 0 || i >= len(items) || out[i] != nil {
			continue // номер не из этой пачки или повторный — молча пропускаем
		}
		rec := decide(it)
		rec.Model, rec.PromptSHA = s.cfg.Model, promptSHA
		out[i] = &rec
	}
	return out, nil
}

// Triage — прогон пачки БЕЗ каких-либо записей: стенд для замера модели и
// промпта (команда `platform triage`).
//
// Не пишет ничего: ни вердиктов, ни попыток, ни расхода из суточного потолка, —
// и потому безопасен при работающем демоне. Нужен он вот зачем: решение
// автомата это право машины убрать чужие слова, и проверять его надо ДО того,
// как она это право получит, а честная проверка одна — прогнать настоящие
// реплики и посмотреть глазами.
//
// Шторм одинаковых сообщений здесь не считается намеренно: его ловит код по
// базе, а не модель, и мнения о нём у неё нет.
func (s *Service) Triage(ctx context.Context, items []platform.Pending) ([]*platform.VerdictRecord, error) {
	if s.gen == nil {
		return nil, errors.New("классификатор не настроен: см. platform.moderation")
	}
	verdicts, err := s.classify(ctx, items)
	if err != nil {
		return nil, err
	}
	// Тот же словарь мата, что и в бою: стенд обязан показывать решение
	// ЦЕЛИКОМ, иначе он предсказывает не то, что произойдёт.
	applyMat(items, verdicts)
	return verdicts, nil
}

// takeBudget списывает один запрос из суточного потолка.
func (s *Service) takeBudget() bool {
	day := time.Now().UTC().Format(time.DateOnly)
	if day != s.day {
		s.day, s.spent = day, 0
	}
	if s.spent >= s.cfg.DailyRequests {
		// Не ошибка: очередь просто ждёт человека. Говорим об этом один раз в
		// сутки — строкой на такт лог превратился бы в шум.
		if s.spent == s.cfg.DailyRequests {
			s.log.Warn("суточный потолок запросов к классификатору исчерпан",
				"потолок", s.cfg.DailyRequests)
			s.spent++
		}
		return false
	}
	s.spent++
	return true
}
