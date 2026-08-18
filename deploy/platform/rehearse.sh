#!/bin/sh
# Учение по восстановлению: поднять дамп в ОТДЕЛЬНУЮ базу и показать наполнение
# обеих рядом.
#
# Бэкап, который ни разу не восстанавливали, — это не бэкап, а надежда: дамп
# может годами писаться, читаться заголовком и не подниматься. Поэтому учение
# отдельным скриптом и с одним аргументом: чтобы его гоняли, а не собирались.
#
# На боевом хосте это безопасно — база называется platform_restore и живой не
# касается, — но час одного ядра оно займёт, и место под вторую копию корпуса
# (около 5,5 ГБ) должно быть свободно.
#
#   ./rehearse.sh /root/backups/pg/platform-20260818-1441.dump

set -u
DUMP="$1"
OUT=/root/backups/restore.done
start=$(date +%s)

docker exec platform-pg sh -c 'dropdb -U "$POSTGRES_USER" --if-exists platform_restore'
docker exec platform-pg sh -c 'createdb -U "$POSTGRES_USER" platform_restore'

docker exec -i platform-pg sh -c \
  'pg_restore -U "$POSTGRES_USER" -d platform_restore --no-owner --no-privileges' \
  <"$DUMP" 2>/root/backups/restore.err
rc=$?

echo "rc=$rc сек=$(( $(date +%s) - start ))" >"$OUT"

# Сверяем не только заметки: сессии и согласия — то, ради чего бэкап и делается,
# а media держит связь строк с байтами на диске.
COUNTS='select (select count(*) from notes), (select count(*) from comments), (select count(*) from users), (select count(*) from consents), (select count(*) from web_sessions), (select count(*) from media)'

printf 'боевая  ' >>"$OUT"
docker exec platform-pg sh -c "psql -qAt -U \"\$POSTGRES_USER\" -d \"\$POSTGRES_DB\" -c '$COUNTS'" >>"$OUT" 2>&1
printf 'копия   ' >>"$OUT"
docker exec platform-pg sh -c "psql -qAt -U \"\$POSTGRES_USER\" -d platform_restore -c '$COUNTS'" >>"$OUT" 2>&1
