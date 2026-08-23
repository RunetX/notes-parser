package platform

// Одна реплика — та, что нужна форме ответа, открывающейся без перезагрузки
// (web/replyform.go). Проверяется не «строка вернулась», а два правила, которые
// живут в самом SQL и подделкой не проверяются вовсе.

import (
	"context"
	"errors"
	"testing"
)

// Заметка стоит в условии не для скорости (хотя и для неё: без неё запрос шёл бы
// по первичному ключу через таблицу на 10,7 млн строк). Она же и граница:
// адрес формы ответа не должен становиться способом вытащить чужую реплику,
// подставив свою заметку.
func TestCommentViewByIDBelongsToItsNote(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestNote(t, p, 312812, 1493279, "Рио")
	ingestComment(t, p, 63207290, 312811, 1038894, 0)

	c, err := p.CommentViewByID(ctx, Viewer{}, 312811, 63207290)
	if err != nil {
		t.Fatalf("чтение реплики: %v", err)
	}
	if c.ID != 63207290 || c.NoteID != 312811 || c.Name() != "ник1038894" {
		t.Errorf("пришло не то: id=%d note=%d имя=%q", c.ID, c.NoteID, c.Name())
	}

	if _, err := p.CommentViewByID(ctx, Viewer{}, 312812, 63207290); !errors.Is(err, ErrNotFound) {
		t.Errorf("реплика отдалась под чужой заметкой: %v", err)
	}

	// Ник берётся ТЕКУЩИЙ, а не снимок: переименование меняет подпись везде,
	// включая «Ответ: …» в форме, — на этом держится и обезличивание.
	if err := p.SetNick(ctx, 1038894, "Пух"); err != nil {
		t.Fatalf("смена ника: %v", err)
	}
	if c, err = p.CommentViewByID(ctx, Viewer{}, 312811, 63207290); err != nil || c.Name() != "Пух" {
		t.Errorf("после переименования имя %q (%v)", c.Name(), err)
	}
}

// Скрытая реплика для читателя просто отсутствует — отвечать ей нельзя, и форма
// не должна открыться. Модератор видит её, как видит в дереве: он работает там,
// где читает.
func TestCommentViewByIDHidesWhatTheThreadHides(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()

	mod := moderator(t, p)
	ingestNote(t, p, 312811, 1493279, "Рио")
	ingestComment(t, p, 63207290, 312811, 1038894, 0)
	if err := p.HideSubject(ctx, mod, CommentSubject(63207290), CatSpam, "реклама"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}

	if _, err := p.CommentViewByID(ctx, Viewer{}, 312811, 63207290); !errors.Is(err, ErrNotFound) {
		t.Errorf("скрытая реплика видна читателю: %v", err)
	}
	if _, err := p.CommentViewByID(ctx, mod, 312811, 63207290); err != nil {
		t.Errorf("скрытая реплика не видна модератору: %v", err)
	}
}
