#!/bin/bash
# sessions.sh <container> <seconds>: holds a lock tree open in app (a
# prepared transaction with a waiter; billing-batch idle in transaction
# blocking checkout, which blocks migrate) and, unless NOLOCK is set, a
# catalog lock in billing so the pack's queries there fail on
# lock_timeout (a per-database failure).
c=$1; t=$2
q() { docker exec -i "$c" psql -X -q -U postgres "$@"; }
q -d app -c "BEGIN; UPDATE accounts SET balance = 2 WHERE id = 1; PREPARE TRANSACTION 'fixture-secret-gid';"
(echo "UPDATE accounts SET balance = 3 WHERE id = 1;"; sleep "$t") | q -d app >/dev/null 2>&1 &
(echo "SET application_name = 'billing-batch'; BEGIN; LOCK TABLE ledger IN SHARE MODE; SELECT txid_current();"; sleep "$t") | q -d app >/dev/null 2>&1 &
sleep 1
(echo "SET application_name = 'checkout'; INSERT INTO ledger VALUES (1, 'x');"; sleep "$t") | q -d app >/dev/null 2>&1 &
sleep 1
(echo "SET application_name = 'migrate'; BEGIN; LOCK TABLE ledger IN ACCESS EXCLUSIVE MODE;"; sleep "$t") | q -d app >/dev/null 2>&1 &
sleep 2
if [ -z "${NOLOCK:-}" ]; then
  (echo "SET application_name = 'ddl'; BEGIN; LOCK TABLE pg_catalog.pg_constraint IN ACCESS EXCLUSIVE MODE;"; sleep "$t") | q -d billing >/dev/null 2>&1 &
fi
sleep 2
# Wait for three lock waiters in app (and the catalog lock in billing).
for _ in $(seq 60); do
  n=$(docker exec -i "$c" psql -X -At -U postgres -d postgres -c "SELECT (SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = 'app') || ',' || (SELECT count(*) FROM pg_locks WHERE relation = 'pg_catalog.pg_constraint'::regclass AND mode = 'AccessExclusiveLock' AND granted)")
  if [ "$n" = "3,1" ] || { [ -n "${NOLOCK:-}" ] && [ "${n%,*}" = 3 ]; }; then break; fi
  sleep 1
done
echo "sessions ready: $n"
