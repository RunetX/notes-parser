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
- One-off import of legacy JSON state (notes / subscribers) into SQLite:
  `go run ./cmd/lovegw import ...` — idempotent (`INSERT OR IGNORE`).
- Windows: `start.bat` / `stop.bat` / `status.bat` / `restart.bat` launch/stop
  the daemon (write-through SQLite is crash-safe, so a hard kill is fine).
- Deploy: container image via `go/Dockerfile` (multi-stage, `CGO_ENABLED=0` →
  `distroless/static`, ~100 MB: a ~23 MB binary plus a static `ffmpeg` copied in
  for ASR; tzdata baked in via `time/tzdata`). `deploy/` has
  `docker-compose.yml` + systemd unit + runbook; config mounts as `/config.json`,
  DB in the `/data` volume, secrets via env
  (`LOVEGW_MIRROR_TOKEN`/`LOVEGW_DM_TOKEN`/`LOVEGW_TG_PROXY`).
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
    replies → `bridge.Core`, dialog messages → `dmbot.Logic`. The mirror bot
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
  - `bridge` — reply→site comment: messenger-agnostic `Core` (at-most-once
    via `processed_replies`, per messenger) + the Telegram handler with
    auto-forward capture (linking a channel post to its discussion thread).
    The auto-forward update arrives once and can be lost to a Bot API failure,
    stranding a note's comments in the queue forever (happened to note 312859
    on 3 Aug 2026), so there is a fallback: the first reply to the thread root
    carries that same forward in `reply_to_message`, and the link is recovered
    from it.
  - `digest` — еженедельный дайджест: слот выпуска (пятница 19:00 Нск, окно
    неделя-до-слота), метрики рубрик по живой БД (заметка/спор/цитата недели,
    новые лица, сравнительные рекорды), черновик с маркерами
    `{note:ID|текст}` и LLM-плейсхолдерами + materials.md с промптами
    (полуручной цикл), рендер per-sink через опциональный интерфейс
    `Publisher` (реализован в tgx/maxx), сплит серии по 3500 видимых рун.
    Планировщик (`RunSchedule`, секция конфига `digest`, дефолт выключен) в
    слот выпуска строит черновик; при настроенной секции `llm` рубрики
    заполняет Claude (`GenerateEditorial`: один запрос со structured outputs,
    ответ валидируется как правки админа — брак откатывает на полуручной
    цикл), а `auto_publish: true` сразу публикует выпуск через sinks. Иначе —
    ЛС админу и премодерация через CLI. Пропущенный слот догоняется в
    течение 48 ч, старше — пропускается.
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
  - `dmbot` — РюмкинЪ; messenger-agnostic dialog engine `Logic` (state in
    `dialog_states`, transport behind an interface — Telegram wrapper here,
    MAX goes through `maxx.Mirror`). Commands: `/login`, `/add_note`,
    `/add_anonymous_note`, `/status`, `/subscribe`, `/unsubscribe`, `/mysubs`.
    `NewTalksLogic` is the second role — a talks-only bot (`/talks`, `/talk`,
    reply→site delivery, admin alerts) that keeps its own `dialog_states`
    namespace (`<messenger>:talks`) so a stuck `pm:<id>` cannot break the
    command bot; sessions, subscriptions and peers stay keyed by messenger, so
    it sees the login made in the command bot. The command bot keeps a
    reply-only router (`SetReplyRouter`) for DMs it delivered before the split.
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
  `LOVEGW_ASR_*` (`ENABLED`, `PROVIDER`, `BASE_URL`, `API_KEY`, `FFMPEG`,
  `MAX_DURATION_SEC`, `USER_DAILY_LIMIT_SEC`, `CONCURRENCY`, `TIMEOUT_SEC` —
  секция `asr`; единственные env с числами/булевыми, разбор в `config.Load`).
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
`sessions` table). All are gitignored — never print, commit, or copy these
values into new files, examples, or logs, and never weaken the project section
of `.gitignore`. (Retired Python state such as `config.json` and `sessions/`
may still linger locally; it is gitignored too.)
