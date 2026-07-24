package archive

import (
	"context"
	"database/sql"
)

// migrateV13SQL — пол анкеты. Собирается обходом профилей под сессией
// пользователя через мобильную версию (m.love.ngs.ru, sex из JSON
// dataFromBlade.layout); в БД пусто = ещё не обходили. SaveGrab колонку не
// трогает (upsert перечисляет только name/age/avatar), так что грабинг пол
// не затирает.
const migrateV13SQL = `
ALTER TABLE users ADD COLUMN gender TEXT NOT NULL DEFAULT '';
`

// SetUserGender проставляет пол анкеты ('male'|'female'|'' — снять). Возвращает
// число затронутых строк (0 — такой анкеты нет).
func (s *Store) SetUserGender(ctx context.Context, id int64, gender string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET gender = ? WHERE id = ?`, gender, id)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AccountsMissingGender — id анкет перечисленных личностей, у которых пол ещё не
// размечен (для точечного обхода когорты отчёта). Порядок — по активности
// (сначала болтливые), чтобы при обрыве успеть самое ценное.
func (s *Store) AccountsMissingGender(ctx context.Context, identities []string) ([]int64, error) {
	if len(identities) == 0 {
		return nil, nil
	}
	args := make([]any, len(identities))
	for i, id := range identities {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id FROM users u
		JOIN v_identity i ON i.user_id = u.id
		JOIN v_user_activity a ON a.id = u.id
		WHERE i.identity IN (`+placeholders(len(identities))+`) AND u.gender = ''
		ORDER BY a.comments DESC`, args...)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

// UsersMissingGender — id анкет без размеченного пола, самые активные первыми,
// не более limit (0 — все). Для сплошного обхода архива.
func (s *Store) UsersMissingGender(ctx context.Context, limit int) ([]int64, error) {
	q := `
		SELECT u.id FROM users u
		JOIN v_user_activity a ON a.id = u.id
		WHERE u.gender = '' ORDER BY a.comments DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

// GenderStats — сколько анкет с полом/без (для отчёта об обходе).
func (s *Store) GenderStats(ctx context.Context) (male, female, unknown int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  SUM(CASE WHEN gender = 'male'   THEN 1 ELSE 0 END),
		  SUM(CASE WHEN gender = 'female' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN gender = ''       THEN 1 ELSE 0 END)
		FROM users`).Scan(&male, &female, &unknown)
	return male, female, unknown, err
}

// scanIDs собирает столбец int64 из результата запроса.
func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ActiveNotesSince — число заметок, опубликованных каждой личностью за окно
// (published_at >= since). Покрывается idx_notes_published(published_at,author_id).
// Отдельно от ActiveCountsSince: отчёту нужна недельная активность именно
// заметками, а не суммой сообщений.
func (s *Store) ActiveNotesSince(ctx context.Context, since string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.identity, COUNT(*)
		FROM notes n JOIN v_identity i ON i.user_id = n.author_id
		WHERE n.author_id IS NOT NULL AND n.published_at >= ?
		GROUP BY i.identity`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var ident string
		var cnt int
		if err := rows.Scan(&ident, &cnt); err != nil {
			return nil, err
		}
		out[ident] = cnt
	}
	return out, rows.Err()
}
