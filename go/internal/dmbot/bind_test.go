package dmbot

// Привязка со стороны бота: подтверждение ником и вход по привязке.

import (
	"context"
	"strings"
	"testing"
	"time"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

// fakeBinding — площадка, умеющая привязку. Помнит, что у неё спросили:
// подтверждение обязано СПРАШИВАТЬ, не тратя кода, — иначе подсунутый код
// срабатывал бы одним переходом.
type fakeBinding struct {
	offers  int
	binds   int
	nick    string
	bound   int64 // messenger_user_id, к которому привязано (0 — привязки нет)
	offErr  error
	bindErr error
}

func (f *fakeBinding) BindOffer(_ context.Context, _ string) (string, error) {
	f.offers++
	return f.nick, f.offErr
}

func (f *fakeBinding) Bind(_ context.Context, _, _ string, mid int64) (string, error) {
	f.binds++
	if f.bindErr != nil {
		return "", f.bindErr
	}
	f.bound = mid
	return f.nick, nil
}

func (f *fakeBinding) BoundLoginLink(_ context.Context, _ string, mid int64) (string, time.Time, error) {
	if f.bound != mid {
		return "", time.Time{}, ErrNoBinding
	}
	return "https://t3h.ru/login/bot?key=bound", time.Now().Add(10 * time.Minute), nil
}

// ПОДТВЕРЖДЕНИЕ НАЗЫВАЕТ НИК, и это единственное, что отличает свой код от
// подсунутого: человек, которому прислали чужой код, видит чужое имя.
func TestBindAsksWithTheNick(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	fb := &fakeBinding{nick: "Рио"}
	l.SetSiteBinding(fb)

	l.HandleText(ctx, 777, "mid.1", "/bind MSG-7K3M-Q2XZ")
	if fb.offers != 1 || fb.binds != 0 {
		t.Fatalf("спрошено %d, привязано %d — код потрачен до подтверждения", fb.offers, fb.binds)
	}
	// СООБЩЕНИЕ С КОДОМ УДАЛЕНО — как с паролем у /login, и по той же причине:
	// это живой ключ от учётной записи, а не имя команды. Прочитавший его в
	// чужой переписке привязал бы СВОЙ мессенджер к чужой записи.
	if len(tr.deleted) != 1 || tr.deleted[0] != "mid.1" {
		t.Errorf("сообщение с кодом привязки осталось в переписке: %v", tr.deleted)
	}
	sent := tr.lastSent()
	if !strings.Contains(sent, "Рио") {
		t.Errorf("в вопросе нет ника учётной записи: %q", sent)
	}
	if !strings.Contains(sent, "Telegram") {
		t.Errorf("не сказано, какой мессенджер привязывается: %q", sent)
	}
}

// Нажатие «Привязать» тратит код и отвечает готовностью.
func TestBindConfirmBinds(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	fb := &fakeBinding{nick: "Рио"}
	l.SetSiteBinding(fb)
	const uid = 777

	l.HandleCallback(ctx, uid, kbd.Callback{MessageID: "mid.1", Payload: kbd.Pack(verbBind, "MSG-7K3M-Q2XZ")})
	if fb.binds != 1 || fb.bound != uid {
		t.Fatalf("привязок %d, привязано к %d", fb.binds, fb.bound)
	}
	if !strings.Contains(tr.lastEdit().text, "Рио") {
		t.Errorf("подтверждение не названо: %q", tr.lastEdit().text)
	}
}

// Чужой мессенджер не уводим молча: у того человека это способ входа, и ответ
// обязан объяснить, что делать.
func TestBindTakenExplains(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.SetSiteBinding(&fakeBinding{nick: "Рио", bindErr: ErrMessengerTaken})

	l.HandleCallback(ctx, 777, kbd.Callback{MessageID: "mid.1", Payload: kbd.Pack(verbBind, "MSG-7K3M-Q2XZ")})
	if !strings.Contains(tr.lastEdit().text, "уже привязан к другой") {
		t.Errorf("отказ не объяснён: %q", tr.lastEdit().text)
	}
}

// РАДИ ЭТОГО ВСЁ И ЗАТЕВАЛОСЬ: /site выдаёт ссылку БЕЗ живой сессии НГС, если
// мессенджер привязан. Прежде здесь был тупик — «сначала войдите на сайт», —
// а у человека без анкеты войти туда нельзя вовсе.
func TestSiteFallsBackToBinding(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	pf := &fakeSiteLogin{}
	l.SetSiteLogin(pf)
	l.SetSiteBinding(&fakeBinding{nick: "Рио", bound: 777})

	l.HandleText(ctx, 777, "mid.1", "/site")
	if pf.calls != 0 {
		t.Error("пошли по дороге сессии НГС, хотя её нет")
	}
	sent := tr.lastSent()
	if !strings.Contains(sent, "key=bound") {
		t.Fatalf("ссылка по привязке не выдана: %q", sent)
	}
	// Ищем ФРАЗУ, а не «/login»: сама ссылка входа ведёт на /login/bot, и
	// наивная проверка ловила бы её же.
	if strings.Contains(sent, "Сначала войдите на сайт") {
		t.Error("рядом со ссылкой зовут входить на НГС — это и был прежний тупик")
	}
}

// А без привязки — прежний ответ: дорога остаётся одна, и молчать о ней нельзя.
func TestSiteWithoutBindingStillAsksForLogin(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.SetSiteLogin(&fakeSiteLogin{})
	l.SetSiteBinding(&fakeBinding{nick: "Рио"})

	l.HandleText(ctx, 777, "mid.1", "/site")
	if !strings.Contains(tr.lastSent(), "/login") {
		t.Errorf("не позвали войти на сайт: %q", tr.lastSent())
	}
}

// Команды нет, пока площадка не умеет привязку: /bind не значится ни в меню, ни
// в разборе — молчание правильнее отлупа «здесь такого нет».
func TestBindCommandAbsentWithoutBinding(t *testing.T) {
	ctx := context.Background()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)

	l.HandleText(ctx, 777, "mid.1", "/bind MSG-7K3M-Q2XZ")
	if strings.Contains(tr.lastSent(), "Зазеркал") {
		t.Errorf("ответил про площадку, которой нет: %q", tr.lastSent())
	}
	for _, c := range botCommands(false, true, true, true, false) {
		if c.Name == "bind" {
			t.Fatal("/bind значится в меню без подключённой привязки")
		}
	}
}
