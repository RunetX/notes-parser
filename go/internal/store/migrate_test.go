package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateV3ToV4 строит базу версии 3 с данными старого формата и
// проверяет, что открытие через Open переносит телеграм-id в message_targets
// и пересобирает пользовательские таблицы с колонкой messenger.
func TestMigrateV3ToV4(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{schemaSQL, migrateV2SQL, migrateV3SQL} {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	seed := []string{
		`INSERT INTO notes (id, text, status, tg_message_id, tg_thread_id, first_seen_at)
		 VALUES ('n1', 'т', 'posted', 10, 900, '2026-07-01T00:00:00Z')`,
		`INSERT INTO notes (id, text, status, first_seen_at)
		 VALUES ('n2', 'т2', 'pending', '2026-07-01T00:00:00Z')`,
		`INSERT INTO comments (id, note_id, author_name, text, tg_message_id, created_at)
		 VALUES (5, 'n1', 'А', 'к', 901, '2026-07-01T00:00:00Z')`,
		`INSERT INTO note_images (note_id, position, url, tg_message_id)
		 VALUES ('n1', 0, 'https://cdn/a.jpg', 902)`,
		`INSERT INTO sessions (tg_user_id, cookies, updated_at) VALUES (42, '[]', '2026-07-01T00:00:00Z')`,
		`INSERT INTO subscriptions (keyword, tg_user_id) VALUES ('Граф', 42)`,
		`INSERT INTO dialog_states (tg_user_id, state, updated_at) VALUES (42, 'await_note', '2026-07-01T00:00:00Z')`,
		`INSERT INTO processed_replies (tg_message_id, processed_at) VALUES (903, '2026-07-01T00:00:00Z')`,
		`PRAGMA user_version = 3`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	msg, _, found, err := st.Target(ctx, MessengerTelegram, TargetNotePost, "n1")
	if err != nil || !found || msg != "10" {
		t.Errorf("note_post: %q %v %v", msg, found, err)
	}
	if _, _, found, _ := st.Target(ctx, MessengerTelegram, TargetNotePost, "n2"); found {
		t.Error("незапощенная заметка не должна получить target")
	}
	n, err := st.NoteByThread(ctx, MessengerTelegram, "900")
	if err != nil || n.ID != "n1" {
		t.Errorf("note_thread: %q %v", n.ID, err)
	}
	c, err := st.CommentByTarget(ctx, MessengerTelegram, "901")
	if err != nil || c.ID != 5 {
		t.Errorf("comment target: %+v %v", c, err)
	}
	if imgs, _ := st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1"); len(imgs) != 0 {
		t.Errorf("иллюстрация была отправлена — не должна попасть в незашедшие: %+v", imgs)
	}

	if _, valid, err := st.SessionCookies(ctx, MessengerTelegram, 42); err != nil || !valid {
		t.Errorf("сессия после миграции: valid=%v err=%v", valid, err)
	}
	if kws, _ := st.SubscriptionsByUser(ctx, MessengerTelegram, 42); len(kws) != 1 || kws[0] != "Граф" {
		t.Errorf("подписки после миграции: %v", kws)
	}
	if s, _ := st.DialogState(ctx, MessengerTelegram, 42); s != "await_note" {
		t.Errorf("состояние диалога после миграции: %q", s)
	}
	if fresh, _ := st.TryMarkReplyProcessed(ctx, MessengerTelegram, "903", n.FirstSeenAt); fresh {
		t.Error("обработанный ответ должен остаться помеченным после миграции")
	}
}
