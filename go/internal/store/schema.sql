-- Схема lovegw v1. Версионируется через PRAGMA user_version (см. migrate.go).

CREATE TABLE notes (
    id              TEXT PRIMARY KEY,               -- id заметки на сайте
    author_id       TEXT NOT NULL DEFAULT '0',
    author_name     TEXT NOT NULL DEFAULT 'Анонимно',
    text            TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending','posted','seeded','archived')),
    tg_message_id   INTEGER,                        -- пост в канале
    tg_thread_id    INTEGER,                        -- автофорвард в группе (корень треда)
    first_seen_at   TEXT NOT NULL,                  -- ISO-8601 UTC
    last_comment_at TEXT,
    archived_at     TEXT
);
CREATE INDEX idx_notes_status ON notes(status);

CREATE TABLE comments (
    id            INTEGER PRIMARY KEY,              -- id комментария на сайте
    note_id       TEXT NOT NULL REFERENCES notes(id),
    author_name   TEXT NOT NULL,
    author_age    TEXT NOT NULL DEFAULT '',
    author_link   TEXT NOT NULL DEFAULT '',
    avatar_url    TEXT NOT NULL DEFAULT '',
    published_at  TEXT,
    text          TEXT NOT NULL,
    tg_message_id INTEGER,                          -- сообщение в группе обсуждения
    created_at    TEXT NOT NULL
);
CREATE INDEX idx_comments_note ON comments(note_id);
CREATE INDEX idx_comments_tg   ON comments(tg_message_id);

CREATE TABLE sessions (
    tg_user_id INTEGER PRIMARY KEY,
    cookies    TEXT NOT NULL,                       -- JSON-массив кук (вместо pickle)
    valid      INTEGER NOT NULL DEFAULT 1,          -- 0 = истекла, ждём повторный /login
    updated_at TEXT NOT NULL,
    last_ok_at TEXT
);

CREATE TABLE subscriptions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    keyword    TEXT NOT NULL,
    tg_user_id INTEGER NOT NULL,
    UNIQUE (keyword, tg_user_id)
);

CREATE TABLE dialog_states (
    tg_user_id INTEGER PRIMARY KEY,
    state      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE processed_replies (
    tg_message_id INTEGER PRIMARY KEY,
    processed_at  TEXT NOT NULL
);
