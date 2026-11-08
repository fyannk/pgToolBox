#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.
#
# Many-backups mode of the generator image: real PostgreSQL 17 and
# pgBackRest 2.60.0 write one posix repository whose stanza holds N backups
# (default 260): a full, then N-1 incrementals with a differential every
# tenth. Every backup carries annotations that inflate backup.info (about
# 4 KiB per backup) and hold the characters a JSON writer must escape
# (quote, backslash, tab, non-ASCII); one backup carries a 100,000-byte
# annotation, so one line of backup.info is longer than the reader's
# 64 KiB read buffer. It is the proof that a large, long-lived stanza's
# backup.info verifies (internal/evidence/formats/pgbackrest
# manybackups_test.go).
#
# It prints, on stdout, a tar stream of backup.info (gzip-compressed: the
# padding compresses to almost nothing), archive.info and the labels of
# pgBackRest's own `info --output=json` (labels.txt, one per line), the
# oracle. backup.info.copy and archive.info.copy are checked to be
# byte-identical to their main file and are not shipped. Logs go to stderr.
#
# Run: docker run --rm --tmpfs /repo --tmpfs /tmp -e N=260 IMAGE pgbackrest-many-backups | tar -xf -
set -eu

N=${N:-260}
work=/tmp/pgbr
pgdata="$work/pgdata"
run="$work/run"
out="$work/out"
bin=/usr/lib/postgresql/17/bin

log() { printf 'pgbackrest-many-backups: %s\n' "$*" >&2; }
as_postgres() { su postgres -s /bin/sh -c "$*" >&2; }

mkdir -p "$work" "$run" "$out" /repo /etc/pgbackrest "$work/lock" "$work/spool"
chown -R postgres:postgres "$work" /repo
chmod 0700 "$work"

cat > /etc/pgbackrest/pgbackrest.conf <<CONF
[global]
repo1-path=/repo/many
repo1-retention-full=9
compress-type=none
start-fast=y
log-level-console=warn
log-level-file=off
lock-path=$work/lock
spool-path=$work/spool

[demo]
pg1-path=$pgdata
pg1-socket-path=$run
CONF
chown postgres:postgres /etc/pgbackrest/pgbackrest.conf
chmod 0600 /etc/pgbackrest/pgbackrest.conf

log "initdb"
as_postgres "$bin/initdb --no-instructions --auth=trust -U postgres -D $pgdata"
cat >> "$pgdata/postgresql.conf" <<CONF
listen_addresses = ''
unix_socket_directories = '$run'
wal_level = replica
archive_mode = on
archive_command = 'pgbackrest --stanza=demo archive-push %p'
fsync = off
CONF

# The backups run from one script as postgres, so the annotations need no
# second level of shell quoting.
cat > "$work/backups.sh" <<'SCRIPT'
set -eu
bin=/usr/lib/postgresql/17/bin
run=/tmp/pgbr/run
$bin/pg_ctl -D /tmp/pgbr/pgdata -l /tmp/pgbr/postgresql.log -w start >&2
pgbackrest --stanza=demo stanza-create
$bin/psql -X -q -h $run -d postgres -c "CREATE TABLE t (id int)"
pad=$(head -c 3000 /dev/zero | tr '\0' 'x')
big=$(head -c 100000 /dev/zero | tr '\0' 'y')
note='quote " backslash \ slash / accents éèà€ tab	end'
pgbackrest --stanza=demo --type=full --annotation=cnpg-backup-name=many-0 "--annotation=note=$note" backup
i=1
while [ "$i" -lt "$N" ]; do
    $bin/psql -X -q -h $run -d postgres -c "INSERT INTO t VALUES ($i)"
    type=incr
    [ $((i % 10)) -eq 0 ] && type=diff
    extra="pad=$pad"
    [ "$i" -eq 123 ] && extra="big=$big"
    pgbackrest --stanza=demo --type=$type --annotation=cnpg-backup-name=many-$i "--annotation=note=$note #$i" "--annotation=$extra" backup
    i=$((i + 1))
done
pgbackrest --stanza=demo --output=json info > /tmp/pgbr/info.json
$bin/pg_ctl -D /tmp/pgbr/pgdata -w -m fast stop >&2
SCRIPT
chown postgres:postgres "$work/backups.sh"
log "$N backups"
as_postgres "N=$N sh $work/backups.sh"

log "reduce"
backup=/repo/many/backup/demo
archive=/repo/many/archive/demo
cmp "$backup/backup.info" "$backup/backup.info.copy"
cmp "$archive/archive.info" "$archive/archive.info.copy"
mkdir -p "$out/backup/demo" "$out/archive/demo"
gzip -9n < "$backup/backup.info" > "$out/backup/demo/backup.info.gz"
cp "$archive/archive.info" "$out/archive/demo/archive.info"
grep -o '"label":"[^"]*"' "$work/info.json" | sed 's/^"label":"//; s/"$//' > "$out/labels.txt"
pgbackrest version > "$out/pgbackrest-version.txt"
tar -C "$out" -cf - .
