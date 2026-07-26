package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTryMarkReplyProcessed(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	first, err := st.TryMarkReplyProcessed(ctx, MessengerTelegram, "100", now)
	if err != nil || !first {
		t.Fatalf("первая пометка: %v %v", first, err)
	}
	second, err := st.TryMarkReplyProcessed(ctx, MessengerTelegram, "100", now)
	if err != nil || second {
		t.Fatalf("повторная пометка должна вернуть false: %v %v", second, err)
	}
}

func TestSessionValidityToggle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if err := st.UpsertSession(ctx, MessengerTelegram, 7, `[{"name":"sid"}]`, now); err != nil {
		t.Fatal(err)
	}
	_, valid, err := st.SessionCookies(ctx, MessengerTelegram, 7)
	if err != nil || !valid {
		t.Fatalf("свежая сессия валидна: %v %v", valid, err)
	}

	if err := st.SetSessionValid(ctx, MessengerTelegram, 7, false, now); err != nil {
		t.Fatal(err)
	}
	_, valid, _ = st.SessionCookies(ctx, MessengerTelegram, 7)
	if valid {
		t.Error("сессия помечена невалидной, но valid=true")
	}

	// Повторный /login (UpsertSession) снова валидирует.
	if err := st.UpsertSession(ctx, MessengerTelegram, 7, `[{"name":"sid2"}]`, now); err != nil {
		t.Fatal(err)
	}
	_, valid, _ = st.SessionCookies(ctx, MessengerTelegram, 7)
	if !valid {
		t.Error("после повторного входа сессия должна быть валидна")
	}
}

func TestSessionCookiesNotFound(t *testing.T) {
	_, _, err := openTest(t).SessionCookies(context.Background(), MessengerTelegram, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидался ErrNotFound, получено: %v", err)
	}
}

func TestDialogStateLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if s, _ := st.DialogState(ctx, MessengerTelegram, 5); s != "" {
		t.Errorf("изначально состояние пустое, получено %q", s)
	}
	if err := st.SetDialogState(ctx, MessengerTelegram, 5, "await_credentials", now); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.DialogState(ctx, MessengerTelegram, 5); s != "await_credentials" {
		t.Errorf("состояние: %q", s)
	}
	// Перезапись состояния.
	st.SetDialogState(ctx, MessengerTelegram, 5, "await_note", now)
	if s, _ := st.DialogState(ctx, MessengerTelegram, 5); s != "await_note" {
		t.Errorf("состояние после перезаписи: %q", s)
	}
	if err := st.ClearDialogState(ctx, MessengerTelegram, 5); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.DialogState(ctx, MessengerTelegram, 5); s != "" {
		t.Errorf("после сброса состояние пустое, получено %q", s)
	}
}

func TestNoteByThread(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	noteID, ok, err := st.CaptureNoteThread(ctx, MessengerTelegram, "10", "900")
	if err != nil || !ok || noteID != "n1" {
		t.Fatalf("захват треда: %q %v %v", noteID, ok, err)
	}
	n, err := st.NoteByThread(ctx, MessengerTelegram, "900")
	if err != nil || n.ID != "n1" {
		t.Errorf("поиск по треду: %q %v", n.ID, err)
	}
	// Write-through в легаси-колонку.
	if n.TGThreadID != 900 {
		t.Errorf("tg_thread_id (write-through): %d", n.TGThreadID)
	}
	// Повторный захват того же треда — false.
	if _, ok, _ := st.CaptureNoteThread(ctx, MessengerTelegram, "10", "901"); ok {
		t.Error("повторный захват должен вернуть false")
	}
	if _, err := st.NoteByThread(ctx, MessengerTelegram, "111"); !errors.Is(err, ErrNotFound) {
		t.Errorf("несуществующий тред: %v", err)
	}
}
