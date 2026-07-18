# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

A Telegram bot that mirrors the "notes" (заметки) section of the dating site love.ngs.ru into Telegram channels, and bridges interaction back: logged-in Telegram users can reply in Telegram and the bot posts their reply as a comment on the site using their saved cookie session. Scraping is done with `requests` + BeautifulSoup against the site's HTML (CSS selectors like `.lv-notes__note-item`, `.lv-note__comment-item`).

README.md: "Simple Telegram-bot with notes parser". Comments, commit messages, and all user-facing bot strings are in Russian (code identifiers are English). Region markers like `#Область ...` / `#Конец области` delimit sections — keep this convention when editing.

## Running

There are no tests, linters, or build tooling. Install dependencies with:

```sh
pip install -r requirements.txt
```

(`websockets` is not in requirements.txt — it is only needed by the local-only `messages.py` prototype.)

**Critical version constraint:** the code uses the synchronous v13-era `python-telegram-bot` API (`telegram.Bot(token=...)`, `bot.send_message(...)`, `bot.get_updates(offset)`, `telegram.constants.PARSEMODE_HTML`). v20+ is async-only and breaks every `bot.*` call.

Two daemons are meant to run as separate processes (each is a hand-rolled infinite polling loop with `time.sleep` throttling; SIGINT triggers a graceful exit that persists `notes.json`):

- `python poster.py` — main daemon: scrapes notes/comments from the site, posts to Telegram channels, mirrors Telegram replies back to the site, DMs keyword subscribers. Configured by `config.json`.
- `python ryumkin.py` — private-chat bot: handles DM commands `/start`, `/login`, `/add_note`, `/add_anonymous_note`, `/status`. On `/login` it authenticates against the site and pickles the user's cookie jar into `sessions/<telegram_user_id>.cookie`. Configured by `ryumkin.json`.

Utilities / legacy (deliberately untracked in git — they exist only in the local working copy; do not treat as entry points):

- `main.py` — legacy monolithic predecessor of poster.py + ryumkin.py, with config hardcoded as module constants. Kept for reference.
- `session_get.py` — one-shot manual test: posts a hardcoded comment to the site using a saved session, dumps the HTML response to `output/index.html`.
- `messages.py` — standalone asyncio WebSocket prototype listening to the site's message stream; not wired into anything.

## Architecture and state

```text
love.ngs.ru (HTML) --requests/BS4--> poster.py --python-telegram-bot--> TG channels
                                        |  state: notes.json (notes + TG message-id map)
Telegram replies --> poster.py --------/   posts comment to site via sessions/<uid>.cookie
Telegram DMs --> ryumkin.py --> site login --> saves sessions/<uid>.cookie
```

**All persistence is JSON files — there is no database.** `db.sqlite` is an empty leftover of a never-implemented migration, and `poster_db.py` is a byte-identical copy of `poster.py` (edit `poster.py`; don't let the `_db` name mislead you).

State files:

- `notes.json` — the core state: scraped notes/comments plus the mapping to Telegram message IDs (`tg_message_id`, `tg_discussion_id`). Models are built by `note_model()` / `comment_model()` in poster.py.
- `config.json` / `ryumkin.json` — per-daemon config: `basic_url`, `tg_token`, `tg_bot_name`, channel/chat IDs, `default_tg_userid_session`; poster.py additionally uses `notes_limit` / `notes_limit_delta`. Both are gitignored (they hold real tokens); committed templates: `config.example.json` / `ryumkin.example.json`.
- `subscribers.json` — `{key: keyword, value: telegram_user_id}` pairs; a scraped comment containing the keyword triggers a DM with a deep link.
- `sessions/*.cookie` — pickled `requests` cookie jars, one per Telegram user; loaded by `get_user_session()` to act on the site as that user.
- `users.json`, `yesterday/` — legacy artifacts of `main.py` / an old snapshot; unused by current code.
- `output/` — debug HTML dumps from `session_get.py`.

## Secrets warning

Real credentials live in the local working copy: bot tokens in `config.json`, `ryumkin.json`, and hardcoded in `main.py`; live user session cookies in `sessions/`. All of these are gitignored — never print, commit, or copy these values into new files, examples, or logs, and never weaken the project section of `.gitignore`.
