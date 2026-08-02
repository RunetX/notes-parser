package asr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetries ужимает backoff, чтобы тесты ретраев не спали секундами.
func fastRetries(t *testing.T) {
	t.Helper()
	prev := retryBase
	retryBase = time.Millisecond
	t.Cleanup(func() { retryBase = prev })
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTranscribeRequestShape(t *testing.T) {
	type captured struct {
		auth, model, language, format, filename string
		audio                                   string
	}
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.auth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("разбор multipart: %v", err)
		}
		got.model = r.FormValue("model")
		got.language = r.FormValue("language")
		got.format = r.FormValue("response_format")
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("поле file: %v", err)
		} else {
			defer file.Close()
			got.filename = header.Filename
			b, _ := io.ReadAll(file)
			got.audio = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"  привет из треда  "}`)
	}))
	defer srv.Close()

	n := NewNexara(NexaraConfig{APIKey: "nx-test"}, srv.URL, testLogger())
	text, err := n.Transcribe(context.Background(), strings.NewReader("WAVDATA"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "привет из треда" {
		t.Errorf("текст не обрезан по краям: %q", text)
	}
	if got.auth != "Bearer nx-test" {
		t.Errorf("авторизация: %q", got.auth)
	}
	if got.model != defaultModel || got.language != language || got.format != "json" {
		t.Errorf("поля запроса: model=%q language=%q format=%q", got.model, got.language, got.format)
	}
	if got.audio != "WAVDATA" || got.filename == "" {
		t.Errorf("файл: %q (%q)", got.audio, got.filename)
	}
}

func TestTranscribeRetriesOn500(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"text":"со второй попытки"}`)
	}))
	defer srv.Close()

	n := NewNexara(NexaraConfig{APIKey: "nx-test"}, srv.URL, testLogger())
	text, err := n.Transcribe(context.Background(), strings.NewReader("WAV"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "со второй попытки" {
		t.Errorf("текст: %q", text)
	}
	if calls.Load() != 2 {
		t.Errorf("запросов: %d, ожидалось 2", calls.Load())
	}
}

func TestTranscribeRetriesExhausted(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	n := NewNexara(NexaraConfig{APIKey: "nx-test"}, srv.URL, testLogger())
	if _, err := n.Transcribe(context.Background(), strings.NewReader("WAV")); err == nil {
		t.Fatal("исчерпанные попытки должны быть ошибкой")
	}
	if calls.Load() != transcribeRetries {
		t.Errorf("запросов: %d, ожидалось %d", calls.Load(), transcribeRetries)
	}
}

func TestTranscribeNoRetryOnClientError(t *testing.T) {
	fastRetries(t)
	cases := []struct {
		name   string
		status int
		auth   bool
	}{
		{"кривой запрос", http.StatusBadRequest, false},
		{"плохой ключ", http.StatusUnauthorized, true},
		{"пустой баланс", http.StatusPaymentRequired, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			n := NewNexara(NexaraConfig{APIKey: "nx-test"}, srv.URL, testLogger())
			_, err := n.Transcribe(context.Background(), strings.NewReader("WAV"))
			if err == nil {
				t.Fatalf("статус %d должен быть ошибкой", c.status)
			}
			if calls.Load() != 1 {
				t.Errorf("запросов: %d, ретраить 4xx нельзя", calls.Load())
			}
			if errors.Is(err, ErrAuth) != c.auth {
				t.Errorf("ErrAuth=%v при статусе %d: %v", !c.auth, c.status, err)
			}
		})
	}
}

func TestTranscribeTimeout(t *testing.T) {
	fastRetries(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	n := NewNexara(NexaraConfig{APIKey: "nx-test", Timeout: 50 * time.Millisecond}, srv.URL, testLogger())
	start := time.Now()
	if _, err := n.Transcribe(context.Background(), strings.NewReader("WAV")); err == nil {
		t.Fatal("таймаут должен быть ошибкой")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("таймаут не сработал: ждали %s", elapsed)
	}
}
