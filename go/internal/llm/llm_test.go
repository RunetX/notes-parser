package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serve(t *testing.T, stopReason, text string, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("разбор тела запроса: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			"content":[{"type":"text","text":%q}],
			"stop_reason":%q,"usage":{"input_tokens":10,"output_tokens":5}}`, text, stopReason)
	}))
}

func TestGenerateJSONRequestShape(t *testing.T) {
	var req map[string]any
	srv := serve(t, "end_turn", `{"ok":true}`, &req)
	defer srv.Close()

	c := New(Config{APIKey: "test-key"}, srv.URL)
	schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	raw, err := c.GenerateJSON(context.Background(), "система", "промпт", schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("ответ: %s", raw)
	}
	if req["model"] != DefaultModel {
		t.Errorf("модель: %v", req["model"])
	}
	out := req["output_config"].(map[string]any)
	format := out["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config.format: %v", format)
	}
	// Без Effort поле не должно появляться вовсе: дайджест и voice остаются
	// на прежнем поведении модели (adaptive).
	if _, ok := out["effort"]; ok {
		t.Errorf("output_config.effort не задавали, а он в запросе: %v", out["effort"])
	}
}

// TestGenerateJSONEffort — рычаг задержки: effort едет в output_config.
func TestGenerateJSONEffort(t *testing.T) {
	var req map[string]any
	srv := serve(t, "end_turn", `{"ok":true}`, &req)
	defer srv.Close()

	c := New(Config{APIKey: "test-key", Effort: "low", Timeout: time.Minute}, srv.URL)
	if _, err := c.GenerateJSON(context.Background(), "с", "п", map[string]any{"type": "object"}); err != nil {
		t.Fatal(err)
	}
	if got := req["output_config"].(map[string]any)["effort"]; got != "low" {
		t.Errorf("output_config.effort: %v", got)
	}
}

func TestGenerateJSONRefusal(t *testing.T) {
	srv := serve(t, "refusal", "", nil)
	defer srv.Close()

	c := New(Config{APIKey: "test-key"}, srv.URL)
	_, err := c.GenerateJSON(context.Background(), "с", "п", map[string]any{"type": "object"})
	if !errors.Is(err, ErrRefusal) {
		t.Fatalf("ожидался ErrRefusal, получено: %v", err)
	}
}

func TestGenerateJSONTruncated(t *testing.T) {
	srv := serve(t, "max_tokens", `{"частичный`, nil)
	defer srv.Close()

	c := New(Config{APIKey: "test-key"}, srv.URL)
	_, err := c.GenerateJSON(context.Background(), "с", "п", map[string]any{"type": "object"})
	if err == nil {
		t.Fatal("обрыв по max_tokens должен быть ошибкой")
	}
}

// serveUsage — сервер, отдающий заданный расход: у кэша расход и есть
// единственный признак, что он сработал.
func serveUsage(t *testing.T, in, cacheWrite, cacheRead, out int, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("разбор тела запроса: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			"content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,
			"cache_read_input_tokens":%d,"output_tokens":%d}}`, in, cacheWrite, cacheRead, out)
	}))
}

// Кэш-точка ставится только по просьбе. Проверяется на самом запросе, а не на
// поле структуры: цена ошибки — наценка 1,25 на каждый одиночный вызов
// дайджеста, и увидеть её можно было бы только по счёту в конце месяца.
func TestSystemCacheOnlyWhenAsked(t *testing.T) {
	cacheOf := func(t *testing.T, cfg Config, system string) any {
		t.Helper()
		var req map[string]any
		srv := serveUsage(t, 10, 0, 0, 5, &req)
		defer srv.Close()
		cfg.APIKey = "test-key"
		c := New(cfg, srv.URL)
		if _, err := c.GenerateJSON(context.Background(), system, "п", map[string]any{"type": "object"}); err != nil {
			t.Fatal(err)
		}
		return req["system"].([]any)[0].(map[string]any)["cache_control"]
	}

	if cc := cacheOf(t, Config{}, "система"); cc != nil {
		t.Errorf("кэш-точка появилась без просьбы: %v", cc)
	}
	cc := cacheOf(t, Config{CacheSystem: true}, "система")
	if cc == nil {
		t.Fatal("CacheSystem не поставил кэш-точку")
	}
	if got := cc.(map[string]any)["type"]; got != "ephemeral" {
		t.Errorf("тип кэш-точки: %v", got)
	}
	// Пустой системный блок помечать нечем: кэшировать нечего.
	if cc := cacheOf(t, Config{CacheSystem: true}, ""); cc != nil {
		t.Errorf("кэш-точка на пустом системном блоке: %v", cc)
	}
}

func TestUsageAccumulates(t *testing.T) {
	srv := serveUsage(t, 100, 40, 900, 7, nil)
	defer srv.Close()

	c := New(Config{APIKey: "test-key"}, srv.URL)
	if got := c.Usage(); got.Calls != 0 {
		t.Fatalf("свежий клиент уже что-то потратил: %s", got)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.GenerateJSON(context.Background(), "с", "п", map[string]any{"type": "object"}); err != nil {
			t.Fatal(err)
		}
	}
	want := Usage{Calls: 2, InputTokens: 200, OutputTokens: 14, CacheCreationTokens: 80, CacheReadTokens: 1800}
	if got := c.Usage(); got != want {
		t.Errorf("расход:\n  дано:    %s\n  ожидалось: %s", got, want)
	}
}

// Отказ классификатора оплачен так же, как удача. Не посчитав его, мы получили
// бы отчёт прогона, который тем сильнее врёт, чем хуже прогон прошёл.
func TestUsageCountsRefusal(t *testing.T) {
	srv := serve(t, "refusal", "", nil)
	defer srv.Close()

	c := New(Config{APIKey: "test-key"}, srv.URL)
	if _, err := c.GenerateJSON(context.Background(), "с", "п", map[string]any{"type": "object"}); !errors.Is(err, ErrRefusal) {
		t.Fatalf("ожидался ErrRefusal, получено: %v", err)
	}
	if got := c.Usage(); got.Calls != 1 || got.InputTokens != 10 {
		t.Errorf("отказ не попал в расход: %s", got)
	}
}
