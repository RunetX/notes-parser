# Развёртывание lovegw

Контейнеризация и деплой демона (M7). Образ — статичный distroless (~23 МБ),
один процесс: зеркало ленты + мост ответов + ЛС-бот РюмкинЪ.

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

затем перенести только `deploy/` (scp) + `config.json` + `data/`, и
`docker compose up -d` (без `--build` — образ уже загружен).

## Вариант без compose — systemd

См. `lovegw.service`: собрать/загрузить образ `lovegw:latest`, положить
`/etc/lovegw/config.json` и `/etc/lovegw/secrets.env` (600), создать
`/var/lib/lovegw/data` (chown 65532:65532), затем `systemctl enable --now lovegw`.

## Секреты

`config.json` и `secrets.env` — только `chmod 600`, никогда в git (уже в
`.gitignore`). В образ не зашиваются — монтируются/передаются на хосте.
