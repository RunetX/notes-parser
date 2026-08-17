# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

`lovegw` is a Telegram bot (Go, module `lovegw`, everything under `go/`) that
mirrors the "notes" (заметки) section of the dating site love.ngs.ru into
Telegram channels, and bridges interaction back: logged-in Telegram users reply
in the discussion group and the bot posts their reply as a comment on the site
using their saved cookie session. A second bot, **РюмкинЪ**, runs in private
chats for site login, posting notes, and keyword subscriptions.

Scraping is done with `net/http` + goquery against the site's HTML (CSS selectors
like `.lv-notes__note-item`, `.lv-note__comment-item`). The project replaced an
earlier Python prototype, which has been removed — recoverable from git history
if ever needed. Conventions: code identifiers are English; comments, commit
messages, and all user-facing bot strings are in Russian.

## Build / test / run

- Build/test: `cd go && go build ./... && go vet ./... && go test ./...`
  (`-race` needs cgo, run it on Linux/CI).
- Run daemon: `go run ./cmd/lovegw run [-seed]` — mirror + reply bridge + DM bot
  under one errgroup. `-seed` on first run records currently-visible notes
  **without** posting them (avoids a burst at cutover; posts only notes that
  appear afterwards).
- Diagnostics: `go run ./cmd/lovegw doctor [-post-test]` checks
  config/DB/site/tokens/queue; `-post-test` verifies the channel→auto-forward
  chain with a silent self-deleting message (safe on the live channel).
- Debug crawl: `go run ./cmd/lovegw crawl notes` / `crawl comments <note_id>`;
  `-save-html <dir>` records real pages as parser fixtures into
  `internal/love/testdata/` (fixtures `notes_feed.html` and
  `comments_312696.html` are real recorded pages; re-record with the same
  command on markup drift).
- Rescue a note the feed scan missed: `go run ./cmd/lovegw pull [-db path] <id>`
  (заводит заметку по прямому id; `-full` дотягивает весь тред в древовидном
  виде и возвращает архивную заметку в отслеживание). Безопасно при работающем
  демоне — постит он сам.
- Weekly digest: `go run ./cmd/lovegw digest [-week N] draft` строит черновик
  недели и материалы для LLM-редактуры (`<dir(db)>/digest/`); админ заполняет
  `<!-- LLM:… -->`-рубрики из materials.md, `digest preview` рендерит выпуск
  per-messenger, `digest publish` постит в каналы (`-force` — «сухой» выпуск
  без LLM-рубрик). Публикация идемпотентна и резюмируема через
  `message_targets` (kind `digest`) — безопасно при работающем демоне
  (поллинг не поднимается).
- Амвон (`pulpit`): публикует только демон, а руками —
  `go run ./cmd/lovegw pulpit draft <note_id>` (черновик реплики по заметке, на
  сайт ничего не уходит — стенд для правки промпта) и `pulpit status`. Тумблер
  «работает сейчас» переключается в ЛС командой `/pulpit` (админская, в меню
  команд не значится).
- Watch moderation live: `go run ./cmd/lovegw modwatch [-db path] watch` — пишет в
  отдельную `modwatch.db` моменты, которых нет в архиве: заметка исчезла из ленты,
  комментарий исчез из треда, появилась иллюстрация, закрыли комментарии. Дальше
  `modwatch report` сверяет эти минуты с присутствием людей (кто писал рядом) и
  сравнивает с контрольными окнами того же часа суток; `modwatch events` —
  сырой список, `modwatch status` — наполнение БД, `modwatch bans` — окна
  суточных запретов, выведенные из ритма жертвы (кандидаты, не находки: слепой
  поиск не отличает запрет от обычной паузы, см. шапку `bans.go`),
  `modwatch guests watch|log` — визиты в СВОЮ анкету под своей сессией
  (`guests log -near "2026-08-12 19:38"` даёт список тех, кто заходил вокруг
  момента действия), `modwatch activity watch|scan|report|log` — присутствие
  людей на сайте, детектор запретов в «Заметки» (`activity report`: «молчит, но
  ходит» = закрыли, «молчит и не заходит» = ушёл; `activity log -near` — кто был
  на сайте вокруг момента действия, включая тех, кто ничего не писал). Время
  везде новосибирское, как на сайте. Сайт только читается, с работающим демоном
  всё совместимо (нужен RU-IP); боевую БД трогает лишь `activity` — и только на
  чтение реплик, чтобы набрать круг наблюдения (`-source`; ключ шифрования ему
  не нужен, сессий он не касается).
- Service site accounts (a second, technical profile: backup access for
  authenticated crawls and rare hand-made comments):
  `go run ./cmd/lovegw account login -name reserve` (логин/пароль со stdin),
  `account list|check|forget`, `account cookie -name reserve` (заголовок Cookie
  локальным скриптам — пишет только в пайп),
  `account say -name reserve -note <id> [-reply <comment_id>] "текст"` —
  комментарий на сайт с подтверждением; печатает id своей реплики. Живут в
  своей `accounts.db` (пакет `acct`), ботам не видны. `-account имя[,имя]` у
  `personas gender` — первый живой аккаунт из списка, это и есть резерв.
- Session cookies at rest: `go run ./cmd/lovegw secrets keygen` → ключ в
  `LOVEGW_SECRET_KEY` → `secrets encrypt` шифрует и `sessions`, и `accounts`
  (идемпотентно; `-old-key-env` — ротация), `secrets status` — что открыто, что
  зашифровано. **После шифрования ключ обязателен**: любая команда, открывающая
  боевую БД, падает, если записи не открываются (иначе демон молча выдал бы
  всем «сессия истекла»). Без ключа всё работает как раньше — куки лежат
  открыто.
- One-off import of legacy JSON state (notes / subscribers) into SQLite:
  `go run ./cmd/lovegw import ...` — idempotent (`INSERT OR IGNORE`).
- Windows: `start.bat` / `stop.bat` / `status.bat` / `restart.bat` launch/stop
  the daemon (write-through SQLite is crash-safe, so a hard kill is fine).
- Deploy: container image via `go/Dockerfile` (multi-stage, `CGO_ENABLED=0` →
  `distroless/static`, ~100 MB: a ~23 MB binary plus a static `ffmpeg` copied in
  for ASR; tzdata baked in via `time/tzdata`). `deploy/` has
  `docker-compose.yml` + systemd unit + runbook; config mounts as `/config.json`,
  state in `deploy/data` bind-mounted to `/data` (owned by uid 65532 — distroless
  runs as `nonroot`), secrets via env
  (`LOVEGW_MIRROR_TOKEN`/`LOVEGW_DM_TOKEN`/`LOVEGW_TG_PROXY`). Compose runs **four**
  services off one image: `lovegw` (the daemon), `modwatch` (the watcher, its
  own `data/modwatch.db`, site read-only), `guests` (визиты в анкету
  владельца: ходит под его сессией из `lovegw.db`, пишет в ту же
  `modwatch.db`) и `activity` (присутствие людей на сайте: круг берёт из
  `lovegw.db` только чтением реплик, ходит анонимно, пишет в `modwatch.db`).
  A named volume is only needed on a
  Windows host, where bind-mount corrupted the DB; on Linux the files sit on the
  host, which makes a backup a plain `sqlite3 .backup`. With MAX enabled, the
  Минцифры PEMs must be in `go/internal/maxx/cacert/` **before** the build — the
  distroless image carries no Russian root CA.
- The site is behind DDoS-Guard geoblocking — non-RU IPs get 403, so crawl/run
  (and the deployed daemon) must run from a Russian IP. Telegram Bot API, blocked
  from inside Russia, is routed through a SOCKS5 proxy (`telegram_proxy`); the
  site stays direct.

## Architecture

- **Storage is SQLite** (`modernc.org/sqlite`, CGo-free) — the single source of
  truth; every write is write-through, so state survives `kill -9`. Schema in
  `internal/store/schema.sql`, versioned via `PRAGMA user_version` (migrations
  in `internal/store/migrate.go`). Since v4, message ids live in
  `message_targets` keyed by `(messenger, kind, ref_id)` with TEXT ids (MAX
  uses string mids); user tables carry a `messenger` column. Telegram values
  are mirrored write-through into the legacy `tg_*` columns for one release.
  Since v7 `subscriptions` is typed — `kind ∈ {keyword, author_notes,
  note_comments}` + `target` (word / `notes.author_id` / `notes.id`), row ids
  preserved across the migration because they live in the payload of «✖»
  buttons in users' chat history. `AddSubscription` enforces
  `SubscriptionLimit` (50 per user, all kinds together) inside a transaction —
  one place covers all four entry points. Since v8 `sessions` carries
  `talks_delivery` / `talks_asked_at` — в какой мессенджер носить личные
  сообщения (`internal/store/delivery.go`). Since v9 there are `pulpit_comments`
  / `pulpit_replies` (амвон, `internal/store/pulpit.go`; FK на `notes` у них
  намеренно нет — свой обход видит заметку раньше зеркала) и `settings` —
  рантайм-флаги вида `pulpit.enabled`: **только флаги, никогда секреты** (те
  живут в env и в шифрованных колонках `sessions`/`accounts`). Since v10
  `sessions.talks_scan` — согласие на чтение личной переписки; пустое значение
  означает «не читаем» (молчание согласием не считается), а миграция переносит в
  него прежнее явное `talks_delivery='on'` и снимает `talks_asked_at` у всех
  остальных — вопрос стал другим. Согласие — свойство сайт-аккаунта, поэтому
  пишется во все сессии паспорта, а читается как «хоть одна `on`»
  (`store.ScanAllowed`): вошедший позже в другом мессенджере получает строку со
  значением по умолчанию, и уже данное согласие от этого пропадать не должно.
- **All site markup selectors live in one const block** in
  `internal/love/parse.go`; a required selector that matches nothing returns a
  typed `MarkupError` (markup-drift detection), while an empty comments page is
  legitimately empty. The site's comment anchor id is `anchor-<n>`.
- Packages:
  - `love` — site client, parsers, auth, cookie sessions.
  - `store` — SQLite; the single source of truth.
  - `tgx` — go-telegram/bot wrapper: per-chat limiters, 429 retry, HTML compose,
    media `file_id` cache.
  - `maxx` — MAX-side `Sink` over the official `maxbot` v2 SDK
    (`platform-api2.max.ru`): channel posting, manual "autoforward",
    reply-based comments, media upload with URL→token cache, per-chat limiters
    + 429 retry. TLS to MAX needs the Russian Trusted CA chain: official PEMs
    are `go:embed`-ded from `maxx/cacert/` when present at build time (see the
    README there), otherwise the host trust store is used. `updates.go` runs
    long polling (`GetUpdates(marker)`) and dispatches: discussion-chat
    replies → `bridge.Core`, dialog messages and button presses → `dmbot.Logic`.
    Нажатия (`message_callback`, ветка `dispatchCallback`): нажавшего берём из
    `callback.user` — в `update.user_id` SDK кладёт ПОЛУЧАТЕЛЯ сообщения, а
    `update.user` для этого типа не заполняет вовсе; пустой mid (сообщение
    удалили) — рабочий случай. Клавиатуры и правка сообщений — `keyboard.go`;
    снять одну клавиатуру, как `editMessageReplyMarkup`, в MAX нечем: и правка,
    и ответ на нажатие принимают тело сообщения целиком. The mirror bot
    always covers channel, discussion chat and DM commands; only the site's
    personal correspondence (talks) can move to a second bot via
    `messengers.max.talks_token` (the legacy `max.dm_token` is migrated to it
    on load).
  - `mirror` — feed watcher + one goroutine per active note with an adaptive
    poll interval; consumes a list of `Sink`s (fan-out: Telegram and MAX can
    mirror in parallel, each with its own thread per note via
    `message_targets`). A sink implementing `ThreadStarter` opens its own
    discussion thread (MAX has no native channel comments — the bot posts a
    copy of the note into the discussion chat itself). That call happens
    *before* the channel post so the «💬 Обсудить» button can deep-link to the
    note's branch (`maxx.MessageLink`: `https://max.ru/c/<chat_id>/<base64url
    of the mid's last 8 bytes>`); on failure it is retried every poll cycle and
    the button falls back to the chat invite link. The site's «не актуальна» mark is recorded in `comments_closed` as
    metadata only — it appears within minutes of publication while comments keep
    arriving, so it must never drive archival; notes are archived solely by the
    week-based `ShouldArchive` rule. A mirrored comment is posted as a reply to
    its addressee's message, so the messenger renders the original as a quote:
    the addressee is the «Ник, …» prefix (`love.AddressPrefix`) resolved against
    the note's already-mirrored commenters — the site's own `parent_id` points at
    the branch root and agrees with the addressee only a third of the time.
    No addressee (no prefix, unknown nick, or the note's own author) → reply to
    the thread root, as before; a rejected reply (message deleted) falls back to
    the root too, or the note's queue would stall forever.
    `mirror.Config.AlertSend` DMs the
    admin after 3 consecutive markup-drift or 403 failures and again on recovery.
    Subscriber DMs are described by `SubEvent` (subscription + note + optional
    comment + message ids) and delivered **only** through `Config.SubNotify` —
    the bot the user actually started; `Sink` has no `NotifySubscriber`, because
    a channel poster cannot DM anyone first. A new note fires
    `author_notes` subscriptions from inside `postNote`, right after
    `SetTarget(TargetNotePost)` — that is where the per-sink post id exists, and
    it gets seed-silence and retry-dedup for free. One comment sends **one** DM
    per person: matches collapse by user, `note_comments` beating `keyword`
    (the note was chosen deliberately, the word may have matched by accident).
    Archiving a note drops subscriptions to its comments — no further comments
    will ever arrive.
  - `bridge` — reply→site comment: messenger-agnostic `Core` (at-most-once
    via `processed_replies`, per messenger) + the Telegram handler with
    auto-forward capture (linking a channel post to its discussion thread).
    The auto-forward update arrives once and can be lost to a Bot API failure,
    stranding a note's comments in the queue forever (happened to note 312859
    on 3 Aug 2026), so there is a fallback: the first reply to the thread root
    carries that same forward in `reply_to_message`, and the link is recovered
    from it.
  - `digest` — еженедельный дайджест: слот выпуска (суббота 09:00 Нск, окно
    неделя-до-слота — слот задаёт и время публикации, И шов недели, поэтому
    он стоит в утренней тишине: вечер пятницы идёт на подъём к суточному пику
    21:00–22:00, и срез в 19:00 рассекал его пополам; обоснование замером —
    в комментарии `digest.DefaultTZ`), метрики рубрик по живой БД (заметка/спор/цитата недели,
    новые лица, сравнительные рекорды), черновик с маркерами
    `{note:ID|текст}` и LLM-плейсхолдерами + materials.md с промптами
    (полуручной цикл), рендер per-sink через опциональный интерфейс
    `Publisher` (реализован в tgx/maxx), сплит серии по 3500 видимых рун.
    Планировщик (`RunSchedule`, секция конфига `digest`, дефолт выключен) в
    слот выпуска строит черновик; при настроенной секции `llm` рубрики
    заполняет Claude (`GenerateEditorial`: structured outputs, ответ
    валидируется как правки админа; брак — до трёх попыток с причиной брака в
    промпте, дальше откат на полуручной цикл: вырожденный ответ бывает разовым,
    а ошибку запроса SDK ретраит сам и переспрашивать её незачем), а
    `auto_publish: true` сразу публикует выпуск через sinks. Иначе —
    ЛС админу и премодерация через CLI. Пропущенный слот догоняется в
    течение 48 ч, старше — пропускается.
  - `news` — внутренние новости проекта: текст админа уходит постом в каналы
    **мимо сайта** (заметки на love.ngs.ru не появляется, в `notes` ничего не
    пишется). Ввод — `/news` в ЛС командному боту, публикация идемпотентна по
    `message_targets` (kind `news`, ref_id — метка времени), так что при сбое
    одного мессенджера повтор досылает только его. Тред обсуждения новость не
    заводит: в Telegram комментарии всё равно появятся автофорвардом канала,
    но на сайт они не уйдут — `bridge` не опознаёт такое сообщение ни как
    заметку, ни как комментарий.
  - `pulpit` — «амвон»: собственная реплика владельца под каждой новой заметкой
    сайта, и по возможности первая. **Голос — прикольщик** в образе надзирателя
    из будки («Паноптикум» — ник владельца; образ идёт вскользь, иначе через
    неделю это одна и та же шутка, а сам ник живой и подставляется в промпт из
    `currentNick`): работа одна — рассмешить, и меняется форма, а не голос.
    Формы — это **приёмы стендапа** (обманутое ожидание, буквально, правило
    трёх, сравнение не туда, встречный вопрос, сценка, я тоже дурак, не тот
    эксперт), у каждого в промпте свой КОРОТКИЙ образец: длина примера задаёт
    длину ответа вернее инструкции. Набор менялся дважды по отзывам владельца:
    формы вида «неожиданный вывод» просили ЗАМЕТИТЬ смешное и давали наблюдение
    сверху (остроумно и не смешно), формы вида «довести до конца» чинили
    позицию, но по устройству длинные — выходили три абзаца, «слишком много
    нужно прочесть». Отсюда правило устройства: **сетап и панч, панч последним,
    ударное слово в конце, после панча НИЧЕГО** (ни пояснения, ни добивки), одно-два
    предложения, потолок 180 рун и 4 строки. Оттуда же запрет афоризма («дело не
    в X, а в Y», «тревожит не то, что…») — он в валидаторе, а не только в
    промпте. **Реплика пишется в два прохода**: черновик, затем правка
    (`punchUp`, свой системный промпт — вычеркнуть всё после панча, переставить
    ударное слово в конец, резать сетап; НИЧЕГО не добавлять, правленая реплика
    короче черновика); провал правки не отменяет реплику, уходит черновик. Первым голосом был проповедник, и он прожил ровно
    одну реплику: её сняли через 15 минут, владельцу дали сутки запрета (см.
    память `pulpit-first-comment-deleted-ban`) — судили не скорость и не формат,
    а сам тон нравоучения. Отсюда главное правило нового голоса, записанное в
    `quipSystem`: смешным должно быть ПОЛОЖЕНИЕ, а не человек — шутка над
    автором это то же нравоучение, только с ухмылкой. Имя «амвон» осталось за
    МЕСТОМ, с которого говорят (таблицы, конфиг, команда), а не за тем, кто на
    нём стоит. **Красная линия**: под настоящей бедой (смерть, болезнь, насилие,
    суицид) модель возвращает `skip=true`, строка уходит в `skipped:no_joke` — и
    отказ шутить не переспрашивают, в отличие от брака. Служба под общим
    errgroup демона, мессенджер-агностична. **Свой клиент сайта и свой обход
    ленты**: у общего `love.Client` лимитер один на всё, и заметка добиралась
    до зеркала за p90 = 619 с при медианных 164 с до первого чужого
    комментария; колбэк `mirror.Config.OnNewNote` остаётся страховкой, оба
    входа сходятся на `TryClaimPulpitNote`. **Тумблер живёт в БД**
    (`settings['pulpit.enabled']`), а не в конфиге: выключает автоматика (в
    контейнер `/config.json` монтируется снаружи), включает админ кнопкой
    `/pulpit`, и переключение обязано пережить рестарт. Однократность держится
    на состоянии: заметку занимает один INSERT, переход `queued → posting`
    пишется ДО отправки, и застрявшая в `posting` строка не переотправляется
    никогда — её судьбу решает верификация треда (своя реплика опознаётся по
    `AuthorID`). **Предохранитель** (`fuse.go`): запрет писать в «Заметки»
    ничего не убирает с площадки, анкета жива, сайт отвечает 200 — единственный
    след в том, что реплика не появилась в треде. Три таких промаха подряд
    (сбой загрузки, снесённая заметка и **не дошедший POST** промахом НЕ
    считаются) или две вычищенные реплики гасят фичу, диагноз подтверждается
    `love.ProfileControl`, админу уходит ЛС. Про POST правило оплачено случаем
    17.08.2026: сайт отвечал 500 на любой комментарий (включая чужие, через
    мост), тред при этом читался прекрасно — и «реплики нет» засчиталось
    промахом, хотя причина известна и невиновна. Поэтому сбой отправки пишется
    на строку причиной `send_failed` (состояние остаётся `posting`: сайт мог
    реплику и принять, ответив ошибкой), а `outcomeOf` считает такое отсутствие
    нейтральным — полосу оно не растит и не разрывает. **Автовосстановления нет**: срок запрета неизвестен,
    включать обратно — только руками. Ответы тем, кто ответил нам, — по монетке
    (`reply_probability`), и монетка бросается ровно один раз на реплику (PK по
    `pulpit_replies.reply_to_id`), иначе 15 % за десяток тактов превращаются в
    80 %. Обращение «Ник, » подставляет инструмент, а не модель.
    **Что площадка делает с текстом комментария** (замер 15.08.2026 по 61 177
    живым комментариям; знание нужно не только амвону): переносы строк работают
    — сайт делает `nl2br`, поэтому пустая строка между абзацами держится;
    пробелы схлопываются, и отступ живёт **только через NBSP** (его подставляет
    `normalize`, модель про это не знает — иначе начнёт сыпать U+00A0 в
    середину фраз); эмодзи сайт подменяет картинками `<img class="emojione">`;
    **BB-коды больше не работают** — `[b]`/`[i]` печатаются буквально (на 61 177
    комментариев ноль), HTML сайт экранирует сам; префикс обращения он выделяет
    жирным сам, и жирный для этого не нужен; длинное тире и кавычки-ёлочки есть
    у полупроцента комментариев, то есть выдают машину — `normalize` меняет их
    на дефис и обычную кавычку.
  - `chantext` — общее HTML-подмножество каналов (`<b>`, `<i>`, `<a href>`):
    валидация, видимая длина в рунах и обрезка без порчи разметки, бюджет
    сообщения 3500 рун. Общий для `digest` и `news`.
  - `asr` — автораспознавание голосовых в тредах: голосовое (и кружок) в
    группе обсуждения — а также запощенное в канал заметок, оно приходит
    автофорвардом (квота таких пишется на id канала) — расшифровывается и
    уходит реплаем на исходное сообщение; что делать с текстом, решает автор
    (ни кнопок, ни состояния). **Только Telegram**, и MAX не подключён
    СОЗНАТЕЛЬНО: там расшифровка встроена в клиент (кнопка «Т» и на канале, и
    в чатах, без всякой подписки), а в Telegram она только по Premium — то
    есть в MAX это был бы оплаченный дубль бесплатного. Не «доделывать»
    MAX-сторону. Побочные факты на случай пересмотра: у аудио-вложений MAX нет
    длительности (только url и token), то есть потолок и квота в секундах без
    скачивания файла не считаются, а нативная расшифровка приезжает в поле
    `transcription` схемы Bot API, которое SDK v2.2.4 теряет при разборе.
    Пакет messenger-agnostic (транспорт — замыкания `Fetch`/`Reply` в
    `asr.Job`), так что подключить мессенджер без своей расшифровки дёшево.
    Телеграм-сторона — `tgx.VoiceHandler` + `Mirror.DownloadFile`/`ReplyText`
    (хук ставится `SetVoiceHandler` до старта поллинга).
    `Service` — воркер-пул с очередью (хендлер апдейта только ставит задачу,
    переполнение — drop + лог) и защитой от расходов: потолок длительности,
    суточная квота в секундах на пользователя и кэш расшифровок по
    `file_unique_id` (таблицы `asr_transcripts`/`asr_usage`, миграция v6) —
    пересланное голосовое не оплачивается повторно. `Transcriber` — клиент
    Nexara (OpenAI Whisper-совместимый API, ретраи с backoff, `ErrAuth` на
    401/402 → разовый алерт админу); провайдер российский, поэтому идёт
    напрямую, мимо `telegram_proxy`. `Converter` — ffmpeg пайпом stdin→stdout
    (в образе статический бинарник, путь в `LOVEGW_ASR_FFMPEG`). При любом
    сбое в треде тишина и запись в лог; единственное сообщение об отказе —
    превышенный потолок длительности. `asr.enabled=false` — гейт как раньше.
  - `llm` — онлайн-клиент Claude API (официальный `anthropic-sdk-go`):
    `GenerateJSON` со structured outputs, дефолтная модель `claude-opus-5`,
    обработка refusal/обрыва; транспорт — `tgx.ProxyTransport(telegram_proxy)`
    (api.anthropic.com недоступен с RU-IP, ходим через тот же прокси, что и
    Bot API). Ключ — `llm.api_key` / env `LOVEGW_LLM_KEY`. Общий клиент для
    дайджеста и будущего поиска (C4).
  - `talks` — личная переписка сайта: один поллер под общим клиентом сайта
    фанит входящие ЛС в личку включённых мессенджеров, ответы реплаем/командой
    уходят на сайт. **Читаем только с согласия человека** (`delivery.go`,
    `store.ScanAllowed`): поллер ходит по сайту под его кукой, а
    `getMessagesHistory` помечает сообщения прочитанными (собеседник видит
    «просмотрено») и всё это время держит человека онлайн — не читать бота
    достаточным отказом не является, поэтому без согласия сайт-аккаунт не
    трогают вовсе (ни списка диалогов, ни истории, ни `SiteIdentity`). Гейт
    стоит в `plan()` **после** `live()`: отсеянного админом не спрашивают.
    Обратное направление (`send.go`) согласием не закрыто намеренно —
    отправляет человек сам, и на сайте от этого ничего не помечается.
    **Получатель у сайт-аккаунта ровно один**: то же чтение истории гасит
    непрочитанное, и второй сессии достанется пустота. Сессии связывает
    `sessions.site_passport_id` (нет — поллер снимает его сам на первом обходе,
    одна попытка за запуск: старые сессии его не хранят). Правило выбора —
    `store.PickDelivery`: явный выбор человека сильнее всего, пока его нет —
    самый свежий вход. **Вопрос человеку один, а не два**: кнопка «читать и
    присылать сюда» — это сразу и согласие, и выбор мессенджера, поэтому
    `Config.AskScan` (замыкание на `dmbot`, у поллера кнопок нет) заменил собой
    прежний вопрос «куда носить»; спрашивают один раз на сессию, отметка живёт в
    `talks_asked_at`, неотвеченный вопрос оставляет переписку непрочитанной.
    Недостижимость (заблокировал бота) снимает сессию с обхода только пока
    выбора не было: у выбравшего это «ЛС не доставляются», а не «отнесём в
    другой мессенджер». `talks.exclude_users` в конфиге — запрет админа поверх
    согласия: попавшего в список даже не спрашивают.
  - `dmbot` — РюмкинЪ; messenger-agnostic dialog engine `Logic` (state in
    `dialog_states`, transport behind an interface — Telegram wrapper here,
    MAX goes through `maxx.Mirror`). Commands: `/login`, `/add_note`,
    `/add_anonymous_note`, `/status`, `/subscribe`, `/unsubscribe`, `/mysubs`.
    Плюс `/delivery` (`delivery.go`) — личные сообщения сайта: читать ли их и
    куда носить. Три состояния экрана: чтения нет («📬 Читать и присылать
    сюда»), читаем и носим сюда («🚫 Не читать мою переписку»), читаем и носим
    в другой мессенджер (плюс «📬 Присылать сюда»). Текст в каждом состоянии
    называет цену — сайт помечает ЛС прочитанными и показывает человека в сети.
    Кнопки «🔕 Не присылать сюда» **больше нет**: согласие даётся кнопкой
    «читать и присылать сюда», а она гасит доставку остальным сессиям аккаунта,
    — значит отказ от доставки гасил бы последнюю живую и останавливал обход
    целиком, то есть делал ровно то же, что «не читать», только называясь
    иначе. Именно эта двусмысленность и была причиной завести отдельное
    согласие. Глагол `argDeliveryOff` при этом жив: кнопки прошлых релизов ещё
    висят в чатах. Выбор мессенджера исключающий, второй гасит
    `store.SetTalksDelivery` одной транзакцией по паспорту сайт-аккаунта — она
    же проставляет согласие. Команда есть и у бота переписки — ЛС доставляет
    именно он; тем же сообщением и теми же кнопками спрашивают согласия поллер
    (`AskTalksScan`) и сам `/login` — момент, когда человек отдал боту доступ к
    аккаунту, для этого вопроса и есть самый уместный.
    Плюс `/profile` (`profile.go`) — своя анкета на сайте: заблокировать и
    вернуть. Состояние **не хранится в БД** (анкету блокируют и на самом сайте,
    флаг бы соврал) — и показ, и применение читают его живьём, а после действия
    перечитывают: сайт отвечает 200 и на отказ, так что «состояние не
    изменилось» — единственный надёжный признак неудачи. Кнопка на экране —
    ровно та, что стоит на сайте: подпись и поле формы приезжают со страницы
    настроек (`love.ProfileControl`), а они у сайта в двух состояниях разные.
    Блокировка идёт через подтверждение, разблокировка — сразу. Способность
    необязательная (`SiteProfile`, type-assertion по образцу `SiteIdentifier`):
    нет её у клиента — нет и команды ни в меню, ни в `/start`. У бота переписки
    команды нет: сайт ему не передаётся.
    Плюс админская `/news` (`SetNews`, пакет `news`): текст → превью →
    подтверждение кнопкой «Опубликовать» (слово «да» осталось запасным путём) →
    пост в каналы. Она видна только
    `messengers.<m>.admin_user_id`, в `/start` не значится, черновик ждёт
    подтверждения прямо в `dialog_states` (`news:<id>\n<html>`) и переживает
    рестарт; при сбое канала состояние остаётся под повтор.
    Плюс админская `/pulpit` (`pulpit.go`, `SetPulpit`): состояние амвона,
    счётчики, последняя реплика со ссылкой на анкор и кнопка включения/
    выключения. Так же не в меню команд и не в `/start`. Пакет `pulpit` отсюда
    **не импортируется**: ручке нужны только «переключить» и «показать отчёт»
    (интерфейс `PulpitControl`), а отчёт собирает сама служба — иначе
    диалоговому ядру пришлось бы знать её состояния и предохранитель.
    Выключение мгновенное, включение после срабатывания предохранителя — через
    подтверждение с текстом причины (образец `askProfileBlock`).
    Кнопки (`callback.go`, общие типы в листовом пакете `kbd`): `/start` даёт
    главное меню, `/add_note` спрашивает авторство (`await_note_kind`),
    `/mysubs` — список с «✖» на каждую подписку и «Добавить» (слово спрашивается
    диалогом, `await_subscription`: в payload оно не влезет, поэтому кнопка
    несёт id строки `subscriptions`), `/talks` — кнопки «открыть» с
    перелистыванием по 8. У каждого приглашения есть «Отмена». Меню команд
    мессенджера публикует `PublishCommands` на старте демона (Telegram
    `setMyCommands`, MAX `PATCH /me/commands`) — после подключения talks-роутера,
    от него зависит, попадут ли в список `/talks`, `/talk` и `/delivery`;
    админская `/news` в меню не значится. Правило правки сообщений: главное меню — пульт, его не
    затирают никогда (список диалогов из меню приходит новым сообщением, а
    перелистывание уже правит его), одноразовый выбор превращается в
    приглашение, список подписок перерисовывается на месте.
    Подписка по заметке (эпик B) заводится из-под поста канала; вход несёт id
    заметки, а вид («✍️ На автора» / «💬 На эту заметку») спрашивается уже в
    ЛС — `offerSubscribe`, единая дорога для трёх входов. В Telegram это
    **ссылка в подвале поста**, `t.me/<РюмкинЪ>?start=sub_<id>`
    (`kbd.StartSub`, разбор в ветке `/start`). Deep-link, а не callback: постер
    и РюмкинЪ — разные боты, постер в ЛС писать не может, а переход заодно
    стартует бота у того, кто его не запускал. И **именно ссылка, а не
    inline-кнопка**: своя клавиатура у поста вытесняет родную кнопку
    «Комментарии» — Telegram рисует их в одном месте и показывает что-то одно,
    а вход в обсуждение нужнее (проверено на живом канале 11.08.2026). В MAX
    бот один и родных комментариев у канала нет, поэтому там callback-кнопка
    рядом с «💬 Обсудить». У анонимной заметки вход есть (подписка на её
    комментарии осмысленна), но вариант «на автора» не предлагается.
    Нажатия разбирает `HandleCallback` —
    зеркало `HandleText`: таблица глаголов (что ответить, кому доступен, где
    доступен, что делает), payload `1:<verb>[:<arg>]` только ASCII и не длиннее
    64 байт (предел Telegram), ответ мессенджеру всегда в роутере и **до**
    работы — публикация в каналы идёт секунды, за это время нажатие протухнет.
    Свойство `public` (единственный такой глагол — `kbd.VerbSubscribe`)
    разрешает нажатие вне диалога; его спрашивает `maxx` через
    `AllowsOutsideDialog`, и там же гасит mid — сообщение под кнопкой это чужой
    пост канала, править его нельзя. Кнопка «🔕 Отписаться» в ЛС-уведомлении
    (`UnsubKeyboard`, глагол `unsub1`) отвечает только тостом и сообщение не
    правит: текст со ссылкой ещё нужен, а в MAX снять одну клавиатуру, не
    переписав тело целиком, нечем.
    **Идемпотентность держится на `dialog_states`**, отдельной таблицы
    обработанных нажатий нет и не нужно: у нажатия нет побочного эффекта,
    который нельзя вывести из состояния, а дедуп по payload сломал бы штатный
    повтор `/news` после сбоя канала. Единственная настоящая гонка —
    параллельные апдейты Telegram (`go-telegram/bot` запускает обработчик в
    горутине) на read-then-write внутри `news.Publish`; закрыта мьютексом по
    пользователю (`lockUser`), тест на два одновременных нажатия это стережёт.
    `NewTalksLogic` is the second role — a talks-only bot (`/talks`, `/talk`,
    `/delivery`, reply→site delivery, admin alerts) that keeps its own `dialog_states`
    namespace (`<messenger>:talks`) so a stuck `pm:<id>` cannot break the
    command bot; sessions, subscriptions and peers stay keyed by messenger, so
    it sees the login made in the command bot. The command bot keeps a
    reply-only router (`SetReplyRouter`) for DMs it delivered before the split.
  - `modwatch` — наблюдатель за действиями модерации (своя `modwatch.db`, схема
    v1). Поля «модератор» на сайте нет, а по речи опознаются только болтливые;
    действие же видно всегда, но архив его не хранит — это снимок уцелевшего.
    Сборщик опрашивает ленту и свежие треды и пишет ЧТО и КОГДА произошло
    (`events`: note_gone / note_returned / comment_gone / image_added /
    comments_closed / note_published / nick_changed) вместе с окном неопределённости
    `[prev_seen, detected]` и контекстом действия: возраст объекта, тишина в
    треде перед ним и число известных реплик (миграция v2 + расчёт на лету для
    старых записей). Контекст нужен, чтобы отделять руку от автоматики:
    закрытие комментариев сайт ставит и сам, но проверка показала, что ни
    фиксированного таймера, ни порога по числу реплик нет (наблюдённые закрытия
    на 3ч50м/181 реплике и 6ч55м/389; в архиве доля закрытых 75–85 % во всех
    корзинах), поэтому вид не выкидывается, а фильтруется `-age-min/-age-max`. Ключевое правило детекции — **охват**: исчезновение
    считается удалением только у объекта с id больше самого старого из
    присутствующих сейчас, иначе уход за нижний край страницы был бы принят за
    снос. **Публикация на сайте устроена так:** простой текст выходит сразу, а
    заметка с картинкой — только после премодерации; если автор дописал картинку
    к уже опубликованному тексту, заметка уезжает на проверку, ПРОПАДАЯ из ленты,
    и возвращается одобренной (`note_returned`). Поэтому `note_gone` с
    последующим возвратом — не действие модератора, и `Analyze` такие события
    отбрасывает (`dropReturned`), а `image_added` — действие АВТОРА, из набора
    `moderationKinds` исключено. `Analyze` — case-control: для каждого события окно присутствия
    сравнивается с тем же окном, сдвинутым на целые сутки (тот же час — та же
    посещаемость), поправка на «активные всегда онлайн» встроена в конструкцию;
    дисперсия считается по сглаженной доле, иначе «был только при действиях» дал
    бы z = 0. Единица наблюдения — **окказия** (заметка + такт опроса), а не
    событие: чистку треда наблюдатель видит разом и штампует всем пропажам один
    `detected_at`, поэтому независимыми их считать нельзя (иначе z растёт как
    √N — на 11.08.2026 это 53 события в 33 окказиях). Контрольные окна
    подбираются не только по часу суток, но и **по числу реплик**: комментарий
    удаляют в живом треде, и без выравнивания окна действий вдвое люднее
    контрольных, то есть нулевая гипотеза уезжает с ×1 на ×1.9. Метод слеп к
    модератору, который не комментирует; верхушку z нельзя читать как список
    имён — порог не откалиброван на множественную проверку, а анкеты, живущие
    короче периода наблюдения, не попадают в контроль и получают значимость
    даром (см. память `modwatch-report-batch-artifact`).
    **Присутствие** (`activity.go` + `silence.go`, схема v4) — второй, отдельный
    от событий канал. Запрет писать в «Заметки» ничего не убирает с площадки и
    потому в `events` невидим, а это самое частое наказание; зато в анкете есть
    `last_activity` (`love.Activity`, мобильный vhost). Поле живое, наш
    анонимный просмотр его НЕ двигает (контроль: анкеты, прочитанные трижды,
    остались со старыми отметками) и «Приватность» его не прячет — под ней сайт
    лишь показывает людям другое поле, `last_activity_for_hide`. Правило
    `ClassifySilence` решает по СВЕЖЕСТИ последнего захода (`Fresh`, сутки), а
    не по хвосту после последней реплики: перестал заходить — «ушёл», а если
    перед этим ещё ходил (`After` ≥ `Margin`) — «ушёл позже». Первая версия
    смотрела как раз на хвост и записала в жертвы Игоря u1514601, который просто
    ушёл на полдня позже, чем замолчал. **Но свежей отметки мало**: у
    вернувшегося из отъезда она такая же, поэтому «запрет?» ставится, только
    если след покрывает `MinSeenDays` (2) РАЗНЫХ суток молчания, иначе «мало
    данных». Это цена ошибки с Актрисой u1431505: её признали закрытой по одному
    снимку, а она вернулась в тот же вечер, 13.08 в 21:27 — снимок поймал первый
    день на площадке после отсутствия (сутки считаются от последней реплики,
    `seenDays`, — так не нужен пояс площадки). Третий порог — `MinMissed`:
    сколько реплик человек не написал против своего же темпа, иначе список
    забивают редкие комментаторы (темп средний за окно, поэтому у затухавшего
    постепенно он завышен — наказание выглядит обрывом на полном ходу).
    Отличить запрет от «читаю, но не пишу» отчёт не может; доказывает
    возвращение — наказание кончается ровно на сроке. `ActivityWatcher` обходит круг активных
    комментаторов (набирается `RosterSource` — по зеркалу, реже по своим
    репликам) порциями по очереди `checked_at`, анонимно и строгим темпом
    (клиент как у `personas gender`: мобильный vhost, jar, джиттер); сбой такта
    его обрывает — 403 приходит волной. Таблица `activity` копит отметки у себя,
    потому что сайт хранит только последнее действие: это заодно даёт
    присутствие тех, кто ничего не пишет, — ту самую слепоту `Analyze`.
  - `secret` — шифрование значений на диске (AES-256-GCM из stdlib, формат
    `enc:v1:<base64 nonce‖ciphertext>`). Ключ живёт ВНЕ базы (env
    `LOVEGW_SECRET_KEY` / `secret_key_file`) — в этом весь смысл: в копии БД его
    нет. Защищает копии и бэкапы (боевую базу бэкапят `sqlite3 .backup`, и её
    копия уезжает на рабочую машину), а не хост, где ключ лежит рядом. AAD
    привязывает шифротекст к строке (`sessions:telegram:<id>`,
    `accounts:<имя>`), поэтому переставить чужую сессию в свою строку нельзя.
    Значение без префикса читается как есть — так живут записи, сделанные до
    включения шифрования. Точек шифрования ровно две:
    `store.SessionCookies` и `store.UpsertSession`; всё остальное (bridge,
    dmbot, talks, profile) ходит через них и о шифровании не знает.
  - `acct` — сервисные аккаунты сайта: технические сессии в своей `accounts.db`
    для обходов под авторизацией и редких ручных комментариев. **Не в
    `sessions`** намеренно: `store.TalksOwners` берёт ВСЕ валидные сессии без
    разбора мессенджера, и служебная строка немедленно попала бы в обход ЛС,
    начав гасить непрочитанное на сайте. Отдельный файл к тому же можно
    принести на рабочую машину без боевой БД, где лежат живые куки
    пользователей. Формат кук — общий с `sessions` (`love.CookiesToJSON`),
    строка остаётся взаимозаменяемой.
  - `legacy` — one-shot importer of old JSON state.
- Reply→site and note-post reuse saved cookie sessions; a 401/403 marks the
  session invalid and DMs the user to re-`/login`. Admin alerts require
  `admin_tg_user_id` set in config.

## Config & network

- Config `go/config.json` (gitignored, template `go/config.example.json`);
  tokens can come from env `LOVEGW_MIRROR_TOKEN` / `LOVEGW_DM_TOKEN` /
  `LOVEGW_TG_TALKS_TOKEN` / `LOVEGW_MAX_TOKEN` / `LOVEGW_MAX_DM_TOKEN` /
  `LOVEGW_MAX_TALKS_TOKEN` / `LOVEGW_DB_PATH` / `LOVEGW_TG_PROXY` /
  `LOVEGW_LLM_KEY` (Claude API для LLM-рубрик дайджеста) /
  `LOVEGW_SECRET_KEY` (ключ шифрования кук; или `LOVEGW_SECRET_KEY_FILE`) /
  `LOVEGW_ACCOUNTS_DB` (база сервисных аккаунтов; пусто — `accounts.db` рядом с
  боевой БД) /
  `LOVEGW_ASR_*` (`ENABLED`, `PROVIDER`, `BASE_URL`, `API_KEY`, `FFMPEG`,
  `MAX_DURATION_SEC`, `USER_DAILY_LIMIT_SEC`, `CONCURRENCY`, `TIMEOUT_SEC` —
  секция `asr`; единственные env с числами/булевыми, разбор в `config.Load`) /
  `LOVEGW_PULPIT_ENABLED` / `LOVEGW_PULPIT_MODEL` /
  `LOVEGW_PULPIT_REPLY_PROBABILITY` (секция `pulpit`: `enabled` —
  гейт самой службы, `owner_profile_id` — анкета владельца, дальше свежесть,
  бюджеты, пороги длины и потолки ответов; «работает ли она сейчас» — не здесь,
  а в `settings['pulpit.enabled']`).
  The `messengers`
  section gates which sinks run (`max` / `telegram`, each with `enabled`);
  legacy flat `mirror_bot`/`dm_bot` configs still load as telegram-only.
- Bot roles per messenger: everything that predates talks stays on the original
  bots (Telegram poster + РюмкинЪ, MAX mirror bot), and `talks_token` moves only
  the site's personal correspondence — plus admin alerts — to a separate bot.
  Without `talks_token` nothing changes: talks runs on РюмкинЪ / the MAX mirror bot.
- Network split: love.ngs.ru needs a Russian IP (403 otherwise), but Telegram's
  API is blocked from inside Russia. A box that reaches both needs nothing
  special. For split networks, `telegram_proxy` in config
  (`http`/`https`/`socks5://…`) routes only the Bot API through a proxy while the
  site goes direct — built in `internal/tgx/proxy.go`, wired into both bots and
  `doctor`.

## Secrets warning

Real credentials live only in the local working copy: bot tokens in
`go/config.json`; live user session cookies in the SQLite DB (`data/`,
`sessions` table) and technical ones in `accounts.db`. All are gitignored —
never print, commit, or copy these values into new files, examples, or logs, and
never weaken the project section of `.gitignore`. (Retired Python state such as
`config.json` and `sessions/` may still linger locally; it is gitignored too.)

Куки шифруются на диске, если задан `LOVEGW_SECRET_KEY` (пакет `secret`). Ключ
хранить **отдельно от бэкапов базы** — иначе шифрование бессмысленно: оно и
защищает ровно копии. Потеря ключа = потеря всех сессий (всем заново `/login`,
сервисным аккаунтам — `account login`). Единственные две команды, которые
намеренно выводят секреты: `secrets keygen` (новый ключ — его надо как-то
передать оператору) и `account cookie` (заголовок для локальных скриптов, и он
отказывается писать в терминал — только в пайп).
