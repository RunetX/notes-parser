-- Схема площадки v1. Версия — в таблице schema_migrations (см. migrate.go).
--
-- ПЛАН ИДЕНТИФИКАТОРОВ — фундамент, который ставится один раз.
--
--   [1 .. 1e11)      записи НГС: id строки РАВЕН id на сайте
--   [1e11 .. 2e11)   нативные: из последовательностей *_native_seq
--   [2e11 .. )       резерв под будущие источники
--
-- Отдельных колонок source/external_id нет намеренно: дискриминатор закодирован
-- в самом ключе, и его физически нельзя рассинхронизировать с содержимым. Отсюда
-- три следствия. Импорт архива (10,7 млн комментариев) — это COPY без единой
-- переразметки id: archive.db уже хранит notes.id/comments.id/users.id равными id
-- сайта, и внешние ключи сойдутся сами. Зеркальную запись невозможно спутать с
-- нативной. Адрес /n/312811 совпадает с love.ngs.ru/notes/312811/, поэтому старые
-- ссылки в каналах можно редиректить, ничего не теряя.
--
-- Запас: на сайте сегодня заметки ~3,2e5, комментарии ~6,4e7, анкеты ~1,5e6, а
-- поток — 830 тыс. комментариев в год. До 1e11 идти тысячелетия.

CREATE SEQUENCE users_native_seq    AS bigint START 100000000000 MINVALUE 100000000000 MAXVALUE 199999999999;
CREATE SEQUENCE notes_native_seq    AS bigint START 100000000000 MINVALUE 100000000000 MAXVALUE 199999999999;
CREATE SEQUENCE comments_native_seq AS bigint START 100000000000 MINVALUE 100000000000 MAXVALUE 199999999999;

-- ---------------------------------------------------------------- люди

-- users — и участники площадки, и «тени»: анкеты, которых мы видели только через
-- зеркало. Тень заводится на каждого автора, пришедшего с НГС, поэтому у любого
-- комментария всегда есть author_id, а вход человека — это не миграция данных, а
-- смена kind у уже существующей строки. Именно поэтому вход через анкету НГС
-- мгновенно делает прошлые реплики своими.
CREATE TABLE users (
    id             bigint PRIMARY KEY,
    -- nick — ТЕКУЩИЙ ник, latest-wins. Истории ников нет и не будет: снапшот ника
    -- на момент реплики — это и есть история. Переименование ретроспективно меняет
    -- отображение везде, и это же готовый механизм уточнения ПДн по требованию
    -- субъекта (152-ФЗ).
    nick           text   NOT NULL,
    avatar_sha     bytea,
    ngs_avatar_url text   NOT NULL DEFAULT '',   -- откуда взяли, для повторной закачки
    kind           smallint NOT NULL DEFAULT 0,  -- 0 тень, 1 участник, 2 служебный
    role           smallint NOT NULL DEFAULT 0,  -- 0 участник, 1 модератор, 2 админ
    -- hide_all — рубильник «скрыть все мои публикации». Отзыв согласия на
    -- распространение (ст. 10.1) исполняется немедленно, а не ручной модерацией.
    hide_all       boolean NOT NULL DEFAULT false,
    anonymized_at  timestamptz,                  -- обезличен по требованию субъекта
    banned_until   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz,
    CONSTRAINT users_id_band CHECK (id > 0 AND id < 200000000000)
);
COMMENT ON COLUMN users.kind IS '0 тень (видели только через зеркало), 1 участник (вошёл), 2 служебный';
COMMENT ON COLUMN users.role IS '0 участник, 1 модератор, 2 админ';

-- Ник не уникален: на НГС тёзки — обычное дело, а ретроспективное переименование
-- уникальность и не потянуло бы. Индекс нужен для поиска по нику при входе.
CREATE INDEX users_nick_lower ON users (lower(nick) text_pattern_ops);

-- identities — чем человек доказал, что анкета его. Строк на пользователя может
-- быть несколько: вошёл кодом в анкете, потом привязал мессенджер.
CREATE TABLE identities (
    kind        text   NOT NULL,   -- 'ngs_profile' | 'telegram' | 'max' | 'invite'
    external_id text   NOT NULL,   -- id анкеты / user_id мессенджера / id инвайта
    user_id     bigint NOT NULL REFERENCES users(id),
    method      text   NOT NULL,   -- 'profile_code' | 'talks_code' | 'bot_deeplink' | 'admin_invite'
    verified_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, external_id)
);
CREATE INDEX identities_user ON identities (user_id);

-- ---------------------------------------------------------------- заметки

CREATE TABLE notes (
    id              bigint PRIMARY KEY,
    -- author_id у нативной анонимной заметки — НАСТОЯЩИЙ автор, а не NULL: он
    -- должен видеть её в «моих» и мочь удалить (ст. 14 анонимные не различает),
    -- модерация — забанить анонимного флудера, а rate-limit — считать по человеку.
    -- Скрытие делается на границе сериализации (вид notes_public), а не хранением.
    -- NULL остаётся только у зеркальных анонимов НГС: там деанонимизировать нечем.
    author_id       bigint REFERENCES users(id),
    anonymous       boolean NOT NULL DEFAULT false,
    body            text   NOT NULL,
    status          smallint NOT NULL DEFAULT 0,
    comments_closed boolean NOT NULL DEFAULT false,
    comment_count   integer NOT NULL DEFAULT 0,   -- денормализация: лента без COUNT(*)
    published_at    timestamptz NOT NULL,
    -- published_exact = false означает, что в published_at лежит момент, когда
    -- заметку УВИДЕЛО зеркало: настоящего времени публикации сайт в ленте не даёт
    -- (в lovegw.db есть только first_seen_at).
    published_exact boolean NOT NULL DEFAULT true,
    last_comment_at timestamptz,
    edited_at       timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notes_id_band CHECK (id > 0 AND id < 200000000000),
    CONSTRAINT notes_anon_has_author CHECK (NOT anonymous OR id < 100000000000 OR author_id IS NOT NULL)
);
COMMENT ON COLUMN notes.status IS '0 виден, 1 скрыт автором, 2 скрыт модерацией, 3 обезличен';

CREATE INDEX notes_feed   ON notes (published_at DESC, id DESC) WHERE status = 0;
CREATE INDEX notes_author ON notes (author_id, published_at DESC) WHERE status = 0;

-- ---------------------------------------------------------------- комментарии

-- Дерево хранится ДВУМЯ рёбрами, и они честно названы, потому что надёжность
-- источников измерена и различна:
--   branch_root_id — корень ветки: ровно то, что даёт десктоп НГС (дерево там
--                    двухуровневое, parent_id указывает на корень);
--   reply_to_id    — настоящий адресат: из мобильного дерева (совпадение 92 %)
--                    либо из префикса «Ник, …» (48 %), что записано в reply_source.
--
-- Внешних ключей на оба ребра намеренно нет — как в internal/archive/schema.sql:
-- родителя могла снести модерация, и висячая ссылка тут не ошибка целостности, а
-- рабочий случай (комментарий просто становится корневым).
CREATE TABLE comments (
    id             bigint PRIMARY KEY,
    note_id        bigint NOT NULL REFERENCES notes(id),
    author_id      bigint REFERENCES users(id),
    -- author_display — единственное место, где приходится хранить снимок ника:
    -- у комментатора зеркала без ссылки на анкету (ProfileIDFromLink == 0)
    -- показать иначе нечего. Осознанное отступление от «истории ников нет»,
    -- локализованное в одной колонке.
    author_display text   NOT NULL DEFAULT '',
    anonymous      boolean NOT NULL DEFAULT false,
    -- body хранится БЕЗ префикса «Ник, »: он снимается в ребро на приёме. Иначе
    -- ник размазан по чужим телам и обезличивание по требованию субъекта
    -- невозможно. Наружу префикс дорисовывается из ТЕКУЩЕГО ника адресата.
    body           text   NOT NULL,
    branch_root_id bigint,
    reply_to_id    bigint,
    reply_source   smallint NOT NULL DEFAULT 0,
    -- path — материализованный путь: сегменты из id, дополненные нулями до 13
    -- знаков, через точку. id монотонно растут по времени и на НГС, и у нас,
    -- поэтому фиксированная ширина плюс побайтовое сравнение (COLLATE "C") дают
    -- «дерево, братья в хронологии» безо всякой сортировки в памяти, а страница
    -- треда берётся одним range-scan. Это не роскошь: треды на 848 комментариев
    -- роняют сам НГС.
    path           text   NOT NULL,
    depth          smallint NOT NULL DEFAULT 0,
    status         smallint NOT NULL DEFAULT 0,
    published_at   timestamptz NOT NULL,
    edited_at      timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT comments_id_band CHECK (id > 0 AND id < 200000000000),
    -- Границу правит 0002: depth < 12 — это одиннадцать уровней, а решение было
    -- про двенадцать. Здесь оставлено как есть: миграции не переписывают, иначе
    -- уже накатанная база разойдётся с файлом молча.
    CONSTRAINT comments_depth   CHECK (depth >= 0 AND depth < 12)
);
COMMENT ON COLUMN comments.reply_source IS '0 нет, 1 префикс «Ник, …», 2 мобильное дерево, 3 нативный ответ, 4 parent_id десктопа';
COMMENT ON COLUMN comments.status IS '0 виден, 1 скрыт автором, 2 скрыт модерацией, 3 обезличен';

CREATE INDEX comments_tree     ON comments (note_id, path COLLATE "C");
CREATE INDEX comments_flat     ON comments (note_id, id);
CREATE INDEX comments_author   ON comments (author_id, id DESC);
CREATE INDEX comments_reply_to ON comments (reply_to_id) WHERE reply_to_id IS NOT NULL;

-- ---------------------------------------------------------------- медиа

-- Байты лежат на диске (/data/media/<2 hex>/<sha256>), в БД только учёт. Отдаёт
-- их Caddy мимо Go: это самый жирный по трафику путь, а ядро на сервере одно.
CREATE TABLE media (
    sha256      bytea PRIMARY KEY,
    mime        text   NOT NULL,
    bytes       integer NOT NULL,
    width       integer,
    height      integer,
    source_url  text   NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    last_hit_at timestamptz NOT NULL DEFAULT now()   -- для дискового LRU
);

CREATE TABLE note_images (
    note_id  bigint   NOT NULL REFERENCES notes(id),
    position smallint NOT NULL,
    sha256   bytea    REFERENCES media(sha256),   -- NULL: URL знаем, байты ещё не забрали
    url      text     NOT NULL,
    PRIMARY KEY (note_id, position)
);

ALTER TABLE users ADD CONSTRAINT users_avatar_fk FOREIGN KEY (avatar_sha) REFERENCES media(sha256);

-- ---------------------------------------------------------------- вход

-- Сессия — непрозрачный токен, в базе только его sha256. Не JWT намеренно: бан и
-- «выйти со всех устройств» обязаны действовать мгновенно, а отзыв JWT требует
-- ровно того же обращения к БД, ради избавления от которого JWT и заводят.
CREATE TABLE web_sessions (
    id           bigserial PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id),
    token_sha    bytea  NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    ua           text NOT NULL DEFAULT '',
    -- ip_hmac — HMAC(ip, pepper), а не сам адрес: сырых IP нет нигде, включая
    -- логи. Чистится через 30 дней.
    ip_hmac      bytea
);
CREATE INDEX web_sessions_user ON web_sessions (user_id) WHERE revoked_at IS NULL;

-- Подтверждение владения анкетой НГС: человек вставляет код в поле «о себе», мы
-- читаем анкету анонимно с мобильного vhost и сверяем. Пароля не просим никогда.
CREATE TABLE auth_challenges (
    id          bigserial PRIMARY KEY,
    kind        text   NOT NULL,     -- 'ngs_profile_code' | 'ngs_talks_code'
    subject     text   NOT NULL,     -- id анкеты НГС
    code_sha    bytea  NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    attempts    smallint NOT NULL DEFAULT 0,
    verified_at timestamptz,
    ip_hmac     bytea
);
-- Живой челлендж на анкету ровно один: иначе перебор кодов становится дешевле.
CREATE UNIQUE INDEX auth_challenges_live ON auth_challenges (kind, subject) WHERE verified_at IS NULL;

-- Вход через бота: Telegram Login Widget непригоден (грузит скрипт с
-- заблокированного telegram.org), поэтому deep-link с nonce.
CREATE TABLE login_nonces (
    nonce_sha         bytea PRIMARY KEY,
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    messenger         text,
    messenger_user_id bigint,
    user_id           bigint REFERENCES users(id),
    confirmed_at      timestamptz
);

-- Инвайты — третий путь и единственный, переживающий смерть НГС: анкету могли
-- снести, а сайт может исчезнуть целиком. bind_user привязывает вошедшего к уже
-- существующей тени, и весь его зеркальный след прошлых лет становится своим.
CREATE TABLE invites (
    code_sha   bytea PRIMARY KEY,
    issued_by  bigint NOT NULL REFERENCES users(id),
    bind_user  bigint REFERENCES users(id),
    note       text   NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    used_by    bigint REFERENCES users(id)
);

-- ---------------------------------------------------------------- согласия

-- Тексты согласий версионируются: иначе нечем доказать, на что человек
-- соглашался в прошлом году.
CREATE TABLE consent_docs (
    kind         text NOT NULL,        -- 'processing' (ст. 9) | 'distribution' (ст. 10.1)
    version      integer NOT NULL,
    sha256       bytea NOT NULL,
    body         text NOT NULL,
    published_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, version)
);

-- Согласий ДВА, и они разные. Закон запрещает собирать согласие на
-- распространение в одном документе с общим и запрещает считать согласием
-- молчание или заранее проставленную галочку — отсюда отдельные строки, а не
-- один флаг. Отзыв distribution = users.hide_all немедленно.
CREATE TABLE consents (
    id         bigserial PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id),
    kind       text   NOT NULL,
    version    integer NOT NULL,
    granted_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz,
    ip_hmac    bytea,
    ua         text NOT NULL DEFAULT '',
    FOREIGN KEY (kind, version) REFERENCES consent_docs(kind, version)
);
CREATE INDEX consents_user ON consents (user_id, kind);

-- ---------------------------------------------------------------- модерация

CREATE TABLE reports (
    id           bigserial PRIMARY KEY,
    reporter_id  bigint NOT NULL REFERENCES users(id),
    subject_kind text   NOT NULL,      -- 'note' | 'comment' | 'user'
    subject_id   bigint NOT NULL,
    reason       text   NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz,
    resolved_by  bigint REFERENCES users(id),
    resolution   text   NOT NULL DEFAULT ''
);
CREATE INDEX reports_open ON reports (created_at) WHERE resolved_at IS NULL;

-- audit_log строго append-only: правки и удаления в нём запрещаются правами роли
-- приложения, а не вежливостью кода.
CREATE TABLE audit_log (
    id           bigserial PRIMARY KEY,
    at           timestamptz NOT NULL DEFAULT now(),
    actor_id     bigint REFERENCES users(id),
    action       text   NOT NULL,   -- hide | restore | ban | anonymize | consent_revoke | export
    subject_kind text   NOT NULL,
    subject_id   bigint NOT NULL,
    details      jsonb  NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_log_subject ON audit_log (subject_kind, subject_id, at DESC);

-- ---------------------------------------------------------------- приём зеркала

-- ingest_state — дисциплина фонового достраивания дерева по мобильной версии.
-- reply_scan_skip ставится тредам, которые мобильная страница не отдаёт: на 848
-- комментариях она воспроизводимо отвечает 500 после минуты ожидания.
CREATE TABLE ingest_state (
    note_id          bigint PRIMARY KEY REFERENCES notes(id),
    last_comment_id  bigint NOT NULL DEFAULT 0,
    reply_scan_at    timestamptz,
    reply_scan_fails smallint NOT NULL DEFAULT 0,
    reply_scan_skip  boolean NOT NULL DEFAULT false
);
CREATE INDEX ingest_state_due ON ingest_state (reply_scan_at NULLS FIRST) WHERE NOT reply_scan_skip;
