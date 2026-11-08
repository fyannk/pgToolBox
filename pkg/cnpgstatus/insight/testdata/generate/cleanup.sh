#!/bin/bash
# cleanup.sh <container>: ends the sessions and the prepared transaction.
c=$1
for d in app billing; do
  docker exec -i "$c" psql -X -q -U postgres -d "$d" -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$d' AND pid <> pg_backend_pid()" >/dev/null
done
docker exec -i "$c" psql -X -q -U postgres -d app -c "ROLLBACK PREPARED 'fixture-secret-gid'" >/dev/null 2>&1 || true
