package dmbot

import (
	"context"
	"strings"
	"testing"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

const pulpitAdminID = 778

// fakePulpit — служба амвона глазами ручки: тумблер и отчёт.
type fakePulpit struct {
	enabled   bool
	offReason string
	by        string
}

func (p *fakePulpit) PulpitStatus(context.Context) (string, bool, string) {
	if p.enabled {
		return "🕯 Амвон включён.", true, p.offReason
	}
	return "⛔ Амвон выключен.", false, p.offReason
}

func (p *fakePulpit) SetPulpitEnabled(_ context.Context, on bool, by string) error {
	p.enabled, p.by = on, by
	if on {
		p.offReason = ""
	}
	return nil
}

func newPulpitLogic(t *testing.T, svc *fakePulpit) (*Logic, *fakeTransport) {
	t.Helper()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.SetPulpit(svc, pulpitAdminID)
	return l, tr
}

// TestPulpitAdminOnly — посторонним команда отвечает как несуществующая: она
// админская и в списке команд не значится.
func TestPulpitAdminOnly(t *testing.T) {
	ctx := context.Background()
	svc := &fakePulpit{enabled: true}
	l, tr := newPulpitLogic(t, svc)

	l.HandleText(ctx, 12345, "1", "/pulpit")
	if tr.lastSent() != msgUnknownCommand {
		t.Errorf("постороннему: %q", tr.lastSent())
	}

	l.HandleText(ctx, pulpitAdminID, "2", "/pulpit")
	if !strings.Contains(tr.lastSent(), "включён") {
		t.Errorf("админу: %q", tr.lastSent())
	}
}

// TestPulpitNotInMenus — ни в меню команд мессенджера, ни в приветствии.
func TestPulpitNotInMenus(t *testing.T) {
	for _, cmd := range botCommands(false, true, true) {
		if cmd.Name == "pulpit" {
			t.Error("админская команда попала в меню мессенджера")
		}
	}
	if strings.Contains(startMessage(false, true, true), "/pulpit") {
		t.Error("админская команда попала в приветствие")
	}
}

// TestPulpitToggle — выключение мгновенное, включение после срабатывания
// предохранителя идёт через подтверждение.
func TestPulpitToggle(t *testing.T) {
	ctx := context.Background()
	svc := &fakePulpit{enabled: true}
	l, tr := newPulpitLogic(t, svc)

	l.HandleCallback(ctx, pulpitAdminID, kbd.Callback{
		MessageID: "10", Payload: kbd.Pack(verbPulpitSet, argPulpitOff),
	})
	if svc.enabled {
		t.Fatal("кнопка «Выключить» не выключила амвон")
	}
	if !strings.HasPrefix(svc.by, "admin:") {
		t.Errorf("в журнале должен остаться автор: %q", svc.by)
	}

	// Выключил предохранитель: включение идёт через вопрос с причиной.
	svc.offReason = "3 реплики подряд не появились в тредах"
	l.HandleCallback(ctx, pulpitAdminID, kbd.Callback{
		MessageID: "11", Payload: kbd.Pack(verbPulpitAsk, argPulpitOn),
	})
	if svc.enabled {
		t.Fatal("вопрос не должен включать амвон")
	}
	if !strings.Contains(tr.lastEdit().text, svc.offReason) {
		t.Errorf("в вопросе нет причины выключения: %q", tr.lastEdit().text)
	}

	l.HandleCallback(ctx, pulpitAdminID, kbd.Callback{
		MessageID: "12", Payload: kbd.Pack(verbPulpitSet, argPulpitOn),
	})
	if !svc.enabled {
		t.Fatal("подтверждение не включило амвон")
	}
}

// TestPulpitCallbackStranger — чужое нажатие ничего не переключает.
func TestPulpitCallbackStranger(t *testing.T) {
	ctx := context.Background()
	svc := &fakePulpit{enabled: true}
	l, _ := newPulpitLogic(t, svc)

	l.HandleCallback(ctx, 12345, kbd.Callback{
		MessageID: "10", Payload: kbd.Pack(verbPulpitSet, argPulpitOff),
	})
	if !svc.enabled {
		t.Error("посторонний выключил амвон")
	}
}

// TestPulpitKeyboardPayloadFits — payload кнопок влезает в предел Telegram.
func TestPulpitKeyboardPayloadFits(t *testing.T) {
	for _, kb := range []*kbd.Keyboard{
		pulpitKeyboard(true, ""),
		pulpitKeyboard(false, ""),
		pulpitKeyboard(false, "запрет писать"),
	} {
		for _, row := range kb.Rows {
			for _, b := range row {
				if !kbd.Fits(b.Payload) {
					t.Errorf("payload не влезает: %q", b.Payload)
				}
			}
		}
	}
}
