package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
	format := req["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Errorf("output_config.format: %v", format)
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
