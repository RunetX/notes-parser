package store

// Обслуживание шифрования сессионных кук: сводка по базе и перешивка записей
// (она же ротация ключа). Сами куки эти методы наружу не отдают — только
// счётчики: команда `lovegw secrets` печатает статистику, а не секреты.

import (
	"context"
	"fmt"

	"lovegw/internal/secret"
)

// SecretStats — состояние шифрования в таблице.
type SecretStats struct {
	Total      int
	Plain      int // лежит открыто
	Encrypted  int // зашифровано и читается текущим ключом
	Unreadable int // зашифровано, но текущим ключом не открывается
}

// SessionSecretStats считает, что в sessions лежит открыто, что зашифровано и
// что не открывается текущим ключом. Unreadable > 0 — либо ключ не тот, либо
// его вовсе не подали: запускать демон в таком виде нельзя.
func (s *Store) SessionSecretStats(ctx context.Context) (SecretStats, error) {
	all, err := sessionRows(ctx, s.db)
	if err != nil {
		return SecretStats{}, err
	}
	var st SecretStats
	for _, r := range all {
		st.Total++
		switch {
		case !secret.Encrypted(r.cookies):
			st.Plain++
		case s.readable(r):
			st.Encrypted++
		default:
			st.Unreadable++
		}
	}
	return st, nil
}

// readable — открывается ли запись текущим ключом.
func (s *Store) readable(r sessionRow) bool {
	_, err := s.key.Open(secret.SessionAAD(r.messenger, r.userID), r.cookies)
	return err == nil
}

// ReencryptSessions перешивает куки под текущий ключ хранилища: открытые
// записи шифрует, зашифрованные старым ключом (old) — перешифровывает.
// Идемпотентно: записи, уже лежащие под текущим ключом, не трогаются, поэтому
// повтор после сбоя безопасен. Возвращает число изменённых строк.
//
// Строка, которую не открыть ни текущим ключом, ни old, останавливает всю
// операцию: молча пропустить её — значит потерять сессию человека при
// следующей ротации.
func (s *Store) ReencryptSessions(ctx context.Context, old secret.Key) (int, error) {
	if !s.key.Enabled() {
		return 0, fmt.Errorf("перешивка сессий: ключ шифрования не задан")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	all, err := sessionRows(ctx, tx)
	if err != nil {
		return 0, err
	}

	changed := 0
	for _, r := range all {
		aad := secret.SessionAAD(r.messenger, r.userID)
		plain, ok, err := secret.Reveal(aad, r.cookies, s.key, old)
		if err != nil {
			return 0, fmt.Errorf("сессия %s/%d: %w", r.messenger, r.userID, err)
		}
		if !ok {
			continue // уже под текущим ключом
		}
		sealed, err := s.key.Seal(aad, plain)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET cookies = ? WHERE messenger = ? AND user_id = ?`,
			sealed, r.messenger, r.userID); err != nil {
			return 0, err
		}
		changed++
	}
	return changed, tx.Commit()
}

// sessionRow — строка sessions глазами перешивки: ключ записи и то, что лежит
// в колонке cookies (открыто или шифротекстом).
type sessionRow struct {
	messenger string
	userID    int64
	cookies   string
}

// sessionRows вычитывает строки целиком до правок: курсор и UPDATE по той же
// таблице живут на одном соединении (db.SetMaxOpenConns(1)).
func sessionRows(ctx context.Context, q querier) ([]sessionRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT messenger, user_id, cookies FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.messenger, &r.userID, &r.cookies); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	return all, rows.Err()
}
