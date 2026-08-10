package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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
	if kws, _ := st.SubscriptionsByUser(ctx, MessengerTelegram, 42); len(kws) != 1 || kws[0].Target != "Граф" {
		t.Errorf("подписки после миграции: %v", kws)
	}
	if s, _ := st.DialogState(ctx, MessengerTelegram, 42); s != "await_note" {
		t.Errorf("состояние диалога после миграции: %q", s)
	}
	if fresh, _ := st.TryMarkReplyProcessed(ctx, MessengerTelegram, "903", n.FirstSeenAt); fresh {
		t.Error("обработанный ответ должен остаться помеченным после миграции")
	}
}

// TestMigrateV4ToV5 строит базу версии 4, открывает через Open (накат v5) и
// проверяет: site-идентичность в sessions, talks_peers/talks_messages,
// маршрутизацию по доставленному ЛС и что v4-данные не пострадали.
func TestMigrateV4ToV5(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{schemaSQL, migrateV2SQL, migrateV3SQL, migrateV4SQL} {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	seed := []string{
		`INSERT INTO sessions (messenger, user_id, cookies, valid, updated_at)
		 VALUES ('telegram', 42, '[]', 1, '2026-07-01T00:00:00Z')`,
		`PRAGMA user_version = 4`,
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

	// v4-регресс: сессия жива после наката v5.
	if _, valid, err := st.SessionCookies(ctx, MessengerTelegram, 42); err != nil || !valid {
		t.Fatalf("сессия после v5: valid=%v err=%v", valid, err)
	}
	if owners, _ := st.SessionOwners(ctx, MessengerTelegram); len(owners) != 1 || owners[0] != 42 {
		t.Errorf("SessionOwners: %v", owners)
	}

	// Site-идентичность: по умолчанию пусто, потом round-trip.
	if pid, pass, nick, err := st.SessionIdentity(ctx, MessengerTelegram, 42); err != nil || pid != "" || pass != "" || nick != "" {
		t.Errorf("идентичность по умолчанию: %q %q %q %v", pid, pass, nick, err)
	}
	if err := st.SetSessionIdentity(ctx, MessengerTelegram, 42, "1472546", "280703879", "Рантье"); err != nil {
		t.Fatal(err)
	}
	if pid, pass, nick, _ := st.SessionIdentity(ctx, MessengerTelegram, 42); pid != "1472546" || pass != "280703879" || nick != "Рантье" {
		t.Errorf("идентичность после записи: %q %q %q", pid, pass, nick)
	}

	// talks_peers: upsert + latest-wins по непустому нику.
	peerID, err := st.UpsertTalkPeer(ctx, TalkPeer{Messenger: MessengerTelegram, OwnerUserID: 42, PassportID: "777", Nick: "Аноним"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.UpsertTalkPeer(ctx, TalkPeer{Messenger: MessengerTelegram, OwnerUserID: 42, PassportID: "777", Nick: "Мария", ProfileID: "555"})
	if err != nil || again != peerID {
		t.Fatalf("повторный upsert дал другой id: %d != %d (%v)", again, peerID, err)
	}
	if p, _ := st.TalkPeerByID(ctx, peerID); p.Nick != "Мария" || p.ProfileID != "555" {
		t.Errorf("latest-wins: %+v", p)
	}
	// Пустой ник не должен затирать заполненный.
	if _, err := st.UpsertTalkPeer(ctx, TalkPeer{Messenger: MessengerTelegram, OwnerUserID: 42, PassportID: "777"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.TalkPeerByID(ctx, peerID); p.Nick != "Мария" {
		t.Errorf("пустой ник затёр заполненный: %q", p.Nick)
	}

	// talks_messages: дедуп входящих, исходящие с NULL не конфликтуют.
	msgID, fresh, err := st.InsertTalkMessage(ctx, TalkMessage{PeerID: peerID, SiteMsgID: "100", Direction: TalkIn, Text: "привет"})
	if err != nil || !fresh {
		t.Fatalf("первое входящее: fresh=%v err=%v", fresh, err)
	}
	if _, fresh, _ := st.InsertTalkMessage(ctx, TalkMessage{PeerID: peerID, SiteMsgID: "100", Direction: TalkIn}); fresh {
		t.Error("повторное входящее с тем же site_msg_id должно быть не fresh")
	}
	o1, f1, _ := st.InsertTalkMessage(ctx, TalkMessage{PeerID: peerID, Direction: TalkOut, Text: "ответ"})
	o2, f2, _ := st.InsertTalkMessage(ctx, TalkMessage{PeerID: peerID, Direction: TalkOut, Text: "ещё"})
	if !f1 || !f2 || o1 == o2 {
		t.Errorf("исходящие с NULL site_msg_id должны быть разными и fresh: %d %d %v %v", o1, o2, f1, f2)
	}

	// Маршрутизация ответа: доставленное ЛS → message_targets → peer.
	if err := st.SetTarget(ctx, MessengerTelegram, TargetPMMessage, strconv.FormatInt(msgID, 10), "555", ""); err != nil {
		t.Fatal(err)
	}
	if p, err := st.PeerByDeliveredPM(ctx, MessengerTelegram, "555"); err != nil || p.ID != peerID {
		t.Errorf("PeerByDeliveredPM: %+v %v", p, err)
	}
	if _, err := st.PeerByDeliveredPM(ctx, MessengerTelegram, "999"); !errors.Is(err, ErrNotFound) {
		t.Errorf("неизвестное ЛС должно давать ErrNotFound, а не %v", err)
	}

	// Курсор двигается.
	if err := st.SetPeerCursor(ctx, peerID, "100", time.Now()); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.TalkPeerByID(ctx, peerID); p.CursorMsgID != "100" {
		t.Errorf("курсор не сдвинулся: %q", p.CursorMsgID)
	}
	if peers, _ := st.TalkPeers(ctx, MessengerTelegram, 42); len(peers) != 1 || peers[0].ID != peerID {
		t.Errorf("TalkPeers: %+v", peers)
	}
}

// TestMigrateV5ToV6 строит базу версии 5, открывает через Open (накат v6) и
// проверяет кэш расшифровок с квотой ASR, а также что данные v5 не пострадали.
func TestMigrateV5ToV6(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{schemaSQL, migrateV2SQL, migrateV3SQL, migrateV4SQL, migrateV5SQL} {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	seed := []string{
		`INSERT INTO sessions (messenger, user_id, cookies, valid, updated_at)
		 VALUES ('telegram', 42, '[]', 1, '2026-08-01T00:00:00Z')`,
		`PRAGMA user_version = 5`,
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

	// v5-регресс: сессия жива после наката v6.
	if _, valid, err := st.SessionCookies(ctx, MessengerTelegram, 42); err != nil || !valid {
		t.Fatalf("сессия после v6: valid=%v err=%v", valid, err)
	}

	if err := st.SaveTranscript(ctx, MessengerTelegram, "AgADXQ", "расшифровка", 12); err != nil {
		t.Fatal(err)
	}
	if text, ok, err := st.Transcript(ctx, MessengerTelegram, "AgADXQ"); err != nil || !ok || text != "расшифровка" {
		t.Errorf("кэш после миграции: %q ok=%v err=%v", text, ok, err)
	}
	if ok, err := st.TryReserveASR(ctx, MessengerTelegram, 42, "2026-08-02", 30, 60); err != nil || !ok {
		t.Errorf("квота после миграции: ok=%v err=%v", ok, err)
	}
}

// TestMigrateV6ToV7 строит базу версии 6 и проверяет переезд подписок на пару
// kind/target. Главное здесь — сохранённые id: они уехали в payload кнопок «✖»
// в чужую историю чатов, и после миграции старое нажатие обязано снимать ту же
// подписку, а не соседнюю.
func TestMigrateV6ToV7(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{schemaSQL, migrateV2SQL, migrateV3SQL, migrateV4SQL, migrateV5SQL, migrateV6SQL} {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	seed := []string{
		`INSERT INTO subscriptions (id, messenger, keyword, user_id) VALUES (7, 'telegram', 'Граф', 42)`,
		`INSERT INTO subscriptions (id, messenger, keyword, user_id) VALUES (9, 'max', 'рюмк', 42)`,
		`INSERT INTO asr_transcripts (messenger, file_key, text, duration_sec, created_at)
		 VALUES ('telegram', 'AgADXQ', 'расшифровка', 12, '2026-08-02T00:00:00Z')`,
		`PRAGMA user_version = 6`,
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

	subs, err := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if err != nil || len(subs) != 1 {
		t.Fatalf("подписки после миграции: %+v %v", subs, err)
	}
	if subs[0].ID != 7 || subs[0].Kind != SubKeyword || subs[0].Target != "Граф" {
		t.Errorf("перенос подписки: %+v", subs[0])
	}
	// Чужой мессенджер не потерялся и не перепутал id.
	all, err := st.Subscriptions(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("все подписки: %+v %v", all, err)
	}
	for _, s := range all {
		if s.ID == 0 || s.Kind != SubKeyword {
			t.Errorf("подписка без id или вида: %+v", s)
		}
	}
	// v6-регресс: кэш расшифровок пережил пересборку соседней таблицы.
	if text, ok, err := st.Transcript(ctx, MessengerTelegram, "AgADXQ"); err != nil || !ok || text != "расшифровка" {
		t.Errorf("кэш ASR после v7: %q ok=%v err=%v", text, ok, err)
	}
}
