#!/bin/bash
# workload.sh <container>: an invalid index (built outside a
# transaction) and statements for pg_stat_statements.
c=$1
q() { docker exec -i "$c" psql -X -q -U postgres "$@"; }
q -d app -c "CREATE UNIQUE INDEX CONCURRENTLY broken_a ON broken (a)" 2>/dev/null || true
for i in $(seq 1 30); do echo "SELECT sum(balance) FROM accounts WHERE id < $i*100;"; done | q -d app >/dev/null 2>&1
for i in $(seq 1 20); do echo "SELECT sum(total) FROM invoices WHERE id < $i*10;"; done | q -d billing >/dev/null
