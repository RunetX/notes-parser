package dmbot

import (
	"context"
	"strings"
	"testing"

	"lovegw/internal/kbd"
	"lovegw/internal/store"
)

const morningAdminID = 779

// fakeMorning — служба утренней заметки глазами ручки: тумблер и отчёт.
type fakeMorning struct {
	enabled   bool
	offReason string
	by        string
}

func (m *fakeMorning) Status(context.Context) (string, bool, string) {
	if m.enabled {
		return "🌅 Утренняя заметка включена.", true, m.offReason
	}
	return "⛔ Утренняя заметка выключена.", false, m.offReason
}

func (m *fakeMorning) SetEnabled(_ context.Context, on bool, by string) error {
	m.enabled, m.by = on, by
	if on {
		m.offReason = ""
	}
	return nil
}

func newMorningLogic(t *testing.T, svc *fakeMorning) (*Logic, *fakeTransport) {
	t.Helper()
	l, tr, _, _ := newTestLogic(t, store.MessengerTelegram)
	l.SetMorning(svc, morningAdminID)
	return l, tr
}

// TestMorningAdminOnly — посторонним команда отвечает как несуществующая: она
// админская и в списке команд не значится.
func TestMorningAdminOnly(t *testing.T) {
	ctx := context.Background()
	svc := &fakeMorning{enabled: true}
	l, tr := newMorningLogic(t, svc)

	l.HandleText(ctx, 12345, "1", "/morning")
	if tr.lastSent() != msgUnknownCommand {
		t.Errorf("постороннему: %q", tr.lastSent())
	}

	l.HandleText(ctx, morningAdminID, "2", "/morning")
	if !strings.Contains(tr.lastSent(), "включена") {
		t.Errorf("админу: %q", tr.lastSent())
	}
}

// TestMorningNotInMenus — ни в меню команд мессенджера, ни в приветствии.
func TestMorningNotInMenus(t *testing.T) {
	for _, cmd := range botCommands(false, true, true) {
		if cmd.Name == "morning" {
			t.Error("админская команда попала в меню мессенджера")
		}
	}
	if strings.Contains(startMessage(false, true, true), "/morning") {
		t.Error("админская команда попала в приветствие")
	}
}

// TestMorningToggle — выключение мгновенное, включение после срабатывания
// предохранителя идёт через подтверждение с текстом причины.
func TestMorningToggle(t *testing.T) {
	ctx := context.Background()
	svc := &fakeMorning{enabled: true}
	l, tr := newMorningLogic(t, svc)

	l.HandleCallback(ctx, morningAdminID, kbd.Callback{
		MessageID: "10", Payload: kbd.Pack(verbMorningSet, argMorningOff),
	})
	if svc.enabled {
		t.Fatal("кнопка «Выключить» не выключила утреннюю заметку")
	}
	if !strings.HasPrefix(svc.by, "admin:") {
		t.Errorf("в журнале должен остаться автор: %q", svc.by)
	}

	svc.offReason = "заметка не появилась в ленте 2026-08-23, 2026-08-24"
	l.HandleCallback(ctx, morningAdminID, kbd.Callback{
		MessageID: "11", Payload: kbd.Pack(verbMorningAsk, argMorningOn),
	})
	if svc.enabled {
		t.Fatal("вопрос не должен включать заметку")
	}
	if !strings.Contains(tr.lastEdit().text, svc.offReason) {
		t.Errorf("в вопросе нет причины выключения: %q", tr.lastEdit().text)
	}

	l.HandleCallback(ctx, morningAdminID, kbd.Callback{
		MessageID: "12", Payload: kbd.Pack(verbMorningSet, argMorningOn),
	})
	if !svc.enabled {
		t.Fatal("подтверждение не включило утреннюю заметку")
	}
}

// TestMorningCallbackStranger — чужое нажатие ничего не переключает.
func TestMorningCallbackStranger(t *testing.T) {
	ctx := context.Background()
	svc := &fakeMorning{enabled: true}
	l, _ := newMorningLogic(t, svc)

	l.HandleCallback(ctx, 12345, kbd.Callback{
		MessageID: "10", Payload: kbd.Pack(verbMorningSet, argMorningOff),
	})
	if !svc.enabled {
		t.Error("посторонний выключил утреннюю заметку")
	}
}

// TestMorningKeyboardPayloadFits — payload кнопок влезает в предел Telegram.
func TestMorningKeyboardPayloadFits(t *testing.T) {
	for _, kb := range []*kbd.Keyboard{
		morningKeyboard(true, ""),
		morningKeyboard(false, ""),
		morningKeyboard(false, "запрет писать"),
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
