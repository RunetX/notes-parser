-- Схема архива грабера v1. Отдельная БД от боевой (bot): исследовательский
-- дамп заметок с комментариями за годы, нормализованный для восстановления
-- социального графа «типажей». Версионируется через PRAGMA user_version.

-- users — типажи, дедуплицированные по числовому id анкеты. Данные обновляются
-- по правилу latest-wins (см. SaveGrab): непустое новое значение перезаписывает
-- старое, аватар/имя/возраст держим самыми свежими. first_seen фиксируется раз.
CREATE TABLE users (
    id          INTEGER PRIMARY KEY,            -- числовой id анкеты (author_id)
    name        TEXT NOT NULL DEFAULT '',
    age         TEXT NOT NULL DEFAULT '',
    profile_url TEXT NOT NULL DEFAULT '',
    avatar_url  TEXT NOT NULL DEFAULT '',
    first_seen  TEXT NOT NULL,                  -- ISO-8601 UTC: когда впервые записали
    last_seen   TEXT NOT NULL                   -- когда последний раз видели
);

CREATE TABLE notes (
    id              INTEGER PRIMARY KEY,             -- id заметки на сайте
    author_id       INTEGER REFERENCES users(id),    -- NULL — аноним
    text            TEXT NOT NULL DEFAULT '',
    images          TEXT NOT NULL DEFAULT '[]',       -- JSON-массив URL иллюстраций
    comments_closed INTEGER NOT NULL DEFAULT 0,
    published_at    TEXT,                             -- ISO-8601 UTC или NULL
    grabbed_at      TEXT NOT NULL
);
CREATE INDEX idx_notes_author ON notes(author_id);

-- comments — дерево через parent_id (0 = корень заметки). parent_id намеренно
-- без внешнего ключа: при частичной выгрузке (-max-pages) родитель может быть
-- ещё не сохранён, висячая ссылка — не ошибка целостности.
CREATE TABLE comments (
    id           INTEGER PRIMARY KEY,               -- id комментария на сайте
    note_id      INTEGER NOT NULL REFERENCES notes(id),
    parent_id    INTEGER NOT NULL DEFAULT 0,         -- 0 — корень; иначе id родителя
    author_id    INTEGER NOT NULL REFERENCES users(id),
    text         TEXT NOT NULL DEFAULT '',
    published_at TEXT
);
CREATE INDEX idx_comments_note   ON comments(note_id);
CREATE INDEX idx_comments_parent ON comments(parent_id);
CREATE INDEX idx_comments_author ON comments(author_id);
