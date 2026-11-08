#!/bin/sh
# Import-graph rules of the repository layout, checked on the real
# package graph rather than by convention:
#
#   pkg/**               imports no internal/** (a pkg/ library is usable alone)
#   pkg/evidence         has no module requirements (zero-dependency module)
#   internal/evidence    imports neither internal/console nor internal/operator
#                        (the agent holds the object-store credentials and
#                        shares nothing with the processes that must never
#                        hold them)
#   internal/console     imports no internal/operator, and no internal/evidence
#                        (the console receives conclusions over HTTPS only)
#   internal/console/status (the read model) imports no internal/console/
#                        operations (the write orchestrator): what both need is
#                        in internal/console/opstate
#   internal/operator    imports no internal/console, and no internal/evidence
#   internal/backupresolve imports no other internal/ package: it is the one
#                        piece the operator and the self-resolving agent
#                        share, so it must not become a path between them
set -eu

mod=github.com/fyannk/pgToolBox
edges=$(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./pkg/... ./internal/... 2>/dev/null \
  | awk -v m="$mod/" '{ for (i = 2; i <= NF; i++) if (index($i, m) == 1) { a = $1; b = $i; sub(m, "", a); sub(m, "", b); print a " " b } }')

status=0
violation() { echo "layering: $1 imports $2 ($3)" >&2; status=1; }

echo "$edges" | while read -r from to; do
  [ -n "$from" ] || continue
  case "$from" in
    pkg/*) case "$to" in internal/*) violation "$from" "$to" "pkg/ must not import internal/" ;; esac ;;
    internal/evidence/*) case "$to" in internal/console/*|internal/operator/*) violation "$from" "$to" "the evidence agent is isolated" ;; esac ;;
    internal/console/status|internal/console/status/*) case "$to" in
        internal/operator/*|internal/evidence/*) violation "$from" "$to" "console and operator/agent are isolated" ;;
        internal/console/operations|internal/console/operations/*) violation "$from" "$to" "the read model does not import the write orchestrator" ;;
      esac ;;
    internal/console/*) case "$to" in internal/operator/*|internal/evidence/*) violation "$from" "$to" "console and operator/agent are isolated" ;; esac ;;
    internal/operator/*) case "$to" in internal/console/*|internal/evidence/*) violation "$from" "$to" "operator and console/agent are isolated" ;; esac ;;
    internal/backupresolve|internal/backupresolve/*) case "$to" in internal/backupresolve|internal/backupresolve/*) ;; internal/*) violation "$from" "$to" "the shared resolver imports no other internal/ package" ;; esac ;;
  esac
done > /tmp/layering.$$ 2>&1 || true
if [ -s /tmp/layering.$$ ]; then cat /tmp/layering.$$ >&2; rm -f /tmp/layering.$$; status=1; else rm -f /tmp/layering.$$; fi

if [ "$(cd pkg/evidence && GOWORK=off go list -m all | wc -l)" -ne 1 ]; then
  echo "layering: pkg/evidence must have no module requirements" >&2
  status=1
fi
[ "$status" = 0 ] && echo "layering rules hold"
exit "$status"
