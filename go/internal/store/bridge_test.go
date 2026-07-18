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

	first, err := st.TryMarkReplyProcessed(ctx, 100, now)
	if err != nil || !first {
		t.Fatalf("первая пометка: %v %v", first, err)
	}
	second, err := st.TryMarkReplyProcessed(ctx, 100, now)
	if err != nil || second {
		t.Fatalf("повторная пометка должна вернуть false: %v %v", second, err)
	}
}

func TestSessionValidityToggle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if err := st.UpsertSession(ctx, 7, `[{"name":"sid"}]`, now); err != nil {
		t.Fatal(err)
	}
	_, valid, err := st.SessionCookies(ctx, 7)
	if err != nil || !valid {
		t.Fatalf("свежая сессия валидна: %v %v", valid, err)
	}

	if err := st.SetSessionValid(ctx, 7, false, now); err != nil {
		t.Fatal(err)
	}
	_, valid, _ = st.SessionCookies(ctx, 7)
	if valid {
		t.Error("сессия помечена невалидной, но valid=true")
	}

	// Повторный /login (UpsertSession) снова валидирует.
	if err := st.UpsertSession(ctx, 7, `[{"name":"sid2"}]`, now); err != nil {
		t.Fatal(err)
	}
	_, valid, _ = st.SessionCookies(ctx, 7)
	if !valid {
		t.Error("после повторного входа сессия должна быть валидна")
	}
}

func TestSessionCookiesNotFound(t *testing.T) {
	_, _, err := openTest(t).SessionCookies(context.Background(), 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ожидался ErrNotFound, получено: %v", err)
	}
}

func TestDialogStateLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if s, _ := st.DialogState(ctx, 5); s != "" {
		t.Errorf("изначально состояние пустое, получено %q", s)
	}
	if err := st.SetDialogState(ctx, 5, "await_credentials", now); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.DialogState(ctx, 5); s != "await_credentials" {
		t.Errorf("состояние: %q", s)
	}
	// Перезапись состояния.
	st.SetDialogState(ctx, 5, "await_note", now)
	if s, _ := st.DialogState(ctx, 5); s != "await_note" {
		t.Errorf("состояние после перезаписи: %q", s)
	}
	if err := st.ClearDialogState(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if s, _ := st.DialogState(ctx, 5); s != "" {
		t.Errorf("после сброса состояние пустое, получено %q", s)
	}
}

func TestNoteByThreadID(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted,
		TGMessageID: 10, FirstSeenAt: time.Now()})
	if _, _, err := st.SetNoteThreadIDByMessageID(ctx, 10, 900); err != nil {
		t.Fatal(err)
	}
	n, err := st.NoteByThreadID(ctx, 900)
	if err != nil || n.ID != "n1" {
		t.Errorf("поиск по треду: %q %v", n.ID, err)
	}
	if _, err := st.NoteByThreadID(ctx, 111); !errors.Is(err, ErrNotFound) {
		t.Errorf("несуществующий тред: %v", err)
	}
}
