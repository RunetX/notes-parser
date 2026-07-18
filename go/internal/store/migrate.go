package store

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// migrate накатывает недостающие миграции. Версия схемы — PRAGMA user_version.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}
	if version < 1 {
		if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("миграция v1: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return err
		}
	}
	return nil
}
