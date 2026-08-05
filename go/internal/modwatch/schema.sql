-- Схема наблюдателя за модерацией v1. Отдельная БД и от боевой (lovegw.db), и
-- от архива (archive.db): архив — снимок «что уцелело», а здесь пишется то,
-- чего в снимке нет по определению — момент, когда объект ИСЧЕЗ, и кто в этот
-- момент был на площадке. Версионируется через PRAGMA user_version.

-- notes — состояние заметки на последний опрос. gone_at ставится, когда заметка
-- пропала из ленты, оставаясь внутри охвата (см. watch.go): это удаление, а не
-- уход за край страницы.
CREATE TABLE notes (
    id              INTEGER PRIMARY KEY,          -- id заметки на сайте
    author_id       INTEGER NOT NULL DEFAULT 0,   -- 0 — аноним
    author_name     TEXT    NOT NULL DEFAULT '',
    text_head       TEXT    NOT NULL DEFAULT '',  -- начало текста: чтобы событие читалось глазами
    images          INTEGER NOT NULL DEFAULT 0,   -- число иллюстраций (картинку ставит модератор)
    comments_closed INTEGER NOT NULL DEFAULT 0,
    published_at    TEXT,                         -- ISO-8601 UTC, известна только из шапки треда
    first_seen      TEXT    NOT NULL,
    last_seen       TEXT    NOT NULL,
    last_polled     TEXT,                         -- когда последний раз опрашивали тред
    gone_at         TEXT                          -- когда исчезла (NULL — на месте)
);
CREATE INDEX idx_notes_live ON notes(gone_at, last_polled);

-- comments — то же для комментариев. Охват ограничен свежими страницами треда
-- (см. Watcher.Depth), поэтому «исчез» фиксируется только внутри охвата.
CREATE TABLE comments (
    id           INTEGER PRIMARY KEY,
    note_id      INTEGER NOT NULL,
    author_id    INTEGER NOT NULL DEFAULT 0,
    author_name  TEXT    NOT NULL DEFAULT '',
    text_head    TEXT    NOT NULL DEFAULT '',
    published_at TEXT    NOT NULL,
    first_seen   TEXT    NOT NULL,
    last_seen    TEXT    NOT NULL,
    gone_at      TEXT
);
CREATE INDEX idx_comments_note      ON comments(note_id, gone_at);
CREATE INDEX idx_comments_published ON comments(published_at, author_id);

-- users — ники на момент наблюдения: смена ника подтверждается модерацией,
-- поэтому переименование тоже событие.
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL
);

-- events — зафиксированное действие. Точный момент неизвестен: он лежит между
-- prev_seen_at (опрос, когда объект был ещё на месте) и detected_at (опрос,
-- когда его не стало). Отчёт считает присутствие по этому интервалу.
CREATE TABLE events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    kind         TEXT    NOT NULL,               -- note_gone|comment_gone|image_added|comments_closed|comments_opened|note_published|nick_changed
    ref_id       INTEGER NOT NULL,               -- id заметки/комментария/анкеты
    note_id      INTEGER NOT NULL DEFAULT 0,
    prev_seen_at TEXT    NOT NULL,
    detected_at  TEXT    NOT NULL,
    details      TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX        idx_events_time   ON events(detected_at, kind);
CREATE UNIQUE INDEX idx_events_unique ON events(kind, ref_id, detected_at);
