#!/bin/sh
# Бэкап боевого хоста площадки: Postgres, три базы SQLite и хранилище медиа.
#
# Зачем это отдельным скриптом, а не «провайдер снимает снапшоты»: с 18.08.2026
# на площадке есть строки, которых нет БОЛЬШЕ НИГДЕ — заметки и комментарии,
# написанные здесь, а не зеркалом. Зеркало восстановимо из lovegw.db, архив — из
# archive.db, а нативное восстановить неоткуда. Туда же согласия: их потеря это
# потеря основания обрабатывать данные, то есть беда не техническая.
#
# Что скрипт НЕ делает намеренно:
#   * не увозит копии с хоста — offsite это отдельный шаг и отдельное решение
#     (адрес, кто хранит ключ); здесь только «копия существует и читается»;
#   * не трогает конфиги и secrets.env. В них LOVEGW_SECRET_KEY, которым
#     расшифровываются куки сессий в lovegw.db, — положить его рядом с той же
#     базой значит обнулить всё шифрование. Их бэкап делается руками и живёт
#     ОТДЕЛЬНО, см. README.
#
# Всё, что пишется, сперва получает суффикс .part: оборванный дамп не должен
# выглядеть готовым — именно его и возьмут в тот единственный раз, когда он
# понадобится.

set -eu

DIR="${BACKUP_DIR:-/root/backups}"
DEPLOY="${DEPLOY_DIR:-/root/platform}"
PG_CONTAINER="${PG_CONTAINER:-platform-pg}"
KEEP_PG="${KEEP_PG:-7}"
KEEP_SQLITE="${KEEP_SQLITE:-14}"
KEEP_MEDIA="${KEEP_MEDIA:-2}"
# Дамп сжатого корпуса — порядка 1,5 ГБ, и место должно остаться на следующий:
# бэкап, упавший на «нет места», обычно молча ломает и предыдущий.
NEED_FREE_MB="${NEED_FREE_MB:-6000}"

stamp=$(date +%Y%m%d-%H%M)
mkdir -p "$DIR/pg" "$DIR/sqlite" "$DIR/media"

log() { printf '%s %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >>"$DIR/backup.log"; }

# Отказ обязан ДОЙТИ до человека. Бэкап, о поломке которого узнают из лога,
# ломается ровно один раз — молча и навсегда; сообщение в ЛС стоит одной строки.
# Секреты подтягиваем в подоболочке: токенам не место в окружении остального
# прохода.
alert() {
	[ -x "$DEPLOY/lovegw" ] || return 0
	( set -a; . "$DEPLOY/secrets.env"; set +a
	  "$DEPLOY/lovegw" alert -config "$DEPLOY/lovegw.json" "$1" ) >/dev/null 2>&1 || true
}
die() { log "ОШИБКА: $*"; alert "бэкап площадки не собрался: $*"; exit 1; }

# Сторож: копия могла не собраться и вчера, и неделю назад — а узнаётся это
# обычно в тот день, когда она понадобилась. Отдельным режимом, чтобы висеть в
# кроне днём, когда сообщение прочтут, а не ночью вместе с самим бэкапом.
if [ "${1:-}" = "check" ]; then
	stale_h="${STALE_HOURS:-30}"
	if [ ! -f "$DIR/last-success" ]; then
		alert "бэкапа площадки нет вовсе: $DIR/last-success отсутствует"
		exit 1
	fi
	age_h=$(( ( $(date +%s) - $(cat "$DIR/last-success") ) / 3600 ))
	if [ "$age_h" -ge "$stale_h" ]; then
		alert "последний удачный бэкап площадки был $age_h ч назад"
		exit 1
	fi
	echo "последний бэкап $age_h ч назад — свежий"
	exit 0
fi

free_mb=$(df -Pm "$DIR" | awk 'NR==2{print $4}')
[ "$free_mb" -ge "$NEED_FREE_MB" ] || die "свободно ${free_mb} МБ, нужно ${NEED_FREE_MB}"

started=$(date +%s)
log "начали (свободно ${free_mb} МБ)"

# ------------------------------------------------------------------ Postgres
#
# Формат custom, а не plain SQL: он даёт выборочное восстановление и параллель,
# а главное — оглавление, по которому целость файла видна сразу. Сжатие zstd
# (PG16+): на одном ядре zlib тут вдвое дороже при том же размере.
pg="$DIR/pg/platform-$stamp.dump"
docker exec "$PG_CONTAINER" sh -c \
	'pg_dump -Fc --compress=zstd:3 -U "$POSTGRES_USER" "$POSTGRES_DB"' >"$pg.part" ||
	die "pg_dump не отработал"

# Дамп custom-формата начинается с «PGDMP». Проверка дешёвая и ловит самое
# частое: место кончилось на середине, docker exec оборвался, вместо дампа
# оказался текст ошибки.
head -c 5 "$pg.part" | grep -q PGDMP || die "дамп не похож на дамп"
size_mb=$(( $(stat -c%s "$pg.part") / 1024 / 1024 ))
[ "$size_mb" -ge 100 ] || die "дамп подозрительно мал: ${size_mb} МБ"
mv "$pg.part" "$pg"
( cd "$DIR/pg" && sha256sum "$(basename "$pg")" >"$(basename "$pg").sha256" )
log "postgres: ${size_mb} МБ"

# -------------------------------------------------------------------- SQLite
#
# Копировать файл на живой базе нельзя — write-through идёт непрерывно, и копия
# выйдет с разорванной транзакцией. Штатный способ — онлайновый backup API; в
# системе нет sqlite3(1), но есть python3, а модуль sqlite3 тот же самый.
for name in lovegw modwatch accounts; do
	src="$DEPLOY/data/$name.db"
	[ -f "$src" ] || continue
	dst="$DIR/sqlite/$name-$stamp.db"
	python3 - "$src" "$dst.part" <<'PY' || die "sqlite: $name"
import sqlite3, sys
src, dst = sys.argv[1], sys.argv[2]
s = sqlite3.connect("file:%s?mode=ro" % src, uri=True)
d = sqlite3.connect(dst)
with d:
    s.backup(d)
d.close()
s.close()
PY
	mv "$dst.part" "$dst"
	# -12, а не -19: замер 15.08.2026 на такой же базе — 5,9× за 3,6 с, дальше
	# растёт время, а не сжатие.
	zstd -q -12 --rm "$dst"
	( cd "$DIR/sqlite" && sha256sum "$(basename "$dst").zst" >"$(basename "$dst").zst.sha256" )
done
log "sqlite: $(ls -1 "$DIR/sqlite"/*-"$stamp".db.zst 2>/dev/null | wc -l) баз"

# --------------------------------------------------------------------- медиа
#
# Хранилище адресуется содержимым и только растёт, поэтому копий держим две, а
# не семь. Без сжатия: там уже сжатые картинки, и tar по ним — чистая трата CPU.
# Пока НГС жив, часть файлов ещё добирается по ссылкам (platform media), так что
# копия каждый раз полнее прежней.
if [ -d "$DEPLOY/data/media" ]; then
	tar -C "$DEPLOY/data" -cf "$DIR/media/media-$stamp.tar.part" media || die "tar медиа"
	mv "$DIR/media/media-$stamp.tar.part" "$DIR/media/media-$stamp.tar"
	log "медиа: $(( $(stat -c%s "$DIR/media/media-$stamp.tar") / 1024 / 1024 )) МБ"
fi

# ------------------------------------------------------------------- прополка
prune() { # каталог, маска, сколько оставить
	ls -1t "$1"/$2 2>/dev/null | tail -n +"$(( $3 + 1 ))" | while read -r f; do rm -f "$f"; done
}
prune "$DIR/pg" 'platform-*.dump' "$KEEP_PG"
prune "$DIR/pg" 'platform-*.dump.sha256' "$KEEP_PG"
prune "$DIR/sqlite" '*.db.zst' $(( KEEP_SQLITE * 3 ))
prune "$DIR/sqlite" '*.db.zst.sha256' $(( KEEP_SQLITE * 3 ))
prune "$DIR/media" 'media-*.tar' "$KEEP_MEDIA"
# .part от прошлых обрывов не должны копиться молча.
find "$DIR" -name '*.part' -mtime +1 -delete 2>/dev/null || true

date +%s >"$DIR/last-success"
log "готово за $(( $(date +%s) - started )) с, занято $(du -sh "$DIR" | cut -f1)"
