package digest

// Источник данных выпуска.
//
// До 22.08.2026 выпуск считался прямо по SQLite, и это было верно ровно пока
// весь разговор шёл на НГС: туда пишет ТОЛЬКО зеркало. С площадкой источник
// разошёлся с предметом — заметки и ответы, написанные здесь, в `notes` и
// `comments` не попадают вовсе, а выпуск публикуется ЗДЕСЬ же. Сводка про
// сообщество, показанная сообществу, обязана считаться по тому, что это
// сообщество видит вокруг себя.
//
// Отсюда интерфейс: у выпуска два источника (SQLite зеркала и Postgres
// площадки), и список запросов — это исчерпывающий ответ на вопрос «что
// дайджест знает о базе». Своими типами, а не строками store, он говорит по
// той же причине, по какой у веб-морды свой `Store`: идентичность человека в
// SQLite — ссылка на анкету, а на площадке — номер строки, и общий тип
// заставил бы одну из сторон врать.

import (
	"context"
	"time"
)

// RecordHorizon — насколько вглубь смотрят сравнительные рекорды.
//
// Год, а не «вся история», и это не экономия запроса. На площадке лежит весь
// архив с 2009-го: «самый длинный тред за всё время» установлен в 2013 году и
// не будет побит никогда, то есть рубрика превращается в еженедельное «рекорд
// не побит». Год — это сезон, внутри которого сравнение и правда что-то
// говорит читателю; заодно агрегат идёт по обозримому куску 10,7-миллионной
// таблицы.
const RecordHorizon = 52 * 7 * 24 * time.Hour

// Note — заметка глазами выпуска.
type Note struct {
	ID          string
	Author      string // идентичность автора; пусто — аноним или без анкеты
	AuthorName  string
	Text        string
	PublishedAt time.Time
}

// Comment — комментарий глазами выпуска. Время уже разрешено источником: у
// зеркала это published_at сайта с фолбэком на момент вставки.
type Comment struct {
	ID          int64
	NoteID      string
	Author      string // идентичность автора; пусто — анкеты нет
	AuthorName  string
	Text        string
	PublishedAt time.Time
}

// CommenterSeen — активность комментатора в окне и его прошлое до окна.
type CommenterSeen struct {
	Author        string
	Name          string
	InWindow      int
	FirstInWindow time.Time
	PrevSeenAt    time.Time // последний комментарий до окна; zero — новичок
}

// AuthorSeen — активность автора заметок в окне и его прошлое до окна.
type AuthorSeen struct {
	Author        string
	Name          string
	NotesInWindow int
	PrevNoteAt    time.Time // последняя заметка до окна; zero — раньше не писал
}

// NoteTotals — итоги обсуждения заметки за горизонт сравнения.
type NoteTotals struct {
	NoteID      string
	PublishedAt time.Time
	Comments    int
	Commenters  int
	FirstAt     time.Time
	LastAt      time.Time
}

// Source — что выпуск спрашивает у базы. Только чтение; окно везде
// (start, end] — start исключительно, end (слот выпуска) включительно.
type Source interface {
	// CommentsBetween — комментарии окна, упорядоченные по заметке и времени.
	CommentsBetween(ctx context.Context, start, end time.Time) ([]Comment, error)
	// NotesByIDs — шапки заметок по списку id: комментарии окна живут и на
	// заметках старше окна.
	NotesByIDs(ctx context.Context, ids []string) (map[string]Note, error)
	// NotesPublishedBetween — заметки, появившиеся в окне.
	NotesPublishedBetween(ctx context.Context, start, end time.Time) ([]Note, error)
	// ActiveNotesSince — заметки с комментарием после since: «обсуждение ещё
	// живо» к моменту выпуска.
	ActiveNotesSince(ctx context.Context, since time.Time) ([]Note, error)
	// CommenterHistory — комментаторы окна с их прошлым до окна.
	CommenterHistory(ctx context.Context, start, end time.Time) ([]CommenterSeen, error)
	// NoteAuthorHistory — авторы заметок окна с их прошлым до окна.
	NoteAuthorHistory(ctx context.Context, start, end time.Time) ([]AuthorSeen, error)
	// NoteTotals — итоги обсуждений за горизонт (заметки, появившиеся после
	// since), в порядке появления.
	NoteTotals(ctx context.Context, since time.Time) ([]NoteTotals, error)
	// PeakCommentHour — самый плотный календарный час одного треда за горизонт:
	// начало часа (UTC), заметка, число комментариев. n = 0 — считать нечего.
	PeakCommentHour(ctx context.Context, since time.Time) (hourStart time.Time, noteID string, n int, err error)
	// ProfileURL — адрес анкеты автора; пусто — анкеты нет (нативный участник
	// площадки, аноним). Знает об этом источник, а не выпуск: «кто такой
	// автор» — это его понятие.
	ProfileURL(author string) string
}
