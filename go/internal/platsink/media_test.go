package platsink

import (
	"context"
	"errors"
	"testing"

	"lovegw/internal/platform"
)

// fakeSite отдаёт байты по ссылке; ссылки, которой нет в карте, «не существует»
// — так на сайте выглядит удалённая вместе с анкетой картинка.
type fakeSite struct {
	files map[string][]byte
	calls []string
}

func (f *fakeSite) FetchMedia(_ context.Context, url string) ([]byte, error) {
	f.calls = append(f.calls, url)
	if b, ok := f.files[url]; ok {
		return b, nil
	}
	return nil, errors.New("404")
}

// Обход добирает байты по ссылкам, оставшимся от бэкфилла, и не спотыкается о
// битую: одна снесённая картинка не должна стоить остальных.
func TestMediaSweepFillsMissing(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()

	good := "https://n1s1.hsmedia.ru/cache/love/avatars/good_100_100_c.jpg"
	dead := "https://n1s1.hsmedia.ru/cache/love/avatars/dead_100_100_c.jpg"
	pic := "https://n1s1.hsmedia.ru/images/1.jpg"

	n := note("312811", "1495073", "Птичка")
	n.AuthorAvatarURL = good
	seedNote(t, e.st, n)
	second := note("312812", "1495074", "Ромашка")
	second.AuthorAvatarURL = dead
	seedNote(t, e.st, second)
	if err := e.st.InsertNoteImage(ctx, n.ID, 0, pic); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rec.Once(ctx); err != nil {
		t.Fatal(err)
	}

	site := &fakeSite{files: map[string][]byte{good: testPNG(t), pic: testPNG(t)}}
	stats, err := NewMediaSweep(e.p, e.media, site, quietLog()).Once(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Avatars != 1 || stats.Images != 1 || stats.Failed != 1 {
		t.Errorf("итог обхода: %+v", stats)
	}

	view, err := e.p.NoteViewByID(ctx, platform.Viewer{}, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if view.Author.AvatarURL == "" {
		t.Error("аватар забран, но на заметке его не видно")
	}
	imgs, err := e.p.NoteImages(ctx, 312811)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 1 || imgs[0].URL == "" {
		t.Errorf("иллюстрация: %+v", imgs)
	}

	// Повторный проход не ходит на сайт вовсе: забранное больше не числится
	// недостающим, иначе обход каждый раз качал бы всё заново.
	site.calls = nil
	again, err := NewMediaSweep(e.p, e.media, site, quietLog()).Once(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if again.Avatars != 0 || again.Images != 0 {
		t.Errorf("повторный проход что-то забрал: %+v", again)
	}
	if len(site.calls) != 1 || site.calls[0] != dead {
		// Единственная ссылка, к которой обход вернётся, — та, что не отдалась.
		t.Errorf("обращения к сайту на втором проходе: %v", site.calls)
	}
}

// Силуэт по умолчанию в ngs_avatar_url не попадает, поэтому и обходу качать
// его не из чего — проверяем на уровне приёма, где это решается.
func TestPlaceholderNeverBecomesPendingMedia(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	n := note("312811", "1495073", "Птичка")
	n.AuthorAvatarURL = "/static/i/new/profile/female300px.png"
	seedNote(t, e.st, n)
	if _, err := e.rec.Once(ctx); err != nil {
		t.Fatal(err)
	}
	missing, err := e.p.MissingAvatars(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("силуэт попал в очередь на закачку: %+v", missing)
	}
}
