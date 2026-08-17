# Бриф: JSON-RPC API love.ngs.ru — каталог методов через интроспекцию

- **Дата:** 2026-08-17
- **Статус:** разведка. Каталоги сняты живьём **анонимно**, без авторизации;
  поведение методов под сессией НЕ проверено (см. §5 — решающий эксперимент).
- **Повод:** вопрос «можно ли разобрать iPhone-приложение, чтобы понять его
  протокол». Разбирать не понадобилось: сервер сам отдаёт описание своего API.

---

## 1. Как найдено

`https://s.ngs.ru/ngs/jsonrpc.min.js` — клиент `jQuery.Ngs.JsonRpc({url, methods})`.
Если `methods` не передан, клиент делает **GET на тот же url** и забирает список
методов у сервера (`a._load_methods`). Фронт список не передаёт нигде, значит
интроспекция включена:

```bash
curl -s -H 'Accept: application/json' https://love.ngs.ru/ajax/     # 56 методов
curl -s -H 'Accept: application/json' https://m.love.ngs.ru/ajax/   # 39 методов
```

Ответ — дескриптор **SMD 2.0**: `{transport, envelope, contentType, SMDVersion,
target, services|methods:{...}}`, у каждого метода имя, `parameters` (имя, тип,
`optional`) и `returns`. Оба GET отдают 200 гостю.

## 2. Два шлюза, два разных API

| | путь | методов | формат ответов |
|---|---|---|---|
| десктоп | `love.ngs.ru/ajax/` | 56 | HTML внутри JSON |
| мобильный | `m.love.ngs.ru/ajax/` | 39 (28 уникальных) | массивы / HTML-фрагменты |

Мобильный — почти наверняка тот же API, на котором живёт приложение из футера
сайта (App Store `id581874584`, «Знакомства»). Ключ дескриптора у них разный:
десктоп — `services`, мобильный — `methods`.

Транспорт совпадает с тем, что уже реализовано в `internal/love/json.go`:
POST `/ajax/`, JSON-RPC 2.0, **позиционные** params, авторизация кукой сессии.

## 3. Поведение на госте (проверено)

- Десктоп, известный метод: чистая JSON-RPC ошибка, HTTP 200
  (`getTagsCloud` → `{"error":{"code":0,"message":"Не удалось получить облако тегов"},"result":false}`).
- Мобильный, **неизвестный** метод: HTTP 200,
  `{"error":{"code":-32601,"message":"Method not found (…)","data":"[m.love.ngs.ru] Exception Ngs_Rpc_Exception_BadRequest: …"}}`.
- Мобильный, **известный** метод (`notes`, `news`, `getNewMessagesCount`, любые
  параметры и типы): **HTTP 502** от nginx.

> ⚠ **Ловушка на будущее.** 502 на мобильном шлюзе — это НЕ шторм сайта и не
> падение площадки: бэкенд фаталит на отсутствии пользователя. Отличается от
> настоящего шторма тем, что GET-страницы и десктопный POST в тот же момент
> отвечают 200, а неизвестный метод отдаёт аккуратный `-32601`. Проверять
> именно этой парой, иначе диагноз уедет в `site-5xx-storms` и предохранитель
> амвона (`fuse.go`) получит ложный промах.

## 3.1 Под живой сессией (проверено 17.08.2026, `cmd/apiprobe`)

Сессия владельца (анкета 1472546, «Паноптикум»), мобильная страница под этой же
кукой отдаёт 129 КБ против 22 КБ у гостя — то есть **мобильный vhost сессию
узнаёт**. Тем не менее:

| вызов | результат |
|---|---|
| `autocompleteRegions("city","Новоси")` — десктоп | 200, данные |
| `autocompleteRegions("Новоси")` — мобильный | **502** |
| `countNewMessages()` — десктоп | 200, `0` |
| `getNewMessagesCount()` — мобильный | **502** |

Первая пара — это **один и тот же логический метод**, разные только сигнатуры;
одна сессия, один момент. Значит 502 — не права, не параметры и не авторизация.

**Что доказано:** прежний вывод «нужна сессия» неверен, 502 не про авторизацию,
параметры или права.

**Что НЕ доказано — что сервис мёртв насовсем.** В эти же сутки сайт не в форме:
комментарии не постятся штатной десктопной версией, а `getTagsCloud` под сессией
отвечает «Не удалось получить облако тегов». Часть бэкенда лежит, и мобильный пул
мог лечь с ней. Косвенный довод в пользу «мёртв» — фронт: `mobNotes.js` зовёт
`LoveRun.LoveSimpleRPC.notes(…)` и `…addNoteComment(…)`, но ни `LoveRun`, ни
строки `ajax` в бандле нет, и страница ленты (ни гостю, ни под сессией) не грузит
ничего, что бы их определяло. Довод слабый: смотрели только `/notes/`, бандл мог
определяться на других мобильных страницах.

**Пересмотреть, когда сайт оживёт** (проверка на две минуты):
`go run ./cmd/apiprobe -user <id>` — пара `autocompleteRegions` десктоп/мобильный.
Ожил мобильный — каталог §4.1 действующий, и всё, что в §5 названо закрытым,
открывается обратно. Не ожил при здоровом сайте — тогда «исторический документ».

## 4. Каталоги

### 4.1 Мобильный (`m.love.ngs.ru/ajax/`, 39)

```
addModerComplaint         (reason:int, comment:string, userInfo:string?) -> bool
addNoteComment            (noteId:int, commentId:int, text:string) -> array
addProfileComplaint       (userId:int, msg:string) -> bool
addTag                    (name:string, tagId:int|null?) -> array|bool
addToFriends              (friendId:int) -> bool
addToIgnore               (userId:int) -> bool
autocompleteRegions       (geo_name:string) -> array
cancelRequestFriends      (friendId:int) -> bool
complainMessage           (messageId:string, comment:string?) -> bool
deleteFromIgnore          (userId:int) -> bool
deletePhotos              (photosIds:string|array, album:string) -> bool
deleteTalks               (passportIds:array|int) -> bool
dislikePhoto              (photoId:string, ownerId:int) -> bool
editUserProperty          (property:string, value:mixed) -> bool
getCountersMenu           () -> bool
getFriends                (action:string, order:int, offset:int) -> array
getMessagesHistory        (passportId:int, before:int) -> array|bool
getNewMessagesCount       () -> int
getPhotoCountLikes        (ids:array) -> bool
getTalksHistory           (before:mixed) -> array
guests                    (sex:int, onlyPhoto:boolean, date:string) -> array
likePhoto                 (photoId:string, ownerId:int) -> bool
meetingsParticipantAction (meetingId:int, participantId:int, action:string) -> string
news                      (date:string) -> array
notes                     (category:string, offset:string) -> array
photoToAvatar             (photoId:int) -> bool|string
photoUpdateTitle          (photo_id:string, album:string, title:string) -> bool
pingOnline                () -> void
readMessages              (passportId:int, ids:string[]?) -> bool
rejectGiftService         (id:string) -> bool
removeFromFriends         (friendId:int) -> bool
removeFromIgnores         (usersIds:int|array) -> bool
removeSympathy            (favoriteId:int) -> bool
removeTag                 (tagId:int) -> bool
sendMessage               (passportId:int, text:string, images:array?) -> mixed
setNewEmail               (email:string) -> bool
setSympathy               (favoriteId:int) -> bool
sympathies                (type:string, date:string) -> array
userRequestPhoto          (passportId:int) -> bool
```

### 4.2 Десктоп (`love.ngs.ru/ajax/`, 56)

```
addGiftService(to:int, service:string, msg:string) -> bool
addGroupToGift(giftId:int, groupId:int?, groupName:string?) -> bool
addModerComplaint(reason:int, comment:string, userInfo:string?) -> bool
addTag(name:string, tagId:int?) -> array|bool
attention(user:int, msg:string) -> bool
autocompleteRegions(type:string, region:string) -> array
complaint(user:int, msg:string) -> bool
countNewMessages() -> int
delBackground() -> bool
delLikePhoto(photoId:string, owner:int) -> bool
deletePhoto(user:int, id:int, album:string) -> array
editPhoto(photoId:int, title:string) -> bool
editUserProperty(property:string, value:mixed) -> bool
friends(friend:int, action:string) -> bool
generateAvatar(user:int, id:int) -> bool
getFriendsOnline() -> array
getGeoBounds(id:int, isRegion:bool?) -> array|null
getGeoObjects(region:string?) -> array
getLikes(photoId:string) -> array
getMapHtml(userId:int) -> string
getMessagesHistory(passportId:int, page:int) -> string|bool
getNewMessages(userId:int) -> array
getNews(offset:int) -> array|bool
getTagsCloud() -> array|bool
getTalksList(page:int, dir:string?) -> array
hideLamaNotice() -> bool
likePhoto(photoId:string, owner:int) -> bool
makeGiftFree(giftId:string, dateFrom:string, dateTo:string, hideAfter:bool, limit:int|null?) -> bool
moveBadPhoto(user:int, id:int) -> bool
movePhoto(user:int, id:int, toAlbum:string) -> array
offBanners(banner:string) -> bool
refuseGiftService(id:string) -> bool
rejectPhoto(user:int, ids:array, reason:string) -> array
removeComplaint(userId:int, complaintId:string) -> bool
removeGiftsGroup(groupId:int) -> bool
removeGroupFromGift(giftId:int, groupId:int) -> bool
removeModerComment(commentId:string, userId:int) -> bool
removeTag(tagId:int) -> bool
renameGiftsGroup(groupId:int, groupNewName:string) -> bool
requestPhoto(passportId:int) -> bool
resetLocation() -> bool
revertBadPhoto(user:int, id:int) -> bool
sendMessage(passportId:int, text:string, imageKeys:string[]?) -> mixed
setBackground(background:int) -> bool
setDislike() -> void
setFreeBackground(background:int) -> bool
setLocation(long:float, lat:float, locationDescription:string?, locationTitle:string?) -> bool
setNewEmail(email:string) -> bool
setNoteHappyEnd(noteId:int, value:bool) -> bool
setSympathy() -> void
spam(id:string, comment:string?) -> boolean|string
stopGiftFree(giftId:string) -> bool
swipeSearch() -> void
sympathize(target:int) -> string
tags(tag:string) -> array
updateProfile(userId:int, type:string, data:array) -> array
```

> ⚠ **SMD — карта, а не контракт и не права доступа.**
> — Типы врут в мелочах: `getMessagesHistory` заявлен `-> string`, а реально
> отдаёт `{html}` (см. `love/talks.go`). Форму ответа проверять живьём.
> — В десктопном каталоге видны модераторские и админские методы
> (`removeModerComment`, `rejectPhoto`, `moveBadPhoto`, `revertBadPhoto`,
> `removeComplaint`, `makeGiftFree`). Дескриптор по роли не фильтруется; права
> проверяются на вызове. **Не трогать.**

## 5. Что это меняет для проекта

**Talks — главное.** Мобильный набор устроен принципиально иначе, чем тот, на
котором мы сидим:

- `getMessagesHistory` возвращает **массив**, а не HTML;
- **`readMessages(passportId, ids?)` — отдельный метод пометки прочитанным**;
- `pingOnline()` — присутствие ставится **явным** вызовом;
- `getNewMessagesCount()` — счётчик без чтения истории;
- `getTalksHistory(before)` — список диалогов массивом (вместо
  `/ajax?request=loadBuddiesList` с HTML).

Существование отдельных `readMessages` и `pingOnline` — довод (не доказательство)
в пользу того, что мобильное чтение истории **не помечает прочитанным и не светит
человека в сети**. Это ровно та цена, ради которой заведено согласие
`sessions.talks_scan` и переформулирован `/delivery`. Согласуется с уже
проверенным фактом, что анонимный просмотр анкет не двигает `last_activity`
(память `last-activity-detects-section-ban`).

**Этот путь закрыт (17.08.2026):** мобильного сервиса нет (§3.1). Ни
`readMessages`, ни `pingOnline`, ни массивная история недоступны — их некому
исполнить.

**Что осталось живого и полезного.** Десктопный `countNewMessages()` работает под
сессией и стоит один дешёвый POST. Сейчас talks его не использует сознательно
(`briefs/love-talks-telegram.md` §1.3: без mark-read счётчик залипает на `>0`) —
но mark-read у нас как раз происходит, историю мы читаем, значит счётчик после
доставки падает и годится как гейт «идти ли в список диалогов вообще».

### Проведено 17.08.2026: узнать о новом ЛС, не открывая его — ДА

`apiprobe -talks`. Служебная учётка (`reserve`, анкета 1515730, паспорт
203594643) пишет владельцу, дальше всё под сессией владельца:

| шаг | `countNewMessages()` |
|---|---|
| до отправки | 0 |
| после отправки | **1** |
| **после `loadBuddiesList`** | **1** — список НЕ гасит |
| после `getMessagesHistory` | 0 — гасит именно она |

Разбор списка сразу дал `Unread=1` у нужного диалога, то есть непрочитанность
видна штатным `love.TalksDialogs`, уже реализованным.

**Что список даёт без пометки:** ник, id анкеты и паспорт собеседника, возраст,
аватар, счётчик непрочитанного (`data-unread-msg` + `.lv-talks__unread-inbox`),
статус «Online», `last_user_time` и карту `user_ids` (паспорт → анкета).

**Чего не даёт: текста сообщения.** Иголка, посланная в тексте ЛС, в ответе
списка не встречается ни разу; запись диалога кончается статусом присутствия.
Слово `preview` в ответе есть, но это про аватары.

### Проверены два неиспользуемых десктопных метода — оба не подходят

| метод | что вернул | гасит счётчик |
|---|---|---|
| `getTalksList(1)` / `(1,"in")` | HTML старой страницы «Сообщения» (7,8 КБ, с чекбоксами `del[…]`), текста ЛС нет | нет |
| `getNewMessages(<свой id>)` | `{html:"",count:0}` | нет |
| `getNewMessages(<id собеседника>)` | `{html,count}` — **текст есть** | **да, 1 → 0** |

Параметр `userId` у `getNewMessages` — это **собеседник**, а не владелец сессии;
на свой id метод честно отвечает «новых нет», на паспорт — `null` (нужен id
анкеты).

Итог: `getTalksList` хуже уже используемого `loadBuddiesList` (старая разметка
вместо нынешней), а `getNewMessages` **платный ровно так же, как история**.
Бесплатного текста на живом шлюзе нет.

Отдельно — почему на `getNewMessages` не стоит переезжать, даже несмотря на то,
что он отдаёт СРАЗУ новые (`count`) и снял бы дедуп по курсору: он
**неперечитываемый**. Пометив прочитанным, второй раз он вернёт пустоту, и
недоставленное (сбой мессенджера) исчезнет навсегда. Нынешний `getMessagesHistory`
страничный, поэтому `needsFetch` может переспросить историю на следующем такте —
на этом стоит гарантия доставки (`poll.go:168-198`). Замена ухудшила бы её.

**Следствие для talks — было бы, если бы не присутствие.** Уведомление «вам
написал X, непрочитанных N» не стоит ни пометки, ни «просмотрено». Но у обхода
есть вторая цена, и она всё перечёркивает — см. следующий раздел.

### Проведено 17.08.2026: обход СВЕТИТ человека в сети

`apiprobe -presence`. Наблюдаем служебную анкету (приватность у неё выключена,
`hide_me=false`, то есть она представительна за обычного пользователя) глазами
владельца: в списке диалогов сайт печатает присутствие собеседника — ровно то,
что видит живой человек.

- **Окно «в сети» ≈ 30 минут, замерено дважды.** 20:42 → погасла к 21:13;
  21:13 → погасла к 21:44. Оба раза около 31 минуты.
- **Один вызов `loadBuddiesList` зажёг «Online».** Самый дешёвый запрос поллера,
  тот, что делается каждый такт.
- **Обычный GET страницы зажёг «Online» тоже.** То есть светит не конкретный
  метод, а любое обращение под кукой. Заодно это оправдывает уже принятое
  решение не звать `SiteIdentity` до согласия (`captureIdentity` в
  `talks/delivery.go`) — предосторожность оказалась не лишней, а точной.
- **«Приватность» НЕ спасает.** У владельца `hide_me=true`, и тем не менее
  служебная учётка видит его в своём списке диалогов как «Online». Значит
  посоветовать человеку «включите скрытие присутствия» нельзя — на бейдж в
  списке диалогов настройка не действует. (Она меняет только то, какое поле
  сайт показывает вместо `last_activity`, см. `last-activity-detects-section-ban`.)

**Вывод: бесплатного обхода нет.** Такт поллера кратно меньше получаса, значит
человек с включённым talks числится на сайте в сети **постоянно**, пока бот
работает, — независимо от того, читаем мы историю или нет. Идея «уведомлять всех
даром, спрашивать только про дочитывание» не складывается: цена не в тексте, а в
самом факте захода под чужой кукой.

Значит нынешнее устройство — правильное: `talks_scan` остаётся условием работы
поллера целиком (`plan()` не пускает несогласившегося дальше `live()`), а текст
в `/delivery` про «показывает человека в сети» — не перестраховка, а факт с
цифрой. Единственное, что можно уточнить в формулировке: онлайн держится ещё
полчаса после последнего обращения.

Прежняя формулировка вопроса (см. ниже) сохранена как история решения.

**Новый решающий эксперимент** — и он важнее прежнего. Вопрос: можно ли **узнать
о новом ЛС, не открывая его**. Если `countNewMessages() > 0` плюс список
диалогов (`/ajax?request=loadBuddiesList`, уже реализован) дают отправителя и
превью текста, а `getMessagesHistory` при этом не зовётся — сайт ничего не
помечает, собеседник не видит «просмотрено», и talks может доставлять
уведомление (возможно, и текст) вообще без цены. Тогда согласие `talks_scan`
меняет смысл: спрашивать надо не «читать ли переписку», а «дочитывать ли».

Порядок: со второй учётки послать ЛС на анкету владельца → `countNewMessages`
(ожидаем 1) → `loadBuddiesList` → посмотреть, есть ли в нём непрочитанность и
текст → снова `countNewMessages` (ожидаем всё ещё 1: список не должен гасить) →
и только в конце, отдельным шагом, `getMessagesHistory` — убедиться, что гасит
именно он.

**`last_activity` (предварительно).** За четыре минуты авторизованных запросов
(GET `/` десктопа, GET мобильной страницы под кукой, четыре десктопных RPC)
отметка присутствия владельца **не сдвинулась**: 19:22 при реальном 19:26. То
есть «человек всё время обхода числится в сети» — свойство не любого
авторизованного запроса. Замер грязный (базовое значение уже равнялось текущей
минуте, шаг обновления поля неизвестен) и требует чистого окна: 10 минут без
единого обращения к сайту под этой сессией, потом один вызов, потом замер.

**Заметки.** `notes(category, offset)` и `addNoteComment(noteId, commentId, text)`.
Из `mobNotes.js` видно фактическую сигнатуру: `notes(category, <число уже
показанных>)`, категория главной ленты — пустая строка, страница 8 штук
(`window.MobileNotesLimit = 8`); ответ `{notes:[…HTML-фрагменты…]}`, то есть это
пагинация вёрстки, а не структурные данные — для зеркала выигрыш меньше, чем
казалось. У `addNoteComment` `commentId` — корень ветки, `0` для корневого
комментария; **возвращает созданный комментарий**, что закрыло бы двусмысленность
случая 17.08.2026 («сайт ответил 500 — принял или нет»), из-за которой в
`pulpit/fuse.go` появилась причина `send_failed`.

**Но оба метода — мобильные, то есть сейчас недоступны.** А в десктопном
каталоге из 56 методов заметок нет вовсе: единственный, кто их касается, —
`setNoteHappyEnd(noteId, value)`, поздняя пристройка про «счастливый конец».

### Заметки живут на легаси-движке — это надо принять как данность

Складывается из четырёх независимых наблюдений:

1. **Публикация идёт формами, не API.** Комментарий — `POST /notes/comments/<id>`
   с полями `noteId, comId, comApiId, reason, content`; заметка —
   `POST /notes/add/` с `action_note[lid] / [href] / [hideme] / [nocom] / [rules]`
   и телом в поле `letter` (`internal/love/post.go`). Скобочные имена полей —
   классический PHP-массив из формы, `letter` — словарь той же эпохи.
2. **Разметка страницы того же возраста:** `<form name="notes" action="" method="post">`
   — постинг сам в себя, без токена.
3. **Поведение текста:** `nl2br`, схлопывание пробелов, мёртвые BB-коды, якорь
   `anchor-<n>` (см. `site-comment-text-formatting`).
4. **Своя нумерация людей:** автор заметки — СТАРЫЙ номер анкеты
   (память `note-author-id-is-an-old-profile-number`), то есть раздел старше
   нынешней системы профилей и таскает с собой её предшественницу.

RPC-шлюз вырос вокруг знакомств — фото, подарки, друзья, симпатии, гео, talks,
модерация, — а заметки остались там, где были с 2009 года.

**Следствие для проекта:** переезда зеркала и моста на JSON не будет. `MarkupError`
и переснятие фикстур — не временное неудобство в ожидании API, а постоянное
свойство раздела; силы разумнее вкладывать в устойчивость селекторов, а не в
поиск обходного интерфейса. Единственный шанс на структурный доступ к заметкам —
если оживёт мобильный шлюз с его `notes`/`addNoteComment`; тогда же появится и
подтверждение публикации комментария, которого не хватило 17.08.

**Гости.** `guests(sex:int, onlyPhoto:bool, date:string)` — у метода есть
параметр `date`, тогда как страница `/guests/` отдаёт 24 записи **без времени**
(память `guests-page-structure`). Стоит проверить, не отдаёт ли API время визита:
это прямо про `modwatch guests`.

**Прочее по мелочи.** `getFriendsOnline()` (десктоп) — присутствие, но только по
друзьям. `spam`, `complaint`, `addProfileComplaint` — жалобы. `editUserProperty`
/ `updateProfile` — правка анкеты (у нас это `dmbot /profile` через формы).

## 5.1 Третий хост: push-канал сообщений (найден 17.08.2026)

Логи выдачи TLS-сертификатов (crt.sh по `%.love.ngs.ru`) знают ровно три имени:
`love.ngs.ru`, `m.love.ngs.ru` и — новое для нас — **`msg.love.ngs.ru`**.

Это **nchan** (push-модуль nginx). Разбор `general.js`:

```
URL:        https://msg.love.ngs.ru<DOMAIN_SUFFIX>/sub/LOVE-<pushStreamMessagesChannelName>
транспорт:  websocket (заголовок Sec-WebSocket-Protocol: ws+meta.nchan),
            при ошибке падает на longpoll; поддержан и eventsource
сообщение:  JSON {text: <URI-encoded>}, внутри {type, microtime, …}
логика:     если с прошлой проверки прошло >240 с — сначала getNewMessagesCount,
            затем подписка; иначе подписка сразу с прошлой отметки
```

Проверено под сессией: на мобильной главной есть `pushStreamMessagesChannelName`
и `lastMessagesCheckTime`, `DOMAIN_SUFFIX` пуст — то есть адрес подписки ровно
`https://msg.love.ngs.ru/sub/LOVE-<канал>`.

> **Имя канала — это ключ.** Кто его знает, тот читает чужие уведомления о
> сообщениях. Обращаться с ним как с кукой: не печатать, не логировать; если
> когда-нибудь окажется в БД — шифровать наравне с сессиями.

**Зачем это talks.** Сегодня поллер каждый такт дёргает `loadBuddiesList` под
кукой человека и тем держит его «в сети» круглосуточно (§5). Подписка на nchan
может снять именно эту цену: ждать события на отдельном хосте, а авторизованный
запрос делать только тогда, когда письмо реально пришло, — то есть светить
человека в те минуты, когда он и так получил бы письмо.

### Проверено 17.08.2026: транспорт и доставка

- **Хост жив, транспорт — WebSocket.** `GET /sub/LOVE-<channel>` с апгрейдом →
  `101 Switching Protocols`, `Sec-WebSocket-Protocol: ws+meta.nchan`,
  `Server: ddos-guard` (нужен RU-IP). Longpoll просто висит без заголовков —
  рабочий транспорт именно WebSocket. Куку хосту слать не нужно: имя канала само
  по себе капабилити, love-сессия в соединении не участвует (в 101 приходят лишь
  куки DDoS-Guard).
- **Имя канала** — на десктопной домашней под сессией, в присваивании
  `pushStreamMessagesChannelName`; вид `messages-<32 hex>`. Это ключ, обращаться
  как с кукой (не печатать, шифровать при хранении).
- **Push доставляет ПОЛНОЕ сообщение, а не сигнал.** Отправили ЛС служебной
  учётке — в канал за секунды пришёл кадр (`apiprobe -nchan`), внутренний JSON:

  ```json
  {"type":"new_message","user_id":<профиль>,"user_passport_id":<паспорт>,
   "user_nick":"…","user_sex":1,"user_age":41,
   "message_text":"…полный текст…","message_id":"<24-hex>",
   "is_express":true,"images":[],"microtime":"…"}
  ```

  То есть отправитель (ник, id, паспорт, пол, возраст), **полный текст**, id
  сообщения и вложения — всё в push. Для доставки уведомления
  `getMessagesHistory` не нужен вовсе.

Минимальный WS-подписчик поверх `crypto/tls` (без зависимостей) — в
`cmd/apiprobe` (`subscribeWS`): апгрейд, чтение немаскированных кадров, разбор
ws+meta. Годится как образец для боевого клиента.

### Решено 17.08.2026: удержание подписки НЕ светит «в сети»

`apiprobe -nchanpresence`. Резерв погас через 30 мин после последнего захода;
затем 5 минут держали WS-подписку, **не обращаясь к love.ngs.ru** — присутствие
резерва глазами владельца на 1-й, 2-й и 4-й минуте оставалось «Была 22:51», ни
разу не «Online».

**Вывод: пассивная push-подписка человека не палит.** Она идёт на отдельный хост
за DDoS-Guard и love-сессию не трогает.

### Что это даёт talks (рекомендация к перестройке)

Сложив три проверенных факта — push несёт полное сообщение, подписка не помечает
прочитанным, подписка не светит в сети, — получаем архитектуру, снимающую ровно
ту цену, из-за которой заведён `talks_scan`:

- держать постоянную **WS-подписку** на канал каждого залогиненного и доставлять
  ЛС в мессенджер **в реальном времени, с текстом и отправителем** — не помечая
  прочитанным, не светя человека в сети;
- нынешний поллер (`loadBuddiesList` каждый такт, который и держит человека
  онлайн круглосуточно, §5) при этом **не нужен для чтения** вовсе;
- к `love.ngs.ru` под кукой ходить только когда человек **сам отвечает** — его
  собственное действие, которое и так сделало бы его видимым;
- имя канала снимается один раз с домашней (это единственный заход под кукой,
  и он засветит человека на ~30 мин) — уместнее всего в момент `/login`, когда
  он и так только что зашёл.

Тогда согласие меняет смысл окончательно и на прочном основании: доставлять
уведомления о ЛС можно **без согласия** (цены нет), а `talks_scan` спрашивать
только под «пометить прочитанным / дочитать историю» — то есть под то
единственное, что видит собеседник.

### Открытые вопросы для боевого клиента

1. **Живучесть имени канала:** ротируется ли, переживает ли перелогин; если
   протухает — как часто нужен заход за новым (каждый такой заход = 30 мин
   «онлайн»).
2. **Переподключение:** WS рвётся — реконнект по тому же каналу
   (без обращения к love.ngs.ru), нужен бэкофф и, вероятно, pong на ping
   (в опыте ping игнорировался — для долгого соединения ответить надо).
3. **Сеть:** msg-хост за DDoS-Guard → нужен RU-IP, как и сам сайт; через
   `telegram_proxy` его гнать НЕ надо (он на стороне сайта, не Telegram).
4. **Пропуски за оффлайн:** переживёт ли подписка простой демона — nchan держит
   буфер сообщений и курсор (`Last-Message-Id`/Etag), но на WS это надо
   проверить; иначе за время простоя ЛС потеряются, и нужен разовый
   `getMessagesHistory` при старте (он пометит прочитанным — это уже под
   согласие).
5. **Пометка прочитанным по желанию:** если человек хочет «дочитывать», это
   `getMessagesHistory`/`readMessages` под его согласие — ровно нынешнее
   поведение, но теперь опциональное, а не обязательное.

## 5.2 Приложение: что дал разбор IPA (17.08.2026)

Скачано без устройства: `ipatool` (ставится `go install github.com/majd/ipatool/v2@latest`),
Apple ID + 2FA, `download -b ru.ngs.LOVE --purchase`. IPA — обычный zip.

Что выяснилось:

- **Приложение живое:** версия 3.31.4 от 20.11.2024 (выпуск 2013-01-15), 97 МБ,
  minimum iOS 10, заметки к версии — починка падений на iOS 15/16. Значит
  мёртвый мобильный RPC-шлюз объясняется скорее аварией, чем закрытием.
- **Нативное UIKit-приложение**, не гибрид: в бандле `.nib` и
  `MainStoryboard_iPhone`, никакого `main.jsbundle` или `www/`.
- **Общий движок на всю группу порталов:** URL-схемы `ngslove`, `ngsnews`,
  `ngsauto`, `ngsjob`, `ngsrealty`, `ngsdo`, `ngs24news`, `ngs55news`; свои
  фреймворки подрядчика `AS*` (`ASNetwork`, `ASMainClasses`, `ASCacheManager`…),
  аналитика AppMetrica, реклама Яндекса.
- **`NSAppTransportSecurity`: `NSAllowsArbitraryLoads = true`, `NSExceptionDomains`
  пуст.** То есть списка доменов в plist нет — надежда на него не оправдалась.

**Адресов в бандле нет вообще.** Поиск по всем 1160 файлам не дал ни одного
вхождения `ngs.ru`: код (и главный бинарь, и фреймворки `AS*`) зашифрован
FairPlay, а все URL — литералы внутри него. Читаемы только строки из подписи.

**Вывод: статикой протокол приложения не достать.** Остаются только варианты с
устройством — любой iPhone/iPad на десять минут с mitmproxy, либо джейлбрейк и
дамп расшифрованного бинаря (`frida-ios-dump`), либо облачный Corellium.
Повторять разбор IPA смысла нет.

Практически же приложение и не нужно: то, ради чего к нему шли, — дешёвый канал
уведомлений — найдено без него (§5.1), и проверяется без устройства.

### 5.2.1 Android-приложение НГС — тупик для знакомств (17.08.2026)

Скачано с RuStore (`ru.ngs.news`, «НГС»). В отличие от iOS — **Capacitor-гибрид**,
весь фронтенд читается: `assets/public/**/*.js` (Vue). Но это **другой бэкенд**:

- хост `mobile.ngs.ru`, API `/gm-api/bff/v1/…` — современный BFF со своей
  авторизацией (`users/login|register|confirm|reset-password`);
- во всём бандле (2625 файлов) **ноль** вхождений `love.ngs.ru`, `passport.ngs`,
  `talks`, `loadBuddies`, `getMessagesHistory`;
- единственный след знакомств — ручка `blocks/get-love`, но это тизер-блок на
  главной новостей, не API знакомств.

Портал новостей уехал на новый стек, знакомства остались на легаси с куками —
общей авторизации между ними нет. Прежняя гипотеза «братья-приложения делят
паспорт» не подтвердилась. **Android-путь для знакомств закрыт.** (Сам по себе
`gm-api/bff` — чистый JSON-API новостей ngs.ru, к lovegw отношения не имеющий.)

## 6. Открытые вопросы

1. ~~Работает ли приложение сегодня~~ — да, версия от 20.11.2024 (§5.2).
   Протокол статикой не достать, нужен телефон; но нужда в нём отпала.
2. ~~Гасит ли `loadBuddiesList` непрочитанное~~ — нет, проверено (§5).
3. Шире ли десктопный каталог под сессией — дескриптор снят гостем.
4. Отдаёт ли `guests` время визита — ждёт выздоровления мобильного шлюза.
5. Оживёт ли мобильный шлюз вместе с сайтом (§3.1). До ответа §4.1 не считать
   ни действующим, ни историческим.

## 7. Инструмент

`go/cmd/apiprobe` — разовый пробник: берёт сессию из боевой БД через
`store.SessionCookies` (куки не печатает), зовёт только читающие методы, меряет
`last_activity` до и после, `-page` снимает страницу под сессией и показывает её
JS-обвязку. В демоне не участвует, в git не добавлен. Удалить, когда §5 закрыт.

## 8. Воспроизведение

Файлы разведки (дескрипторы, `mobNotes.js`, `general.js`) — в скретчпаде сессии,
в репозиторий не копировались. Каталоги пересниматься должны одной командой из §1:
дескриптор самодостаточен. Сайту нужен RU-IP, как и всему остальному.
