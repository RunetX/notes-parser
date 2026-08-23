package rullm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// serve поднимает поддельную AI Studio и, если попросили, кладёт разобранное
// тело запроса в got.
func serve(t *testing.T, finish, content string, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("разбор тела запроса: %v", err)
			}
			(*got)["_auth"] = r.Header.Get("Authorization")
			(*got)["_folder"] = r.Header.Get("x-folder-id")
			(*got)["_path"] = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},
			"finish_reason":%q}],"usage":{"total_tokens":42}}`, content, finish)
	}))
}

func client(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(Config{APIKey: "test-key", FolderID: "b1gtest"}, url)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Форма запроса — то, что проверено пробой на живом API 23.08.2026: URI модели
// с folder, strict-схема, ключ и каталог заголовками. Тест сторожит именно её:
// разъедься любая деталь — и провайдер ответит отказом, а не молча другим.
func TestRequestShape(t *testing.T) {
	var req map[string]any
	srv := serve(t, "stop", `{"ok":true}`, &req)
	defer srv.Close()

	schema := map[string]any{"type": "object", "additionalProperties": false}
	raw, err := client(t, srv.URL).GenerateJSON(context.Background(), "правила", "пачка", schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("ответ: %s", raw)
	}
	if req["_path"] != "/chat/completions" {
		t.Errorf("путь: %v", req["_path"])
	}
	if req["model"] != "gpt://b1gtest/"+DefaultModel {
		t.Errorf("модель: %v", req["model"])
	}
	if req["_auth"] != "Api-Key test-key" {
		t.Errorf("заголовок ключа: %v", req["_auth"])
	}
	if req["_folder"] != "b1gtest" {
		t.Errorf("x-folder-id: %v", req["_folder"])
	}
	if req["temperature"] != float64(0) {
		t.Errorf("температура: %v (триаж обязан быть воспроизводим)", req["temperature"])
	}
	f := req["response_format"].(map[string]any)
	js := f["json_schema"].(map[string]any)
	if f["type"] != "json_schema" || js["strict"] != true || js["schema"] == nil {
		t.Errorf("response_format: %v", f)
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("сообщения: %v", msgs)
	}
}

// Обрыв по потолку обязан называться своим именем: оборванный ответ приходит с
// текстом, и «не разобрался в JSON» увело бы от настоящей причины.
func TestTruncatedAnswerNamesMaxTokens(t *testing.T) {
	srv := serve(t, "length", `{"items":[{"n":1`, nil)
	defer srv.Close()

	_, err := client(t, srv.URL).GenerateJSON(context.Background(), "s", "p", nil)
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("ожидалась ошибка про max_tokens, получено: %v", err)
	}
}

// Отказ по входному тексту — рабочий случай классификатора, у него свой тип
// ошибки, чтобы в логе он не читался как поломка сети.
func TestContentFilterHasOwnError(t *testing.T) {
	srv := serve(t, "content_filter", "", nil)
	defer srv.Close()

	_, err := client(t, srv.URL).GenerateJSON(context.Background(), "s", "p", nil)
	if !errors.Is(err, ErrFiltered) {
		t.Fatalf("ожидался ErrFiltered, получено: %v", err)
	}
}

func TestEmptyAnswerIsError(t *testing.T) {
	srv := serve(t, "stop", "   ", nil)
	defer srv.Close()

	if _, err := client(t, srv.URL).GenerateJSON(context.Background(), "s", "p", nil); err == nil {
		t.Fatal("пустой ответ обязан быть ошибкой")
	}
}

// Перегрузка повторяется, а отказ по ключу — нет: повтор его не вылечит, зато
// сожжёт одну из трёх попыток, которые даёт строке platmod.
func TestRetriesOnlyWhatIsWorthRetrying(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		wantCalls int32
	}{
		{"перегрузка", http.StatusTooManyRequests, retries + 1},
		{"внутренняя ошибка", http.StatusInternalServerError, retries + 1},
		{"негодный ключ", http.StatusUnauthorized, 1},
		{"негодная схема", http.StatusBadRequest, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tc.code)
				fmt.Fprint(w, `{"error":"нет"}`)
			}))
			defer srv.Close()

			_, err := client(t, srv.URL).GenerateJSON(context.Background(), "s", "p", nil)
			if err == nil {
				t.Fatal("ожидалась ошибка")
			}
			if got := atomic.LoadInt32(&calls); got != tc.wantCalls {
				t.Errorf("обращений к провайдеру: %d, ожидалось %d", got, tc.wantCalls)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(tc.code)) {
				t.Errorf("код отказа не назван: %v", err)
			}
		})
	}
}

// Успех после икоты: первая попытка падает, вторая отвечает.
func TestRecoversAfterTransientFailure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	raw, err := client(t, srv.URL).GenerateJSON(context.Background(), "s", "p", nil)
	if err != nil {
		t.Fatalf("после повтора ожидался успех: %v", err)
	}
	if string(raw) != "{}" {
		t.Errorf("ответ: %s", raw)
	}
}

// Без ключа или каталога клиент не создаётся вовсе: запрос без них всё равно
// получит отказ, но уже в бою и с невнятным текстом.
func TestRefusesIncompleteConfig(t *testing.T) {
	if _, err := New(Config{FolderID: "b1g"}, ""); err == nil {
		t.Error("клиент без ключа не должен создаваться")
	}
	if _, err := New(Config{APIKey: "k"}, ""); err == nil {
		t.Error("клиент без folder_id не должен создаваться")
	}
}
