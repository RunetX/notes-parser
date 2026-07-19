package store

import (
	"context"
	_ "embed"
	"fmt"
)

//go:embed schema.sql
var schemaSQL string

// migrateV2SQL — аватар автора заметки и её иллюстрации. Аватар живого
// человека показываем у поста заметки, иллюстрации — первым сообщением в
// треде. Медиа отдаёт CDN love.ngs.ru, поэтому их байты качаем сами.
const migrateV2SQL = `
ALTER TABLE notes ADD COLUMN author_avatar_url TEXT NOT NULL DEFAULT '';
CREATE TABLE note_images (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    note_id       TEXT NOT NULL REFERENCES notes(id),
    position      INTEGER NOT NULL DEFAULT 0,        -- порядок в заметке
    url           TEXT NOT NULL,                     -- полноразмерная картинка
    tg_message_id INTEGER,                           -- сообщение в треде (NULL = ещё нет)
    UNIQUE (note_id, url)
);
CREATE INDEX idx_note_images_note ON note_images(note_id);
`

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
	if version < 2 {
		if _, err := s.db.ExecContext(ctx, migrateV2SQL); err != nil {
			return fmt.Errorf("миграция v2: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
			return err
		}
	}
	return nil
}
