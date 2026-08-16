package store

// Круг активных комментаторов по зеркалу. Нужен наблюдателю за модерацией:
// его собственная история начинается с первого запуска, а здесь реплики лежат
// с начала мирроринга — то есть замолчавшего видно и тогда, когда он замолчал
// до того, как наблюдателя вообще включили.
//
// Ограничение, которое надо помнить, читая результат: зеркало ведёт заметки,
// пока они активны (неделя), поэтому реплики в старых тредах сюда не попадают.
// Человек, пишущий только в архивных заметках, выглядит замолчавшим.

import (
	"context"
	"time"

	"lovegw/internal/love"
)

// Commenter — сколько человек написал за окно и когда замолчал.
type Commenter struct {
	UserID      int64
	Nick        string // ник на момент последней реплики
	Comments    int
	LastComment time.Time
}

// Commenters возвращает авторов реплик за окно, у кого их не меньше
// minComments. Безанкетные (ссылка на профиль пустая) пропускаются: следить за
// присутствием такого нечем.
func (s *Store) Commenters(ctx context.Context, since time.Time, minComments int) ([]Commenter, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT author_link, author_name, COALESCE(published_at, created_at) AS at
          FROM comments
         WHERE COALESCE(published_at, created_at) >= ?
         ORDER BY at`, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byUser := map[int64]*Commenter{}
	for rows.Next() {
		var link, name, at string
		if err := rows.Scan(&link, &name, &at); err != nil {
			return nil, err
		}
		id := love.ProfileIDFromLink(link)
		if id == 0 {
			continue
		}
		c, ok := byUser[id]
		if !ok {
			c = &Commenter{UserID: id}
			byUser[id] = c
		}
		c.Comments++
		// Идём по возрастанию времени, поэтому последняя запись побеждает — и
		// ник берётся тот, под которым человек писал последний раз.
		c.Nick = name
		t, err := parseTime(at)
		if err != nil {
			return nil, err
		}
		c.LastComment = t.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Commenter, 0, len(byUser))
	for _, c := range byUser {
		if c.Comments >= minComments {
			out = append(out, *c)
		}
	}
	return out, nil
}
