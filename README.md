# lovegw

Telegram-бот, который зеркалит раздел заметок (заметки) сайта знакомств
love.ngs.ru в Telegram-каналы и связывает взаимодействие обратно: залогиненные
в Telegram пользователи отвечают в группе обсуждения, а бот публикует их ответ
комментарием на сайте от их имени. Отдельный ЛС-бот **РюмкинЪ** умеет вход на
сайт, публикацию заметок и подписки на ключевые слова.

Написан на Go (модуль `lovegw`, каталог [`go/`](go/)). Хранилище — SQLite без
CGo; запись сквозная, поэтому состояние переживает `kill -9`. Скрапинг — на
`net/http` + goquery по HTML сайта. Помимо демона, у бинарника есть офлайн-слой
архива: массовая выгрузка заметок в отдельную `archive.db` и аналитика
«персонажей» поверх неё (склейка альт-анкет, интересы, отношения, отчёт).

## Быстрый старт

```sh
cd go
go build ./...
go run ./cmd/lovegw run       # демон: зеркало + мост ответов + ЛС-бот
go run ./cmd/lovegw doctor    # диагностика конфига/БД/сайта/токенов/очереди
```

Конфиг — `go/config.json` (шаблон [`go/config.example.json`](go/config.example.json));
токены можно задать через переменные окружения `LOVEGW_MIRROR_TOKEN` /
`LOVEGW_DM_TOKEN`, путь к БД — `LOVEGW_DB_PATH`, прокси — `LOVEGW_TG_PROXY`.
На Windows демоном управляют батники `start.bat` / `stop.bat` / `status.bat` /
`restart.bat`.

**Сеть.** love.ngs.ru за геоблоком DDoS-Guard — с не-российских IP отдаёт 403,
поэтому демон и все команды, ходящие на сайт, должны работать с российского IP.
При этом Telegram Bot API из России недоступен: для раздельной сети задайте
`telegram_proxy` в конфиге — проксируется только Bot API, а сайт идёт напрямую.

## Команды

Все команды — подкоманды одного бинарника: `go run ./cmd/lovegw <команда> …`
(или собранный `lovegw <команда> …`). Флаги можно писать и до, и после
позиционных аргументов (`crawl notes -save-html dir` работает). У всех команд,
читающих конфиг, есть `-config config.json` (путь по умолчанию — относительно
текущего каталога, поэтому запускать удобнее из `go/`).

### Демон и эксплуатация

```sh
lovegw run    [-seed]
lovegw doctor [-post-test]
lovegw repost <note_id> [<note_id> …]
lovegw import [-notes notes.json] [-sessions sessions_export.json] [-subscribers subscribers.json]
```

- **`run`** — основной демон: зеркалирование ленты и комментариев, мост
  «ответ в группе → комментарий на сайте», ЛС-бот РюмкинЪ — всё под одним
  errgroup. `-seed` на первом запуске запоминает видимые заметки **без**
  постинга (иначе при пустой БД случится залп из всей текущей ленты).
- **`doctor`** — диагностика: конфиг, БД, доступ к сайту, токены ботов,
  очередь обновлений. `-post-test` дополнительно проверяет цепочку
  «пост в канал → автофорвард в группу» тихим самоудаляющимся сообщением
  (безопасно на живом канале).
- **`repost`** — удаляет заметку из Telegram (пост, тред, комментарии,
  иллюстрации) и из БД, чтобы демон перепостил её заново по текущей логике.
  Отладочная команда — запускать при остановленном демоне.
- **`import`** — разовый идемпотентный импорт JSON-состояния старой
  Python-версии в SQLite (`INSERT OR IGNORE`); можно указывать любое
  подмножество файлов.

### Отладка парсинга

```sh
lovegw crawl notes                [-save-html dir]
lovegw crawl comments <note_id>   [-save-html dir]
```

Разбирает ленту или страницу комментариев и печатает JSON. `-save-html`
записывает сырую страницу как фикстуру парсера — при дрейфе вёрстки
перезаписывайте фикстуры в `internal/love/testdata/` этой же командой:

```sh
lovegw crawl notes -save-html internal/love/testdata
lovegw crawl comments <note_id> -save-html internal/love/testdata
```

### Архив (archive.db)

Отдельная БД (по умолчанию `data/archive.db`, флаг `-db`) для офлайн-анализа:
users/notes/comments, дерево ответов через `parent_id`, типажи дедуплицированы
по id анкеты.

```sh
lovegw grab   [-db archive.db] [-json] [-out dir] [-save-html dir] [-view tree|linear] [-max-pages N] <note_id>
lovegw export [-db archive.db] [-out dir] <note_id>
lovegw backfill [-db archive.db] [-proxy] [-workers N] [-interval-ms MS] [-from ID] [-to ID] [-start-page N] [-refresh] [-limit N]
```

- **`grab`** — разовая выгрузка одной заметки со всеми страницами комментариев
  с сайта в `archive.db` (только чтение, без кук). `-json` дополнительно пишет
  денормализованный снимок `<id>.json` в `-out`.
- **`export`** — полностью офлайн: собирает заметку из `archive.db` во
  вложенное JSON-дерево `<id>.json` (для визуализаций и внешних инструментов).
- **`backfill`** — массовая выгрузка диапазона заметок: перечисляет живые id
  обходом ленты и внахлёст скармливает их пулу воркеров. Идемпотентна — после
  остановки или 403 достаточно перезапустить. `-proxy` гонит сайт через
  `telegram_proxy` (беречь основной IP), `-from`/`-to` — границы id
  (по умолчанию от 240866 — старее комментарии уже удалены), `-start-page` —
  с какой страницы ленты начинать (экономит тысячи страниц на старом
  диапазоне), `-refresh` — пере-обходить уже загруженные.

### Персонажи (personas)

Слой распознавания личностей поверх `archive.db`: вероятностная, ревью-гейтед
склейка альт-анкет одного человека в «личности», плюс интересы и отношения из
текстов. Общие флаги: `-db archive.db`, `-out dump` (каталог выгрузок).

Основной конвейер склейки — текстовые самораскрытия:

```sh
lovegw personas flag [-patterns file]        # скан комментариев по шаблонам самораскрытия → disclosure_hits
lovegw personas candidates [-limit N]        # выгрузка непроработанных помет с контекстом (hits.jsonl + users_index.json)
# … внешний LLM-проход по hits.jsonl → links.json …
lovegw personas link [-in links.json]        # импорт извлечённых связей в alias_candidates
lovegw personas cluster [-min-score F] [-max-persona N] [-min-density F]
                                             # склейка связных компонент в personas + отчёт
lovegw personas set <persona_id> <confirmed|rejected|pending>   # вердикт после ревью
```

`cluster` защищён гардами от транзитивной переклейки: `-max-persona`
(компонент крупнее — не склеивать) и `-min-density` (минимальная плотность
рёбер для компонент больше 4 анкет).

Дополнительные сигналы склейки:

```sh
lovegw personas avatars fetch  [-proxy] [-workers N] [-limit N] [-refresh]   # скачать аватары, посчитать dHash
lovegw personas avatars cluster [-max-dist D] [-generic-max N]               # пары по похожести аватаров
lovegw personas stylometry build   [-min-chars N] [-dims N]                  # стилевые профили авторов
lovegw personas stylometry cluster [-min-cosine F] [-top-k N] [-max-pairs N] # пары по близости стиля
lovegw personas ensemble [-ens-top-k N] [-handoff-days D] [-ens-floor F]     # композит: стиль + handoff + круг общения
```

Диагностика:

```sh
lovegw personas portrait [-top N] <p<id>|u<id>|user_id>   # портрет личности/анкеты: стиль, собеседники
lovegw personas diag <id> <id> …                          # ground-truth сверка набора анкет (стиль/собеседники/время)
```

Факты (интересы) и отношения — тот же паттерн «scan/score → candidates →
внешний LLM → import»:

```sh
lovegw personas facts scan [-topics file] [-min-hits N] [-min-notes N] [-evidence N]
lovegw personas facts candidates [-min-hits N] [-min-notes N]      # → facts.jsonl на LLM-разметку
lovegw personas facts import [-in facts_llm.json]                  # → identity_facts

lovegw personas relations score [-rel-min-replies N]               # тон направленных пар по эвристикам
lovegw personas relations candidates [-cand-replies N] [-band-min N] [-band-top N] [-exchanges N]
                                                                   # → pairs.jsonl на LLM-разметку
lovegw personas relations import [-in relations_llm.json]          # → relation_edges
```

Пол анкет — обходом их профилей через мобильную версию сайта
(десктопная банит серию запросов почти мгновенно, мобильный vhost терпит):

```sh
lovegw personas gender [-active-days N] [-tg-user id] [-limit N]
```

Ходит под сессией сайта из БД бота (`-tg-user`, по умолчанию
`admin_tg_user_id`; нужен `/login` в РюмкинЪ). Идемпотентно — обходятся только
анкеты без пола, волны 403 пережидаются с нарастающей паузой, так что остаток
добирается простым перезапуском.

Итоговый отчёт:

```sh
lovegw personas report [-report-top N] [-active-days N] [-html]
```

Сводка «персонажей»: топ личностей, интересы, отношения. `-active-days N`
ограничивает когорту активными за последние N суток, `-html` дополнительно
собирает `characters.html`.

## Развёртывание (Docker)

Боевой запуск — в контейнере (образ ~23 МБ, distroless/static). Артефакты и
пошаговая инструкция — в [`deploy/`](deploy/) (подробно — [deploy/README.md](deploy/README.md)):
multi-stage [`go/Dockerfile`](go/Dockerfile), `docker-compose.yml`, systemd-юнит,
шаблон секретов.

```sh
cd deploy
docker compose up -d --build
```

Хост нужен с **российским IP** (иначе сайт отдаёт 403); Telegram Bot API идёт
через SOCKS5-прокси (`telegram_proxy` / `LOVEGW_TG_PROXY`). Конфиг и БД
(`data/lovegw.db`) в репозиторий не входят — переносятся на хост отдельно.

## Разработка

```sh
cd go
go vet ./...
go test ./...
```

Селекторы вёрстки сайта собраны в одном const-блоке
[`internal/love/parse.go`](go/internal/love/parse.go); тесты гоняются на реальных
фикстурах в `internal/love/testdata/`. Обязательный селектор, не нашедший
элемент, возвращает типизированную `MarkupError` (детекция дрейфа вёрстки).
Идентификаторы — по-английски, комментарии и строки бота — по-русски.
