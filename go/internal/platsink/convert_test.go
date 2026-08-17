package platsink

import (
	"testing"
	"time"

	"lovegw/internal/platform"
	"lovegw/internal/store"
)

var seen = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// Заметка с живым автором: id анкеты становится тенью, время публикации —
// НЕточным (сайт его не отдаёт, у нас лежит момент первого показа).
func TestNoteFrom(t *testing.T) {
	in, err := noteFrom(store.Note{
		ID: "312811", AuthorID: "1495073", AuthorName: "Птичка",
		AuthorAvatarURL: "https://n1s1.hsmedia.ru/cache/love/avatars/abc_100_100_c.jpg",
		Text:            "текст заметки", FirstSeenAt: seen, CommentsClosed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != 312811 || in.Author.ID != 1495073 || in.Author.Nick != "Птичка" {
		t.Errorf("автор и id: %+v", in)
	}
	if in.Anonymous {
		t.Error("заметка с анкетой считается анонимной")
	}
	if in.PublishedExact || !in.PublishedAt.Equal(seen) {
		t.Errorf("время публикации: %v, exact=%v", in.PublishedAt, in.PublishedExact)
	}
	if !in.CommentsClosed {
		t.Error("потеряна отметка «не актуальна»")
	}
}

// Аноним НГС приходит без анкеты (author_id = «0»), и деанонимизировать его
// нечем: тени нет, ник «Анонимно» никуда не попадает.
func TestNoteFromAnonymous(t *testing.T) {
	in, err := noteFrom(store.Note{
		ID: "312812", AuthorID: "0", AuthorName: "Анонимно",
		AuthorAvatarURL: "/static/i/new/profile/anonymous300px.png",
		Text:            "аноним", FirstSeenAt: seen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !in.Anonymous || in.Author.ID != 0 {
		t.Errorf("аноним разобран как %+v", in.Author)
	}
	if in.Author.AvatarURL != "" {
		t.Errorf("силуэт по умолчанию попал в ссылку аватара: %q", in.Author.AvatarURL)
	}
}

func TestNoteFromRejectsNonNumericID(t *testing.T) {
	if _, err := noteFrom(store.Note{ID: "notes/312811"}); err == nil {
		t.Error("нечисловой id заметки принят молча")
	}
}

// Найденный адресат превращает обращение в ребро и убирает его из тела; не
// найденный — оставляет текст как есть, иначе «кому отвечали» исчезло бы совсем.
func TestCommentFromPrefix(t *testing.T) {
	c := store.Comment{
		ID: 63207431, NoteID: "312811", AuthorName: "Мавр",
		AuthorLink: "https://love.ngs.ru/profile/1331380/",
		Text:       "Птичка, согласен", PublishedAt: seen,
	}
	with := commentFrom(312811, c, 63207290)
	if with.Body != "согласен" || with.ReplyToID != 63207290 || with.ReplySource != platform.ReplyPrefix {
		t.Errorf("адресат найден, но реплика разобрана как %+v", with)
	}
	if with.Author.ID != 1331380 {
		t.Errorf("анкета автора не снята со ссылки: %d", with.Author.ID)
	}

	without := commentFrom(312811, c, 0)
	if without.Body != c.Text || without.ReplySource != platform.ReplyNone {
		t.Errorf("адресат не найден, но текст изменён: %+v", without)
	}
}

// Комментатор без ссылки на анкету — рабочий случай: тени нет, показывать его
// будет снимок ника (author_display).
func TestCommentFromWithoutProfile(t *testing.T) {
	in := commentFrom(312811, store.Comment{
		ID: 63207432, AuthorName: "Гость", Text: "просто текст", CreatedAt: seen,
	}, 0)
	if in.Author.ID != 0 || in.Author.Nick != "Гость" {
		t.Errorf("безанкетный комментатор: %+v", in.Author)
	}
	// Времени сайт не дал — берём момент, когда реплику увидело зеркало:
	// колонка в Postgres NOT NULL, «неизвестно» в ней не выразить.
	if !in.PublishedAt.Equal(seen) {
		t.Errorf("время реплики: %v", in.PublishedAt)
	}
}
