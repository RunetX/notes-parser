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

// migrateV4SQL — мессенджер-гейт: id сообщений выносятся из tg_*-колонок в
// таблицу message_targets с измерением messenger (телеграм, MAX, ...), а
// пользовательские таблицы получают колонку messenger. Id сообщений — TEXT:
// в Telegram это числа, в MAX — строковые mid. Старые tg_*-колонки остаются
// на один релиз денормализованным кэшем telegram-значений (write-through в
// SetTarget); дропнуть в v5.
const migrateV4SQL = `
CREATE TABLE message_targets (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger  TEXT    NOT NULL,          -- 'telegram' | 'max'
    kind       TEXT    NOT NULL,          -- 'note_post' | 'note_thread' | 'comment' | 'note_image'
    ref_id     TEXT    NOT NULL,          -- id заметки / комментария / иллюстрации (как TEXT)
    chat_ref   INTEGER,                   -- id канала/чата мессенджера (опц.; адаптер знает свой из конфига)
    message_id TEXT,                      -- id сообщения в мессенджере
    thread_id  TEXT,                      -- корень треда обсуждения (для note_thread)
    created_at TEXT    NOT NULL,
    UNIQUE (messenger, kind, ref_id)
);
CREATE INDEX idx_message_targets_thread ON message_targets(messenger, thread_id);
CREATE INDEX idx_message_targets_msg    ON message_targets(messenger, kind, message_id);

INSERT INTO message_targets (messenger, kind, ref_id, message_id, created_at)
SELECT 'telegram', 'note_post', id, CAST(tg_message_id AS TEXT), strftime('%Y-%m-%dT%H:%M:%SZ','now')
FROM notes WHERE tg_message_id IS NOT NULL;

INSERT INTO message_targets (messenger, kind, ref_id, thread_id, created_at)
SELECT 'telegram', 'note_thread', id, CAST(tg_thread_id AS TEXT), strftime('%Y-%m-%dT%H:%M:%SZ','now')
FROM notes WHERE tg_thread_id IS NOT NULL;

INSERT INTO message_targets (messenger, kind, ref_id, message_id, created_at)
SELECT 'telegram', 'comment', CAST(id AS TEXT), CAST(tg_message_id AS TEXT), strftime('%Y-%m-%dT%H:%M:%SZ','now')
FROM comments WHERE tg_message_id IS NOT NULL;

INSERT INTO message_targets (messenger, kind, ref_id, message_id, created_at)
SELECT 'telegram', 'note_image', CAST(id AS TEXT), CAST(tg_message_id AS TEXT), strftime('%Y-%m-%dT%H:%M:%SZ','now')
FROM note_images WHERE tg_message_id IS NOT NULL;

CREATE TABLE sessions_v4 (
    messenger  TEXT    NOT NULL DEFAULT 'telegram',
    user_id    INTEGER NOT NULL,
    cookies    TEXT    NOT NULL,
    valid      INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT    NOT NULL,
    last_ok_at TEXT,
    PRIMARY KEY (messenger, user_id)
);
INSERT INTO sessions_v4 (messenger, user_id, cookies, valid, updated_at, last_ok_at)
SELECT 'telegram', tg_user_id, cookies, valid, updated_at, last_ok_at FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_v4 RENAME TO sessions;

CREATE TABLE dialog_states_v4 (
    messenger  TEXT    NOT NULL DEFAULT 'telegram',
    user_id    INTEGER NOT NULL,
    state      TEXT    NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (messenger, user_id)
);
INSERT INTO dialog_states_v4 (messenger, user_id, state, updated_at)
SELECT 'telegram', tg_user_id, state, updated_at FROM dialog_states;
DROP TABLE dialog_states;
ALTER TABLE dialog_states_v4 RENAME TO dialog_states;

CREATE TABLE subscriptions_v4 (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger  TEXT    NOT NULL DEFAULT 'telegram',
    keyword    TEXT    NOT NULL,
    user_id    INTEGER NOT NULL,
    UNIQUE (messenger, keyword, user_id)
);
INSERT INTO subscriptions_v4 (messenger, keyword, user_id)
SELECT 'telegram', keyword, tg_user_id FROM subscriptions;
DROP TABLE subscriptions;
ALTER TABLE subscriptions_v4 RENAME TO subscriptions;

CREATE TABLE processed_replies_v4 (
    messenger    TEXT NOT NULL DEFAULT 'telegram',
    message_id   TEXT NOT NULL,
    processed_at TEXT NOT NULL,
    PRIMARY KEY (messenger, message_id)
);
INSERT INTO processed_replies_v4 (messenger, message_id, processed_at)
SELECT 'telegram', CAST(tg_message_id AS TEXT), processed_at FROM processed_replies;
DROP TABLE processed_replies;
ALTER TABLE processed_replies_v4 RENAME TO processed_replies;
`

// migrate накатывает недостающие миграции. Версия схемы — PRAGMA user_version;
// migrations[i] переводит схему на версию i+1, применяется по возрастанию.
func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL,    // v1 — базовая схема
		migrateV2SQL, // v2 — аватар автора заметки и иллюстрации
		migrateV3SQL, // v3 — флаг «комментарии закрыты»
		migrateV4SQL, // v4 — message_targets и измерение messenger (гейт MAX)
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
