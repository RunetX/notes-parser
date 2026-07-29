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
- One-off import of legacy JSON state (notes / subscribers) into SQLite:
  `go run ./cmd/lovegw import ...` — idempotent (`INSERT OR IGNORE`).
- Windows: `start.bat` / `stop.bat` / `status.bat` / `restart.bat` launch/stop
  the daemon (write-through SQLite is crash-safe, so a hard kill is fine).
- Deploy: container image via `go/Dockerfile` (multi-stage, `CGO_ENABLED=0` →
  `distroless/static`, ~23 MB; tzdata baked in via `time/tzdata`). `deploy/` has
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
    replies → `bridge.Core`, dialog messages → `dmbot.Logic`. With
    `messengers.max.dm_token` set, DMs (dialogs, talks, alerts, subscriber
    notifications) run on a separate bot — same split as Telegram's
    poster/РюмкинЪ pair; without it one MAX bot covers channel, chat and DMs.
  - `mirror` — feed watcher + one goroutine per active note with an adaptive
    poll interval; consumes a list of `Sink`s (fan-out: Telegram and MAX can
    mirror in parallel, each with its own thread per note via
    `message_targets`). A sink implementing `ThreadStarter` opens its own
    discussion thread (MAX has no native channel comments — the bot posts a
    copy of the note into the discussion chat itself, retried every poll
    cycle). A note the site marks «не актуальна» (comments
    frozen) is archived early after a final comment flush — the only signal for
    that state is the feed link text, so it's a soft optimization (falls back to
    the week-based archival on wording drift). `mirror.Config.AlertSend` DMs the
    admin after 3 consecutive markup-drift or 403 failures and again on recovery.
  - `bridge` — reply→site comment: messenger-agnostic `Core` (at-most-once
    via `processed_replies`, per messenger) + the Telegram handler with
    auto-forward capture (linking a channel post to its discussion thread).
  - `dmbot` — РюмкинЪ; messenger-agnostic dialog engine `Logic` (state in
    `dialog_states`, transport behind an interface — Telegram wrapper here,
    MAX goes through `maxx.Mirror`). Commands: `/login`, `/add_note`,
    `/add_anonymous_note`, `/status`, `/subscribe`, `/unsubscribe`, `/mysubs`.
  - `legacy` — one-shot importer of old JSON state.
- Reply→site and note-post reuse saved cookie sessions; a 401/403 marks the
  session invalid and DMs the user to re-`/login`. Admin alerts require
  `admin_tg_user_id` set in config.

## Config & network

- Config `go/config.json` (gitignored, template `go/config.example.json`);
  tokens can come from env `LOVEGW_MIRROR_TOKEN` / `LOVEGW_DM_TOKEN` /
  `LOVEGW_MAX_TOKEN` / `LOVEGW_MAX_DM_TOKEN` / `LOVEGW_DB_PATH` /
  `LOVEGW_TG_PROXY`. The `messengers`
  section gates which sinks run (`max` / `telegram`, each with `enabled`);
  legacy flat `mirror_bot`/`dm_bot` configs still load as telegram-only.
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
