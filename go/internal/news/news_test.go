package news

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lovegw/internal/chantext"
	"lovegw/internal/store"
)

// fakePub — канал-приёмник: считает посты и умеет падать по требованию.
type fakePub struct {
	name  string
	posts []string
	fail  error
}

func (p *fakePub) Name() string { return p.name }

func (p *fakePub) PostChannelHTML(_ context.Context, html string) (string, error) {
	if p.fail != nil {
		return "", p.fail
	}
	p.posts = append(p.posts, html)
	return p.name + ".mid." + string(rune('a'+len(p.posts)-1)), nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestPrepare(t *testing.T) {
	html, err := Prepare("  <b>Новость</b> проекта  ")
	if err != nil {
		t.Fatalf("валидный текст: %v", err)
	}
	if html != "<b>Новость</b> проекта" {
		t.Errorf("обрезка пробелов: %q", html)
	}

	bad := []struct{ text, want string }{
		{"   ", "пустой текст"},
		{"<div>чужой тег</div>", "не поддерживается"},
		{"<b>незакрытый", "незакрытый тег"},
		{"5 < 7", "экранировать"},
		{strings.Repeat("а", chantext.MessageBudget+1), "сократите"},
	}
	for _, tc := range bad {
		_, err := Prepare(tc.text)
		if err == nil {
			t.Errorf("Prepare(%.20q) прошёл, ожидалась ошибка", tc.text)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Prepare(%.20q) = %v, ожидалось про %q", tc.text, err, tc.want)
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	base := time.Date(2026, 8, 4, 19, 30, 12, 0, time.UTC)
	if got := NewID(base); got != "20260804-193012" {
		t.Errorf("NewID = %q", got)
	}
	if NewID(base) == NewID(base.Add(time.Second)) {
		t.Error("id новостей в соседние секунды должны различаться")
	}
}

func TestPublishFanOutAndIdempotency(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	tg := &fakePub{name: store.MessengerTelegram}
	mx := &fakePub{name: store.MessengerMax}
	svc := New(st, []Publisher{tg, mx}, slog.Default())

	if !svc.Ready() {
		t.Fatal("служба с приёмниками должна быть готова")
	}

	res := svc.Publish(ctx, "20260804-193012", "<b>Новость</b>")
	if len(res) != 2 || !res[0].Sent || !res[1].Sent {
		t.Fatalf("оба канала должны получить новость: %+v", res)
	}
	if len(tg.posts) != 1 || len(mx.posts) != 1 {
		t.Fatalf("постов: telegram %d, max %d", len(tg.posts), len(mx.posts))
	}
	if _, _, found, _ := st.Target(ctx, store.MessengerMax, store.TargetNews, "20260804-193012"); !found {
		t.Error("публикация должна оставить след в message_targets")
	}

	// Повтор с тем же id ничего не шлёт.
	res = svc.Publish(ctx, "20260804-193012", "<b>Новость</b>")
	for _, r := range res {
		if r.Sent || r.Err != nil {
			t.Errorf("повтор не публикует заново: %+v", r)
		}
	}
	if len(tg.posts) != 1 || len(mx.posts) != 1 {
		t.Errorf("после повтора постов: telegram %d, max %d", len(tg.posts), len(mx.posts))
	}
	if Failed(res) {
		t.Error("повтор без ошибок")
	}
}

func TestPublishPartialFailureRollsForward(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	tg := &fakePub{name: store.MessengerTelegram}
	mx := &fakePub{name: store.MessengerMax, fail: errors.New("токен протух")}
	svc := New(st, []Publisher{tg, mx}, slog.Default())

	res := svc.Publish(ctx, "20260804-193012", "<b>Новость</b>")
	if !res[0].Sent {
		t.Error("сбой MAX не должен мешать telegram")
	}
	if res[1].Err == nil {
		t.Error("ошибка MAX должна попасть в отчёт")
	}
	if !Failed(res) {
		t.Error("Failed видит непрошедший канал")
	}
	if report := Report(res); !strings.Contains(report, "telegram: опубликовано") ||
		!strings.Contains(report, "max: ошибка — токен протух") {
		t.Errorf("отчёт админу: %q", report)
	}

	// Канал починился — повтор досылает только его, telegram не задваивается.
	mx.fail = nil
	res = svc.Publish(ctx, "20260804-193012", "<b>Новость</b>")
	if res[0].Sent {
		t.Error("telegram уже опубликован, повторно не шлём")
	}
	if !res[1].Sent {
		t.Error("MAX должен получить новость при повторе")
	}
	if len(tg.posts) != 1 || len(mx.posts) != 1 {
		t.Errorf("постов после доката: telegram %d, max %d", len(tg.posts), len(mx.posts))
	}
}

func TestReadyEmpty(t *testing.T) {
	var nilSvc *Service
	if nilSvc.Ready() {
		t.Error("nil-служба не готова")
	}
	if New(openTestStore(t), nil, slog.Default()).Ready() {
		t.Error("служба без приёмников не готова")
	}
}
