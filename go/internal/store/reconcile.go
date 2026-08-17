package store

// Чтение зеркальных данных для сверки с площадкой (эпик E, Ш2). Направление
// одностороннее: SQLite остаётся источником правды для всего, что пришло с НГС,
// а Postgres площадки — его потребитель. Поэтому здесь только выборки.

import "context"

// CommentTally — сколько комментариев заметки лежит в зеркале и какой у них
// максимальный id. Пара, а не один max: `pull -full` дотягивает старые реплики
// ПОСЛЕ новых, и сверка по одному лишь max их бы не заметила.
type CommentTally struct {
	Count int
	MaxID int64
}

// CommentTallies — по счётчику на каждую заметку с комментариями. Один запрос
// на всё зеркало: сверка бегает раз в пять минут, и ходить по заметкам
// поодиночке она не должна.
func (s *Store) CommentTallies(ctx context.Context) (map[string]CommentTally, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT note_id, COUNT(*), MAX(id) FROM comments GROUP BY note_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]CommentTally)
	for rows.Next() {
		var (
			id string
			t  CommentTally
		)
		if err := rows.Scan(&id, &t.Count, &t.MaxID); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// CommentsForNote — все комментарии заметки по возрастанию id, то есть в
// порядке появления. Порядок обязателен: по нему считается и место в дереве
// (адресат всегда старше ответа), и обращение «Ник, …» — кто из уже
// написавших отзывается на этот ник.
func (s *Store) CommentsForNote(ctx context.Context, noteID string) ([]Comment, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+commentColumns+`
		FROM comments WHERE note_id = ? ORDER BY id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectComments(rows)
}

// NoteImageCounts — сколько иллюстраций у каждой заметки. Картинку автор
// дописывает и к уже опубликованной заметке (сайт отправляет её на
// премодерацию и возвращает), поэтому сверять приходится не только новые.
func (s *Store) NoteImageCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT note_id, COUNT(*) FROM note_images GROUP BY note_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ClosedNoteIDs — заметки, закрытые сайтом для комментариев. Отметка приходит
// уже после публикации (сайт ставит её через минуты), а у приёмника зеркала нет
// события про неё вовсе — значит переносить её может только сверка.
func (s *Store) ClosedNoteIDs(ctx context.Context) (map[string]bool, error) {
	return s.queryIDs(ctx, `SELECT id FROM notes WHERE comments_closed = 1`)
}

// NoteImagesFor — иллюстрации заметки в порядке показа, независимо от того,
// ушли ли они куда-нибудь (в отличие от UnsentNoteImagesFor).
func (s *Store) NoteImagesFor(ctx context.Context, noteID string) ([]NoteImage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, note_id, position, url
		FROM note_images WHERE note_id = ? ORDER BY position, id`, noteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var imgs []NoteImage
	for rows.Next() {
		var img NoteImage
		if err := rows.Scan(&img.ID, &img.NoteID, &img.Position, &img.URL); err != nil {
			return nil, err
		}
		imgs = append(imgs, img)
	}
	return imgs, rows.Err()
}
