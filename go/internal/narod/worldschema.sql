-- Схема мира narod v1. Версионируется через PRAGMA user_version (см. world.go).
--
-- Отдельная база, а не таблицы в lovegw.db: реплей гоняется на машине
-- разработчика, где боевой базы нет вовсе, и КАЖДЫЙ его прогон заводит свой мир
-- (иначе калибровочные прогоны накапливали бы отношения в том же графе, что и
-- живая игра). Рантайм-тумблер службы при этом живёт НЕ здесь, а в settings
-- боевой базы: выключатель обязан работать и тогда, когда мир не открылся.

-- actors — все, кто в этом мире действует. Живые люди и ручные персонажи
-- владельца тоже здесь: отношения к ним копятся так же, иначе персонажи не
-- смогут их помнить.
CREATE TABLE actors (
    id               TEXT PRIMARY KEY,           -- slug карточки | h:<platform_user_id> | m:<имя>
    kind             TEXT NOT NULL,              -- persona | human | manual
    platform_user_id INTEGER,                    -- анкета на площадке (NULL — вне её)
    nick             TEXT NOT NULL,
    card_path        TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);
CREATE UNIQUE INDEX actors_platform_user ON actors(platform_user_id)
    WHERE platform_user_id IS NOT NULL;

-- journal — личная память персонажа. Пишется ДО публикации: реплика, ушедшая на
-- площадку, но не попавшая в журнал, сделала бы персонажа противоречащим
-- самому себе, а это ровно то, чего эмуляция не прощает.
CREATE TABLE journal (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id   TEXT NOT NULL REFERENCES actors(id),
    at         TEXT NOT NULL,
    kind       TEXT NOT NULL,                    -- comment | note | inner_event
    note_id    INTEGER,
    comment_id INTEGER,
    text       TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT ''          -- JSON, свободное поле службы
);
CREATE INDEX journal_actor ON journal(actor_id, id DESC);
CREATE INDEX journal_note  ON journal(note_id) WHERE note_id IS NOT NULL;

-- edges — граф отношений. Шкалы [-10..+10]; направленные, потому что симпатия
-- не взаимна по своей природе.
CREATE TABLE edges (
    src         TEXT NOT NULL REFERENCES actors(id),
    dst         TEXT NOT NULL REFERENCES actors(id),
    sympathy    REAL NOT NULL DEFAULT 0,
    irritation  REAL NOT NULL DEFAULT 0,
    familiarity REAL NOT NULL DEFAULT 0,         -- сколько раз вообще сталкивались
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (src, dst)
);

-- episodes — «счёты»: конкретные случаи со ссылками на реплики. Шкала говорит,
-- КАК персонажи относятся друг к другу, эпизод — ПОЧЕМУ; без второго «а помнишь»
-- взяться неоткуда.
CREATE TABLE episodes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    src         TEXT NOT NULL REFERENCES actors(id),
    dst         TEXT NOT NULL REFERENCES actors(id),
    at          TEXT NOT NULL,
    kind        TEXT NOT NULL,                   -- поддел | вступился | сцепились | digest
    summary     TEXT NOT NULL,
    comment_ids TEXT NOT NULL DEFAULT '',        -- JSON-массив
    note_id     INTEGER,
    compressed  INTEGER NOT NULL DEFAULT 0       -- 1 — сжатая выжимка старых эпизодов
);
CREATE INDEX episodes_pair ON episodes(src, dst, id DESC);

-- dice — брошенные монетки. Решение пишется ДО броска и ключом (актор, событие):
-- без этого пятнадцать процентов за десяток тактов превращаются в восемьдесят
-- (урок pulpit/reply.go).
CREATE TABLE dice (
    actor_id TEXT NOT NULL REFERENCES actors(id),
    event_id TEXT NOT NULL,
    p        REAL NOT NULL,
    roll     REAL NOT NULL,
    verdict  TEXT NOT NULL,                      -- come | skip
    reason   TEXT NOT NULL DEFAULT '',
    at       TEXT NOT NULL,
    PRIMARY KEY (actor_id, event_id)
);

-- plans — отложенный приход. Персонаж, решивший ответить через сорок минут,
-- обязан ответить через сорок минут и после рестарта демона, поэтому намерение
-- лежит в базе, а не в памяти. Переходы состояния — CAS, как у амвона.
CREATE TABLE plans (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id            TEXT NOT NULL REFERENCES actors(id),
    event_id            TEXT NOT NULL,
    note_id             INTEGER NOT NULL,
    reply_to_comment_id INTEGER NOT NULL DEFAULT 0,
    target_actor        TEXT NOT NULL DEFAULT '',
    due_at              TEXT NOT NULL,
    state               TEXT NOT NULL,           -- queued | posting | done | dropped
    reason              TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    UNIQUE (actor_id, event_id)
);
CREATE INDEX plans_due ON plans(state, due_at);

-- threads — что мир знает о ходе разговора. Нужна затуханию: вероятность новой
-- реплики падает с каждым кругом, иначе тред не кончается никогда.
CREATE TABLE threads (
    note_id          INTEGER PRIMARY KEY,
    state            TEXT NOT NULL,              -- live | closed
    rounds           INTEGER NOT NULL DEFAULT 0,
    persona_replies  INTEGER NOT NULL DEFAULT 0,
    last_activity_at TEXT NOT NULL
);

-- gen_runs — журнал генерации: что просили, что вышло, почему не опубликовали.
-- Дропы здесь — не отладочный лог, а требование DoD: «промолчал» и «сломалось»
-- обязаны различаться на глаз.
CREATE TABLE gen_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id     INTEGER,
    actor_id    TEXT NOT NULL REFERENCES actors(id),
    at          TEXT NOT NULL,
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    drafts      INTEGER NOT NULL DEFAULT 0,
    verdict     TEXT NOT NULL,                   -- posted | dropped | skipped
    drop_reason TEXT NOT NULL DEFAULT '',
    text_final  TEXT NOT NULL DEFAULT '',
    evals       TEXT NOT NULL DEFAULT ''         -- JSON
);
CREATE INDEX gen_runs_actor ON gen_runs(actor_id, id DESC);

-- llm_spend — расход по дням. Служба тратит деньги на каждую реплику, а
-- каскад «персонаж отвечает персонажу» умеет разгоняться сам, поэтому потолок
-- считается по фактам, а не по намерениям.
CREATE TABLE llm_spend (
    day        TEXT PRIMARY KEY,
    calls      INTEGER NOT NULL DEFAULT 0,
    in_tokens  INTEGER NOT NULL DEFAULT 0,
    out_tokens INTEGER NOT NULL DEFAULT 0
);

-- cursor — где остановились в журнале событий площадки.
CREATE TABLE cursor (
    k TEXT PRIMARY KEY,
    v INTEGER NOT NULL
);
