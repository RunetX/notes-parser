# lovegw

Telegram-бот, который зеркалит раздел заметок (заметки) сайта знакомств
love.ngs.ru в Telegram-каналы и связывает взаимодействие обратно: залогиненные
в Telegram пользователи отвечают в группе обсуждения, а бот публикует их ответ
комментарием на сайте от их имени. Отдельный ЛС-бот **РюмкинЪ** умеет вход на
сайт, публикацию заметок и подписки на ключевые слова.

Написан на Go (модуль `lovegw`, каталог [`go/`](go/)). Хранилище — SQLite без
CGo; запись сквозная, поэтому состояние переживает `kill -9`. Скрапинг — на
`net/http` + goquery по HTML сайта.

## Запуск

```sh
cd go
go build ./...
go run ./cmd/lovegw run       # демон: зеркало + мост ответов + ЛС-бот
go run ./cmd/lovegw doctor    # диагностика конфига/БД/сайта/токенов/очереди
```

Конфиг — `go/config.json` (шаблон [`go/config.example.json`](go/config.example.json));
токены можно задать через переменные окружения `LOVEGW_MIRROR_TOKEN` /
`LOVEGW_DM_TOKEN`. На Windows демоном управляют батники `start.bat` / `stop.bat`
/ `status.bat` / `restart.bat`.

**Сеть.** love.ngs.ru за геоблоком DDoS-Guard — с не-российских IP отдаёт 403,
поэтому демон и `crawl` должны работать с российского IP. При этом Telegram Bot
API из России недоступен: для раздельной сети задайте `telegram_proxy` в
конфиге — проксируется только Bot API, а сайт идёт напрямую.

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

Отладочный скрап с записью фикстур:

```sh
go run ./cmd/lovegw crawl notes -save-html internal/love/testdata
go run ./cmd/lovegw crawl comments <note_id> -save-html internal/love/testdata
```
