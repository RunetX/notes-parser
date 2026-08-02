# Развёртывание lovegw

Контейнеризация и деплой демона (M7). Образ — статичный distroless (~100 МБ:
~23 МБ бинарник плюс статический `ffmpeg` для распознавания голосовых), один
процесс: зеркало ленты + мост ответов + ЛС-бот РюмкинЪ (+ бот личной переписки,
если задан `talks_token`).

## Требования к хосту

- **Российский IP.** love.ngs.ru за DDoS-Guard: с не-российского IP — 403.
  Демон ходит на сайт **напрямую**, поэтому хост должен быть в РФ.
- **Docker** (+ compose plugin) и **git**. Go ставить не нужно — сборка идёт
  внутри `golang`-контейнера.
- Telegram Bot API из РФ заблокирован → идёт через **SOCKS5-прокси** на
  заграничном боксе (`telegram_proxy` / `LOVEGW_TG_PROXY`).

> Заграничный VPS (например, Amnezia-выход) для роли хоста **не подходит** — с
> него сайт отдаст 403. Он годится только как выход для Bot API (прокси).

## Шаг 0 — на рабочей машине: запушить

```sh
git push        # чтобы на хосте в клоне были все коммиты (в т.ч. M7)
```

## Шаг 1 — подготовить хост

```sh
apt-get update && apt-get install -y docker.io docker-compose-plugin git
systemctl enable --now docker
```

## Шаг 2 — получить код

```sh
git clone git@github.com:RunetX/notes-parser.git && cd notes-parser/deploy
```

Нет SSH-доступа к GitHub с хоста — добавьте deploy-key или клонируйте по HTTPS.

## Шаг 3 — перенести конфиг и состояние (их нет в репо)

Схема конфига — как у `go/config.example.json`; заполнить `mirror_bot.channel_id`,
`discussion_chat_id`, `signature`, `admin_tg_user_id`, `telegram_proxy`. Токены —
в `config.json` (chmod 600) или в `secrets.env` (env перебивает конфиг, см.
`secrets.env.example`).

С рабочей машины по scp:

```sh
scp go/config.json     user@ХОСТ:~/notes-parser/deploy/config.json
scp go/data/lovegw.db  user@ХОСТ:~/notes-parser/deploy/data/lovegw.db  # см. cutover ниже
```

На хосте — права:

```sh
chmod 600 config.json
mkdir -p data && sudo chown -R 65532:65532 data   # том пишет nonroot (uid 65532)
```

## Шаг 4 — собрать и запустить

```sh
docker compose up -d --build
docker compose logs -f
```

## Шаг 5 — перенос состояния без дублей (cutover)

Демон хранит всё в SQLite (`data/lovegw.db`): id постов/тредов, сессии, подписки.

1. **Перенести существующую БД (рекомендуется):** скопировать рабочий
   `data/lovegw.db` в `deploy/data/` **до первого старта** — мост и отслеживание
   заметок продолжатся бесшовно, ничего не перепостится. (chown 65532:65532.)
2. **Старт с чистой БД:** первый запуск с `-seed`, чтобы текущие заметки ленты
   запомнились **без** постинга (иначе улетят пачкой как «новые»):

   ```sh
   docker compose run --rm lovegw run -seed -config /config.json
   docker compose up -d
   ```

## Шаг 6 — проверка боем

```sh
docker compose run --rm lovegw doctor -config /config.json
```

Ждём все ✔ (конфиг, БД, сайт, прокси telegram, оба бота).

## Шаг 7 — включить личную переписку (talks), мультисессия

Зеркалит входящие ЛС сайта в личку РюмкинЪ каждого залогиненного пользователя;
ответ реплаем уходит на сайт от его сессии. Включается отдельно, поверх рабочего
демона.

**Секция `talks` в `config.json` (боевые значения мультисессии):**

```json
"talks": {
  "enabled": true,
  "admin_only": false,        // все залогиненные, не только админ
  "allow_send": true,         // двусторонний: ответы реплаем идут на сайт
  "poll_interval_s": 30,
  "idle_poll_interval_s": 300,
  "max_dialogs_per_tick": 5,
  "max_requests_per_min": 6,  // общий бюджет talks к сайту; при росте числа сессий поднимать
  "store_text": false,        // приватность: в БД только метаданные, текст живёт в мессенджере
  "retention_days": 30        // старые сообщения talks чистятся автоматически
}
```

**Порядок раскатки:**

1. **Бэкап БД** перед первым стартом с talks: `cp data/lovegw.db data/lovegw.db.bak`.
   Включение мигрирует схему до v5 (аддитивно; откат бинарника на той же БД
   безопасен).
2. Прогнать чек — покажет, сколько сессий будет обходиться:
   ```sh
   docker compose run --rm lovegw doctor -config /config.json
   # talks/telegram: мультисессия: N валидных сессий будут обходиться
   ```
3. Применить конфиг и перезапустить: `docker compose up -d --build`.
4. Первый цикл разошлёт **непрочитанные** входящие всех сессий (прочитанное на
   сайте не трогается), дальше — только новое. Двусторонний режим активен сразу.

**Бюджет запросов к сайту.** talks ходит через тот же site-лимитер, что и зеркало
(один IP). При многих сессиях один полный цикл ограничен `max_requests_per_min` —
при росте числа пользователей поднимать его и/или `poll_interval_s`, следя за
логами на 403.

**Изоляция и откат.**
- Протухшая сессия одного пользователя (гостевой ответ сайта) помечается
  невалидной, ему уходит подсказка `/login` — **поллер для остальных не падает**.
- 3 подряд `403`/дрейфа API → talks сам встаёт (kill-switch) + алерт админу;
  **зеркало заметок продолжает работать**.
- Мгновенный откат: `talks.enabled=false` + `docker compose up -d` — зеркало и
  мост живут как раньше.

## Шаг 8 — еженедельный дайджест (опционально)

Секция `digest` в `config.json` (`"enabled": true`; слот по умолчанию —
пятница 19:00 Нск). Демон в слот выпуска строит черновик и материалы в
`/data/digest/` и шлёт админу ЛС; публикация — за админом после правки
LLM-рубрик. `/data` — named volume (не bind-mount, см. комментарий в
compose), поэтому файлы достаём/возвращаем через `docker cp` в работающий
контейнер демона:

```sh
docker cp lovegw:/data/digest/digest-<неделя>.draft.txt .
docker cp lovegw:/data/digest/digest-<неделя>.materials.md .
vi digest-<неделя>.draft.txt                        # вставить рубрики из materials.md
docker cp digest-<неделя>.draft.txt lovegw:/data/digest/
docker compose run --rm lovegw digest -config /config.json preview
docker compose run --rm lovegw digest -config /config.json publish
```

(Альтернатива — правка прямо в томе от root:
`sudo vi "$(docker volume inspect deploy_lovegw-data -f '{{.Mountpoint}}')/digest/…"`.)

Публикация идемпотентна (message_targets) — безопасна при работающем демоне;
`-force` публикует «сухой» выпуск без LLM-рубрик. Пропущенный слот (демон
лежал) догоняется в течение 48 часов, старше — пропускается.

**Полная автоматизация через Claude API.** Задайте ключ в `secrets.env`
(`LOVEGW_LLM_KEY` — значение из `pepper/.env`, `ANTHROPIC_API_KEY`) и
включите `"auto_publish": true` в секции `digest`: в слот демон сам заполнит
LLM-рубрики (Claude, запросы через `LOVEGW_TG_PROXY`) и опубликует выпуск,
прислав админу итог в ЛС. При сбое или невалидном ответе LLM автопубликации
не будет — придёт обычное «черновик готов», дальше полуручной цикл выше.
Разовая редактура вручную:

```sh
docker compose run --rm lovegw digest -config /config.json -llm -force draft
docker compose run --rm lovegw digest -config /config.json publish
```

## Шаг 9 — распознавание голосовых (опционально)

Голосовое (и кружок) в треде обсуждения бот расшифровывает и отвечает текстом
реплаем. Что делать с текстом, решает автор: фича без кнопок и диалогов.
Посты самого канала не распознаются — там и Telegram, и MAX показывают
собственную расшифровку по кнопке, платить провайдеру за то же самое незачем.

Включение: секция `asr` в `config.json` (`"enabled": true`) и ключ Nexara в
`secrets.env` (`LOVEGW_ASR_API_KEY`). Провайдер российский — запросы идут
**напрямую**, мимо `LOVEGW_TG_PROXY`. `ffmpeg` уже в образе, отдельно ставить
нечего. Проверка после запуска: `docker compose run --rm lovegw doctor -config
/config.json` — строки `asr` и `asr/ffmpeg`.

Расходы ограничены с трёх сторон: потолок длительности одного сообщения
(`max_duration_sec`, дефолт 90 — длиннее бот коротко объяснит, что не
расшифровывает), суточная квота на пользователя в секундах аудио
(`user_daily_limit_sec`) и кэш расшифровок в БД по id файла — пересланное
голосовое второй раз не оплачивается. При сбое провайдера в треде тишина,
ошибка в логах; про неверный ключ или пустой баланс админ получает ЛС один раз.
Откат — `"enabled": false` и рестарт: гейт работает как раньше.

## Эксплуатация

```sh
docker compose logs -f                       # логи
git pull && docker compose up -d --build     # обновление (рестарт crash-safe)
docker compose down                          # остановка
```

## Вариант без git/исходников на хосте

Собрать образ на рабочей машине и перенести бинарём образа:

```sh
docker save lovegw:latest | gzip | ssh user@ХОСТ 'gunzip | docker load'
```

(Образ ~100 МБ из-за статического `ffmpeg` — перенос заметно дольше, чем был.)

затем перенести только `deploy/` (scp) + `config.json` + `data/`, и
`docker compose up -d` (без `--build` — образ уже загружен).

## Вариант без compose — systemd

См. `lovegw.service`: собрать/загрузить образ `lovegw:latest`, положить
`/etc/lovegw/config.json` и `/etc/lovegw/secrets.env` (600), создать
`/var/lib/lovegw/data` (chown 65532:65532), затем `systemctl enable --now lovegw`.

## Секреты

`config.json` и `secrets.env` — только `chmod 600`, никогда в git (уже в
`.gitignore`). В образ не зашиваются — монтируются/передаются на хосте.
