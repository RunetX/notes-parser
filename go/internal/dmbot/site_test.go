package dmbot

import (
	"context"
	"strings"
	"testing"
	"time"

	"lovegw/internal/store"
)

// fakeSiteLogin — площадка, выдающая ссылку. Считает вызовы: ключ должен
// рождаться РОВНО один раз на просьбу, иначе прошлая ссылка умирала бы, не успев
// доехать до человека.
type fakeSiteLogin struct {
	calls   int
	profile int64
	nick    string
	err     error
}

func (f *fakeSiteLogin) BotLoginLink(_ context.Context, profileID int64, nick, _ string, _ int64) (string, time.Time, error) {
	f.calls++
	f.profile, f.nick = profileID, nick
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "https://t3h.ru/login/bot?key=abc", time.Now().Add(10 * time.Minute), nil
}

// Без живой сессии сайта ссылку не выдаём: ключ входа выдаётся под
// доказательство, а протухшая кука не доказывает ничего.
func TestSiteБезСессииЗовётКLogin(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	pf := &fakeSiteLogin{}
	l.SetSiteLogin(pf)

	l.HandleText(ctx, 777, "mid.1", "/site")
	if pf.calls != 0 {
		t.Fatal("ключ выдан без сессии сайта")
	}
	if !strings.Contains(tr.lastSent(), "/login") {
		t.Errorf("не позвали войти на сайт: %q", tr.lastSent())
	}
}

// С живой сессией — ссылка, и в ней сказано главное: одноразовая, живёт минуты,
// пересылать нельзя. Последнее не вежливость: кто перешёл, тот и вошёл.
func TestSiteВыдаётСсылку(t *testing.T) {
	ctx := context.Background()
	l, tr, _, st := newTestLogic(t, store.MessengerTelegram)
	pf := &fakeSiteLogin{}
	l.SetSiteLogin(pf)
	const uid = 777

	l.HandleText(ctx, uid, "mid.1", "/login")
	l.HandleText(ctx, uid, "mid.2", "user secret")
	if err := st.SetSessionIdentity(ctx, store.MessengerTelegram, uid, "1493279", "280703879", "Рио"); err != nil {
		t.Fatal(err)
	}

	l.HandleText(ctx, uid, "mid.3", "/site")
	if pf.calls != 1 {
		t.Fatalf("вызовов выдачи ключа: %d, ожидался 1", pf.calls)
	}
	if pf.profile != 1493279 || pf.nick != "Рио" {
		t.Errorf("площадке передали анкету %d/%q", pf.profile, pf.nick)
	}
	sent := tr.lastSent()
	for _, want := range []string{"https://t3h.ru/login/bot?key=abc", "одноразовая", "не пересылайте"} {
		if !strings.Contains(sent, want) {
			t.Errorf("в сообщении нет %q: %q", want, sent)
		}
	}
}

// Команды нет вовсе, пока площадка не подключена: /site тогда не значится ни в
// меню, ни в разборе — молчание правильнее отлупа «здесь такого нет».
func TestSiteБезПлощадкиНеОтвечает(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.HandleText(ctx, 777, "mid.1", "/site")
	if strings.Contains(tr.lastSent(), "Зазеркал") {
		t.Errorf("ответил про площадку, которой нет: %q", tr.lastSent())
	}
	for _, c := range botCommands(false, true, true, false) {
		if c.Name == "site" {
			t.Fatal("/site значится в меню без подключённой площадки")
		}
	}
}
