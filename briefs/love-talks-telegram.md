# Бриф: личные сообщения love.ngs.ru (talks) → Telegram + MAX

- **Дата:** 2026-07-29
- **Статус:** разведка (Ф0). Разделы §3–§4 помечены ⚠ — предположения до снятия
  фактов Ф0; §1–§2 и established-pattern (§0) проверены по коду `main` (`c571023`).
- **Цель:** мост личной переписки сайта («talks» — встроенный мессенджер
  love.ngs.ru): входящие ЛС сайта доставлять в личку бота, ответ из мессенджера
  отправлять на сайт от имени сессии пользователя. **Сразу оба мессенджера** —
  Telegram и MAX (общее ядро + фан-аут). Итерация 1 — только админ (одна сессия
  сайта), итерация 2 — все залогиненные.

---

## 0. Established pattern — на что опираемся (проверено по коду)

Интерактивные контуры уже разделены на **мессенджер-агностичное ядро + тонкие
обёртки**, MAX — полноценный ровня Telegram (коммит `9bc4fc8`). Talks строим по
этому же лекалу, ничего не изобретая:

- `dmbot.Logic` (`internal/dmbot/logic.go`) — ядро диалогов: команды, состояния
  (`dialog_states`), `/login` → сессия сайта, подписки. Параметризовано
  `messenger string` + интерфейсом `Transport` (`Send`, `DeleteMessage`).
  Telegram: `dmbot.Bot`/`tgTransport`. MAX: `dmbot.NewLogic(…, mx,
  store.MessengerMax, …)`, транспорт — `maxx.Mirror`.
- `bridge.Core` (`internal/bridge/core.go`) — «реплай в обсуждении → комментарий
  на сайте», `ProcessReply(ctx, replyMsgID, userID, replyToID, text)`,
  at-most-once через `processed_replies`. Telegram: `bridge.Handler`; MAX:
  зовётся из `maxx.Dispatch`.
- `maxx.Mirror.Start(ctx, onUpdate)` — приёмный long-poll; `maxx.Dispatch(bridge,
  dm)` роутит: сообщение из диалога → `dm.HandleText`, реплай в чате обсуждения →
  `bridge.ProcessReply`, `bot_started` → `dm.Greet`.
- Сборка `runDaemon` (`cmd/lovegw/main.go:174`): Telegram — зеркало-постер +
  `dmbot.Bot` + `bridge.Handler`; MAX — один `mx.Start(gctx, mx.Dispatch(maxCore,
  maxDM))`. Сессии и `/login` работают на обоих (`sessions` PK `(messenger,
  user_id)`); `message_targets` уже TEXT-id под MAX.

**Вывод:** talks-ядро — новый агностичный `Engine` в `internal/talks` + интерфейс
доставки `PMTransport`, а поллер сайта — **один**, с фан-аутом входящих на
включённые мессенджеры (как `mirror` фанит `[]Sink`). Дуал не добавляет запросов
к сайту: к сайту ходит только поллер, фан-аут идёт в мессенджеры.

---

## 1. Зафиксированные решения

1. **Оба мессенджера сразу** — общее ядро + фан-аут. По духу проекта MAX —
   основной приёмник, Telegram — резерв; talks гейтится отдельно per-messenger и
   по умолчанию **выключен**.
2. **Итерация 1 — только админ** (одна сессия сайта по `admin_user_id`). Метода
   «перечислить все сессии» в сторе пока нет — для admin-only он не нужен
   (образец авторизованного обхода под сессией — `personas_gender.go:37-131`).
3. **Чтение — поллинг по курсору списка диалогов**, mark-read **выключен**:
   собеседник не видит «прочитано», пока пользователь сам не ответит. Значит
   `countNewMessages` как дельта-сигнал **не используем** (он залипает на `>0`
   без mark-read).
4. **Приватность — только метаданные** (`store_text=false` по умолчанию): в БД
   id/направление/курсоры/ник собеседника, сам текст сообщений живёт **только в
   мессенджере**. Это переписка третьих лиц — данные чувствительнее заметок.
5. **Ответ на сайт — только явным реплаем** на доставленное ЛС или залипанием на
   диалог (`/talk N`). Неявный «последний активный диалог» **отклонён**: цена
   ошибки маршрутизации — сообщение уходит живому человеку и не отзывается.
6. **Медиа в MVP** — входящее фото → текст-заглушка «📷 фото» + прямая ссылка;
   исходящие только текст (`input#image_file` не трогаем).
7. **Транспорт talks — в `internal/love`** (`talks.go`), оркестрация и ядро — в
   новом пакете `internal/talks`. Общий `rate.Limiter` сайта **не дробим** —
   поллер переиспользует тот же `*love.Client`, что и зеркало (куки per-request
   уже поддержаны: `get(ctx, path, cookies…)`, `postForm(…, cookies)`).

---

## 2. Разведка talks — что уже известно (из снимка `love.ngs.ru.mhtml`)

Снимок — главная страница под логином (view-source), содержит серверный каркас
мессенджера и bootstrap-JSON, но **не** сами сообщения (их грузит JS) и **не**
`talks.js`.

| Аспект | Факт |
|---|---|
| Название | мессенджер сайта — **«talks»** |
| Серверный HTML | пустой каркас `.lv-talks js_talks`, `js_buddy-list` (список диалогов), `js_msgr-dialog[data-page]` (лента), форма `#writing` (`textarea#text2`, `input#image_file`) — **отрисованы пустыми**, данные грузит JS |
| Следствие | goquery-парсинг HTML как для заметок **не применим**; нужны AJAX/JSON-эндпоинты |
| Логика | `/static/js/min/talks.js?v=<Love.currentVersion>` — **в снимке нет, разобрать в Ф0** |
| RPC | подключён `s.ngs.ru/ngs/jsonrpc.min.js`; логин уже ходит на `/ajax?request=login` (`love/auth.go`) — вероятная форма и у talks |
| CSRF | `Love.token` (32-hex); `Love.user` = id анкеты |
| Реалтайм | nginx push-stream: `Love.pushStreamMessagesChannelName = "messages-<hash>"` (hash — серверный секрет со страницы), `Love.lastMessagesCheckTime` (unix float) |
| Идентичность | `window.dataFromBlade.layout`: `user.passport_id`, **`talkLink:"/talks/<passport_id>"`**, `header_content.countNewMessages`, `countNewMessagesGuests`, `talksLimit:10` |
| ⚠ Адресация | диалог адресуется по **`passport_id`** (`/talks/280703879`), а весь остальной код — по id анкеты (`/profile/<id>/`). Нужен маппинг анкета↔паспорт |
| Разбор JSON-блоба | `dataFromBlade.layout` уже умеем читать — приём `parseGenderMobile` (`love/profile.go:78`); тем же снимаем `Love.token`/`passport_id`/`countNewMessages` |
| В MVP не нужно | `real_talk` (переписка только для «реальных»), «экспресс»-доставка, жалобы, удаление истории |

**Таблица эндпоинтов talks (снято в Ф0 живьём, 2026-07-29, сессия админа):**

| Операция | Метод + URL | Параметры запроса | Ключевые поля ответа | Статус |
|---|---|---|---|---|
| Список диалогов | **GET** `/ajax?request=loadBuddiesList` | `before`, `limit`, `stick_passport_id` (опц.), `anticache` (ms) | `{loadBuddiesList:{error, html, data:{user_ids[], last_user_time, last_user_express}}}` — диалоги = **rendered HTML в `loadBuddiesList.html`** (НЕ в `data.html` — тот пустой); `data` несёт лишь массив паспортов `user_ids` и метки времени | ✅ снято (16 КБ JSON); ⚠ envelope-путь исправлен по read-only прогону 29.07 (парсер читал `data.html` → 0 диалогов) |
| История диалога | **JSON-RPC POST** `/ajax/` | `{"jsonrpc":"2.0","method":"getMessagesHistory","params":[passportId, page],"id":N}`; `page` 1-based, `MSG_LIMIT=20` | `result.html` — **rendered HTML** сообщений (парсить goquery) | ✅ снято |
| Отправка | **JSON-RPC POST** `/ajax/` | `{"jsonrpc":"2.0","method":"sendMessage","params":[passportId, "<text>", []],"id":N}`; авторизация по кукам, **без `Love.token`** | `result` (JSON-RPC); созданное сообщение придёт и историей/пушем | ✅ снято (DevTools, PHP `Love_Page_Ajax::sendMessage`) |
| Отметка прочитанного | `/ajax?request=markAsReadMessages` | `passport_id`, … | — | в MVP off (mark_read=false) |
| Неавторизован (гость) | те же без кук | — | **HTTP 200** + `{"…":{"data":[],"html":"","error":"Ошибка авторизации"}}` | ✅ снято — НЕ 401/403 |
| Идентичность | GET `/` под сессией | — | `Love.user`(id анкеты), `passport_id`, `talkLink`, `Love.token`(32hex), `currentVersion`, `countNewMessages`, `talksLimit` | ✅ снято |
| Push-stream | канал `Love.pushStreamMessagesChannelName`=`messages-<hash>` | — | событие входящего — `newMessage` (`obj.type=="new_message"`) | URL подписки не снят (MVP: поллинг) |

**Ключевые выводы Ф0 (для Ф4):**

1. **Детектор протухшей сессии — по телу, а не по коду:** неавторизованный запрос
   отдаёт **200 + `error:"Ошибка авторизации"`** и пустой `data`. `bridge.isAuthError`
   (подстроки «статус 401/403») здесь бесполезен — в `love/json.go` проверять поле
   `error` == «Ошибка авторизации» (и/или пустой `data`) → `ErrUnauthorized`.
2. **Ответы — rendered HTML, не структурированный JSON:** и список диалогов
   (`loadBuddiesList.html`, НЕ `data.html`), и история (`result.html`) отдаются
   готовым HTML. Значит Ф4 парсит их goquery (новые селекторы в const-блоке
   `love/talks.go`), а не `encoding/json` по полям сообщений. `user_ids[]` в
   `loadBuddiesList.data` даёт паспорта, но адресацию берём из data-атрибутов
   разметки (`data-user-passport-id`). ⚠ Ловушка, всплывшая на read-only прогоне
   29.07: HTML именно в `loadBuddiesList.html`; парсер сперва читал `data.html`
   (пустой) и молча отдавал 0 диалогов — фикстура повторяла ту же ошибку, поэтому
   юнит-тест не ловил. Исправлено + фикстура приведена к реальной форме.
3. **Транспорт двойной:** список/отметка — `GET/POST /ajax?request=<name>` (форма,
   JSON-ответ), история и отправка — **JSON-RPC POST `/ajax/`** одинаковым
   конвертом `{jsonrpc:"2.0", method, params:[…], id}`, авторизация по кукам.
   `love/json.go` нужен и `getJSON` (query), и `rpcCall(method, params…)`.
4. **Отправка снята:** `sendMessage(passportId, text, [])` (третий параметр —
   вложения, для текста пустой массив); `Love.token` в теле не нужен.
5. Идентичность (`SiteIdentity` для `dmbot.captureIdentity`) снимается регулярками
   с авторизованной `/`: `Love.user`=анкета, `passport_id`, ник — из
   `dataFromBlade.layout` (как `parseGenderMobile`).

---

## 3. Архитектура ⚠ (пост-Ф0; уточняется по фактам разведки)

### 3.1 Store — миграция v5 (аддитивная; текущая версия v4, `migrate.go:126`)

Добавить `const migrateV5SQL` и элемент в срез `migrations`; тест —
`TestMigrateV4ToV5` по образцу `TestMigrateV3ToV4`.

```sql
-- site-идентичность владельца сессии: без неё не связать анкету, паспорт и ник.
ALTER TABLE sessions ADD COLUMN site_profile_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN site_passport_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN site_nick        TEXT NOT NULL DEFAULT '';

CREATE TABLE talks_peers (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    messenger     TEXT    NOT NULL,            -- 'telegram' | 'max'
    owner_user_id INTEGER NOT NULL,            -- владелец сессии (чья переписка)
    passport_id   TEXT    NOT NULL,            -- собеседник в talks
    profile_id    TEXT    NOT NULL DEFAULT '', -- id анкеты /profile/<id>/, если известен
    nick          TEXT    NOT NULL DEFAULT '',
    avatar_url    TEXT    NOT NULL DEFAULT '',
    cursor_msg_id TEXT    NOT NULL DEFAULT '', -- последнее втянутое сообщение сайта
    last_event_at TEXT,                        -- для адаптивного интервала
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
```

- Миграция аддитивна → откат на прошлый бинарник работает на той же БД.
- Site-идентичность существующих сессий доливаем **не в SQL**, а мягко в Go при
  первом успешном запросе под сессией (пустое поле → снять с `layout` → `UPDATE`);
  писать также в `tryLogin` (`logic.go:170`).
- `message_targets`: новый `kind = 'pm_message'` (`ref_id` = `talks_messages.id`
  как TEXT, `message_id` = id доставленного ЛС в мессенджере, `chat_ref` = user
  id). `SetTarget/Target` универсальны; `writeThroughLegacy` по `default` вернёт
  `nil` — ок. Kind `pm_thread` **не заводим** (в личке тредов нет).
- `processed_replies`: ключ ответов из личного чата префиксуем `dm:<msg_id>`.
  PK `(messenger, message_id)`, а id уникален лишь в пределах чата → без
  префикса ответ в личке столкнётся с id из чата обсуждения. Значение уже TEXT —
  миграция не нужна.

### 3.2 `internal/love/talks.go` + JSON-хелпер

- Типы `TalkDialog`, `TalkMessage`, `TalkCounters`; методы `TalksDialogs`,
  `TalksHistory(after)`, `TalksSend`, опц. `TalksMarkRead`, `SiteIdentity`.
- **Единый const-блок эндпоинтов** (по образцу const-блока селекторов в
  `parse.go`).
- Новый `love/json.go`: `getJSON`/`postJSON` поверх общего лимитера с
  `X-Requested-With: XMLHttpRequest`, `Accept: application/json`, подстановкой
  `Love.token`. Типизированный `SchemaError` (нет обязательного поля = дрейф API,
  аналог `MarkupError`) + `ErrUnauthorized` и `isGuestResponse` (сайт на
  неавторизованный запрос отдаёт **200 + гостевой** HTML/JSON, а не 401/403 —
  текущий `bridge.isAuthError` по подстрокам «статус 401/403» это не ловит).

### 3.3 Пакет `internal/talks`

```go
// Клиент сайта — всё, что нужно поллеру (реализует *love.Client).
type SiteTalks interface {
    Dialogs(ctx, ck []*http.Cookie, limit int) ([]love.TalkDialog, error)
    History(ctx, ck []*http.Cookie, passportID, afterMsgID string, limit int) ([]love.TalkMessage, error)
    Send(ctx, ck []*http.Cookie, passportID, text string) (love.TalkMessage, error)
    MarkRead(ctx, ck []*http.Cookie, passportID, lastMsgID string) error // опц.
}

// Доставка в личку мессенджера. В ОТЛИЧИЕ от dmbot.Transport ВОЗВРАЩАЕТ id —
// он нужен для message_targets (чтобы роутить ответ обратно).
type PMTransport interface {
    Name() string
    SendPM(ctx, userID int64, html string) (msgID string, err error)
    Confirm(ctx, userID int64, msgID string, ok bool) // реакция ✅/⚠️ на исходящее
}
```

- ⚠ существующий `dmbot.Transport.Send` id не возвращает; и Telegram
  (`SendMessage`→`.ID`), и MAX (внутренний `send`→mid) id знают — надо лишь
  пробросить в новый `SendPM`.
- `Watcher.Run(ctx)` — поллер по образцу `mirror`: owner-сессии → `Dialogs`
  (1 запрос) → диалоги где `last>cursor` → `History(after=cursor)` (≤
  `MaxDialogs` за тик) → `InsertTalkMessage` (dedup по `RowsAffected`) → свежие
  `direction='in'` → фан-аут `SendPM` по включённым транспортам → `SetTarget` →
  `SetPeerCursor`. Исходящие из истории (написанные с сайта в браузере) пишем в
  БД, в мессенджер не дублируем.
- Роутинг ответа `HandleReply`: реплай/`/talk` → найти peer через
  `message_targets`/состояние → `TryMarkReplyProcessed("dm:"+id)` **до** POST
  (at-most-once, как `bridge`) → сверка `peer.owner==from.ID` (жёстко) → `Send`
  → запись `direction='out'` → `Confirm(✅)`. Ошибка → `Confirm(⚠️)` + причина,
  без ретрая.
- `Config{ AdminOnly, AdminUserID, Interval, IdleInterval, MaxDialogs,
  AllowSend, MarkRead(false), StoreText(false), MaxReqPerMin, AlertSend }`.
- `alerter` из `mirror/alert.go` вынести в `internal/alerts` (сейчас unexported,
  нужен и поллеру talks: 3 подряд фейла → DM админу, восстановление).

### 3.4 Подключение к мессенджерам (симметрично §0)

- **Telegram:** `dmbot.handle` (`dmbot.go:79`) сейчас отбрасывает
  `msg.ReplyToMessage` — расширить, чтобы реплай на доставленное ЛС уходил в
  talks-роутер (либо через `/talk`-состояние в `dialog_states`, без изменения
  сигнатуры ядра).
- **MAX:** `maxx.dispatchMessage` (`updates.go:79`) диалоговые сообщения шлёт в
  `dm.HandleText` **без** `msg.Link` — прокинуть link (там `link.message.mid`
  родителя, как в мосте) в talks-роутер.
- **Поллер** `talks.Watcher` добавить в `runDaemon` рядом с `mirror` под общий
  `errgroup`, отдав ему `st`, `client`, транспорты включённых мессенджеров и
  `fanOutAlerts(alerters)`.

### 3.5 UX и config-гейт

- Входящее: `💌 <b>Ник</b>, возраст · <a href="…/profile/<id>/">анкета</a>\n\n
  <текст>` (HTML, escape, обрезка ~3500). Ответ — реплаем.
- Команды: `/talks` (список диалогов с непрочитанным), `/talk N` (залипнуть на
  диалоге — состояние `pm:<peer_id>` в `dialog_states`), `/cancel`. Строка «ЛС:
  вкл/выкл, диалогов N, последняя проверка …» в `/status`.
- Ответ без реплая и без залипания → подсказка «ответьте реплаем или /talks».

```jsonc
"messengers": { "telegram": { "talks": {
  "enabled": false,          // MVP выключен по умолчанию (и для max — так же)
  "admin_only": true,
  "allow_send": true,        // read-only режим для обкатки
  "poll_interval_s": 30,
  "idle_poll_interval_s": 300,
  "max_dialogs_per_tick": 5,
  "max_requests_per_min": 6, // бюджет на общем лимитере сайта
  "mark_read": false,
  "store_text": false,       // приватность: текст только в мессенджере
  "retention_days": 30
}}}
```

`doctor`: чек «talks» — есть валидная сессия админа, заполнена site-идентичность,
эндпоинт списка диалогов отвечает JSON ожидаемой формы.

---

## 4. Фазы (каждая проверяема; зеркало всё время зелёное)

| Ф | Что | Критерий готовности | Статус |
|---|---|---|---|
| **0** | **Разведка API talks** под живой сессией админа (runbook §5): эндпоинты, формат, гостевой ответ, идентичность | Таблица §2 заполнена | ✅ **read-path снят вживую 2026-07-29** (список/история/гость/идентичность); ❗открыт только send-эндпоинт (нужен DevTools-захват) |
| 1 | Store v5: `talks_peers`/`talks_messages`, site-идентичность в `sessions`, kind `pm_message`, store-API | `TestMigrateV4ToV5` зелёный, повторное открытие идемпотентно, регресс v4 цел | ✅ **готово** (коммит `store: v5 … [Ф1]`) |
| 2 | `internal/talks` на фейках + вынос `alerter` в `internal/alerts` | Юнит-тесты: курсор, дедуп, at-most-once, фан-аут, adaptive interval, kill-switch; `mirror`-тесты зелёные | ✅ **готово** (коммит `talks: поллер … [Ф2]`) |
| 3 | Оба мессенджера: `PMTransport` для Telegram и MAX (`SendPM`+`Confirm`), проброс reply-linkage, команды `/talks`/`/talk`/`/cancel`, снятие site-идентичности при `/login` | Реплай доходит до роутера в обоих; идентичность пишется; dmbot/maxx-тесты зелёные | ✅ **готово** (коммит `talks: проводка … [Ф3]`) |
| 4 | `love/talks.go`+`json.go` по фактам Ф0 (AJAX/JSON-RPC, HTML-парсеры) + `ErrUnauthorized`/guest-детектор | Парсеры и гость→`ErrUnauthorized` покрыты тестами; клиент проверен живьём (dialogs/history/send сняты) | ✅ **готово** (коммит `love: клиент talks … [Ф4]`) |
| 5 | Сшивка в `runDaemon` + config-гейт + `doctor` | Watcher собирается, `SetTalkRouter` в TG+MAX, config `talks`, doctor-чек; сборка/vet/тесты зелёные | ✅ **готово** (`[Ф5]`); read-path проверен вживую read-only командой `lovegw talks watch` (коммит `ac46ea7`) — там же вскрыт+исправлен envelope-баг `loadBuddiesList` (парсер читал `data.html` → 0 диалогов); доставка 💌 в Telegram подтверждена вживую |
| 6 | Мультисессия: все залогиненные (`SessionOwners`, `admin_only=false`), изоляция протухших сессий, retention/приватность | Несколько пользователей параллельно; истёкшая сессия одного не роняет поллер (per-user `ErrUnauthorized`→invalidate+`/login`, не kill-switch); ретеншн чистит старое; общий бюджет запросов ≤ лимита (один site-лимитер, tunable) | ✅ **готово**; проверено вживую read-only: 2 сессии обходятся (297934912+1909965797, по 3 диалога), `PurgeLoop` + doctor-репорт мультисессии; ⏳ прод-раскатка `admin_only=false`/`allow_send=true` — по runbook на домашнем сервере |
| 7 (опц.) | push-stream long-poll вместо/в дополнение к поллингу | Мгновенная доставка при нулевом росте числа запросов к сайту | после Ф0 |

**Порядок старта:** Ф1 → Ф2 → Ф3 (фундамент на фейках, без живого API) параллельно
с Ф0.

---

## 5. Топ-риск

Поллинг talks и/или CSRF-жёсткий JSON-RPC ловит **403 DDoS-Guard** на боевом IP —
и вместе с talks ложится **уже работающее зеркало заметок** (общий IP, общий
лимитер, общий процесс). Это единственный компонент фичи, способный уронить
рабочий продукт.

**Откат (три уровня):**
1. `messengers.<m>.talks.enabled=false` — мгновенное выключение, остальной демон
   не трогается.
2. **Kill-switch в рантайме:** 3 подряд `ErrForbidden` или дрейф API (`SchemaError`)
   → поллер talks останавливается сам + алерт админу, зеркало продолжает работать.
   **Временный отказ сюда не входит:** 5xx фронта и обрыв связи (`ErrSiteUnavailable`)
   только копятся в алертере — три подряд дают одно сообщение админу, поллинг
   продолжается на холостом интервале. Разделено 12.08.2026 после боевого случая:
   502 на `loadBuddiesList` уронил поллер до ручного рестарта, а остановка здесь
   стоит входящих ЛС — история сайта отдаёт только последнюю страницу.
3. Схема v5 аддитивна → откат на предыдущий бинарник на той же БД без down-миграции.

Плюс приватность-откат: `store_text:false` (дефолт) — в БД только метаданные.

**Возможный исход Ф0:** если talks завязан на антибот-подпись запроса в JS
(привязка токена к времени/UA) — честный откат в «браузерный» режим
(headless/CDP) либо отказ от фичи. Записать как исход, а не замалчивать.

**Бюджет запросов (явно):** общий лимитер `rate.Every(2000ms)` burst 2 = **30
req/min** на процесс. Зеркало ест ~6–12/min. Talks выделяем **≤6/min**
(собственный `rate.Every(10s)` burst 3) — при 30-сек тике это список + 1 история.
Максимальная добавленная задержка запроса зеркала ≈ 2–6 с — приемлемо. Дуал
доставки к бюджету **не добавляет** (поллер один, фан-аут в мессенджеры).

### Блокер Ф0 — гигиена секретов (сделать до любого `git add`)

`.gitignore` игнорирует `config.json`, но **не** паттерн `config.*.json`, и явно
«testdata НЕ игнорируем». Значит untracked `go/config.max-spike.json` (**живой
MAX-токен**) и `go/internal/love/testdata/love.ngs.ru.mhtml` (**живой
`Love.token`, `passport_id`, hash push-канала**) уедут в историю при первом
`git add`. До коммитов фичи: добавить в `.gitignore` `config.*.json` (кроме
`*.example.json`), санитизировать/игнорировать `love.ngs.ru.mhtml`. Сырьё Ф0
(чужая переписка + токены) — только в scratch; в репозиторий кладём
**санитизированные** `talks_*.json` (ники/тексты/id заменены).

### Runbook Ф0 (нужен RU-IP + живая сессия админа)

Инструмент — новая отладочная подкоманда `lovegw talks <probe|dialogs|history|
send>` (`go/cmd/lovegw/talks.go`), **не** `crawl talks` (тот анонимный/read-only;
talks под сессией и с операцией записи). Сессия из БД по `admin_user_id` (образец
— `personas_gender.go:37-131`). Флаги: `-config`, `-tg-user`, `-out <dir>` (сырьё
в scratch), `-dry-run`.

1. **`talks probe`** → `GET /` под куками → извлечь `Love.token`, `Love.user`,
   `Love.currentVersion`, `Love.lastMessagesCheckTime`,
   `Love.pushStreamMessagesChannelName`, `dataFromBlade.layout` (`passport_id`,
   `talkLink`, `countNewMessages`, `talksLimit`). **Проверить, есть ли те же поля
   на `/notes/`** — если да, зеркало и так тянет эту страницу, токен/счётчик
   достаются бесплатно.
2. **Скачать и разобрать `talks.js`** (`/static/js/min/talks.js?v=<version>` —
   статика, сессия не нужна). Грепать литералы: `/ajax`, `request=`, `jsonrpc`,
   `method`, `talks`, `messages`, `dialog`, `history`, `send`, `read`, `sub`,
   `push`. Выписать URL/форму каждой операции, где прикладывается `Love.token`
   (поле формы/заголовок), пагинацию истории (по id или времени).
3. **`GET /talks/<passport_id>`** под сессией → подтвердить, что история не
   рендерится сервером; сохранить каркас.
4. **`talks dialogs` / `talks history <passport_id>`** → сырые JSON; обязательно
   снять **гостевой вариант** (без кук) — эталон для детектора протухшей сессии.
5. **`talks send`** второму аккаунту: форма запроса/успеха, id созданного
   сообщения, эхо в истории, реакция на `real_talk`/пустой текст/лимит длины.
6. **`Love.token`**: повторить `send`/`dialogs` со «старым» токеном через 15 мин
   / 2 ч / после нового логина; зафиксировать, обязателен ли предварительный GET
   страницы (тогда цена отправки — 2 запроса).
7. **Маппинг паспорт↔анкета:** есть ли в JSON диалога ссылка на анкету; если нет
   — `profile_id` заполняем лениво (или оставляем пустым, ссылку на
   `/talks/<passport_id>`).
8. **Проба на бан:** гонять `dialogs` интервалом 30–60 мин, следить за 403 и за
   тем, нужен ли cookie jar для кук DDoS-Guard (как в `genderClient`).

**Успех Ф0** = таблица §2 заполнена по 4 операциям + гостевой ответ; известно
поведение токена; санитизированные фикстуры лежат; с §3 снята пометка ⚠.

---

## 6. Что нужно от пользователя

- Живая сессия админа в БД (`/login` в РюмкинЪ) + запуск с российского IP.
- **Второй аккаунт на сайте** (или согласный собеседник) для теста отправки —
  self-messaging в talks, вероятно, недоступен.
- Подтверждение приватности — **дано: только метаданные** (`store_text=false`),
  `retention_days` = 30 по умолчанию.
- Решение по `mark_read` — **дано: не отмечать** (поллинг по курсору списка).

---

## 7. Открытые вопросы (решить по ходу)

1. Срок жизни `Love.token`, привязка к странице/сессии — нужен ли GET
   авторизованной страницы перед каждой отправкой.
2. push-stream: URL подписки, поведение висящего соединения под DDoS-Guard,
   ротация `<hash>`; такое соединение должно идти **мимо** лимитера.
3. Медиа: фото ссылкой/заглушкой (MVP) или скачиванием через `FetchMedia` и
   отправкой в мессенджер (умеем).
4. `real_talk`: что вернёт сайт при ответе «нереальному» пользователю — нужен
   читаемый текст ошибки.
5. Системные события в ленте talks (подарки, «экспресс», симпатии) — фильтровать
   или показывать; эхо своих исходящих (написанных с сайта) в мессенджер — надо ли.
6. Несколько сессий одного человека / несколько аккаунтов мессенджера на одну
   анкету (актуально для Ф6).

---

## 8. Ссылки

- `go/internal/love/testdata/love.ngs.ru.mhtml` — снимок главной под логином
- `/static/js/min/talks.js?v=<Love.currentVersion>` — логика мессенджера (скачать в Ф0)
- `s.ngs.ru/ngs/jsonrpc.min.js` — JSON-RPC клиент сайта
- `briefs/max-messenger-gate.md` — образец фазировки и Ф0-спайка
- Established-pattern: `internal/dmbot/logic.go`, `internal/bridge/core.go`,
  `internal/maxx/updates.go`, `cmd/lovegw/main.go:174` (`runDaemon`)
- nginx push-stream module — механика реалтайм-канала

---

## 9. Текущее состояние проекта

- Гейт MAX завершён (`c571023`): MAX — **полноценный ровня Telegram** (зеркало +
  мост + РюмкинЪ-MAX через один long-poll `mx.Dispatch`). Ядра `dmbot.Logic` и
  `bridge.Core` уже мессенджер-агностичны — talks ложится тем же лекалом.
- `store` на `user_version 4`; `message_targets` универсален (TEXT-id) — v5
  ложится аддитивно. В `sessions` нет site-идентичности (добавляем в v5).
- `love.Client` умеет куки per-request; **JSON-хелпера нет** (только HTML+form) —
  добавляем `getJSON`/`postJSON`. Селекторы/эндпоинты — единый const-блок с
  дисциплиной typed-ошибки (`MarkupError` → аналог `SchemaError` для JSON).
- `alerter` в `mirror` unexported — выносим в `internal/alerts` для повторного
  использования поллером talks.
- ⚠ Untracked `go/config.max-spike.json` и `testdata/love.ngs.ru.mhtml` содержат
  живые секреты и **не** покрыты `.gitignore` — закрыть до коммитов (§5).
