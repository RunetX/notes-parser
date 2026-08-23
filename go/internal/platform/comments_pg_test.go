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

// ПЕРЕЕЗД строки. Дерево перестраивается под открытой страницей: зеркало знает
// адресата только по обращению «Ник, …» и разрешает его в ПОСЛЕДНЮЮ реплику
// этого человека, а настоящее ребро приносит обход мобильной версии. По границе
// id переехавшая строка не приезжает никогда — id у неё прежний.
//
// Замер 23.08.2026, боевая заметка 313058: Kowalski 63238879 был нарисован
// страницей на глубине 4 (угаданный адресат — последняя реплика Аматы), обход
// переставил его на 2, и следующая же строка треда, его собственный ответ,
// приехала с глубиной 3 — МЕНЬШЕ, чем у родителя строкой выше. Ветка на экране
// выросла не там, а обновление показывало правильную.
func TestCommentsMovedCarriesReparented(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 63238869, 313058, 1, 0)        // Амата, корень треда
	ingestComment(t, p, 63238877, 313058, 1, 63238869) // её же реплика ниже
	ingestComment(t, p, 63238879, 313058, 2, 63238877) // Kowalski — адресат УГАДАН

	after, err := p.ThreadFreshAfter(ctx, 313058)
	if err != nil {
		t.Fatalf("граница: %v", err)
	}
	// Граница переездов не бывает пустой даже в заметке, где не переезжало
	// ничего: пустая означает «переездов не носим», и ПЕРВЫЙ же переезд прошёл
	// бы мимо открытой страницы.
	if !after.Moved.On() {
		t.Fatal("граница переездов пуста у живой заметки")
	}
	got, _, err := p.CommentsMoved(ctx, Viewer{}, 313058, after.Moved, FreshLimit)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("на только что нарисованной странице уже есть переезды: %v", ids(got))
	}

	// Обход мобильной версии: на самом деле Kowalski отвечал корню треда.
	st, err := p.ApplyReplyTree(ctx, 313058, map[int64]int64{
		63238869: 0, 63238877: 63238869, 63238879: 63238869})
	if err != nil {
		t.Fatalf("дерево: %v", err)
	}
	if st.Edges != 1 {
		t.Fatalf("переставлено рёбер %d, ожидалось 1", st.Edges)
	}

	got, next, err := p.CommentsMoved(ctx, Viewer{}, 313058, after.Moved, FreshLimit)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(got) != 1 || got[0].ID != 63238879 {
		t.Fatalf("переезды принесли %v, ожидалась одна строка 63238879", ids(got))
	}
	// Строка приезжает С НОВЫМ МЕСТОМ и целиком: вместе с ребром у неё сменились
	// глубина и подпись адресата, поэтому переставить её на странице, не
	// перерисовав, значило бы оставить на ней прежнего собеседника.
	if got[0].Depth != 2 {
		t.Errorf("глубина переехавшей %d, ожидалась 2", got[0].Depth)
	}
	if got[0].ReplyTo == nil || got[0].ReplyTo.CommentID != 63238869 {
		t.Errorf("адресат переехавшей %+v, ожидался 63238869", got[0].ReplyTo)
	}
	// И граница сдвинулась: второй раз того же не приносят.
	again, _, err := p.CommentsMoved(ctx, Viewer{}, 313058, next, FreshLimit)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("переезд приехал второй раз: %v", ids(again))
	}
}

// Порция переездов режется потолком, и продолжиться она обязана С СЕРЕДИНЫ:
// одна транзакция обхода штампует все свои строки ОДНИМ временем, поэтому
// граница — пара «время, id», а не время. Границей по одному времени хвост
// переезда терялся бы молча.
func TestCommentsMovedResumesInsideOneScan(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 100, 313058, 1, 0)
	for _, id := range []int64{200, 300, 400, 500} {
		ingestComment(t, p, id, 313058, 2, 100) // зеркало свалило всех под корень
	}
	after, err := p.ThreadFreshAfter(ctx, 313058)
	if err != nil {
		t.Fatalf("граница: %v", err)
	}
	// На самом деле это цепочка, и переезжают трое — одной транзакцией, то есть
	// с одной отметкой времени.
	if _, err := p.ApplyReplyTree(ctx, 313058, map[int64]int64{
		100: 0, 200: 100, 300: 200, 400: 300, 500: 400}); err != nil {
		t.Fatalf("дерево: %v", err)
	}

	first, next, err := p.CommentsMoved(ctx, Viewer{}, 313058, after.Moved, 2)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(first) != 2 || first[0].ID != 300 || first[1].ID != 400 {
		t.Fatalf("первая порция %v, ожидались 300 и 400", ids(first))
	}
	rest, _, err := p.CommentsMoved(ctx, Viewer{}, 313058, next, 2)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(rest) != 1 || rest[0].ID != 500 {
		t.Fatalf("хвост порции %v, ожидалась строка 500", ids(rest))
	}
}

// Скрытое модератором в переезды не идёт — как и в добор, и на страницу. Но
// граница обязана через него ПЕРЕСТУПИТЬ: упрись она в скрытую строку, добор
// приносил бы пустоту до самой перезагрузки.
func TestCommentsMovedStepsOverHidden(t *testing.T) {
	p := testPlatform(t)
	ctx := context.Background()
	ingestNote(t, p, 313058, 175869, "Паноптикум")
	ingestComment(t, p, 100, 313058, 1, 0)
	ingestComment(t, p, 200, 313058, 2, 100)
	ingestComment(t, p, 300, 313058, 3, 100)
	after, err := p.ThreadFreshAfter(ctx, 313058)
	if err != nil {
		t.Fatalf("граница: %v", err)
	}
	mod := moderator(t, p)
	if err := p.HideSubject(ctx, mod, CommentSubject(300), CatFlood, "проверка"); err != nil {
		t.Fatalf("скрытие: %v", err)
	}
	if _, err := p.ApplyReplyTree(ctx, 313058, map[int64]int64{
		100: 0, 200: 100, 300: 200}); err != nil {
		t.Fatalf("дерево: %v", err)
	}

	got, next, err := p.CommentsMoved(ctx, Viewer{}, 313058, after.Moved, FreshLimit)
	if err != nil {
		t.Fatalf("переезды: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("скрытая строка приехала переездом: %v", ids(got))
	}
	if !next.At.After(after.Moved.At) {
		t.Errorf("граница застряла на скрытой строке: %+v", next)
	}
	// Модератору она видна — там же, где он читает.
	got, _, err = p.CommentsMoved(ctx, mod, 313058, after.Moved, FreshLimit)
	if err != nil {
		t.Fatalf("переезды модератора: %v", err)
	}
	if len(got) != 1 || got[0].ID != 300 {
		t.Errorf("модератор не увидел переезд скрытого: %v", ids(got))
	}
}
