package digest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"lovegw/internal/store"
)

// fakePub — фейковый приёмник выпуска.
type fakePub struct {
	name   string
	links  map[string]string // threadID → deep-link ("" — фолбэк)
	posts  []string
	failAt int // 1-based номер части, на которой падать; 0 — не падать
}

func (f *fakePub) Name() string { return f.name }

func (f *fakePub) PostChannelHTML(_ context.Context, html string) (string, error) {
	if f.failAt > 0 && len(f.posts)+1 == f.failAt {
		return "", errors.New("сеть моргнула")
	}
	f.posts = append(f.posts, html)
	return fmt.Sprintf("m%d", len(f.posts)), nil
}

func (f *fakePub) ThreadLink(threadID string) string { return f.links[threadID] }

func TestResolveLinks(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// Тред заметки 1 известен, заметка 2 не отправлялась, у 3 тред без ссылки.
	if err := st.SetTarget(ctx, "tg", store.TargetNoteThread, "1", "", "555"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTarget(ctx, "tg", store.TargetNoteThread, "3", "", "999"); err != nil {
		t.Fatal(err)
	}
	p := &fakePub{name: "tg", links: map[string]string{"555": "https://t.me/c/x/555"}}
	d := Draft{Sections: [][]string{
		{"<b>Шапка</b>", "тред {note:1|раз} и {note:2|два}"},
		{"фолбэк {note:3|три}"},
	}}
	blocks, err := ResolveLinks(ctx, st, d, p, "https://t3h.ru/")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 || !blocks[0].NewSection || blocks[1].NewSection || !blocks[2].NewSection {
		t.Fatalf("границы секций: %+v", blocks)
	}
	if want := `<a href="https://t.me/c/x/555">раз</a>`; !strings.Contains(blocks[1].Text, want) {
		t.Errorf("ссылка на тред: %q", blocks[1].Text)
	}
	// Фолбэк — страница ПЛОЩАДКИ: ссылок на НГС проект не ставит нигде
	// (27.08.2026), а у зеркальной строки id равен id на сайте.
	if want := `<a href="https://t3h.ru/n/2">два</a>`; !strings.Contains(blocks[1].Text, want) {
		t.Errorf("фолбэк без треда: %q", blocks[1].Text)
	}
	if want := `<a href="https://t3h.ru/n/3">три</a>`; !strings.Contains(blocks[2].Text, want) {
		t.Errorf("фолбэк при недоступном deep-link: %q", blocks[2].Text)
	}
}

// Без площадки фолбэку взяться неоткуда: подпись остаётся текстом, а не
// ссылкой в никуда. Это ровно тот случай, ради которого прямая публикация в
// каналы и живёт — работа БЕЗ площадки.
func TestResolveLinksWithoutOurBaseKeepsPlainText(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	d := Draft{Sections: [][]string{{"фолбэк {note:3|три}"}}}
	blocks, err := ResolveLinks(ctx, st, d, &fakePub{name: "tg"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].Text; got != "фолбэк три" {
		t.Errorf("подпись без адреса: %q", got)
	}
}

func TestPublishResumable(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// Два абзаца по ~2000 видимых рун — выпуск гарантированно в двух частях.
	d := Draft{Sections: [][]string{
		{"<b>Один</b>", strings.Repeat("а", 2000)},
		{"<b>Два</b>", strings.Repeat("б", 2000)},
	}}

	// Первая попытка падает на второй части.
	p := &fakePub{name: "tg", failAt: 2}
	sent, err := Publish(ctx, st, p, d, "2026-W31", "https://love.test")
	if err == nil || sent != 1 {
		t.Fatalf("ожидался сбой после первой части: sent=%d err=%v", sent, err)
	}
	if _, _, done, _ := st.Target(ctx, "tg", store.TargetDigest, "2026-W31"); done {
		t.Fatal("головная запись не должна появиться до всех частей")
	}

	// Повтор докатывает только недостающую часть.
	p.failAt = 0
	sent, err = Publish(ctx, st, p, d, "2026-W31", "https://love.test")
	if err != nil || sent != 1 {
		t.Fatalf("докат: sent=%d err=%v", sent, err)
	}
	if len(p.posts) != 2 {
		t.Fatalf("всего постов: %d", len(p.posts))
	}
	if !strings.Contains(p.posts[1], "(2/2)") || strings.Contains(p.posts[1], "аа") {
		t.Errorf("вторая часть: %q…", p.posts[1][:80])
	}
	msgID, _, done, _ := st.Target(ctx, "tg", store.TargetDigest, "2026-W31")
	if !done || msgID != "m1" {
		t.Fatalf("головная запись: done=%v msg=%q", done, msgID)
	}

	// Третий вызов — выпуск уже опубликован, ничего не шлём.
	sent, err = Publish(ctx, st, p, d, "2026-W31", "https://love.test")
	if err != nil || sent != 0 || len(p.posts) != 2 {
		t.Fatalf("повторная публикация: sent=%d err=%v posts=%d", sent, err, len(p.posts))
	}
}

func TestPublishSingleMessage(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	p := &fakePub{name: "max"}
	d := Draft{Sections: [][]string{{"<b>Короткий выпуск</b>", "текст"}}}
	sent, err := Publish(ctx, st, p, d, "2026-W31", "https://love.test")
	if err != nil || sent != 1 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	if strings.Contains(p.posts[0], "(1/1)") {
		t.Errorf("одиночное сообщение не нумеруется: %q", p.posts[0])
	}
}
