# Бриф: гейт мессенджеров + интеграция MAX

- **Дата:** 2026-07-20; актуализирован 2026-07-26 (сверка с кодом — §3.5, §9)
- **Статус:** к реализации
- **Цель:** ввести мессенджер-гейт. **MAX — основной**, **Telegram — резервный и выключен по умолчанию**. M7 (Docker) уже выполнен — гейт встраивается в готовый деплой (§3.5).

---

## 1. Зафиксированные решения

1. **Маппинг MAX = «канал + отдельный чат обсуждения»** — верный порт телеграм-модели. У каналов MAX **нет нативных комментариев**, поэтому «автофорвард» (связку пост↔обсуждение) бот делает сам.
2. **Резерв = дуал.** При включении обоих мессенджеров зеркалим в оба параллельно (fan-out на несколько `Sink`). Дефолт: MAX `enabled:true`, Telegram `enabled:false`.
3. **Токен MAX-бота у пользователя есть** (модерацию прошёл). Значит цель — рабочая интеграция, а не заглушка.
4. **Приём апдейтов — long polling** (`GetUpdates`), без вебхука/публичного URL (как в TG-версии; важно для бокса за NAT).

---

## 2. MAX Bot API — итог ресёрча (факты, на которые опираемся)

| Аспект | Значение |
|---|---|
| Транспорт | REST `https://platform-api2.max.ru`, JSON, токен в заголовке `Authorization` |
| Rate limit | **30 req/s** |
| Старый хост | `platform-api.max.ru` **отключён 19.07.2026** — только v2.x SDK/новый хост |
| **TLS-сертификат** | Цепочка завязана на **корневой + промежуточный CA Минцифры** (Russian Trusted Root/Sub CA). На голом distroless/scratch верификация **упадёт** — CA надо вшить в образ **или** инъектировать в `http.Client` |
| Каналы vs чаты | Есть и каналы (вещание), и групповые чаты. `/chats` возвращает групповые чаты. ЛС адресуются по `user_id`, группы/каналы — по `chat_id` |
| **Нет нативных комментариев к каналу** | «Комментарии под постом» реализуют сторонние боты (ze-post и пр.): кнопка «💬 Обсудить» → обсуждение в **отдельном чате** |
| Медиа | Изображения ≤ 50 МБ и ≤ 7680×7680 px (оба критерия); видео ≤ 250 МБ. Поток: upload → token → attach |
| Апдейты | `update_type: message_created` (sender/recipient/body). В апдейте **нет id бота**. Вебхук-подписки «протухают» — надо перерегистрировать; long-poll этого лишён |

### 2.1 Go SDK `github.com/max-messenger/max-bot-api-client-go` (пакет `maxbot`, v2.x)

- `New(token string, opts ...Option) (*Api, error)`
- Опции: `WithHTTPClient(HttpClient)` (← сюда наш клиент с CA Минцифры + опц. прокси), `WithBaseURL`, `WithApiTimeout`, `WithPauseTimeout` (пауза long-poll), `WithUpdateHandler`, `WithDebugMode`.
- Апдейты: `GetUpdates(ctx) <-chan schemes.UpdateInterface`; ошибки — `GetErrors() <-chan error`. Вебхук: `GetUpdateHandler(...)`.
- Сообщение (fluent): `NewMessage().SetChat(chatID)` / `.SetUser(userID)` `.SetText(...)` `.SetFormat(schemes.Format)` `.SetNotify(...)` `.SetDisableLinkPreview(...)` `.AddKeyboard(kb)`.
- **Ответ на сообщение:** `SetReply(text, id string)` / `Reply(text, msg)`; форвард — `SetForward(id)`.
- **Медиа:** `AddPhotoByToken(token)` / `AddPhoto(*schemes.PhotoTokens)`, `AddVideo/AddAudio/AddFile(*schemes.UploadedInfo)`; загрузка — `Api.Uploads`.
- Кнопки: `InlineKeyboard(...)`, `Btn(text,payload,intent)` (callback), `BtnLink(text,url)`, `BtnApp(...)` и др.
- Чаты/каналы: `Api.Chats`.
- Ошибки типизированы: `APIError` (code/message), `NetworkError`, `TimeoutError`, `SerializationError`.

---

## 3. Архитектура

### 3.1 Store — обобщить маппинг под мессенджеры (миграция **v4**)

Сейчас маппинг телеграм-специфичен: `notes.tg_message_id`, `notes.tg_thread_id`, `comments.tg_message_id`, `note_images.tg_message_id`, а также `sessions/dialog_states/subscriptions/processed_replies` завязаны на `tg_user_id`/`tg_message_id`. Дуал требует по записи на **каждый** мессенджер, а у MAX своё пространство `user_id`.

Ввести таблицу целей сообщений:

```sql
CREATE TABLE message_targets (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger  TEXT    NOT NULL,          -- 'telegram' | 'max'
    kind       TEXT    NOT NULL,          -- 'note_post' | 'note_thread' | 'comment' | 'note_image'
    ref_id     TEXT    NOT NULL,          -- id заметки (TEXT) / комментария / иллюстрации (как TEXT)
    chat_ref   INTEGER,                   -- id канала/чата мессенджера (опц.; адаптер знает свой из конфига)
    message_id INTEGER,                   -- id сообщения в мессенджере
    thread_id  INTEGER,                   -- корень треда/чата обсуждения (для note_thread)
    created_at TEXT    NOT NULL,
    UNIQUE (messenger, kind, ref_id)
);
CREATE INDEX idx_message_targets_lookup ON message_targets(messenger, kind, ref_id);
CREATE INDEX idx_message_targets_thread ON message_targets(messenger, thread_id);
```

- **Миграция существующих TG-данных** в `message_targets` как `messenger='telegram'` (`note_post`/`note_thread`/`comment`/`note_image`). `message_id`/`thread_id` известны из старых колонок; `chat_ref` можно оставить NULL (адаптер знает канал/чат из конфига). Делается в Go при старте (не чистый PRAGMA — но конфиг для message_id/thread_id не нужен).
- Старые `tg_*` колонки **оставить на один релиз** как фолбэк, читать через новый слой; дропнуть в v5.
- Измерение `messenger` добавить в `sessions` (PK `(messenger, user_id)`), `dialog_states` (PK `(messenger, user_id)`), `subscriptions` (UNIQUE `(messenger, keyword, user_id)`), `processed_replies` (PK `(messenger, message_id)`). Дефолт существующих строк — `'telegram'`.
- Store-API: `SetTarget(ctx, messenger, kind, refID, chatRef, msgID, threadID)`, `Target(ctx, messenger, kind, refID) (…, found)`, `NoteByThread(ctx, messenger, threadID)`, `UnsentComments`/`UnsentNoteImages` — по (note, messenger).

### 3.2 Абстракция `Messenger` + fan-out

- Интерфейс: `Messenger = Sink` (уже есть: `PostNote/PostComment/PostNoteImage/NotifySubscriber`) **+** `Start(ctx)` (long-poll) **+** `AdminNotify(ctx, text)` **+** `DeepLink(...)`.
- Реализации: `tgx` (есть), `maxx` (новая).
- `mirror` принимает **список** `Sink` (fan-out на все включённые) вместо одного; трекинг id — per-messenger через `message_targets` (каждый Sink возвращает свой message_id/thread, mirror раскладывает по мессенджерам).
- Bridge (приём ответов) и DM-бот — **свои на каждый мессенджер**.
- `mirror.Sink.PostNote/PostComment` уже возвращают `int64` (id сообщения) — сохранить сигнатуры, но mirror должен знать, какому мессенджеру принадлежит id. Варианты: (а) Sink несёт `Name() string`, mirror пишет `SetTarget(name, …)`; (б) fan-out-обёртка возвращает `map[messenger]int64`. **Рекомендация:** `Sink.Name()` + per-sink запись в `message_targets`.

### 3.3 Пакет `maxx` (новый)

- Клиент: `maxbot.New(token, maxbot.WithHTTPClient(<клиент с CA Минцифры + опц. прокси>))`.
- **Sink:**
  - `PostNote` → пост в **канал** (`SetChat(channelID)`), медиа-аватар через upload+`AddPhotoByToken`.
  - `PostNoteImage` → иллюстрация в **чат обсуждения** (первым сообщением треда).
  - `PostComment` → `SetReply(text, <root/parent id>)` в **чате обсуждения**.
  - `NotifySubscriber` → ЛС по `user_id`.
- **«Ручной автофорвард»:** пост в канал → копия заметки в чат обсуждения → на посте канала кнопка (`BtnLink`/callback) ведёт в чат → id корневого сообщения в чате = аналог `tg_thread_id` (`kind='note_thread'`).
- **Bridge:** приём `message_created` из чата обсуждения (long-poll) → reply → комментарий на сайт под сессией MAX-пользователя. At-most-once через `processed_replies (messenger='max', message_id)`.
- Формат текста: смапить наш HTML-compose на `schemes.Format` MAX (уточнить: markdown/html — в Ф0).

### 3.4 Config-гейт

```jsonc
{
  "messengers": {
    "primary": "max",
    "max": {
      "enabled": true,
      "token": "…",              // env LOVEGW_MAX_TOKEN
      "channel_id": 0,
      "discussion_chat_id": 0
    },
    "telegram": {
      "enabled": false,          // РЕЗЕРВ, выключен по умолчанию
      "token": "…",              // env LOVEGW_MIRROR_TOKEN
      "channel_id": 0,
      "discussion_chat_id": 0,
      "dm_token": "…"            // env LOVEGW_DM_TOKEN
    }
  }
}
```

- Обратная совместимость: если `messengers` нет, читать плоские `mirror_bot`/`dm_bot` как `telegram` (enabled по факту наличия токена). Плавный переход конфига.
- `runDaemon`: поднять включённые мессенджеры, собрать `[]Sink` для `mirror`, по одному bridge/DM на мессенджер, `AdminNotify` — от primary (или от всех).

### 3.5 Docker / сертификат Минцифры

- **M7 уже выполнен** (коммит `0bc9ba4`, до фиксации этого брифа): multi-stage `go/Dockerfile`, `CGO_ENABLED=0` → `distroless/static` (~23 МБ, tzdata вшит через `time/tzdata`); `deploy/` — docker-compose + systemd-юнит + runbook; конфиг монтируется как `/config.json`, БД в томе `/data`, секреты через env (`LOVEGW_MIRROR_TOKEN`/`LOVEGW_DM_TOKEN`/`LOVEGW_TG_PROXY`).
- **Решение по CA:** инъектировать CA в `http.Client` (self-contained, как `tgx.ProxyClient`). В `distroless/static` системного cert-store нет, так что это единственный практичный путь: встроить `russian_trusted_root_ca` + `russian_trusted_sub_ca` (PEM) в бинарник через `//go:embed`, добавить к `x509.SystemCertPool()` в TLS-конфиге MAX-клиента.
- **Сетевая топология:** Bot API Telegram ходит через SOCKS5 (`telegram_proxy`, `internal/tgx/proxy.go`), сайт — напрямую с российского IP. MAX — российский сервис, `platform-api2.max.ru` доступен с того же IP напрямую: **прокси maxx-клиенту не нужен**, только CA.
- Проверить `doctor`: добавить чек доступности `platform-api2.max.ru` (иначе на не-настроенном CA — молчаливый TLS-фейл).

---

## 4. Фазы (каждая проверяема; Telegram всё время зелёный)

| Ф | Что | Токен MAX? | Критерий готовности | Статус |
|---|---|---|---|---|
| **0** | **Спайк**: канал ↔ чат обсуждения ↔ захват ответов на реальном боте | **да** | Документирована точная механика + поля `schemes` | ждёт токена |
| 1 | Store v4: `message_targets` + per-messenger sessions/replies + миграция TG | нет | Тесты миграции; повторное открытие БД зелёное; TG-регресс не сломан | **готово** |
| 2 | Абстракция `Messenger` + fan-out `mirror` (на текущем TG) | нет | `mirror` работает через `[]Sink`; TG-тесты зелёные | **готово** |
| 3 | `maxx`: Sink + канал/чат-обсуждения + bridge | да | Заметка → канал+чат; ответ в MAX → комментарий на сайт | Sink+треды готовы (тесты на httptest); **мост — после Ф0** |
| 4 | Config-гейт + дуал + TG off по умолчанию | нет | Один конфиг гоняет MAX/оба; дефолт — только MAX | **готово** |
| 5 | РюмкинЪ-MAX (ЛС: /login, заметки, подписки) | да | Полный ЛС-функционал в MAX | после Ф0 |
| 6 | CA Минцифры в бинарник + doctor-чек (Docker/деплой уже есть, §3.5) | — | Образ ходит в `platform-api2` из distroless | **готово** (PEM кладутся при сборке, `maxx/cacert/README.md`) |

**Порядок старта:** Ф1 → Ф2 (фундамент, без токена, TG не ломаем) параллельно с поднятием MAX-бота под Ф0-спайк.

### Отклонения реализации от брифа (по факту кода и SDK v2.2.4)

1. **Id сообщений MAX — строки (`mid`)**, а не числа: `message_targets.message_id/thread_id` — TEXT, `Sink` возвращает строковые id (Telegram отдаёт число десятичной строкой), `processed_replies` — `(messenger, message_id TEXT)`.
2. **SDK v2**: конструктор `maxbot.NewApi` (не `New`); апдейты — `Subscriptions.GetUpdates(ctx, marker)` с ручным маркером (канальных `GetUpdates() <-chan` из v1 больше нет); вложения `AddAttachByToken(token, AttachImage)`; `SetDisableLinkPreview` есть (query-параметр).
3. **«Ручной автофорвард» — через `mirror.ThreadStarter`**, а не разовый колбэк: приёмник, умеющий открыть тред сам (MAX постит копию заметки в чат обсуждения), вызывается на каждом цикле опроса до успеха — самовосстановление при сбое копии. Кнопка «Обсудить» на посте канала отложена до Ф0 (нужна ссылка чата).
4. **PEM CA Минцифры не в репозитории**: подлинность корня доверия не проверить в песочнице — файлы кладутся из Госуслуг в `go/internal/maxx/cacert/` перед сборкой (гитигнорятся), иначе фолбэк на системное хранилище; `doctor` показывает TLS-фейл явно.
5. **Алерты при дуале** (открытый вопрос §7) — во все включённые мессенджеры с заданным `admin_user_id` (у каждого мессенджера своё пространство id, поле в секции).

---

## 5. Топ-риск

Механику «канал ↔ чат обсуждения ↔ захват ответов» доки MAX **однозначно не описывают** (её делают сторонние comment-боты). **Ф0-спайк на реальном боте обязателен до Ф3.** Возможный исход: чистого «канал+комментарии» не выходит → честно откатываемся в модель «групповой чат» (заметка = сообщение, комментарии = `SetReply`, ответы юзеров ловим из чата). Тогда пересматриваем п.1.1.

### Что проверить в Ф0-спайке (эмпирически, на боевом токене)
1. Бот постит в канал (`SetChat(channelID)`); получает `message_id`.
2. Под постом канала добавляется кнопка (`BtnLink`/callback), ведущая в чат обсуждения.
3. Бот состоит в чате обсуждения и **получает `message_created`** оттуда по long-poll.
4. Бот кладёт копию заметки в чат → её `message_id` = корень треда; ответы пользователей связываем с заметкой по этому корню (через `SetReply`/`link`).
5. Формат ответа-ссылки: как в апдейте представлен reply/link (mid родителя).
6. Загрузка изображения: upload → token → `AddPhotoByToken` в пост канала.
7. Уточнить `schemes.Format` (markdown/html) для нашего compose.

Артефакт спайка: заметки + (опц.) выкидной `cmd/maxspike` (не коммитим в основную ветку).

### Ранбук спайка (по реализованному коду; нужен RU-IP для сайта)

1. **Токен** → `LOVEGW_MAX_TOKEN` (в конфиг не коммитить). Проверка TLS+токена:
   `curl -H "Authorization: $LOVEGW_MAX_TOKEN" https://platform-api2.max.ru/me`.
   TLS-ошибка → положить PEM Минцифры в `go/internal/maxx/cacert/` (README там) и пересобрать, либо установить в систему.
2. **Тестовые канал и чат обсуждения** в MAX, бот — админом в оба.
3. **Снять id**: `curl -H "Authorization: $LOVEGW_MAX_TOKEN" "https://platform-api2.max.ru/updates"` — в апдейтах `bot_added` лежит `chat_id` (сырое поле `is_channel` различает канал/чат); свой `user_id` для алертов — из `bot_started` (написать боту в ЛС).
4. **Тестовый конфиг**: `messengers.max{enabled, channel_id, discussion_chat_id, admin_user_id}`, `telegram.enabled=false`, **отдельный** `db_path` (например `data/max-spike.db`), `notes_limit: 2`. Затем `lovegw doctor` — все чекы ✔.
5. **Живой прогон**: `lovegw run -config config.max-spike.json` на пустой БД — первые заметки ленты уйдут в тестовый канал (это и нужно; лимит 2 защищает от бурста). Смотреть: HTML-рендер (`<b>`, кликабельная ссылка профиля), превью выключено, аватар вложением, копия в чате = корень треда, комментарии reply'ем, иллюстрация в тред.
6. **Замеры по рискам**: длинная заметка/комментарий → лимит длины из ответа 400 (R2); бурст → точный код 429 для `maxx.isTooManyRequests`; ссылка чата — `GET /chats/<discussion_chat_id>` → поле `link` (для будущей кнопки «Обсудить»).
7. **Для моста**: ответить руками в чате обсуждения и снять `GET /updates` — форма `message_created` с reply-link (mid родителя).
8. Итоги → в этот бриф (снять R2, код 429, механика кнопки), после чего открываются Ф3-остаток (мост) и Ф5.

**Снято 26.07 (шаги 1–3 пройдены):** токен жив (`/me` отвечает), TLS с сертификатами Минцифры работает. Id каналов и групповых чатов — **отрицательные** int64 (~−7,7·10¹³), id пользователей и ЛС-диалогов — положительные (лимитеры maxx это учитывают). `bot_added` несёт `chat_id` + `is_channel`; `bot_started` — `user_id` (админ: 25978651). Форма `message_created` из группового чата подтверждена (recipient.chat_id/chat_type, body.mid/seq/text, sender).

**Снято 26.07, живой прогон (шаги 5 и 7):** зеркалирование работает end-to-end — пост в канал, копия-тред в чат, комментарий с аватаром-вложением реплаем в тред (заметки 312782/312783). Три факта:
1. **HTML подтверждён на уровне API**: сервер разбирает `format=html` в плоский `text` + массив `markup` со спанами (`{"type":"strong"|"link","url",from,length}`) — наши `<b>`/`<a href>` легли в strong+link, сущности декодированы.
2. **Аватар комментария дошёл вложением** (attachments[0].type=image, payload.photo_id/token/url) — upload-флоу живой.
3. **Форма reply для моста**: ответ пользователя на сообщение бота несёт `body.link = {"type":"reply", "chat_id", "sender"(бот), "message":{mid родителя, text, attachments, markup}}` — mid родителя в `link.message.mid`, изоморфно TG-мосту: `NoteByThread(max, mid)` / `CommentByTarget(max, mid)` покрывают резолв цели. Мост технически разблокирован; для постинга на сайт от имени MAX-пользователя нужны его куки-сессии → Ф5 (/login в РюмкинЪ-MAX).

**Не снято:** точный код 429 (бурст не ловили), лимит длины текста (R2), ссылка чата для кнопки «Обсудить» (`GET /chats/<id>` → `link`).

---

## 6. Что нужно от пользователя (для MAX-фаз)

- Токен MAX-бота → `go/config.json` (`messengers.max.token`) или env `LOVEGW_MAX_TOKEN`.
- `channel_id` канала MAX (бот добавлен **админом**).
- `discussion_chat_id` чата обсуждения (бот в чате, может писать/читать).

---

## 7. Открытые вопросы (решить по ходу)
- Формат текста MAX (markdown/html) — уточнить в Ф0, смапить compose.
- `AdminNotify` при дуале — от primary или от всех включённых?
- Дропать `tg_*` колонки в v5 или оставить навсегда как денормализованный кэш?
- Нужен ли РюмкинЪ-MAX в первом релизе или ЛС-функции могут временно остаться только в TG.

---

## 8. Ссылки
- dev.max.ru/docs-api — офиц. REST-доки
- github.com/max-messenger/max-bot-api-client-go — Go SDK (v2.x, `maxbot`)
- pkg.go.dev/github.com/max-messenger/max-bot-api-client-go — поверхность SDK
- habr.com/ru/articles/1060586/ — грабли MAX Bot API (апдейты, user_id/chat_id, вебхуки)
- maxstat.ru/blog/api-max-razrabotchikov-prosyat-obnovit-adres-i-sertifikaty-instruktsiya — переход на platform-api2 + CA Минцифры
- bothelp.io/ru/blog/kak-vklyuchit-kommentarii-v-kanale-max — про отсутствие нативных комментариев и comment-боты

---

## 9. Текущее состояние проекта (актуализировано 2026-07-26)
- **Ядро, на которое опирается план, с 20.07 не менялось:** `internal/mirror` (тот же `Sink`, один приёмник), `internal/store` (схема основной БД на `user_version 3` — миграция v4 из §3.1 ложится как есть), `internal/config` (плоские `mirror_bot`/`dm_bot` + `telegram_proxy` — обратная совместимость из §3.4 актуальна), `tgx`/`bridge`/`dmbot` без изменений. План §3 в силе без правок.
- Появился офлайн-контур `internal/archive`: отдельная БД `archive.db` со **своей** схемой и миграциями + команды `grab`/`export`/`backfill`/`personas` (аналитика поверх архива). С рабочей БД демона не пересекается и на план MAX не влияет; нумерация миграций в §3.1 относится к основной БД (`internal/store`), не к архивной.
- `internal/love` расширен парсерами (древовидный вид комментариев, шапка заметки, профили/пол через мобильную версию); интерфейс `SiteClient`, который потребляет `mirror`, не менялся.
- M7 (Docker + артефакты деплоя) выполнен — детали в §3.5.
- Проект полностью на Go (Python-легаси удалён, коммит `7401510`). Корень: `CLAUDE.md`, `README.md`, `briefs/`, `deploy/`, `docs/`, `go/`.
