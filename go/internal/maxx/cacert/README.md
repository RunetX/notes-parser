# Сертификаты Минцифры для platform-api2.max.ru

TLS-цепочка API MAX подписана Russian Trusted Root/Sub CA. Чтобы бинарник
ходил в API из контейнера (distroless/static) или с хоста без установленных
гос-сертификатов, положите сюда официальные PEM **перед сборкой**:

    russian_trusted_root_ca.pem
    russian_trusted_sub_ca.pem

Источник — Госуслуги (https://www.gosuslugi.ru/crt):

    https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt
    https://gu-st.ru/content/lending/russian_trusted_sub_ca_pem.crt

(расширение переименовать в `.pem`; содержимое уже в PEM-формате).
Файлы вшиваются через `go:embed` (см. `../mintsifra.go`); каталог с ними
гитигнорится — сертификаты хоть и публичные, но их подлинность каждый
проверяет сам при скачивании с Госуслуг.

Если PEM не положить, `MintsifraClient()` вернёт nil и клиент MAX будет
доверять системному хранилищу хоста: на российском боксе с установленными
по инструкции Госуслуг сертификатами это работает; `lovegw doctor`
покажет TLS-ошибку, если доверия нет.
