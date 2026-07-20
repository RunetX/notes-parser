# Развёртывание lovegw

Контейнеризация и деплой демона (M7). Образ — статичный distroless, один
процесс: зеркало ленты + мост ответов + ЛС-бот РюмкинЪ.

## Требования к хосту

- **Российский IP.** love.ngs.ru за DDoS-Guard: с не-российского IP — 403.
  Демон ходит на сайт **напрямую**, поэтому хост должен быть в РФ.
- **Docker** (compose или systemd).
- Telegram Bot API из РФ заблокирован → идёт через **SOCKS5-прокси** на
  заграничном боксе (`telegram_proxy` / `LOVEGW_TG_PROXY`). Прокси уже поднят
  на Amnezia-VPS (`socks5://tgproxy:…@45.151.183.247:39080`).

> Заграничный Amnezia-VPS для роли хоста **не подходит** — с него сайт отдаст
> 403. Он только выход для Bot API.

## Конфиг

Схема — как у `go/config.example.json`. Скопируйте в `deploy/config.json` и
заполните: `mirror_bot.channel_id`, `discussion_chat_id`, `signature`,
`admin_tg_user_id`, `telegram_proxy`. Токены — либо прямо в `config.json`
(chmod 600), либо в `secrets.env` (env перебивает конфиг, см.
`secrets.env.example`).

## Сборка и запуск (compose)

```sh
cd deploy
cp <ваш рабочий config.json> ./config.json      # каналы, подпись, прокси
mkdir -p data && sudo chown -R 65532:65532 data # том пишет nonroot (uid 65532)
docker compose up -d --build
docker compose logs -f
```

Проверка перед боем — `doctor` в том же образе:
```sh
docker compose run --rm lovegw doctor -config /config.json
```
Ждём все ✔ (конфиг, БД, сайт, прокси telegram, оба бота).

## Перенос состояния (cutover без дублей)

Демон хранит всё в SQLite (`data/lovegw.db`): id постов/тредов, сессии,
подписки. Есть два пути:

1. **Перенести существующую БД (рекомендуется):** скопируйте рабочий
   `data/lovegw.db` в `deploy/data/` до первого старта — мост и отслеживание
   заметок продолжатся бесшовно, ничего не перепостится.
   ```sh
   sudo cp /path/to/lovegw.db deploy/data/lovegw.db
   sudo chown 65532:65532 deploy/data/lovegw.db
   ```
2. **Старт с чистой БД:** первый запуск с `-seed`, чтобы текущие заметки ленты
   запомнились **без** постинга (иначе они улетят пачкой как «новые»):
   ```sh
   docker compose run --rm lovegw run -seed -config /config.json
   ```
   затем обычный `docker compose up -d`.

## Вариант без compose — systemd

См. `lovegw.service`: собрать/загрузить образ `lovegw:latest`, положить
`/etc/lovegw/config.json` и `/etc/lovegw/secrets.env` (600), создать
`/var/lib/lovegw/data` (chown 65532:65532), затем
`systemctl enable --now lovegw`.

## Эксплуатация

- Логи: `docker compose logs -f` (или `journalctl -u lovegw -f`).
- Обновление: `docker compose up -d --build` (write-through SQLite crash-safe —
  рестарт безопасен).
- Секреты (`config.json`, `secrets.env`) — только 600, никогда в git.
