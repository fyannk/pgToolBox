#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.
#
# Runs inside the generator image (see Dockerfile), as root. Real PostgreSQL 17
# and pgBackRest 2.60.0, running as the postgres user, write three posix
# repositories under /repo (a container volume):
#
#   plain      repo1 of stanza demo, unencrypted, gzip
#   encrypted  repo2 of stanza demo, repo-cipher-type=aes-256-cbc
#   format6    a stanza created with the opt-in repository format 6 (encrypted)
#   retention  repo1 of a second cluster's stanza demo, two full backups,
#              reduced before (retention-before) and after (retention-after)
#              a real `pgbackrest expire` with repo1-retention-full=1
#
# The history on plain and encrypted: stanza-create; WAL archiving; a full,
# a differential and an incremental backup; a restore of the incremental that
# promotes to timeline 2; an incremental backup on timeline 2.
#
# The script then prints, on stdout, a tar stream of the reduced fixture tree:
# per repository the info files and their .copy, every backup.manifest and its
# .copy, the timeline history files, a listing of every key under archive/
# with its size and modification time in epoch seconds (listing.tsv), and pgBackRest's own `info --output=json`
# (pgbackrest-info.json), which the Go tests use as an oracle. Logs go to
# stderr. The passphrase is a fixed test value, documented in the testdata
# README; it protects nothing.
set -eu

cipher_pass='pgtoolbox-fixture-test-only-passphrase'
work=/tmp/pgbr
pgdata="$work/pgdata"
run="$work/run"
out="$work/out"
bin=/usr/lib/postgresql/17/bin

log() { printf 'pgbackrest-fixtures: %s\n' "$*" >&2; }
as_postgres() { su postgres -s /bin/sh -c "$*" >&2; }
sql() { as_postgres "$bin/psql -X -q -o /dev/null -v ON_ERROR_STOP=1 -h $run -d postgres -c \"$1\""; }
# Every backup carries the annotation Opera's plugin writes, so the join of a
# CNPG Backup to a label can be tested on real files.
backup() { as_postgres "pgbackrest --stanza=demo --repo=$1 --type=$2 --annotation=cnpg-backup-name=fixture-repo$1-$3 backup"; }
switch_wal() { sql "SELECT pg_switch_wal();"; }
write_rows() { sql "INSERT INTO fixture SELECT g, md5(g::text) FROM generate_series(1, $1) g;"; }

mkdir -p "$work" "$run" "$out" /repo /etc/pgbackrest "$work/lock" "$work/spool"
chown -R postgres:postgres "$work" /repo
chmod 0700 "$work"

cat > /etc/pgbackrest/pgbackrest.conf <<CONF
[global]
repo1-path=/repo/plain
repo1-retention-full=9
repo2-path=/repo/encrypted
repo2-retention-full=9
repo2-cipher-type=aes-256-cbc
repo2-cipher-pass=$cipher_pass
compress-type=gz
start-fast=y
log-level-console=warn
log-level-file=off
lock-path=$work/lock
spool-path=$work/spool

[demo]
pg1-path=$pgdata
pg1-socket-path=$run
CONF
cat > /etc/pgbackrest/format6.conf <<CONF
[global]
repo1-path=/repo/format6
repo1-cipher-type=aes-256-cbc
repo1-cipher-pass=$cipher_pass
log-level-console=warn
log-level-file=off
lock-path=$work/lock

[demo]
pg1-path=$pgdata
pg1-socket-path=$run
CONF
chown postgres:postgres /etc/pgbackrest/*.conf
chmod 0600 /etc/pgbackrest/*.conf

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
as_postgres "$bin/pg_ctl -D $pgdata -l $work/postgresql.log -w start"

log "stanza-create, check"
as_postgres "pgbackrest --stanza=demo stanza-create"
as_postgres "pgbackrest --stanza=demo check"
sql "CREATE TABLE fixture (id integer, payload text);"
write_rows 10000
switch_wal

log "full backups"
backup 1 full full
backup 2 full full
write_rows 10000
switch_wal

log "differential backups"
backup 1 diff diff
backup 2 diff diff
write_rows 10000
switch_wal

log "incremental backups"
backup 1 incr incr-tl1
backup 2 incr incr-tl1
# WAL written on timeline 1 after the last backup: the restore below branches
# before it, so timeline 1 keeps segments past the switch point.
write_rows 10000
switch_wal
as_postgres "pgbackrest --stanza=demo check"

log "restore of the incremental, promoted to timeline 2"
as_postgres "$bin/pg_ctl -D $pgdata -w -m fast stop"
as_postgres "pgbackrest --stanza=demo --repo=1 --delta --type=immediate --target-action=promote restore"
as_postgres "$bin/pg_ctl -D $pgdata -l $work/postgresql.log -w start"
tries=0
until [ "$(su postgres -s /bin/sh -c "$bin/psql -X -At -h $run -d postgres -c 'SELECT pg_is_in_recovery()'")" = f ]; do
    tries=$((tries + 1))
    [ "$tries" -lt 120 ] || { log "promotion did not finish"; exit 1; }
    sleep 1
done
write_rows 10000
switch_wal

log "incremental backups on timeline 2"
backup 1 incr incr-tl2
backup 2 incr incr-tl2
write_rows 10000
switch_wal
as_postgres "pgbackrest --stanza=demo check"

log "format 6 stanza"
as_postgres "pgbackrest --config=/etc/pgbackrest/format6.conf --stanza=demo --repo1-format=6 stanza-create"

as_postgres "$bin/pg_ctl -D $pgdata -w -m fast stop"

log "reduce"
reduce() {
    variant=$1
    root=/repo/${2:-$variant}
    target=$out/$variant
    mkdir -p "$target"
    (
        cd "$root"
        find . -type f \( -name archive.info -o -name archive.info.copy -o -name backup.info -o -name backup.info.copy \
            -o -name backup.manifest -o -name backup.manifest.copy -o -name '????????.history' \) -print |
            while read -r file; do
                mkdir -p "$target/$(dirname "$file")"
                cp "$file" "$target/$file"
            done
        if [ -d archive ]; then
            find archive -type f -printf '%p\t%s\t%T@\n' | sed 's/\.[0-9]*$//' | LC_ALL=C sort > "$target/listing.tsv"
        fi
    )
}
reduce plain
reduce encrypted
reduce format6
su postgres -s /bin/sh -c "pgbackrest --stanza=demo --repo=1 --output=json info" > "$out/plain/pgbackrest-info.json"
su postgres -s /bin/sh -c "pgbackrest --stanza=demo --repo=2 --output=json info" > "$out/encrypted/pgbackrest-info.json"

# The retention check: a second cluster archives to its own repository, takes
# two full backups under a retention that keeps them all, and is reduced
# before and after an expire that keeps one full (and the WAL from its
# archive start). The first cluster is stopped, so the socket is free.
log "retention: two full backups, then expire with repo1-retention-full=1"
pgdata="$work/pgdata-retention"
cat > /etc/pgbackrest/retention.conf <<CONF
[global]
repo1-path=/repo/retention
repo1-retention-full=9
compress-type=gz
start-fast=y
log-level-console=warn
log-level-file=off
lock-path=$work/lock
spool-path=$work/spool

[demo]
pg1-path=$pgdata
pg1-socket-path=$run
CONF
chown postgres:postgres /etc/pgbackrest/retention.conf
chmod 0600 /etc/pgbackrest/retention.conf
retention() { as_postgres "pgbackrest --config=/etc/pgbackrest/retention.conf --stanza=demo $*"; }
as_postgres "$bin/initdb --no-instructions --auth=trust -U postgres -D $pgdata"
cat >> "$pgdata/postgresql.conf" <<CONF
listen_addresses = ''
unix_socket_directories = '$run'
wal_level = replica
archive_mode = on
archive_command = 'pgbackrest --config=/etc/pgbackrest/retention.conf --stanza=demo archive-push %p'
fsync = off
CONF
as_postgres "$bin/pg_ctl -D $pgdata -l $work/postgresql-retention.log -w start"
retention stanza-create
retention check
sql "CREATE TABLE fixture (id integer, payload text);"
write_rows 10000
switch_wal
retention --type=full --annotation=cnpg-backup-name=fixture-retention-full-1 backup
write_rows 10000
switch_wal
retention --type=full --annotation=cnpg-backup-name=fixture-retention-full-2 backup
write_rows 10000
switch_wal
retention check
as_postgres "$bin/pg_ctl -D $pgdata -w -m fast stop"
reduce retention-before retention
su postgres -s /bin/sh -c "pgbackrest --config=/etc/pgbackrest/retention.conf --stanza=demo --output=json info" > "$out/retention-before/pgbackrest-info.json"
retention --repo1-retention-full=1 expire
reduce retention-after retention
su postgres -s /bin/sh -c "pgbackrest --config=/etc/pgbackrest/retention.conf --stanza=demo --output=json info" > "$out/retention-after/pgbackrest-info.json"

pgbackrest version > "$out/pgbackrest-version.txt"

tar -C "$out" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - .
