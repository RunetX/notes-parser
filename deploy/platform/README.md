# Площадка: выкатка и обслуживание

Три сервиса на отдельном хосте: `postgres`, `platform` (веб-морда, подкоманда
`lovegw web`), `caddy` (TLS и отдача медиа). Адрес хоста и ключ — в памяти
проекта (`deploy-host-vps`), в репозиторий они не пишутся.

Файл отдельный от `deploy/docker-compose.yml` **временно**: пока боевой демон
живёт на старом хосте, поднимать здесь общий compose нельзя — он стартанёт второй
экземпляр ботов на тех же токенах, а `getUpdates` у Telegram single-consumer,
это 409 и дубли в канале. После переезда демона файлы сливаются.

## Выкатка

Образ на этом хосте **не собирается никогда**: `go build` просит ~1,5 ГБ RSS и
25 минут на одном ядре — это уронило бы и площадку, и зеркало. Бинарник
собирается на рабочей машине, на хосте вокруг него оборачивается тонкий образ
(`Dockerfile.prebuilt`, distroless).

```sh
# на рабочей машине, из каталога go/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/lovegw-linux ./cmd/lovegw
gzip -9 -c /tmp/lovegw-linux | ssh <хост> "gunzip -c > /root/platform/lovegw.new \
  && chmod +x /root/platform/lovegw.new && mv /root/platform/lovegw.new /root/platform/lovegw \
  && cd /root/platform && docker build -q -f Dockerfile.prebuilt -t lovegw:latest ."
```

Сжатый бинарник — 10 МБ вместо 32, а канал до хоста ~1 МБ/с: разница между
десятью секундами и минутой.

## Схема

Миграции накатываются **отдельной командой**, а не стартом контейнера: схему
меняет администратор в известный момент, а не любой перезапуск.

```sh
docker compose run --rm --entrypoint /lovegw platform platform -config /config.json migrate
docker compose up -d platform
docker compose run --rm --entrypoint /lovegw platform platform -config /config.json doctor
```

`web` при расхождении версий **отказывается стартовать** — это тоже намеренно:
веб-морда, молча работающая на чужой схеме, портит данные, а не падает.

## Тесты против настоящего Postgres

Половина решений пакета `platform` живёт в SQL (маскирование анонима в SELECT,
порядок по `COLLATE "C"`, частичные индексы, `ON CONFLICT`), поэтому
интеграционные тесты идут против настоящей базы. На хосте для этого заведена
отдельная база `platform_test` — тесты **сносят в ней схему целиком** перед
каждым тестом, и в них стоит предохранитель: имя базы обязано содержать «test»,
иначе прогон падает, не тронув ничего.

```sh
ssh -f -N -L 127.0.0.1:5433:<ip контейнера pg>:5432 <хост>   # ip: docker inspect platform-pg
LOVEGW_TEST_PG_DSN='postgres://platform:<пароль>@127.0.0.1:5433/platform_test?sslmode=disable' \
  go test ./internal/platform/
```

Без переменной интеграционные тесты пропускаются, так что `go test ./...` на
машине без Postgres остаётся зелёным. Прогон через туннель — около двух минут,
почти целиком это задержки сети, а не работа.

## Грабли, оплаченные временем

**Общий якорь `x-hardening` ломает Postgres.** Он писался под distroless-бинарник,
которому не нужно ничего, и снимает все привилегии. Официальный образ Postgres
стартует от root, готовит каталоги и переключается на пользователя `postgres`:
без `cap_add: [CHOWN, DAC_OVERRIDE, FOWNER, SETUID, SETGID]` контейнер уходит в
вечный рестарт.

**В `postgres:18` сменился PGDATA** — `/var/lib/postgresql/18/docker`, а точка
монтирования образа `/var/lib/postgresql`. Привычный по прошлым версиям путь
`pgdata:/var/lib/postgresql/data` **бьёт мимо данных**: контейнер работает, но
база живёт в анонимном томе и исчезает при первом пересоздании. Проверять
фактом: `SHOW data_directory` должен указывать внутрь тома.

**`config.json` обязан принадлежать uid 65532** — distroless работает под
`nonroot` и иначе падает на `permission denied`.

**`docker exec` без `-i`** не пробрасывает stdin: `docker exec pg psql -f /dev/stdin
< file.sql` молча ничего не выполняет и возвращает успех. Результат проверять
списком таблиц, а не кодом возврата.

## Секреты

`secrets.env` (пароль Postgres, DSN) и `config.json` лежат только на хосте, в
репозитории их нет и быть не должно. Пароль базы наружу не публикуется: портов у
`postgres` нет вовсе, доступ только по внутренней сети compose.
