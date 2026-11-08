#!/bin/bash
# build.sh <dir>: builds expogen into <dir>. expogen runs custom queries
# through CloudNativePG v1.30.0's own metrics package (QueryRunner,
# mappings, client_golang), so its output is what the exporter serves.
# The package is copied from the module at that tag; its plugin
# collector (internal imports) is cut, and gen.go.txt adds Generate,
# which runs the queries once against plain connections.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh <dir>}
cnpg=v1.30.0
mkdir -p "$out/metrics"
cd "$out"
[ -f go.mod ] || go mod init expogen >/dev/null 2>&1
go mod download -x "github.com/cloudnative-pg/cloudnative-pg@$cnpg" >/dev/null 2>&1 || true
src=$(go env GOMODCACHE)/github.com/cloudnative-pg/cloudnative-pg@$cnpg
[ -d "$src" ] || { go get "github.com/cloudnative-pg/cloudnative-pg@$cnpg"; }
for f in collector.go mappings.go parser.go; do
  install -m 0644 "$src/pkg/management/postgres/metrics/$f" metrics/$f
done
sed -i -e '/^\/\/ PluginCollector is the interface/,$d' \
  -e '/^\t"slices"$/d' -e '/cnpg-i\/pkg\/metrics"$/d' \
  -e '/cloudnative-pg\/api\/v1"$/d' -e '/internal\/cnpi\/plugin\//d' metrics/collector.go
install -m 0644 "$here/gen.go.txt" metrics/gen.go
install -m 0644 "$here/main.go.txt" main.go
# The ConfigMap's data.queries block, unindented.
awk '/^  queries: \|/ { on = 1; next } on && /^    / { print substr($0, 5); next } on && /^$/ { print; next } on { exit }' \
  "$src/config/manager/default-monitoring.yaml" > default-queries.yaml
go get "github.com/cloudnative-pg/cloudnative-pg@$cnpg" >/dev/null
go mod tidy >/dev/null
go build -o expogen .
echo "built $out/expogen"
