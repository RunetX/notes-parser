# Многостадийная сборка lovegw. Бинарник статичный (CGO_ENABLED=0,
# modernc.org/sqlite — чистый Go), поэтому рантайм — distroless/static.
# Таймзоны вшиты в бинарник импортом time/tzdata (в distroless их нет).

# ---- build ----
FROM golang:1.26 AS build
WORKDIR /src
# Кэш модулей отдельным слоем — не тянем зависимости при правках кода.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lovegw ./cmd/lovegw

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="lovegw" \
      org.opencontainers.image.description="Шлюз заметок love.ngs.ru в Telegram"
# БД по умолчанию в томе /data; конфиг монтируется как /config.json.
ENV LOVEGW_DB_PATH=/data/lovegw.db
WORKDIR /data
COPY --from=build /out/lovegw /lovegw
USER nonroot:nonroot
ENTRYPOINT ["/lovegw"]
CMD ["run", "-config", "/config.json"]
