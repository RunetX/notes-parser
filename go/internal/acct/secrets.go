package acct

// Обслуживание шифрования кук сервисных аккаунтов: сводка и перешивка под
// текущий ключ (она же ротация). Зеркало store/secrets.go — разница только в
// таблице и контексте записи.

import (
	"context"
	"fmt"

	"lovegw/internal/secret"
)

// SecretStats — состояние шифрования в таблице accounts.
type SecretStats struct {
	Total      int
	Plain      int
	Encrypted  int
	Unreadable int
}

// SecretStatsOf считает, что лежит открыто, что зашифровано и что не
// открывается текущим ключом.
func (s *Store) SecretStatsOf(ctx context.Context) (SecretStats, error) {
	rows, err := s.rawCookies(ctx)
	if err != nil {
		return SecretStats{}, err
	}
	var st SecretStats
	for name, stored := range rows {
		st.Total++
		switch {
		case !secret.Encrypted(stored):
			st.Plain++
		case s.readable(name, stored):
			st.Encrypted++
		default:
			st.Unreadable++
		}
	}
	return st, nil
}

// Reencrypt перешивает куки под текущий ключ: открытое шифрует, зашифрованное
// старым ключом (old) перешифровывает. Идемпотентно. Возвращает число
// изменённых строк.
func (s *Store) Reencrypt(ctx context.Context, old secret.Key) (int, error) {
	if !s.key.Enabled() {
		return 0, fmt.Errorf("перешивка аккаунтов: ключ шифрования не задан")
	}
	rows, err := s.rawCookies(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	changed := 0
	for name, stored := range rows {
		aad := secret.AccountAAD(name)
		plain, need, err := secret.Reveal(aad, stored, s.key, old)
		if err != nil {
			return 0, fmt.Errorf("аккаунт %s: %w", name, err)
		}
		if !need {
			continue
		}
		sealed, err := s.key.Seal(aad, plain)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET cookies = ? WHERE name = ?`, sealed, name); err != nil {
			return 0, err
		}
		changed++
	}
	return changed, tx.Commit()
}

// rawCookies отдаёт колонку как есть (имя → содержимое), без расшифровки.
func (s *Store) rawCookies(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, cookies FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, cookies string
		if err := rows.Scan(&name, &cookies); err != nil {
			return nil, err
		}
		out[name] = cookies
	}
	return out, rows.Err()
}

func (s *Store) readable(name, stored string) bool {
	_, err := s.key.Open(secret.AccountAAD(name), stored)
	return err == nil
}
