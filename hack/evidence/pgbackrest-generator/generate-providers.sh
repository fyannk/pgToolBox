#!/bin/sh
# Copyright 2026 The pgToolBox Authors.
# Licensed under the Apache License, Version 2.0.
#
# Provider mode of the generator image (hack/evidence/test-pgbackrest-providers.sh
# runs it). Real PostgreSQL 17 and pgBackRest 2.60.0 write straight to object
# storage over TLS, with the CA of the journey (/certs/ca.crt):
#
#   repo1  S3 (MinIO), path-style, repo1-path=/pgbr/orders, unencrypted
#   repo2  Azure (Azurite), path-style, repo2-path=/pgbr/orders, unencrypted
#   repo3  S3 (MinIO), repo3-path=/pgbr/orders-enc, aes-256-cbc
#
# History, the same on every repository: stanza-create, WAL archiving to all
# three, then a full, a differential and an incremental backup per
# repository, each followed by a WAL switch. The bucket and the container
# exist beforehand; pgBackRest creates neither.
#
# Inputs (environment): S3_HOST, S3_PORT, S3_BUCKET, S3_KEY, S3_KEY_SECRET,
# AZURE_HOST, AZURE_PORT, AZURE_CONTAINER, AZURE_ACCOUNT, AZURE_KEY. Every
# credential is a throwaway test value of the journey (the Azure key is
# Azurite's published development key). Logs go to stderr; nothing is
# printed on success but the pgBackRest version.
set -eu

cipher_pass='pgtoolbox-fixture-test-only-passphrase'
work=/tmp/pgbr
pgdata="$work/pgdata"
run="$work/run"
bin=/usr/lib/postgresql/17/bin

log() { printf 'pgbackrest-providers: %s\n' "$*" >&2; }
as_postgres() { su postgres -s /bin/sh -c "$*" >&2; }
sql() { as_postgres "$bin/psql -X -q -o /dev/null -v ON_ERROR_STOP=1 -h $run -d postgres -c \"$1\""; }
backup() { as_postgres "pgbackrest --stanza=demo --repo=$1 --type=$2 --annotation=cnpg-backup-name=provider-repo$1-$2 backup"; }
switch_wal() { sql "SELECT pg_switch_wal();"; }
write_rows() { sql "INSERT INTO fixture SELECT g, md5(g::text) FROM generate_series(1, $1) g;"; }

mkdir -p "$work" "$run" /etc/pgbackrest "$work/lock" "$work/spool"
chown -R postgres:postgres "$work"
chmod 0700 "$work"

cat > /etc/pgbackrest/pgbackrest.conf <<CONF
[global]
repo1-type=s3
repo1-path=/pgbr/orders
repo1-s3-bucket=$S3_BUCKET
repo1-s3-endpoint=$S3_HOST
repo1-s3-region=us-east-1
repo1-s3-key=$S3_KEY
repo1-s3-key-secret=$S3_KEY_SECRET
repo1-s3-uri-style=path
repo1-storage-port=$S3_PORT
repo1-storage-ca-file=/certs/ca.crt
repo1-retention-full=9

repo2-type=azure
repo2-path=/pgbr/orders
repo2-azure-account=$AZURE_ACCOUNT
repo2-azure-key=$AZURE_KEY
repo2-azure-container=$AZURE_CONTAINER
repo2-azure-uri-style=path
repo2-storage-host=$AZURE_HOST
repo2-storage-port=$AZURE_PORT
repo2-storage-ca-file=/certs/ca.crt
repo2-retention-full=9

repo3-type=s3
repo3-path=/pgbr/orders-enc
repo3-s3-bucket=$S3_BUCKET
repo3-s3-endpoint=$S3_HOST
repo3-s3-region=us-east-1
repo3-s3-key=$S3_KEY
repo3-s3-key-secret=$S3_KEY_SECRET
repo3-s3-uri-style=path
repo3-storage-port=$S3_PORT
repo3-storage-ca-file=/certs/ca.crt
repo3-cipher-type=aes-256-cbc
repo3-cipher-pass=$cipher_pass
repo3-retention-full=9

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
as_postgres "$bin/pg_ctl -D $pgdata -l $work/postgresql.log -w start"

log "stanza-create, check"
as_postgres "pgbackrest --stanza=demo stanza-create"
as_postgres "pgbackrest --stanza=demo check"
sql "CREATE TABLE fixture (id integer, payload text);"
write_rows 10000
switch_wal

for type in full diff incr; do
    log "$type backups"
    for repo in 1 2 3; do
        backup "$repo" "$type"
    done
    write_rows 10000
    switch_wal
done
as_postgres "pgbackrest --stanza=demo check"
as_postgres "$bin/pg_ctl -D $pgdata -w -m fast stop"
pgbackrest version
