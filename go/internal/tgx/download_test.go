package tgx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-telegram/bot"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// secretToken — «токен бота» в тестах: ссылка на файл Bot API содержит его в
// пути, поэтому проверяем, что он не утекает в ошибки.
const secretToken = "123456:AAsecret-bot-token"

// downloadMirror собирает Mirror поверх тестового сервера Bot API. filePath —
// что вернёт getFile; onDownload обслуживает скачивание файла.
func downloadMirror(t *testing.T, fileSize int64, onDownload http.HandlerFunc) *Mirror {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+secretToken+"/getFile", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"result":{"file_id":"AgADvoice","file_unique_id":"AQADunique",
			"file_size":%d,"file_path":"voice/file_1.oga"}}`, fileSize)
	})
	mux.HandleFunc("/file/bot"+secretToken+"/voice/file_1.oga", onDownload)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	b, err := bot.New(secretToken, bot.WithSkipGetMe(), bot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return &Mirror{b: b, hc: srv.Client(), log: testLogger()}
}

func TestDownloadFile(t *testing.T) {
	m := downloadMirror(t, 1024, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "OGGDATA")
	})
	data, err := m.DownloadFile(context.Background(), "AgADvoice")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "OGGDATA" {
		t.Errorf("скачано: %q", data)
	}
}

func TestDownloadFileRejectsOversized(t *testing.T) {
	var downloaded bool
	m := downloadMirror(t, fileSizeLimit+1, func(http.ResponseWriter, *http.Request) {
		downloaded = true
	})
	if _, err := m.DownloadFile(context.Background(), "AgADvoice"); err == nil {
		t.Fatal("файл сверх лимита должен быть ошибкой")
	}
	if downloaded {
		t.Error("слишком большой файл не должен качаться")
	}
}

// Ссылка на файл Bot API содержит токен бота: ошибка транспорта не должна
// вытащить его в лог.
func TestDownloadFileHidesToken(t *testing.T) {
	m := downloadMirror(t, 1024, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("сервер не поддерживает hijack — сломать соединение нечем")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // обрыв: net/http завернёт адрес запроса в *url.Error
	})
	_, err := m.DownloadFile(context.Background(), "AgADvoice")
	if err == nil {
		t.Fatal("обрыв соединения должен быть ошибкой")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("токен бота утёк в ошибку: %v", err)
	}
}

// deadTransport обрывает любой запрос: net/http завернёт адрес в *url.Error.
type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("сеть недоступна")
}

// ProbePendingUpdates зовёт Bot API мимо библиотеки (та getUpdates не
// экспортирует), поэтому маскировать токен в адресе некому, кроме нас; ошибку
// печатает doctor.
func TestProbePendingUpdatesHidesToken(t *testing.T) {
	_, err := ProbePendingUpdates(context.Background(), secretToken,
		&http.Client{Transport: deadTransport{}})
	if err == nil {
		t.Fatal("сбой транспорта должен быть ошибкой")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Errorf("токен бота утёк в ошибку: %v", err)
	}
}
