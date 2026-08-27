// Package morning — утренняя заметка сообщества: одна заметка «доброе утро» в
// сутки, публикуется НА love.ngs.ru от анкеты владельца.
//
// Почему на НГС, а не на своей площадке (решение владельца 24.08.2026): оттуда
// заметку принесёт зеркало — и на `t3h.ru`, и в оба канала мессенджеров, — то
// есть текст существует один раз, у него один адрес и один тред. Публикуй мы на
// площадке, в ленте НГС утра бы не было вовсе, а там ещё живут люди.
//
// Что делает служба:
//  1. в слот (05:00 Нск) смотрит, не написал ли «доброе утро» кто-то другой —
//     если написал, молчит и говорит владельцу в ЛС: хозяин утра человек;
//  2. собирает поводы дня из интернет-календарей (`internal/holidays`);
//  3. просит модель написать текст ВОКРУГ этих поводов и проверяет ответ;
//  4. публикует и потом убеждается, что заметка появилась в ленте.
//
// Однократность держит первичный ключ по дню в `morning_notes`: ни второй
// прогон, ни рестарт, ни ручной догон второй заметки не дадут.
package morning

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"lovegw/internal/alerts"
	"lovegw/internal/holidays"
	"lovegw/internal/love"
	"lovegw/internal/store"
)

// JSONGenerator — онлайн-LLM, отвечающий строго по схеме. Тот же контракт, что
// у амвона и дайджеста; реализация — llm.Client.
type JSONGenerator interface {
	GenerateJSON(ctx context.Context, system, prompt string, schema map[string]any) ([]byte, error)
}

// Site — что утренняя заметка делает с сайтом. Список узкий и объявлен здесь,
// у потребителя: он же исчерпывающий ответ на вопрос «что служба вправе
// сделать с НГС». Прочитать ленту, опубликовать заметку и, если запахло
// баном, спросить у сайта состояние анкеты.
type Site interface {
	FetchNotes(ctx context.Context) ([]love.Note, error)
	PostNote(ctx context.Context, cookies []*http.Cookie, text string, anonymous bool) error
	ProfileControl(ctx context.Context, cookies []*http.Cookie) (love.ProfileControl, error)
}

// Config — параметры службы. Нулевые значения дополняются дефолтами: служба
// должна подниматься и с полупустой секцией конфига.
type Config struct {
	OwnerProfileID string // анкета владельца: сессия и опознание своей заметки
	BaseURL        string // адрес сайта — для ссылок в отчёте
	Model          string // имя модели, только для отчёта в /morning

	Loc   *time.Location
	Hour  int
	Grace time.Duration

	GenerateTimeout time.Duration
	MinRunes        int
	MaxRunes        int
	MaxLines        int
	HistorySize     int // сколько своих прошлых заметок показываем модели
	MaxFacts        int // сколько поводов подаём модели

	Sources    []holidays.Source
	FuseMisses int

	// AlertSend — ЛС владельцу (может быть nil).
	AlertSend func(ctx context.Context, text string)
}

// Дефолты. Длина снята не с потолка: заметка должна читаться с телефона за
// полминуты, а не быть простынёй — в ленте площадки длинный текст с 23.08.2026
// вообще сворачивается показом (`web.longBodyRunes` = 1500 знаков), и утро,
// упирающееся в кнопку «показать целиком», перестаёт быть утром.
const (
	defaultHour            = 7
	defaultGrace           = 3 * time.Hour
	defaultGenerateTimeout = 90 * time.Second
	defaultMinRunes        = 200
	defaultMaxRunes        = 1200
	defaultMaxLines        = 14
	defaultHistorySize     = 7
	defaultMaxFacts        = 12
	defaultFuseMisses      = 2
)

// tick — как часто просыпается служба. Минута, а не таймер до слота: тот же
// цикл должен ещё и убеждаться, что опубликованная заметка появилась в ленте, а
// два таймера на одну службу читаются хуже, чем один дешёвый такт. Такт без
// работы стоит одного чтения SQLite и в сеть не ходит.
const tick = time.Minute

// verifyDelay / maxChecks — через сколько и сколько раз ищем свою заметку в
// ленте. Сайт показывает её сразу, но лента у нас читается своим клиентом с
// лимитером, и минута форы избавляет от ложного «не появилась».
const (
	verifyDelay = 2 * time.Minute
	verifyEvery = 5 * time.Minute
	maxChecks   = 3
)

// alertKey — ключ троттлера алертов. Он один: у службы ровно одна новость,
// стоящая ЛС, — «выключилась сама».
const alertKey = "утро"

func (c Config) withDefaults() Config {
	if c.Loc == nil {
		c.Loc = time.UTC
	}
	if c.Hour <= 0 {
		c.Hour = defaultHour
	}
	if c.Grace <= 0 {
		c.Grace = defaultGrace
	}
	if c.GenerateTimeout <= 0 {
		c.GenerateTimeout = defaultGenerateTimeout
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
	if c.MaxFacts <= 0 {
		c.MaxFacts = defaultMaxFacts
	}
	if c.FuseMisses <= 0 {
		c.FuseMisses = defaultFuseMisses
	}
	return c
}

// Service — служба под общим errgroup демона.
type Service struct {
	st   *store.Store
	site Site
	gen  JSONGenerator
	cfg  Config
	log  *slog.Logger
	// alert — порог 1: о срабатывании предохранителя владелец узнаёт сразу, но
	// один раз (дальше фича выключена и сообщать не о чем).
	alert *alerts.Alerter
	// now — источник времени; поле, чтобы тест был детерминированным.
	now func() time.Time
}

// New собирает службу. gen == nil — генерировать нечем: служба поднимется, но
// писать не будет (об этом скажет лог на старте).
func New(st *store.Store, site Site, gen JSONGenerator, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		st:    st,
		site:  site,
		gen:   gen,
		cfg:   cfg.withDefaults(),
		log:   log,
		alert: alerts.New(cfg.AlertSend, 1),
		now:   time.Now,
	}
}

// Run крутит цикл до отмены контекста. Ошибки наружу не выпускаются: утренняя
// заметка не критична для зеркала, и её сбой не должен ронять демон.
func (s *Service) Run(ctx context.Context) error {
	s.log.Info("утренняя заметка запущена",
		"анкета", s.cfg.OwnerProfileID, "слот", fmt.Sprintf("%02d:00 %s", s.cfg.Hour, s.cfg.Loc),
		"модель", s.cfg.Model, "календари", len(s.cfg.Sources))
	if s.gen == nil {
		s.log.Warn("утренняя заметка: LLM не настроен — заметок не будет (нужен llm.api_key)")
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	s.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.cycle(ctx)
		}
	}
}

// cycle — один такт: сперва довести вчерашнее и сегодняшнее до ясности
// (верификация), потом решить про слот.
func (s *Service) cycle(ctx context.Context) {
	s.verifyPending(ctx)

	enabled, err := s.Enabled(ctx)
	if err != nil {
		s.log.Error("утренняя заметка: чтение тумблера", "err", err)
		return
	}
	day, slot := SlotFor(s.now(), s.cfg.Loc, s.cfg.Hour)
	done, err := s.dayDone(ctx, day)
	if err != nil {
		s.log.Error("утренняя заметка: чтение дня", "день", day, "err", err)
		return
	}
	// Дорогое спрашиваем только тогда, когда без него не обойтись: пока день
	// закрыт или тумблер выключен, в сеть не ходим вовсе.
	in := decideInput{Enabled: enabled, Now: s.now(), Slot: slot, Grace: s.cfg.Grace, Done: done}
	if v := decide(in); v.Action == actIdle {
		return
	}
	// Лента читается ОДИН раз за такт и отвечает сразу на два вопроса: не
	// сказано ли утро кем-то другим и жива ли вчерашняя наша заметка. Второй
	// вопрос не стоит ни одного лишнего запроса именно поэтому.
	feed, err := s.site.FetchNotes(ctx)
	if err != nil {
		s.log.Warn("утренняя заметка: лента не прочиталась", "err", err)
		return // не зная ленты, публиковать нельзя: вдруг утро уже сказано
	}
	s.checkOwnPresence(ctx, feed, day)
	start, end := DayBounds(slot)
	in.Foreign = s.foreignGreeting(ctx, feed, start, end)
	in.HasSession = s.hasSession(ctx)

	switch v := decide(in); v.Action {
	case actIdle:
		return
	case actMark:
		s.mark(ctx, day, v)
	case actPost:
		s.publish(ctx, day, slot)
	}
}

// mark записывает день конечным состоянием и, где надо, зовёт владельца.
func (s *Service) mark(ctx context.Context, day string, v verdict) {
	first, err := s.st.MarkMorning(ctx, day, v.State, v.Reason, s.now())
	if err != nil {
		s.log.Error("утренняя заметка: отметка дня", "день", day, "err", err)
		return
	}
	if !first {
		return // день уже записан кем-то раньше — второй раз не сообщаем
	}
	s.log.Info("утренняя заметка: пропуск", "день", day, "причина", v.Reason)
	switch v.Reason {
	case reasonSomeone:
		s.notify(ctx, fmt.Sprintf("🌅 Сегодня доброе утро уже написал кто-то другой (%s) — я промолчал.",
			s.noteLink(v.Detail)))
	case reasonNoSession:
		s.notify(ctx, "🌅 Утренняя заметка не вышла: нет живой сессии анкеты "+
			s.cfg.OwnerProfileID+". Нужен /login.")
	}
}

func (s *Service) notify(ctx context.Context, text string) {
	if s.cfg.AlertSend == nil {
		return
	}
	s.cfg.AlertSend(ctx, text)
}

func (s *Service) noteLink(noteID string) string {
	if noteID == "" {
		return "заметка"
	}
	if s.cfg.BaseURL == "" {
		return "заметка " + noteID
	}
	return strings.TrimSuffix(s.cfg.BaseURL, "/") + "/notes/" + noteID + "/"
}

// dayDone — есть ли уже строка этого дня (в любом состоянии).
func (s *Service) dayDone(ctx context.Context, day string) (bool, error) {
	_, err := s.st.MorningByDay(ctx, day)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// hasSession — есть ли живая сессия владельца. Куки читаются полностью: сессия
// бывает записана, но недействительна, и об этом надо знать ДО генерации.
func (s *Service) hasSession(ctx context.Context) bool {
	_, err := s.cookies(ctx)
	return err == nil
}

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
	cookies, err := love.CookiesFromJSON([]byte(cookiesJSON), s.now())
	if err != nil {
		return nil, err
	}
	if len(cookies) == 0 {
		return nil, errors.New("в сессии нет живых кук")
	}
	return cookies, nil
}

// Enabled — работает ли служба сейчас. Флага нет — считаем включённой: тумблер
// заводится в момент первого выключения, и отсутствие записи не должно значить
// «выключено» (иначе выкатка молча отменяла бы фичу).
func (s *Service) Enabled(ctx context.Context) (bool, error) {
	v, found, err := s.st.Flag(ctx, store.FlagMorningEnabled)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	return v == "1", nil
}

// SetEnabled переключает тумблер. by — кто переключил: «admin:<id>» или «fuse».
func (s *Service) SetEnabled(ctx context.Context, on bool, by string) error {
	v := "0"
	if on {
		v = "1"
	}
	if err := s.st.SetFlag(ctx, store.FlagMorningEnabled, v, by, s.now()); err != nil {
		return err
	}
	if on {
		// Причину выключения снимаем: она относилась к прошлому разу, и
		// оставленная, она соврёт в первом же отчёте.
		if err := s.st.SetFlag(ctx, store.FlagMorningOffReason, "", by, s.now()); err != nil {
			return err
		}
	}
	s.log.Info("утренняя заметка: тумблер", "включено", on, "кто", by)
	return nil
}
