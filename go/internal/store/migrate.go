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

// migrateV3SQL — флаг «комментарии закрыты»: сайт пометил заметку неактуальной
// («не актуальна» в ленте), новые комментарии на ней невозможны. Воркер такой
// заметки после финального дозабора комментариев уходит в архив досрочно, не
// дожидаясь недельного таймаута.
const migrateV3SQL = `
ALTER TABLE notes ADD COLUMN comments_closed INTEGER NOT NULL DEFAULT 0;
`

// migrate накатывает недостающие миграции. Версия схемы — PRAGMA user_version;
// migrations[i] переводит схему на версию i+1, применяется по возрастанию.
func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL,    // v1 — базовая схема
		migrateV2SQL, // v2 — аватар автора заметки и иллюстрации
		migrateV3SQL, // v3 — флаг «комментарии закрыты»
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("чтение user_version: %w", err)
	}
	for i, migration := range migrations {
		target := i + 1
		if version >= target {
			continue
		}
		if _, err := s.db.ExecContext(ctx, migration); err != nil {
			return fmt.Errorf("миграция v%d: %w", target, err)
		}
		// PRAGMA не биндит параметры; target — наш внутренний int, не ввод.
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", target)); err != nil {
			return err
		}
	}
	return nil
}
