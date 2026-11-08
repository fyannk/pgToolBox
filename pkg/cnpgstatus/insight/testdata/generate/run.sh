#!/bin/bash
# run.sh <major> <out.prom> [scale]: one exposition of the pack (with
# CloudNativePG's default queries before it) from a throwaway
# postgres:<major> container, removed afterwards. With "scale", the many
# database gets 20,000 tables and 80,400 indexes (an hour or more).
# NOLOCK=1 skips the catalog lock in billing.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
major=${1:?usage: run.sh <major> <out.prom> [scale]}; out=${2:?usage: run.sh <major> <out.prom> [scale]}
pack=$(cd "$here/../../../../.." && pwd)/charts/pgtools/files/insight-pack/queries.yaml
work=$(mktemp -d); c=insight-fixture-$major-$$
trap 'docker rm -f "$c" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
"$here/build.sh" "$work/expogen"
docker run -d --name "$c" -e POSTGRES_HOST_AUTH_METHOD=trust "postgres:$major" \
  -c shared_preload_libraries=pg_stat_statements -c max_prepared_transactions=10 -c wal_level=logical \
  -c compute_query_id=on -c track_io_timing=on >/dev/null
until docker exec "$c" pg_isready -q -U postgres 2>/dev/null; do sleep 1; done
sleep 2
docker exec -i "$c" psql -X -q -U postgres -d postgres < "$here/setup.sql" >/dev/null
if [ "${3:-}" = scale ]; then
  docker exec -i "$c" psql -X -q -U postgres -d postgres < "$here/scale.sql" >/dev/null
else
  docker exec -i "$c" psql -X -q -U postgres -d postgres -c "CREATE DATABASE many" >/dev/null
fi
# The exporter may not connect to many: target_databases "*" skips it.
docker exec -i "$c" psql -X -q -U postgres -d postgres -c "REVOKE CONNECT ON DATABASE many FROM PUBLIC"
"$here/workload.sh" "$c"
"$here/sessions.sh" "$c" 600
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$c")
"$work/expogen/expogen" -host "$ip" -db app -queries "$work/expogen/default-queries.yaml,$pack" > "$out"
"$here/cleanup.sh" "$c"
echo "wrote $out"
