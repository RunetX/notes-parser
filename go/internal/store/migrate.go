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

// migrateV5SQL — личные сообщения сайта (talks). Миграция строго аддитивна:
// site-идентичность владельца сессии (id анкеты/паспорт/ник) + таблицы
// диалогов и сообщений talks. Ничего не пересоздаёт и не дропает (в т.ч.
// tg_*-колонки живы) — откат на прошлый бинарник работает на той же БД:
// старый код при user_version=5 просто не видит новых таблиц. Текст сообщений
// (talks_messages.text) при store_text=false остаётся пустым — приватность.
const migrateV5SQL = `
ALTER TABLE sessions ADD COLUMN site_profile_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN site_passport_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN site_nick        TEXT NOT NULL DEFAULT '';

CREATE TABLE talks_peers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger     TEXT    NOT NULL,            -- 'telegram' | 'max'
    owner_user_id INTEGER NOT NULL,            -- владелец сессии (чья переписка)
    passport_id   TEXT    NOT NULL,            -- собеседник в talks (/talks/<passport_id>)
    profile_id    TEXT    NOT NULL DEFAULT '', -- id анкеты /profile/<id>/, если известен
    nick          TEXT    NOT NULL DEFAULT '',
    avatar_url    TEXT    NOT NULL DEFAULT '',
    cursor_msg_id TEXT    NOT NULL DEFAULT '', -- последнее втянутое сообщение сайта
    last_event_at TEXT,                        -- для адаптивного интервала/сортировки
    created_at    TEXT    NOT NULL,
    UNIQUE (messenger, owner_user_id, passport_id)
);

CREATE TABLE talks_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    peer_id     INTEGER NOT NULL REFERENCES talks_peers(id),
    site_msg_id TEXT,                          -- NULL у исходящего до подтверждения
    direction   TEXT    NOT NULL CHECK (direction IN ('in','out')),
    text        TEXT    NOT NULL DEFAULT '',   -- '' при store_text=false
    media_url   TEXT    NOT NULL DEFAULT '',
    sent_at     TEXT,                          -- время по сайту
    created_at  TEXT    NOT NULL
);
CREATE INDEX idx_talks_messages_peer ON talks_messages(peer_id, id);
-- дедуп входящих; частичный индекс — несколько исходящих с NULL не конфликтуют
CREATE UNIQUE INDEX idx_talks_messages_site
    ON talks_messages(peer_id, site_msg_id) WHERE site_msg_id IS NOT NULL;
`

// migrateV6SQL — автораспознавание голосовых (ASR). Аддитивна: кэш расшифровок
// по стабильному id файла (в telegram это file_unique_id — переживает
// пересылку, в отличие от file_id) и суточный расход секунд аудио на
// пользователя. Ничего не дропает — откат бинарника на той же БД безопасен.
const migrateV6SQL = `
CREATE TABLE asr_transcripts (
    messenger    TEXT    NOT NULL,            -- 'telegram' | 'max'
    file_key     TEXT    NOT NULL,            -- стабильный id файла в мессенджере
    text         TEXT    NOT NULL,
    duration_sec INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT    NOT NULL,
    PRIMARY KEY (messenger, file_key)
);

CREATE TABLE asr_usage (
    messenger TEXT    NOT NULL,
    user_id   INTEGER NOT NULL,               -- автор голосового (или id канала)
    day       TEXT    NOT NULL,               -- 'YYYY-MM-DD' UTC: сброс в 07:00 Нск
    seconds   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (messenger, user_id, day)
);
`

// migrateV7SQL — типизированные подписки (эпик B): вместо одного keyword — пара
// kind + target. Пересборка таблицы, как в v4, но id переносятся ЯВНО: они
// уехали в payload кнопок «✖» («1:unsub:<id>») в чужую историю чатов, и после
// переезда старое нажатие обязано означать ту же подписку. Старый UNIQUE
// (messenger, keyword, user_id) строго сильнее нового при kind='keyword', так
// что конфликтов на переносе быть не может. Откат бинарника после v7 невозможен
// (старый код читает колонку keyword) — как и после v4.
const migrateV7SQL = `
CREATE TABLE subscriptions_v7 (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger TEXT    NOT NULL DEFAULT 'telegram',
    user_id   INTEGER NOT NULL,
    kind      TEXT    NOT NULL CHECK (kind IN ('keyword','author_notes','note_comments')),
    target    TEXT    NOT NULL,   -- слово / notes.author_id / notes.id
    UNIQUE (messenger, user_id, kind, target)
);
INSERT INTO subscriptions_v7 (id, messenger, user_id, kind, target)
SELECT id, messenger, user_id, 'keyword', keyword FROM subscriptions;
DROP TABLE subscriptions;
ALTER TABLE subscriptions_v7 RENAME TO subscriptions;

-- Кому слать: точечно на каждую новую заметку и на каждый новый комментарий.
-- Выборку по пользователю и подсчёт под предел обслуживает префикс UNIQUE.
CREATE INDEX idx_subscriptions_target ON subscriptions(kind, target);
-- Имя автора в строке /mysubs берётся из его последней заметки.
CREATE INDEX idx_notes_author ON notes(author_id);
`

// migrateV8SQL — выбор мессенджера доставки личных сообщений. Аддитивна: два
// поля у сессии — сам выбор и отметка, что про него уже спрашивали. Ничего не
// дропает, откат бинарника на той же БД безопасен (старый код колонок не
// видит и носит ЛС во все мессенджеры, как раньше).
const migrateV8SQL = `
ALTER TABLE sessions ADD COLUMN talks_delivery TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN talks_asked_at TEXT;
`

// migrateV9SQL — амвон (пакет pulpit): собственный комментарий владельца под
// каждой новой заметкой и редкие ответы тем, кто ему ответил. Аддитивна:
// откат бинарника на той же БД безопасен (старый код новых таблиц не видит).
//
// FK на notes у pulpit_comments НАМЕРЕННО нет, хотя foreign_keys(1) включён:
// амвон обходит ленту своим клиентом и видит заметку раньше, чем её вставит
// зеркало, — FK связал бы домены отказа ровно там, где мы их разводим.
//
// pulpit_replies ключуется чужой репликой: монетка «отвечать или нет» бросается
// ровно один раз на реплику, иначе 15 % за десяток циклов превращаются в 80 %.
//
// settings — рантайм-флаги. В проекте тумблеры жили только в конфиге, потому что
// ни один не менялся в рантайме; здесь выключает автоматика (в контейнер
// config.json монтируется снаружи, писать в него нельзя), а включает админ
// кнопкой, и выключение обязано пережить рестарт. ТОЛЬКО ФЛАГИ, НИКОГДА
// СЕКРЕТЫ: секреты живут в env и в шифрованных колонках sessions/accounts.
const migrateV9SQL = `
CREATE TABLE pulpit_comments (
    note_id    TEXT PRIMARY KEY,                 -- заметка сайта (без FK, см. выше)
    state      TEXT NOT NULL,                    -- queued|posting|posted|confirmed|missing|vanished|skipped|failed
    reason     TEXT NOT NULL DEFAULT '',         -- почему пропустили/не вышло
    form       TEXT NOT NULL DEFAULT '',         -- форма проповеди (укор, притча, …)
    text       TEXT NOT NULL DEFAULT '',         -- отправленный текст
    comment_id INTEGER,                          -- id своей реплики на сайте (после верификации)
    seen_at    TEXT NOT NULL,                    -- когда заметка занята: от него считается свежесть
    posted_at  TEXT,
    checked_at TEXT,
    checks     INTEGER NOT NULL DEFAULT 0,       -- сколько раз перечитывали тред в поисках своей реплики
    created_at TEXT NOT NULL
);
CREATE INDEX idx_pulpit_comments_state ON pulpit_comments(state, seen_at);

CREATE TABLE pulpit_replies (
    reply_to_id INTEGER PRIMARY KEY,             -- чужая реплика, на которую отвечаем
    note_id     TEXT NOT NULL,
    author_id   TEXT NOT NULL DEFAULT '',        -- её автор: одному человеку в заметке отвечаем один раз
    state       TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    text        TEXT NOT NULL DEFAULT '',
    comment_id  INTEGER,
    decided_at  TEXT NOT NULL,                   -- когда бросили монетку
    posted_at   TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX idx_pulpit_replies_note ON pulpit_replies(note_id);

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT ''          -- кто переключил: 'admin:<id>' или 'fuse'
);
`

// migrate накатывает недостающие миграции. Версия схемы — PRAGMA user_version;
// migrations[i] переводит схему на версию i+1, применяется по возрастанию.
func (s *Store) migrate(ctx context.Context) error {
	migrations := []string{
		schemaSQL,    // v1 — базовая схема
		migrateV2SQL, // v2 — аватар автора заметки и иллюстрации
		migrateV3SQL, // v3 — флаг «комментарии закрыты»
		migrateV4SQL, // v4 — message_targets и измерение messenger (гейт MAX)
		migrateV5SQL, // v5 — личные сообщения сайта (talks)
		migrateV6SQL, // v6 — кэш расшифровок и суточная квота ASR
		migrateV7SQL, // v7 — типизированные подписки (kind/target)
		migrateV8SQL, // v8 — выбор мессенджера доставки ЛС
		migrateV9SQL, // v9 — амвон: свои реплики под заметками и рантайм-флаги
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
