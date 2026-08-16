package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestInsertNoteDuplicateIgnored(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	n := Note{ID: "1", Text: "текст", Status: StatusSeeded, FirstSeenAt: time.Now()}

	added, err := st.InsertNote(ctx, n)
	if err != nil || !added {
		t.Fatalf("первая вставка: added=%v err=%v", added, err)
	}
	added, err = st.InsertNote(ctx, n)
	if err != nil || added {
		t.Fatalf("дубль должен игнорироваться: added=%v err=%v", added, err)
	}

	ids, err := st.KnownNoteIDs(ctx)
	if err != nil || !ids["1"] || len(ids) != 1 {
		t.Fatalf("KnownNoteIDs: %v, %v", ids, err)
	}
}

func TestNoteAvatarRoundtrip(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	want := "https://cdn/avatars/x.jpg"
	if _, err := st.InsertNote(ctx, Note{
		ID: "1", Text: "т", Status: StatusPosted,
		AuthorAvatarURL: want, FirstSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.NoteByID(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthorAvatarURL != want {
		t.Errorf("author_avatar_url: got %q, want %q", got.AuthorAvatarURL, want)
	}
}

// TestScanNoteBrokenTimeIsError: битая строка времени в БД — ошибка чтения, а
// не Note с нулевым FirstSeenAt (нулевое время означало бы мгновенную
// архивацию заметки через ShouldArchive).
func TestScanNoteBrokenTimeIsError(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE notes SET first_seen_at = 'мусор' WHERE id = 'n1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteByID(ctx, "n1"); err == nil {
		t.Fatal("битое first_seen_at должно давать ошибку чтения")
	} else if errors.Is(err, ErrNotFound) {
		t.Fatalf("битое время замаскировано под ErrNotFound: %v", err)
	}
}

// TestSingleRowErrorIsNotNotFound закрепляет контракт однострочных выборок:
// ошибка БД не маскируется под ErrNotFound. Вызывающие различают эти случаи
// (bridge молча отбрасывает «не найдено», но обязан логировать ошибку БД).
func TestSingleRowErrorIsNotNotFound(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close() // закрытая БД: любой вызов обязан вернуть ошибку, но не ErrNotFound

	calls := map[string]func() error{
		"NoteByID":        func() error { _, err := st.NoteByID(ctx, "1"); return err },
		"NoteByThread":    func() error { _, err := st.NoteByThread(ctx, MessengerTelegram, "t1"); return err },
		"CommentByTarget": func() error { _, err := st.CommentByTarget(ctx, MessengerTelegram, "m1"); return err },
		"TalkPeerByID":    func() error { _, err := st.TalkPeerByID(ctx, 1); return err },
		"TalkMessageByID": func() error { _, err := st.TalkMessageByID(ctx, 1); return err },
		"PeerByDeliveredPM": func() error {
			_, err := st.PeerByDeliveredPM(ctx, MessengerTelegram, "m1")
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s: закрытая БД должна давать ошибку", name)
			continue
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("%s: ошибка БД замаскирована под ErrNotFound: %v", name, err)
		}
	}
}

func TestNoteImagesUnsentAndMark(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i, u := range []string{"https://cdn/a.jpg", "https://cdn/b.jpg"} {
		if err := st.InsertNoteImage(ctx, "n1", i, u); err != nil {
			t.Fatal(err)
		}
	}
	// Повторная вставка того же URL идемпотентна.
	if err := st.InsertNoteImage(ctx, "n1", 0, "https://cdn/a.jpg"); err != nil {
		t.Fatal(err)
	}
	imgs, err := st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 2 {
		t.Fatalf("ожидалось 2 неотправленных иллюстрации, получено %d", len(imgs))
	}
	if imgs[0].URL != "https://cdn/a.jpg" {
		t.Errorf("порядок нарушен: %q", imgs[0].URL)
	}
	if err := st.SetTarget(ctx, MessengerTelegram, TargetNoteImage,
		strconv.FormatInt(imgs[0].ID, 10), "555", ""); err != nil {
		t.Fatal(err)
	}
	imgs, _ = st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1")
	if len(imgs) != 1 || imgs[0].URL != "https://cdn/b.jpg" {
		t.Fatalf("после отметки должна остаться одна иллюстрация b, получено %+v", imgs)
	}
}

func TestDeleteNoteCascade(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertComment(ctx, Comment{ID: 1, NoteID: "n1", AuthorName: "А", Text: "к", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertNoteImage(ctx, "n1", 0, "https://cdn/a.jpg"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteNote(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NoteByID(ctx, "n1"); err == nil {
		t.Error("заметка должна быть удалена")
	}
	if imgs, _ := st.UnsentNoteImagesFor(ctx, MessengerTelegram, "n1"); len(imgs) != 0 {
		t.Errorf("иллюстрации должны быть удалены: %+v", imgs)
	}
	ids, _ := st.CommentIDs(ctx, "n1")
	if len(ids) != 0 {
		t.Errorf("комментарии должны быть удалены: %v", ids)
	}
}

func TestSubscriptionsAddListRemove(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	word := func(messenger, target string, userID int64) Subscription {
		return Subscription{Messenger: messenger, UserID: userID, Kind: SubKeyword, Target: target}
	}
	added, err := st.AddSubscription(ctx, word(MessengerTelegram, "Граф", 42))
	if err != nil || !added {
		t.Fatalf("первая подписка: added=%v err=%v", added, err)
	}
	added, _ = st.AddSubscription(ctx, word(MessengerTelegram, "Граф", 42))
	if added {
		t.Error("дубль подписки должен игнорироваться")
	}
	if _, err := st.AddSubscription(ctx, word(MessengerTelegram, "Барон", 42)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSubscription(ctx, word(MessengerTelegram, "Граф", 99)); err != nil {
		t.Fatal(err)
	}

	kws, err := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(kws) != 2 || kws[0].Target != "Барон" || kws[1].Target != "Граф" { // ORDER BY label
		t.Errorf("подписки пользователя 42: %v", kws)
	}

	removed, err := st.RemoveSubscription(ctx, MessengerTelegram, 42, SubKeyword, "Граф")
	if err != nil || !removed {
		t.Fatalf("удаление: removed=%v err=%v", removed, err)
	}
	removed, _ = st.RemoveSubscription(ctx, MessengerTelegram, 42, SubKeyword, "Граф")
	if removed {
		t.Error("повторное удаление должно вернуть false")
	}
	// Подписка другого пользователя на «Граф» не затронута.
	kws, _ = st.SubscriptionsByUser(ctx, MessengerTelegram, 99)
	if len(kws) != 1 || kws[0].Target != "Граф" {
		t.Errorf("подписки пользователя 99: %v", kws)
	}
	if kws[0].ID == 0 {
		t.Error("выборка по пользователю должна заполнять id: по нему отписывает кнопка")
	}

	// Снятие по id: чужую подписку по нему снять нельзя.
	foreign := kws[0].ID
	if _, ok, err := st.RemoveSubscriptionByID(ctx, MessengerTelegram, 42, foreign); err != nil || ok {
		t.Errorf("id чужой подписки не должен срабатывать: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := st.RemoveSubscriptionByID(ctx, MessengerMax, 99, foreign); ok {
		t.Error("id из другого мессенджера не должен срабатывать")
	}
	sub, ok, err := st.RemoveSubscriptionByID(ctx, MessengerTelegram, 99, foreign)
	if err != nil || !ok || sub.Target != "Граф" || sub.Kind != SubKeyword {
		t.Errorf("снятие по id: %+v ok=%v err=%v", sub, ok, err)
	}
	if _, ok, _ := st.RemoveSubscriptionByID(ctx, MessengerTelegram, 99, foreign); ok {
		t.Error("повторное снятие по id должно вернуть false")
	}
}

// TestSubscriptionsAllKinds — три вида живут рядом и не путаются: одна и та же
// цель у разных видов — разные подписки, а подписи целей приходят из notes.
func TestSubscriptionsAllKinds(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	if _, err := st.InsertNote(ctx, Note{
		ID: "312886", AuthorID: "515996", AuthorName: "Ягода",
		Text: "Купила вчера кота", Status: StatusPosted, FirstSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []Subscription{
		{Messenger: MessengerTelegram, UserID: 42, Kind: SubKeyword, Target: "312886"},
		{Messenger: MessengerTelegram, UserID: 42, Kind: SubAuthorNotes, Target: "515996"},
		{Messenger: MessengerTelegram, UserID: 42, Kind: SubNoteComments, Target: "312886"},
	} {
		if added, err := st.AddSubscription(ctx, sub); err != nil || !added {
			t.Fatalf("подписка %+v: added=%v err=%v", sub, added, err)
		}
	}

	subs, err := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if err != nil || len(subs) != 3 {
		t.Fatalf("подписки: %+v %v", subs, err)
	}
	// Порядок: слова → авторы → заметки.
	if subs[0].Kind != SubKeyword || subs[1].Kind != SubAuthorNotes || subs[2].Kind != SubNoteComments {
		t.Errorf("порядок видов: %+v", subs)
	}
	if subs[0].Label != "312886" {
		t.Errorf("подпись слова — само слово: %q", subs[0].Label)
	}
	if subs[1].Label != "Ягода" {
		t.Errorf("подпись автора из notes: %q", subs[1].Label)
	}
	if subs[2].Label != "Ягода: Купила вчера кота" {
		t.Errorf("подпись заметки из notes: %q", subs[2].Label)
	}

	// Неизвестная цель — пустая подпись, а не пропавшая строка.
	if _, err := st.AddSubscription(ctx, Subscription{
		Messenger: MessengerTelegram, UserID: 43, Kind: SubNoteComments, Target: "999999",
	}); err != nil {
		t.Fatal(err)
	}
	orphan, _ := st.SubscriptionsByUser(ctx, MessengerTelegram, 43)
	if len(orphan) != 1 || orphan[0].Label != "" {
		t.Errorf("подписка на неизвестную заметку: %+v", orphan)
	}

	// Точечная выборка подписчиков цели — ей пользуется рассылка по автору.
	authors, err := st.SubscribersByTarget(ctx, MessengerTelegram, SubAuthorNotes, "515996")
	if err != nil || len(authors) != 1 || authors[0].UserID != 42 {
		t.Errorf("подписчики автора: %+v %v", authors, err)
	}
	if none, _ := st.SubscribersByTarget(ctx, MessengerMax, SubAuthorNotes, "515996"); len(none) != 0 {
		t.Errorf("чужой мессенджер не должен попадать в выборку: %+v", none)
	}

	// Архивация заметки снимает подписки на её комментарии — и только их.
	n, err := st.RemoveNoteSubscriptions(ctx, "312886")
	if err != nil || n != 1 {
		t.Fatalf("снятие подписок на заметку: n=%d err=%v", n, err)
	}
	left, _ := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if len(left) != 2 {
		t.Errorf("после архивации должны остаться слово и автор: %+v", left)
	}
}

// TestSubscriptionLimit — предел общий на все виды и держится в сторе, чтобы
// его не обошли параллельные нажатия.
func TestSubscriptionLimit(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)

	for i := range SubscriptionLimit {
		sub := Subscription{
			Messenger: MessengerTelegram, UserID: 42,
			Kind: SubKeyword, Target: fmt.Sprintf("слово-%d", i),
		}
		if added, err := st.AddSubscription(ctx, sub); err != nil || !added {
			t.Fatalf("подписка %d: added=%v err=%v", i, added, err)
		}
	}
	over := Subscription{Messenger: MessengerTelegram, UserID: 42, Kind: SubKeyword, Target: "лишнее"}
	if _, err := st.AddSubscription(ctx, over); !errors.Is(err, ErrSubscriptionLimit) {
		t.Fatalf("подписка сверх предела: err=%v", err)
	}
	subs, _ := st.SubscriptionsByUser(ctx, MessengerTelegram, 42)
	if len(subs) != SubscriptionLimit {
		t.Errorf("отказ не должен оставлять строку: %d", len(subs))
	}
	// Повтор уже имеющейся подписки на пределе — не ошибка: ничего не растёт.
	dup := Subscription{Messenger: MessengerTelegram, UserID: 42, Kind: SubKeyword, Target: "слово-0"}
	if added, err := st.AddSubscription(ctx, dup); err != nil || added {
		t.Errorf("дубль на пределе: added=%v err=%v", added, err)
	}
	// Предел на пользователя, а не на таблицу.
	other := Subscription{Messenger: MessengerTelegram, UserID: 43, Kind: SubKeyword, Target: "своё"}
	if added, err := st.AddSubscription(ctx, other); err != nil || !added {
		t.Errorf("подписка другого пользователя: added=%v err=%v", added, err)
	}
}

func TestMarkNoteCommentsClosed(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, err := st.InsertNote(ctx, Note{ID: "n1", Text: "т", Status: StatusPosted, FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// Новая заметка открыта для комментариев.
	n, err := st.NoteByID(ctx, "n1")
	if err != nil || n.CommentsClosed {
		t.Fatalf("новая заметка не должна быть закрыта: closed=%v err=%v", n.CommentsClosed, err)
	}
	changed, err := st.MarkNoteCommentsClosed(ctx, "n1")
	if err != nil || !changed {
		t.Fatalf("первая отметка должна сработать: changed=%v err=%v", changed, err)
	}
	// Повторная отметка идемпотентна и не логируется повторно.
	changed, _ = st.MarkNoteCommentsClosed(ctx, "n1")
	if changed {
		t.Error("повторная отметка должна вернуть false (уже закрыта)")
	}
	n, err = st.NoteByID(ctx, "n1")
	if err != nil || !n.CommentsClosed {
		t.Fatalf("после отметки заметка закрыта: closed=%v err=%v", n.CommentsClosed, err)
	}
}

func TestUpsertSessionOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	now := time.Now()

	if err := st.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"old"}]`, now); err != nil {
		t.Fatal(err)
	}
	// Повторный /login заменяет куки той же строкой sessions.
	if err := st.UpsertSession(ctx, MessengerTelegram, 42, `[{"name":"new"}]`, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertNote(ctx, Note{ID: "7", Text: "т", Status: StatusPosted,
		FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Повторное открытие: миграция не падает, данные на месте.
	st, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids, err := st.KnownNoteIDs(ctx)
	if err != nil || !ids["7"] {
		t.Fatalf("данные не пережили переоткрытие: %v, %v", ids, err)
	}
}
