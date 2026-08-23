package web

// Проверки мгновенности живого канала.
//
// Их три, и каждая закрывает свою половину размена «звонок вместо такта»:
// звонок должен поднимать хаб БЕЗ ожидания такта, отказ подписки должен молча
// возвращать канал к прежнему частому опросу, а возвращение подписки — снова
// разрежать такт. Средняя важнее двух других: сломанная деградация выглядит как
// работающий канал ровно до того дня, когда Postgres перезапустят.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"lovegw/internal/platform"
)

// watchedLive — шина, умеющая звонить. Подписка здесь управляемая: тест решает,
// когда она встанет и когда оборвётся, — иначе проверять деградацию пришлось бы
// настоящим Postgres, которого у `go test ./...` нет.
type watchedLive struct {
	fakeLive

	mu     sync.Mutex
	ring   func()
	drop   chan error // положить сюда ошибку — подписка кончилась
	tries  int
	listen chan struct{} // сигнал тесту: подписка встала
	moved  map[int64]time.Time
	asked  []int64
}

func newWatched() *watchedLive {
	return &watchedLive{drop: make(chan error, 1), listen: make(chan struct{}, 8)}
}

func (w *watchedLive) ListenLive(ctx context.Context, ready, ring func()) error {
	w.mu.Lock()
	w.tries++
	w.ring = ring
	w.mu.Unlock()
	ready()
	select {
	case w.listen <- struct{}{}:
	default:
	}
	select {
	case err := <-w.drop:
		return err
	case <-ctx.Done():
		return nil
	}
}

// MovedNotesSince — переезды, о которых тест решает сам. Заодно ЗАПОМИНАЕТ, о
// чём спросили: половина смысла третьего насоса в том, что спрашивает он только
// про открытые треды.
func (w *watchedLive) MovedNotesSince(_ context.Context, notes []int64, after time.Time) (map[int64]time.Time, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, notes...)
	out := map[int64]time.Time{}
	for _, id := range notes {
		if at, ok := w.moved[id]; ok && at.After(after) {
			out[id] = at
		}
	}
	return out, nil
}

func (w *watchedLive) move(note int64, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.moved == nil {
		w.moved = map[int64]time.Time{}
	}
	w.moved[note] = at
}

func (w *watchedLive) askedAbout() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int64(nil), w.asked...)
}

// call — звонок «снаружи», как его сделал бы Postgres.
func (w *watchedLive) call() {
	w.mu.Lock()
	ring := w.ring
	w.mu.Unlock()
	if ring != nil {
		ring()
	}
}

// Главное обещание брифа: событие доходит до вкладки по ЗВОНКУ, а не по такту.
// Тест ждёт заведомо меньше такта-страховки — если сигнал приедет, то приехал
// он звонком.
func TestHubRingDeliversWithoutTick(t *testing.T) {
	src := newWatched()
	src.events = []platform.LiveEvent{
		{ID: 7, Kind: platform.EventComment, NoteID: 312811, At: time.Now()},
	}
	h := newHub(src, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	l := &listener{ch: make(chan liveMsg, listenerBuffer), topics: []string{noteTopic(312811)}}
	if !h.subscribe(l, maxLiveConns, maxLivePerUser) {
		t.Fatal("хаб не принял слушателя")
	}
	defer h.unsubscribe(l)

	waitFor(t, func() bool { return h.Mode() == modeNotify })
	src.call()

	select {
	case m := <-l.ch:
		if m.Note != 312811 || m.Kind != "comment" {
			t.Fatalf("пришёл не тот сигнал: %+v", m)
		}
	case <-time.After(hubTickNotify / 4):
		t.Fatal("звонок не поднял хаб: сигнал не пришёл раньше такта")
	}
}

// Деградация. Подписка обрывается — канал обязан вернуться к прежнему частому
// опросу САМ, сказать об этом ровно одной строкой и продолжить носить сигналы.
func TestHubFallsBackToPollingWhenListenDies(t *testing.T) {
	src := newWatched()
	var buf syncBuffer
	h := newHub(src, slog.New(slog.NewTextHandler(&buf, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	waitFor(t, func() bool { return h.Mode() == modeNotify })
	if h.every() != hubTickNotify {
		t.Fatalf("при живой подписке такт %s, ожидался %s", h.every(), hubTickNotify)
	}

	l := &listener{ch: make(chan liveMsg, listenerBuffer), topics: []string{noteTopic(312811)}}
	if !h.subscribe(l, maxLiveConns, maxLivePerUser) {
		t.Fatal("хаб не принял слушателя")
	}
	defer h.unsubscribe(l)

	src.drop <- errors.New("соединение разорвано")
	waitFor(t, func() bool { return h.Mode() == modePoll })
	if h.every() != hubTickPoll {
		t.Fatalf("без подписки такт %s, ожидался %s", h.every(), hubTickPoll)
	}

	// Сигналы продолжают идти — теперь тактом. Событие заводим ПОСЛЕ падения,
	// чтобы принести его мог только опрос.
	src.mu.Lock()
	src.events = []platform.LiveEvent{
		{ID: 9, Kind: platform.EventComment, NoteID: 312811, At: time.Now()},
	}
	src.mu.Unlock()
	select {
	case <-l.ch:
	case <-time.After(3 * hubTickPoll):
		t.Fatal("после падения подписки сигналы не идут вовсе")
	}

	// Одна строка предупреждения на падение, а не строка на попытку: бэкофф
	// переподписки крутится всё это время, и шумящий лог топит в себе то самое,
	// ради чего его читают.
	if n := strings.Count(buf.String(), "LISTEN отвалился"); n != 1 {
		t.Fatalf("предупреждений о падении подписки %d, ожидалось ровно 1:\n%s", n, buf.String())
	}
}

// Возвращение. Подписка встала снова — такт обязан снова стать редким, иначе
// «страховка» навсегда осталась бы опросом.
func TestHubReturnsToNotifyAfterRecovery(t *testing.T) {
	src := newWatched()
	h := newHub(src, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	waitFor(t, func() bool { return h.Mode() == modeNotify })
	<-src.listen
	src.drop <- errors.New("соединение разорвано")
	waitFor(t, func() bool { return h.Mode() == modePoll })

	// Переподписка идёт с бэкоффа в секунду — ждём её саму, а не гадаем.
	waitForLong(t, func() bool { return h.Mode() == modeNotify }, 2*listenRetryMin+time.Second)
	if h.every() != hubTickNotify {
		t.Fatalf("после возвращения подписки такт %s, ожидался %s", h.every(), hubTickNotify)
	}
	src.mu.Lock()
	tries := src.tries
	src.mu.Unlock()
	if tries < 2 {
		t.Fatalf("переподписки не было: попыток %d", tries)
	}
}

// Без способности звонить канал работает как работал: не поломка, а прежний
// опрос. Проверяется тем же полем, которое показывает healthz.
func TestHubStaysPollingWithoutWatcher(t *testing.T) {
	h := newHub(&fakeLive{}, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)

	time.Sleep(20 * time.Millisecond)
	if h.Mode() != modePoll {
		t.Fatalf("без подписки режим %q, ожидался %q", h.Mode(), modePoll)
	}
	if h.every() != hubTickPoll {
		t.Fatalf("без подписки такт %s, ожидался %s", h.every(), hubTickPoll)
	}
}

// Процентиль считается ближайшим рангом — проверяем на числах, которые видно
// глазом: сводка задержки уходит владельцу, и ошибка в ней хуже её отсутствия.
func TestPercentileNearestRank(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	if got := pct(d, 50); got != 50*time.Millisecond {
		t.Errorf("медиана %s, ожидалось 50ms", got)
	}
	if got := pct(d, 95); got != 95*time.Millisecond {
		t.Errorf("p95 %s, ожидалось 95ms", got)
	}
	if got := pct(nil, 95); got != 0 {
		t.Errorf("p95 пустого среза %s, ожидался ноль", got)
	}
}

// Отрицательный замер не считается: часы морды и часы Postgres разные, и
// расхождение между ними — это не мгновенный ответ.
func TestObserveIgnoresNegative(t *testing.T) {
	h := newHub(&fakeLive{}, quietLog())
	h.observe(-time.Second)
	h.observe(10 * time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lat) != 1 || h.lat[0] != 10*time.Millisecond {
		t.Fatalf("в сводку попало %v", h.lat)
	}
}

// Переехавшая ветка доходит до открытой страницы СИГНАЛОМ, а не следующей
// репликой. Ради этого теста всё и затевалось: разговор в треде затихает сразу
// после ответа, новых фактов не будет, и без своего сигнала человек остаётся с
// веткой, выросшей по догадке зеркала, до перезагрузки.
func TestHubSignalsMovedBranch(t *testing.T) {
	src := newWatched()
	h := newHub(src, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)
	waitFor(t, func() bool { return h.Mode() == modeNotify })

	l := &listener{ch: make(chan liveMsg, listenerBuffer), topics: []string{noteTopic(312811)}}
	if !h.subscribe(l, maxLiveConns, maxLivePerUser) {
		t.Fatal("хаб не принял слушателя")
	}
	defer h.unsubscribe(l)

	src.move(312811, time.Now().Add(time.Minute))
	src.call() // звонок, какой делает ApplyReplyTree

	select {
	case m := <-l.ch:
		if m.Kind != "move" || m.Note != 312811 {
			t.Fatalf("пришёл не тот сигнал: %+v", m)
		}
	case <-time.After(hubTickNotify / 4):
		t.Fatal("о переезде не сказали: страница осталась бы с угаданной веткой")
	}
}

// Спрашиваем только про ОТКРЫТЫЕ треды. Не педантизм: индекс переездов ведёт с
// note_id, и вопрос «покажи все переезды» — это перебор таблицы на 10,7 млн
// строк на каждом проходе хаба.
func TestHubAsksOnlyAboutWatchedNotes(t *testing.T) {
	src := newWatched()
	h := newHub(src, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.run(ctx)
	waitFor(t, func() bool { return h.Mode() == modeNotify })

	// Без слушателей о переездах не спрашивают вовсе.
	src.call()
	waitFor(t, func() bool { return len(src.askedAbout()) == 0 })

	l := &listener{ch: make(chan liveMsg, listenerBuffer), topics: []string{noteTopic(312811)}}
	if !h.subscribe(l, maxLiveConns, maxLivePerUser) {
		t.Fatal("хаб не принял слушателя")
	}
	defer h.unsubscribe(l)
	src.call()

	waitFor(t, func() bool { return len(src.askedAbout()) > 0 })
	for _, id := range src.askedAbout() {
		if id != 312811 {
			t.Fatalf("спросили про заметку %d, которую никто не открывал", id)
		}
	}
}

// Ядро обязано подходить обоим интерфейсам живого канала. Проверка выглядит
// пустой, но закрывает самый тихий из отказов: способности подключаются
// type-assertion'ом, и разошедшаяся подпись не даёт ни ошибки сборки, ни
// падения — площадка просто НАВСЕГДА остаётся на такте, и выглядит это как
// работающий канал.
func TestPlatformSatisfiesLiveInterfaces(t *testing.T) {
	var p any = (*platform.Platform)(nil)
	if _, ok := p.(Live); !ok {
		t.Error("platform.Platform больше не Live — живого канала нет вовсе")
	}
	if _, ok := p.(LiveWatcher); !ok {
		t.Error("platform.Platform больше не LiveWatcher — канал молча уехал на такт")
	}
	if _, ok := p.(LiveMover); !ok {
		t.Error("platform.Platform больше не LiveMover — переезды ветки снова молчат")
	}
}

// ---------------------------------------------------------------- помощники

// syncBuffer — лог под мьютексом: пишет его горутина подписки, читает тест.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForLong(t *testing.T, ok func() bool, life time.Duration) {
	t.Helper()
	deadline := time.Now().Add(life)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("не дождались состояния")
}
