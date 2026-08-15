// Пакет pulpit — «амвон»: собственная реплика владельца под каждой новой
// заметкой love.ngs.ru, и по возможности первая в треде.
//
// Смысл фичи — присутствие: читатель должен видеть один и тот же голос под
// каждой заметкой. Отсюда три следствия, определившие устройство пакета:
//
//   - СКОРОСТЬ. Первый чужой комментарий приходит через медианные 164 секунды
//     после публикации, а зеркало узнаёт о заметке за p90 = 619 с — оно делит
//     общий лимитер love.Client с опросом комментариев всех живых заметок.
//     Поэтому у амвона свой клиент сайта, свой (медленный, 0,2 rps) лимитер и
//     свой цикл обхода ленты; колбэк зеркала (Config.OnNewNote) остаётся
//     страховкой, оба входа сходятся на store.TryClaimPulpitNote.
//   - ОДНОКРАТНОСТЬ. Дубль комментария необратим — отозвать реплику нечем.
//     Однократность держится не транзакциями, а состоянием в БД: заметку
//     занимает ровно один INSERT, переход queued → posting пишется ДО отправки,
//     и застрявшая в posting строка не переотправляется никогда (её судьбу
//     решает верификация треда).
//   - ПРЕДОХРАНИТЕЛЬ. Запрет писать в «Заметки» ничего не убирает с площадки и
//     потому невидим в событиях: единственный признак — «страница прочиталась,
//     а нашей реплики в ней нет». Три таких промаха подряд гасят фичу через
//     settings['pulpit.enabled'], и обратно её включает только человек: срок
//     запрета неизвестен, автовключение = второй бан.
//
// Пакет мессенджер-агностичен: про tgx/maxx он не знает, алерт — замыкание,
// ручка /pulpit ходит сюда через интерфейс, объявленный в dmbot.
package pulpit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"lovegw/internal/alerts"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// Site — то, что амвон требует от клиента сайта (реализация — siteAdapter
// поверх *love.Client, см. site.go).
type Site interface {
	FetchNotes(ctx context.Context) ([]love.Note, error)
	FetchCommentsPage(ctx context.Context, noteID string) (love.CommentsPage, error)
	// TreeComments — тред в древовидном виде: только он проставляет ParentID,
	// без которого не найти ответы на нашу реплику.
	TreeComments(ctx context.Context, noteID string) ([]love.Comment, error)
	PostComment(ctx context.Context, cookies []*http.Cookie, noteID, comAPIID, text string) error
	ProfileControl(ctx context.Context, cookies []*http.Cookie) (love.ProfileControl, error)
	SiteIdentity(ctx context.Context, cookies []*http.Cookie) (profileID, passportID, nick string, err error)
}

// JSONGenerator — онлайн-LLM, отвечающий строго по JSON-схеме (тот же контракт,
// что у дайджеста; реализация — llm.Client).
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// Config — параметры амвона. Нулевые значения дополняются дефолтами (см.
// withDefaults): служба должна подниматься и с полупустой секцией конфига.
type Config struct {
	OwnerProfileID string // id анкеты владельца: сессия и опознание своей реплики
	BaseURL        string // адрес сайта — для ссылок на анкор в ручке
	Model          string // имя модели, только для отчёта в /pulpit

	FeedInterval    time.Duration
	Freshness       time.Duration
	MaxLatency      time.Duration
	GenerateTimeout time.Duration
	MaxPerDay       int

	MinRunes     int
	MaxRunes     int
	MaxLines     int
	AllowEmoji   bool
	HistorySize  int
	FormCooldown int

	ReplyProbability float64
	RepliesPerNote   int
	RepliesPerDay    int
	ReplyWindow      time.Duration
	FuseMisses       int

	// AlertSend (может быть nil) — ЛС админу: срабатывание предохранителя.
	AlertSend func(ctx context.Context, text string)
}

// Дефолты службы — те же числа, что в config.Load, но пакет обязан быть
// самодостаточным: его поднимают и из CLI (pulpit draft), где конфига секции
// может не быть вовсе.
const (
	defaultFeedInterval    = 20 * time.Second
	defaultFreshness       = 15 * time.Minute
	defaultMaxLatency      = 180 * time.Second
	defaultGenerateTimeout = 45 * time.Second
	defaultMaxPerDay       = 25
	defaultMinRunes        = 40
	defaultMaxRunes        = 400
	defaultMaxLines        = 12
	defaultHistorySize     = 20
	defaultFormCooldown    = 5
	defaultRepliesPerNote  = 1
	defaultRepliesPerDay   = 3
	defaultReplyWindow     = 24 * time.Hour
	defaultFuseMisses      = 3
)

// eventQueue — сколько заметок держим от колбэка зеркала. Больше и не нужно:
// колбэк — страховка, основной путь свой.
const eventQueue = 8

// alertKey — ключ троттлера алертов. Он один: у амвона ровно одна новость,
// стоящая ЛС админу, — «выключился сам».
const alertKey = "амвон"

// identityTTL — как часто перечитывать свой ник с сайта. Ник протухает: у одной
// анкеты в боевой БД две строки сессий с разными никами («Монах» 26.07,
// «Рантье» 30.07), а по нику опознаются ответы нам.
const identityTTL = 24 * time.Hour

func (c Config) withDefaults() Config {
	if c.FeedInterval <= 0 {
		c.FeedInterval = defaultFeedInterval
	}
	if c.Freshness <= 0 {
		c.Freshness = defaultFreshness
	}
	if c.MaxLatency <= 0 {
		c.MaxLatency = defaultMaxLatency
	}
	if c.GenerateTimeout <= 0 {
		c.GenerateTimeout = defaultGenerateTimeout
	}
	if c.MaxPerDay <= 0 {
		c.MaxPerDay = defaultMaxPerDay
	}
	if c.MinRunes <= 0 {
		c.MinRunes = defaultMinRunes
	}
	if c.MaxRunes <= 0 {
		c.MaxRunes = defaultMaxRunes
	}
	if c.MaxLines <= 0 {
		c.MaxLines = defaultMaxLines
	}
	if c.HistorySize <= 0 {
		c.HistorySize = defaultHistorySize
	}
	if c.FormCooldown <= 0 {
		c.FormCooldown = defaultFormCooldown
	}
	if c.RepliesPerNote <= 0 {
		c.RepliesPerNote = defaultRepliesPerNote
	}
	if c.RepliesPerDay <= 0 {
		c.RepliesPerDay = defaultRepliesPerDay
	}
	if c.ReplyWindow <= 0 {
		c.ReplyWindow = defaultReplyWindow
	}
	if c.FuseMisses <= 0 {
		c.FuseMisses = defaultFuseMisses
	}
	return c
}

// Service — служба амвона под общим errgroup демона.
type Service struct {
	st   *store.Store
	site Site
	gen  JSONGenerator
	cfg  Config
	log  *slog.Logger
	// alert — порог 1: о срабатывании предохранителя админ узнаёт сразу, но
	// один раз (дальше фича выключена и сообщать не о чем).
	alert *alerts.Alerter

	// rand — монетка «отвечать ли на ответ». Поле, а не пакетная функция,
	// чтобы тест был детерминированным.
	rand   func() float64
	events chan love.Note

	mu     sync.Mutex
	nick   string    // свой ник на сайте: по нему опознаются обращения к нам
	nickAt time.Time // когда снимали
	// cold — первый обход после старта или после включения тумблера: заметки
	// ленты только помечаются, иначе рестарт демона выдал бы пять проповедей
	// подряд под старьё.
	cold bool
	// wasEnabled — состояние тумблера на прошлом такте: переход выключено →
	// включено снова взводит холодный старт.
	wasEnabled bool
	// replyAt / replyCursor — темп обхода веток: ответы не срочные, и смотреть
	// все треды каждый такт значило бы утроить нагрузку на сайт (см. reply.go).
	replyAt     time.Time
	replyCursor int
}

// New собирает службу. gen == nil — генерировать нечем: служба поднимется, но
// писать не будет (об этом скажет лог на старте).
func New(st *store.Store, site Site, gen JSONGenerator, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		st:     st,
		site:   site,
		gen:    gen,
		cfg:    cfg.withDefaults(),
		log:    log,
		alert:  alerts.New(cfg.AlertSend, 1),
		rand:   rand.Float64,
		events: make(chan love.Note, eventQueue),
		cold:   true,
	}
}

// OnNewNote — страховочный вход: зеркало увидело новую заметку. Неблокирующий
// по построению: зеркалирование не должно ждать амвон (переполненную очередь
// подберёт свой обход ленты).
func (s *Service) OnNewNote(_ context.Context, n love.Note) {
	select {
	case s.events <- n:
	default:
		s.log.Warn("амвон: очередь заметок переполнена", "note", n.ID)
	}
}

// Run крутит цикл до отмены контекста. Ошибки внутрь не выпускаются: амвон —
// не критичная для зеркала служба, его сбой не должен ронять демон.
func (s *Service) Run(ctx context.Context) error {
	s.log.Info("амвон запущен",
		"анкета", s.cfg.OwnerProfileID, "модель", s.cfg.Model,
		"интервал_ленты", s.cfg.FeedInterval, "свежесть", s.cfg.Freshness,
		"потолок_в_сутки", s.cfg.MaxPerDay, "вероятность_ответа", s.cfg.ReplyProbability)
	if s.gen == nil {
		s.log.Warn("амвон: LLM не настроен — реплик не будет (нужен llm.api_key)")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case n := <-s.events:
			// Страховочный вход. Холодный старт действует и здесь: после
			// простоя демона зеркало считает новыми все пропущенные заметки, и
			// без этого под ними разом появилась бы очередь проповедей.
			// Суточный счёт (-1) берётся из БД на месте — такт ленты сюда
			// своего счётчика не передаёт.
			s.mu.Lock()
			cold := s.cold
			s.mu.Unlock()
			s.handleNote(ctx, n, cold, -1)
		case <-timer.C:
			s.cycle(ctx)
			timer.Reset(jitter(s.cfg.FeedInterval))
		}
	}
}

// cycle — один такт: лента, догон застрявших, верификация, предохранитель,
// ответы на ответы. Порядок по убыванию срочности: свежая заметка ждать не может.
func (s *Service) cycle(ctx context.Context) {
	enabled, err := s.Enabled(ctx)
	if err != nil {
		s.log.Error("амвон: чтение тумблера", "err", err)
		return
	}
	s.mu.Lock()
	if enabled && !s.wasEnabled {
		// Включили руками: первый обход после этого — холодный, иначе под
		// старыми заметками ленты разом появится очередь проповедей.
		s.cold = true
	}
	s.wasEnabled = enabled
	cold := s.cold
	s.mu.Unlock()
	if !enabled {
		return
	}

	s.resumeQueued(ctx)
	if s.feedCycle(ctx, cold) {
		s.mu.Lock()
		s.cold = false
		s.mu.Unlock()
	}
	s.verifyCycle(ctx)
	s.checkFuse(ctx)
	s.replyCycle(ctx)
}

// Enabled — включён ли амвон сейчас. Конфиг решает, есть ли служба вообще, а
// рантайм-тумблер (settings) — работает ли она; отсутствие флага значит
// «включён», потому что сама служба поднимается только при pulpit.enabled.
func (s *Service) Enabled(ctx context.Context) (bool, error) {
	v, found, err := s.st.Flag(ctx, store.FlagPulpitEnabled)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	return v != "0", nil
}

// SetPulpitEnabled переключает рантайм-тумблер (ручка /pulpit). Включение
// снимает причину прошлого выключения: она относилась к прошлой жизни.
func (s *Service) SetPulpitEnabled(ctx context.Context, on bool, by string) error {
	value := "0"
	if on {
		value = "1"
	}
	if err := s.st.SetFlag(ctx, store.FlagPulpitEnabled, value, by, time.Now()); err != nil {
		return err
	}
	if on {
		if err := s.st.SetFlag(ctx, store.FlagPulpitOffReason, "", by, time.Now()); err != nil {
			return err
		}
		// Взводим алерт заново: троттлер молчит, пока горит, — без сброса о
		// втором срабатывании предохранителя админ не узнал бы вовсе.
		s.alert.OK(ctx, alertKey)
	}
	s.log.Info("амвон: тумблер переключён", "включён", on, "кем", by)
	return nil
}

// disable гасит фичу и зовёт админа. Автовосстановления нет намеренно: срок
// запрета неизвестен, а самовольное возвращение — прямая дорога ко второму бану.
func (s *Service) disable(ctx context.Context, reason, detail string) {
	now := time.Now()
	if err := s.st.SetFlag(ctx, store.FlagPulpitEnabled, "0", "fuse", now); err != nil {
		s.log.Error("амвон: не удалось выключить тумблер", "err", err)
	}
	_ = s.st.SetFlag(ctx, store.FlagPulpitOffReason, reason, "fuse", now)
	_ = s.st.SetFlag(ctx, store.FlagPulpitOffAt, now.UTC().Format(time.RFC3339), "fuse", now)
	_ = s.st.SetFlag(ctx, store.FlagPulpitOffBy, "fuse", "fuse", now)
	s.mu.Lock()
	s.wasEnabled = false
	s.mu.Unlock()
	s.log.Error("амвон выключен предохранителем", "причина", reason, "подробности", detail)
	s.alert.Fail(ctx, alertKey, "выключен сам — "+reason+". "+detail+
		"\nВключить обратно только руками: /pulpit")
}

// PulpitStatus — отчёт для ручки /pulpit. Текст собирается здесь, а не в
// dmbot: так диалоговому ядру не нужно знать ни типов амвона, ни его состояний
// (пакет pulpit оттуда не импортируется).
func (s *Service) PulpitStatus(ctx context.Context) (report string, enabled bool, offReason string) {
	enabled, err := s.Enabled(ctx)
	if err != nil {
		return "Амвон: не удалось прочитать состояние (" + err.Error() + ")", false, ""
	}
	offReason, _, _ = s.st.Flag(ctx, store.FlagPulpitOffReason)

	var b strings.Builder
	if enabled {
		b.WriteString("🕯 Амвон включён.")
	} else {
		b.WriteString("⛔ Амвон выключен." + s.offText(ctx, offReason))
	}
	total, day, last, err := s.st.PulpitStats(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		s.log.Error("амвон: статистика", "err", err)
	}
	fmt.Fprintf(&b, "\nРеплик: %d за сутки, %d всего. Модель: %s.", day, total, s.cfg.Model)
	if last.NoteID != "" {
		fmt.Fprintf(&b, "\n\nПоследняя (%s, %s):\n%s",
			last.PostedAt.Local().Format("02.01 15:04"), lastStateText(last), last.Text)
		if link := s.commentLink(last); link != "" {
			b.WriteString("\n" + link)
		}
	}
	if streak, deleted, err := s.fuseStreak(ctx); err == nil && (streak > 0 || deleted > 0) {
		fmt.Fprintf(&b, "\n\nПредохранитель: промахов подряд %d из %d, пропавших реплик %d.",
			streak, s.cfg.FuseMisses, deleted)
	}
	return b.String(), enabled, offReason
}

// offText — почему и когда амвон выключили. Пустая причина бывает у ручного
// выключения: там объяснять нечего.
func (s *Service) offText(ctx context.Context, reason string) string {
	if reason == "" {
		return ""
	}
	out := "\nПричина: " + reason
	offAt, _, _ := s.st.Flag(ctx, store.FlagPulpitOffAt)
	if offAt == "" {
		return out
	}
	if by, _, _ := s.st.Flag(ctx, store.FlagPulpitOffBy); by != "" {
		return out + " (" + offAt + ", " + by + ")"
	}
	return out + " (" + offAt + ")"
}

// commentLink — якорь своей реплики на сайте (по нему её видно глазами).
func (s *Service) commentLink(row store.PulpitComment) string {
	if row.CommentID == 0 || s.cfg.BaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/notes/comments/%s/#anchor-%d",
		strings.TrimSuffix(s.cfg.BaseURL, "/"), row.NoteID, row.CommentID)
}

func lastStateText(row store.PulpitComment) string {
	switch row.State {
	case store.PulpitConfirmed:
		return "на месте"
	case store.PulpitMissing:
		return "в треде не найдена"
	case store.PulpitVanished:
		return "заметку снесли"
	case store.PulpitPosting:
		return "отправка не подтверждена"
	default:
		return row.State
	}
}

// cookies — сессия владельца анкеты. Куки живут только здесь и в store: ни в
// логи, ни в алерты они не попадают никогда.
func (s *Service) cookies(ctx context.Context) ([]*http.Cookie, error) {
	messenger, userID, err := s.st.SessionForProfile(ctx, s.cfg.OwnerProfileID)
	if err != nil {
		return nil, err
	}
	cookiesJSON, valid, err := s.st.SessionCookies(ctx, messenger, userID)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, fmt.Errorf("сессия анкеты %s недействительна", s.cfg.OwnerProfileID)
	}
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), time.Now())
	if err != nil {
		return nil, err
	}
	if len(cookies) == 0 {
		return nil, errors.New("в сессии нет живых кук")
	}
	return cookies, nil
}

// ownNick — свой ник на сайте: им опознаются обращения «Ник, …» к нам. Ник
// протухает (его меняют), поэтому раз в сутки снимаем заново; бесплатный
// источник — имя автора своей же подтверждённой реплики (setNick).
func (s *Service) ownNick(ctx context.Context) string {
	s.mu.Lock()
	nick, at := s.nick, s.nickAt
	s.mu.Unlock()
	if nick != "" && time.Since(at) < identityTTL {
		return nick
	}
	cookies, err := s.cookies(ctx)
	if err != nil {
		s.log.Warn("амвон: сессия для снятия ника недоступна", "err", err)
		return nick
	}
	profileID, passportID, fresh, err := s.site.SiteIdentity(ctx, cookies)
	if err != nil || fresh == "" {
		s.log.Warn("амвон: ник не снят", "err", err)
		s.mu.Lock()
		s.nickAt = time.Now() // не долбим сайт каждый такт
		s.mu.Unlock()
		return nick
	}
	if messenger, userID, err := s.st.SessionForProfile(ctx, s.cfg.OwnerProfileID); err == nil {
		if err := s.st.SetSessionIdentity(ctx, messenger, userID, profileID, passportID, fresh); err != nil {
			s.log.Error("амвон: сохранение идентичности", "err", err)
		}
	}
	s.setNick(fresh)
	return fresh
}

// currentNick — известный ник без похода на сайт ("" — ещё не снимали).
func (s *Service) currentNick() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nick
}

// setNick запоминает свежий ник и логирует смену: молчаливая протухшая подпись
// ломает детект ответов и выглядит как «люди перестали отвечать».
func (s *Service) setNick(nick string) {
	if nick == "" {
		return
	}
	s.mu.Lock()
	old := s.nick
	s.nick, s.nickAt = nick, time.Now()
	s.mu.Unlock()
	if old != "" && old != nick {
		s.log.Info("амвон: свой ник на сайте сменился", "было", old, "стало", nick)
	}
}

// jitter размывает такт на ±20 %: ровный интервал запросов к сайту — примета
// бота, а DDoS-Guard там не декоративный.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Second
	}
	delta := float64(d) * 0.2
	return time.Duration(float64(d) - delta + 2*delta*rand.Float64())
}

// dayStart — начало суточного окна счётчиков (скользящие сутки, а не календарные:
// нам важен темп, а не отчётный день).
func dayStart(now time.Time) time.Time { return now.Add(-24 * time.Hour) }
