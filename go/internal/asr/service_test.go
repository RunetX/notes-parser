package asr

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lovegw/internal/store"
)

// fakeTranscriber считает вызовы: главное в тестах — сколько раз платим провайдеру.
type fakeTranscriber struct {
	text  string
	err   error
	calls atomic.Int32
}

func (f *fakeTranscriber) Transcribe(_ context.Context, wav io.Reader) (string, error) {
	f.calls.Add(1)
	_, _ = io.ReadAll(wav)
	return f.text, f.err
}

type fakeConverter struct {
	err   error
	calls atomic.Int32
}

func (f *fakeConverter) ToWAV(_ context.Context, in []byte) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte("WAV:"), in...), nil
}

// recorder — счётчики транспорта: скачивания и реплаи в тред.
type recorder struct {
	fetched atomic.Int32
	replies []string
	err     error
}

func (r *recorder) job(fileKey string, duration int, userID int64) Job {
	return Job{
		Messenger: store.MessengerTelegram,
		FileKey:   fileKey,
		Duration:  duration,
		UserID:    userID,
		Fetch: func(context.Context) ([]byte, error) {
			r.fetched.Add(1)
			if r.err != nil {
				return nil, r.err
			}
			return []byte("OGG"), nil
		},
		Reply: func(_ context.Context, text string) error {
			r.replies = append(r.replies, text)
			return nil
		},
	}
}

func newTestService(t *testing.T, tr Transcriber, conv Converter, cfg Config) (*Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(tr, conv, st, cfg, testLogger()), st
}

func TestProcessHappyPath(t *testing.T) {
	tr := &fakeTranscriber{text: "текст расшифровки"}
	conv := &fakeConverter{}
	s, st := newTestService(t, tr, conv, Config{MaxDurationSec: 90, UserDailyLimitSec: 600})
	rec := &recorder{}

	s.process(context.Background(), rec.job("KEY1", 12, 42))

	if len(rec.replies) != 1 || rec.replies[0] != transcriptPrefix+"текст расшифровки" {
		t.Fatalf("реплай: %#v", rec.replies)
	}
	if rec.fetched.Load() != 1 || conv.calls.Load() != 1 || tr.calls.Load() != 1 {
		t.Errorf("шаги: fetch=%d convert=%d transcribe=%d",
			rec.fetched.Load(), conv.calls.Load(), tr.calls.Load())
	}
	// Расшифровка закэширована, секунды списаны.
	if text, ok, _ := st.Transcript(context.Background(), store.MessengerTelegram, "KEY1"); !ok || text != "текст расшифровки" {
		t.Errorf("кэш: %q ok=%v", text, ok)
	}
	day := time.Now().UTC().Format(time.DateOnly)
	if used, _ := st.ASRUsage(context.Background(), store.MessengerTelegram, 42, day); used != 12 {
		t.Errorf("списано секунд: %d, ожидалось 12", used)
	}
}

func TestProcessTooLong(t *testing.T) {
	tr := &fakeTranscriber{text: "не должно распознаваться"}
	s, _ := newTestService(t, tr, &fakeConverter{}, Config{MaxDurationSec: 90})
	rec := &recorder{}

	s.process(context.Background(), rec.job("KEY1", 120, 42))

	// Единственный случай, когда бот пишет в тред при отказе.
	if len(rec.replies) != 1 || !strings.Contains(rec.replies[0], "90") {
		t.Fatalf("объяснение про лимит: %#v", rec.replies)
	}
	if rec.fetched.Load() != 0 || tr.calls.Load() != 0 {
		t.Errorf("длинное голосовое не должно ни качаться, ни распознаваться: fetch=%d transcribe=%d",
			rec.fetched.Load(), tr.calls.Load())
	}
}

func TestProcessCacheHitSkipsProvider(t *testing.T) {
	tr := &fakeTranscriber{text: "свежая"}
	s, st := newTestService(t, tr, &fakeConverter{}, Config{MaxDurationSec: 90, UserDailyLimitSec: 600})
	ctx := context.Background()
	if err := st.SaveTranscript(ctx, store.MessengerTelegram, "KEY1", "из кэша", 10); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}

	s.process(ctx, rec.job("KEY1", 10, 42))

	if len(rec.replies) != 1 || rec.replies[0] != transcriptPrefix+"из кэша" {
		t.Fatalf("реплай: %#v", rec.replies)
	}
	if tr.calls.Load() != 0 || rec.fetched.Load() != 0 {
		t.Errorf("пересланное голосовое не должно стоить запроса: transcribe=%d fetch=%d",
			tr.calls.Load(), rec.fetched.Load())
	}
	day := time.Now().UTC().Format(time.DateOnly)
	if used, _ := st.ASRUsage(ctx, store.MessengerTelegram, 42, day); used != 0 {
		t.Errorf("кэш-хит не должен тратить квоту, списано %d", used)
	}
}

func TestProcessQuotaExhaustedIsSilent(t *testing.T) {
	tr := &fakeTranscriber{text: "текст"}
	s, st := newTestService(t, tr, &fakeConverter{}, Config{MaxDurationSec: 90, UserDailyLimitSec: 60})
	ctx := context.Background()
	day := time.Now().UTC().Format(time.DateOnly)
	if ok, err := st.TryReserveASR(ctx, store.MessengerTelegram, 42, day, 60, 60); err != nil || !ok {
		t.Fatalf("подготовка квоты: ok=%v err=%v", ok, err)
	}
	rec := &recorder{}

	s.process(ctx, rec.job("KEY1", 30, 42))

	if len(rec.replies) != 0 {
		t.Errorf("исчерпанная квота не должна писать в тред: %#v", rec.replies)
	}
	if tr.calls.Load() != 0 {
		t.Errorf("запросов к провайдеру: %d", tr.calls.Load())
	}
	// Соседний пользователь работает как ни в чём не бывало.
	other := &recorder{}
	s.process(ctx, other.job("KEY2", 30, 43))
	if len(other.replies) != 1 {
		t.Errorf("чужая квота не должна мешать: %#v", other.replies)
	}
}

func TestProcessFailuresAreSilent(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		tr   *fakeTranscriber
		conv *fakeConverter
		rec  *recorder
	}{
		{"провайдер недоступен", &fakeTranscriber{err: errors.New("сеть")}, &fakeConverter{}, &recorder{}},
		{"конвертация упала", &fakeTranscriber{text: "т"}, &fakeConverter{err: errors.New("ffmpeg")}, &recorder{}},
		{"файл не скачался", &fakeTranscriber{text: "т"}, &fakeConverter{}, &recorder{err: errors.New("403")}},
		{"пустая расшифровка", &fakeTranscriber{text: "   "}, &fakeConverter{}, &recorder{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, st := newTestService(t, c.tr, c.conv, Config{MaxDurationSec: 90})
			s.process(ctx, c.rec.job("KEY1", 10, 42))
			if len(c.rec.replies) != 0 {
				t.Errorf("тред должен молчать: %#v", c.rec.replies)
			}
			if _, ok, _ := st.Transcript(ctx, store.MessengerTelegram, "KEY1"); ok {
				t.Error("неудачная попытка не должна попадать в кэш")
			}
		})
	}
}

func TestProcessAlertsAdminOnAuthError(t *testing.T) {
	tr := &fakeTranscriber{err: ErrAuth}
	s, _ := newTestService(t, tr, &fakeConverter{}, Config{MaxDurationSec: 90})
	var alerts []string
	s.SetAlert(func(_ context.Context, text string) { alerts = append(alerts, text) })

	ctx := context.Background()
	s.process(ctx, (&recorder{}).job("KEY1", 10, 42))
	s.process(ctx, (&recorder{}).job("KEY2", 10, 42))

	if len(alerts) != 1 {
		t.Fatalf("о сбое ключа сообщаем один раз: %#v", alerts)
	}
	if !strings.Contains(alerts[0], alertKey) {
		t.Errorf("алерт без ключа: %q", alerts[0])
	}
}

func TestEnqueueDropsWhenFull(t *testing.T) {
	s, _ := newTestService(t, &fakeTranscriber{}, &fakeConverter{}, Config{Concurrency: 1})
	rec := &recorder{}
	// Воркеры не запущены: очередь (Concurrency*4) заполняется до отказа.
	for i := 0; i < cap(s.jobs); i++ {
		if !s.Enqueue(rec.job("KEY"+string(rune('A'+i)), 10, 42)) {
			t.Fatalf("очередь отказала на %d-й задаче из %d", i, cap(s.jobs))
		}
	}
	if s.Enqueue(rec.job("OVERFLOW", 10, 42)) {
		t.Error("переполненная очередь должна отказывать, а не блокировать поллинг")
	}
}

func TestEnqueueDedupsInflight(t *testing.T) {
	s, _ := newTestService(t, &fakeTranscriber{}, &fakeConverter{}, Config{Concurrency: 1})
	rec := &recorder{}
	if !s.Enqueue(rec.job("KEY1", 10, 42)) {
		t.Fatal("первая задача должна встать в очередь")
	}
	if !s.Enqueue(rec.job("KEY1", 10, 42)) {
		t.Error("дубль должен схлопываться, а не считаться отказом")
	}
	if len(s.jobs) != 1 {
		t.Errorf("в очереди %d задач, ожидалась одна", len(s.jobs))
	}
}

func TestRunProcessesQueue(t *testing.T) {
	tr := &fakeTranscriber{text: "из воркера"}
	s, _ := newTestService(t, tr, &fakeConverter{}, Config{MaxDurationSec: 90, Concurrency: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	replied := make(chan string, 1)
	job := Job{
		Messenger: store.MessengerTelegram,
		FileKey:   "KEY1",
		Duration:  10,
		UserID:    42,
		Fetch:     func(context.Context) ([]byte, error) { return []byte("OGG"), nil },
		Reply: func(_ context.Context, text string) error {
			replied <- text
			return nil
		},
	}
	if !s.Enqueue(job) {
		t.Fatal("задача не встала в очередь")
	}
	select {
	case text := <-replied:
		if text != transcriptPrefix+"из воркера" {
			t.Errorf("реплай: %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("воркер не обработал задачу")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился по отмене контекста")
	}
}
