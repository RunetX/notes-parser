-- Личная переписка (эпик L). ЭТА МИГРАЦИЯ И ЕСТЬ ПЕРЕХОД В 149-ФЗ.
--
-- Девятнадцатью строками выше по времени, в шапке 0012, было написано прямо: в
-- events нет ни одной колонки со свободным текстом от участника, потому что
-- «одна новая колонка „текст" превратит уведомления в личные сообщения
-- незаметно для всех, включая того, кто её добавит; отсутствие такой колонки
-- делает это правкой, которую видно в diff». Вот эта правка. Её видно.
--
-- ПОВОД. Сообщество переехало на t3h.ru, а личка осталась на НГС: люди,
-- познакомившиеся ЗДЕСЬ, могут написать друг другу только там, где у части из
-- них анкета уже мертва (у владельца площадки — с 10.09.2026), либо обменявшись
-- контактом на виду у всего треда. Решение владельца 11.09.2026: делаем полную
-- переписку и принимаем обязанности организатора распространения информации.
--
-- ЦЕНА НАЗВАНА ЗАРАНЕЕ И ПРИНЯТА. Уведомление РКН; хранение содержания шесть
-- месяцев и сведений о приёме-передаче год НА ТЕРРИТОРИИ РОССИИ (п. 3 ч. 1
-- ст. 10.1 149-ФЗ); выдача уполномоченным органам по запросу. Сроки в
-- platform/mail.go — не наша осторожность, а закон, и они одновременно пол и
-- потолок: раньше не даёт 149-ФЗ, дольше не даёт 152-ФЗ.
--
-- СКВОЗНОГО ШИФРОВАНИЯ НЕТ, и это не недоделка. Переписка, которую оператор
-- прочесть не умеет, от требования выдать не освобождает — она делает его
-- невыполнимым, то есть превращает защиту участников в штраф оператору. Здесь
-- тела лежат открытыми, и сказано об этом прямо в тексте согласия
-- (consents/talks.v1.txt): копия базы — это копия переписки.
--
-- ГРАНИЦА ПЕРЕНЕСЕНА ВИДИМО, а не размыта. Колонка с текстом появилась ЗДЕСЬ, в
-- своей таблице со своей шапкой; events получает ссылку (dialog_id), как
-- получал её на заметку и на реплику, и текста письма не видит по-прежнему —
-- ни колонкой, ни выдержкой (см. notificationColumns: у письма note_id и
-- comment_id пусты, и left(coalesce(...)) выходит пустым сам собой).

-- Переписка пары.
--
-- Ключ — НОРМАЛИЗОВАННАЯ пара, и это решение, а не вкус. CHECK (lo_id < hi_id)
-- вместе с уникальным индексом даёт РОВНО ОДНУ строку на пару при любом порядке
-- отправителя и получателя — то есть два встречных первых письма не заводят двух
-- переписок, — и заодно делает невозможной переписку с самим собой БЕЗ ЕДИНОЙ
-- ПРОВЕРКИ В КОДЕ. Тот же приём, которым единственность двойника держит база, а
-- не проверка: двух одновременно нажавших проверка не разводит.
CREATE TABLE mail_dialogs (
    id       bigserial PRIMARY KEY,
    lo_id    bigint NOT NULL REFERENCES users(id),
    hi_id    bigint NOT NULL REFERENCES users(id),
    -- started_by — кто написал первым. По нему считается потолок ПЕРВЫХ писем
    -- незнакомцам: рассылка отличается от разговорчивости не числом писем, а
    -- числом новых собеседников, и меряется она здесь, а не в mail_messages.
    started_by      bigint NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_message_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT mail_pair_ordered CHECK (lo_id < hi_id)
);

COMMENT ON TABLE mail_dialogs IS
    'переписка пары участников; ключ — нормализованная пара (lo_id < hi_id)';

CREATE UNIQUE INDEX mail_dialogs_pair ON mail_dialogs (lo_id, hi_id);
-- Вход потолка первых писем: «сколько переписок этот человек завёл за окно».
CREATE INDEX mail_dialogs_started ON mail_dialogs (started_by, created_at);

-- Сторона переписки: ДВЕ строки на диалог, по одной на человека.
--
-- Не пара колонок lo_unread/hi_unread у самого диалога, хотя так короче. С парой
-- КАЖДЫЙ запрос обрастает `CASE WHEN lo_id = $me THEN ... ELSE ... END` — ровно
-- тот вид кода, в котором величину считают верно, а достают не ту, и заметить
-- это можно только на живых людях. Две строки делают «мой список переписок»
-- обычным range-scan по (user_id, last_message_at).
CREATE TABLE mail_sides (
    dialog_id bigint NOT NULL REFERENCES mail_dialogs(id) ON DELETE CASCADE,
    user_id   bigint NOT NULL REFERENCES users(id),
    -- peer_id денормализован по тому же доводу, что note_id у реакций и
    -- вопросов: список переписок рисует ник собеседника, и лезть за ним в
    -- mail_dialogs с разбором «а с какой я стороны» не надо ни разу.
    peer_id   bigint NOT NULL REFERENCES users(id),
    -- sent — сколько писем написал в эту переписку сам. Ноль у собеседника
    -- означает «я ему до сих пор незнаком», и на этом стоит правило
    -- UnansweredMax: три письма подряд тому, кто не ответил НИ РАЗУ.
    sent      int NOT NULL DEFAULT 0,
    -- unread — денормализация явным UPDATE, как notes.comment_count, а не
    -- триггером: счётчик правит та же транзакция, что пишет письмо, и другого
    -- места, где он меняется, нет.
    unread       int    NOT NULL DEFAULT 0,
    last_read_id bigint NOT NULL DEFAULT 0,
    last_message_at timestamptz NOT NULL DEFAULT now(),
    -- hidden_at — «убрать переписку у себя». НЕ блокировка: новое письмо гасит
    -- отметку и переписка всплывает обратно. Иначе скрытие молча стало бы
    -- блокировкой, о которой отправитель не знает, — а площадка решила говорить
    -- прямо, и от навязчивости здесь бережёт чёрный список, а не тишина.
    hidden_at timestamptz,

    PRIMARY KEY (dialog_id, user_id)
);

COMMENT ON TABLE mail_sides IS
    'сторона переписки: по строке на каждого из двоих, счётчики и отметки';

CREATE INDEX mail_sides_inbox ON mail_sides (user_id, last_message_at DESC)
    WHERE hidden_at IS NULL;
-- Частичный: счётчик писем спрашивается на КАЖДОЙ странице вошедшего, рядом с
-- колокольчиком, и платить за него полным индексом по всем сторонам незачем.
CREATE INDEX mail_sides_unread ON mail_sides (user_id) WHERE unread > 0;

-- ВОТ ОНА — колонка со свободным текстом от участника, которой в events нет и
-- не будет. Здесь лежат слова, сказанные одним человеком другому наедине:
-- площадка обязана их хранить, обязана выдать по запросу уполномоченного органа
-- и не вправе показать никому больше — ни другим участникам, ни автомату
-- модерации (в moderation_queue письмо не ставится вовсе), ни модератору, кроме
-- той цитаты, которую получатель сам приложит к жалобе.
CREATE TABLE mail_messages (
    id        bigserial PRIMARY KEY,
    dialog_id bigint NOT NULL REFERENCES mail_dialogs(id) ON DELETE CASCADE,
    sender_id bigint NOT NULL REFERENCES users(id),
    body      text   NOT NULL,
    sent_at   timestamptz NOT NULL DEFAULT now(),
    -- purged_at — содержание стёрто по сроку, а СТРОКА ЖИВЁТ. Сроки у закона
    -- разные: содержание полгода, сведения о приёме-передаче год, — и разводят
    -- их два шага уборки, а не вторая таблица. Вторая таблица метаданных была бы
    -- вторым источником правды о том, кто кому писал.
    purged_at timestamptz
);

COMMENT ON TABLE mail_messages IS
    'письма: содержание хранится полгода, метаданные год (п. 3 ч. 1 ст. 10.1 149-ФЗ)';

CREATE INDEX mail_messages_thread ON mail_messages (dialog_id, id);
-- Вход потолка частоты. Полоса идентификаторов у писем СВОЯ, с единицы: у
-- enforceRate нижний край полосы теперь параметр, и письма передают ноль.
CREATE INDEX mail_messages_rate ON mail_messages (sender_id, sent_at);
-- Вход уборки: «всё, что старше срока».
CREATE INDEX mail_messages_age ON mail_messages (sent_at);

-- Чёрный список. Решение владельца 11.09.2026: закрытому говорим ПРЯМО, а не
-- делаем вид, что письмо ушло. Молчаливая блокировка копит у отправителя
-- переписку, которой никто не читает, — а это и есть травля, только невидимая.
CREATE TABLE mail_blocks (
    user_id    bigint NOT NULL REFERENCES users(id),
    blocked_id bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, blocked_id)
);

COMMENT ON TABLE mail_blocks IS
    'кто кому запретил себе писать; проверяется в обе стороны одним запросом';

-- Жалоба на письмо. Единственная дверь модератора в переписку, и открывает её
-- ПОЛУЧАТЕЛЬ.
--
-- Несёт ЦИТАТУ-СНИМОК, а не ссылку на письмо: содержание уйдёт по сроку через
-- полгода, а разобранная жалоба обязана его пережить — иначе решение модератора
-- через год объясняется догадкой, ровно как решение о скрытии без записи в
-- audit_log. Внешнего ключа на mail_messages нет намеренно, тем же приёмом, что
-- у ngs_outbox.object_id: письма может не стать, а жалоба остаётся.
CREATE TABLE mail_reports (
    id          bigserial PRIMARY KEY,
    reporter_id bigint NOT NULL REFERENCES users(id),
    author_id   bigint NOT NULL REFERENCES users(id),
    message_id  bigint NOT NULL,
    dialog_id   bigint NOT NULL,
    quote       text   NOT NULL,
    reason      text   NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    resolved_by bigint REFERENCES users(id),
    resolution  text   NOT NULL DEFAULT ''
);

COMMENT ON TABLE mail_reports IS
    'жалоба получателя на письмо; цитата — снимок, она переживает срок хранения';

CREATE UNIQUE INDEX mail_reports_once ON mail_reports (reporter_id, message_id);
CREATE INDEX mail_reports_open ON mail_reports (created_at) WHERE resolved_at IS NULL;

-- Ссылка события на переписку — ровно такая же, как note_id и comment_id.
-- Текста письма в шине нет и не будет: у события письма обе прежние ссылки
-- пусты, поэтому выражение выдержки (left(coalesce(c.body, nt.body, ''), $4))
-- отдаёт пустую строку само, без единой оговорки в запросе.
ALTER TABLE events ADD COLUMN dialog_id bigint REFERENCES mail_dialogs(id) ON DELETE CASCADE;

COMMENT ON COLUMN events.dialog_id IS
    'переписка, о которой факт; текста письма в шине нет — только ссылка';
