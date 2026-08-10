package dmbot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"lovegw/internal/news"
	"lovegw/internal/store"
)

const newsAdminID = 777

// fakeChannel — канал-приёмник новостей. Мьютекс нужен для проверки двойного
// нажатия из двух горутин: в Telegram апдейты обрабатываются параллельно.
type fakeChannel struct {
	mu    sync.Mutex
	name  string
	posts []string
	fail  error
}

func (c *fakeChannel) Name() string { return c.name }

func (c *fakeChannel) PostChannelHTML(_ context.Context, html string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return "", c.fail
	}
	c.posts = append(c.posts, html)
	return "mid.1", nil
}

// postCount — сколько раз новость реально ушла в канал.
func (c *fakeChannel) postCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

// setFail переключает канал в отказ и обратно.
func (c *fakeChannel) setFail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = err
}

// newNewsLogic — бот команд с подключённой публикацией новостей.
func newNewsLogic(t *testing.T) (*Logic, *fakeTransport, *fakeChannel) {
	t.Helper()
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	ch := &fakeChannel{name: store.MessengerTelegram}
	l.SetNews(news.New(st, []news.Publisher{ch}, slog.Default()), newsAdminID)
	return l, tr, ch
}

func TestNewsFlowPublishes(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	if !strings.Contains(tr.lastSent(), "текст новости") {
		t.Fatalf("приглашение к вводу: %q", tr.lastSent())
	}

	l.HandleText(ctx, newsAdminID, "2", "<b>Бот научился</b> расшифровывать голосовые")
	if !strings.Contains(tr.lastSent(), "Публикуем?") {
		t.Fatalf("превью с подтверждением: %q", tr.lastSent())
	}
	if len(ch.posts) != 0 {
		t.Fatal("до подтверждения в канал ничего не уходит")
	}

	l.HandleText(ctx, newsAdminID, "3", "да")
	if len(ch.posts) != 1 || ch.posts[0] != "<b>Бот научился</b> расшифровывать голосовые" {
		t.Fatalf("пост в канал: %+v", ch.posts)
	}
	if !strings.Contains(tr.lastSent(), "telegram: опубликовано") {
		t.Errorf("отчёт админу: %q", tr.lastSent())
	}
	if state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID); state != "" {
		t.Errorf("после публикации состояние снимается: %q", state)
	}
}

func TestNewsRejectsNonAdmin(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID+1, "1", "/news")
	if tr.lastSent() != msgUnknownCommand {
		t.Errorf("посторонний не должен видеть команду: %q", tr.lastSent())
	}
	if state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID+1); state != "" {
		t.Errorf("состояние постороннему не заводим: %q", state)
	}

	// И без подключённой службы команды нет даже у админа.
	plain, trPlain, _, _ := newTestLogic(t, store.MessengerTelegram)
	plain.HandleText(ctx, newsAdminID, "1", "/news")
	if trPlain.lastSent() != msgUnknownCommand {
		t.Errorf("без службы новостей команды нет: %q", trPlain.lastSent())
	}
	if len(ch.posts) != 0 {
		t.Error("в канал ничего не ушло")
	}
}

func TestNewsCancelAndBadMarkup(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<div>чужой тег</div>")
	if !strings.Contains(tr.lastSent(), "не поддерживается") {
		t.Fatalf("ошибка разметки объясняется: %q", tr.lastSent())
	}
	if state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID); state != stateAwaitNews {
		t.Fatalf("после ошибки разметки ждём исправленный текст, состояние: %q", state)
	}

	// Исправленный текст принимается, но отвечаем «нет» — публикации нет.
	l.HandleText(ctx, newsAdminID, "3", "<b>исправил</b>")
	l.HandleText(ctx, newsAdminID, "4", "нет")
	if len(ch.posts) != 0 {
		t.Errorf("отказ не публикует: %+v", ch.posts)
	}
	if !strings.Contains(tr.lastSent(), "не опубликована") {
		t.Errorf("сообщение об отмене: %q", tr.lastSent())
	}
	if state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID); state != "" {
		t.Errorf("после отказа состояние снимается: %q", state)
	}
}

// Сбой канала оставляет черновик: повторное «да» досылает новость.
func TestNewsRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	l, tr, ch := newNewsLogic(t)
	ch.fail = errors.New("bot api 502")

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<i>новость</i>")
	l.HandleText(ctx, newsAdminID, "3", "да")
	if !strings.Contains(tr.lastSent(), "bot api 502") {
		t.Fatalf("админ узнаёт причину: %q", tr.lastSent())
	}
	state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID)
	if !strings.HasPrefix(state, stateNewsPrefix) {
		t.Fatalf("черновик остаётся для повтора: %q", state)
	}

	ch.fail = nil
	l.HandleText(ctx, newsAdminID, "4", "да")
	if len(ch.posts) != 1 || ch.posts[0] != "<i>новость</i>" {
		t.Fatalf("повтор досылает новость: %+v", ch.posts)
	}
	if state, _ := l.st.DialogState(ctx, l.stateNS, newsAdminID); state != "" {
		t.Errorf("успешный повтор снимает состояние: %q", state)
	}
}

// Команда прерывает диалог новости, как и любой другой.
func TestNewsCancelByCommand(t *testing.T) {
	ctx := context.Background()
	l, _, ch := newNewsLogic(t)

	l.HandleText(ctx, newsAdminID, "1", "/news")
	l.HandleText(ctx, newsAdminID, "2", "<b>новость</b>")
	l.HandleText(ctx, newsAdminID, "3", "/cancel")
	l.HandleText(ctx, newsAdminID, "4", "да")
	if len(ch.posts) != 0 {
		t.Errorf("после /cancel «да» ничего не публикует: %+v", ch.posts)
	}
}

func TestParseNewsState(t *testing.T) {
	id, html, ok := parseNewsState("news:20260804-193012\n<b>текст</b>")
	if !ok || id != "20260804-193012" || html != "<b>текст</b>" {
		t.Errorf("разбор состояния: %q %q %v", id, html, ok)
	}
	for _, bad := range []string{"pm:5", "news:20260804-193012", "news:\nтекст", "news:id\n", ""} {
		if _, _, ok := parseNewsState(bad); ok {
			t.Errorf("состояние %q не новостное", bad)
		}
	}
}
