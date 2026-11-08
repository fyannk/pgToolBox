#!/bin/sh
# Verifies every Go source file starts with the Apache-2.0 boilerplate in
# hack/boilerplate/boilerplate.go.txt (one holder: the pgToolBox Authors, see
# AUTHORS). Generated files are skipped; controller-gen writes the same header
# into them (Makefile, `generate`).
#
# A build constraint may precede the header (test/e2e), and leading whitespace
# is ignored.
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
b="$tmp/boilerplate.go.txt"
sed 's/^[[:space:]]*//' hack/boilerplate/boilerplate.go.txt > "$b"
lines=$(wc -l < "$b")

status=0
for f in $(git ls-files '*.go' ':!:**/zz_generated*.go'); do
  if ! awk 'NR == 1 && /^\/\/go:build/ { skip = 1; next } skip && /^$/ { skip = 0; next } { print }' "$f" \
      | head -n "$lines" | sed 's/^[[:space:]]*//' | diff -q - "$b" > /dev/null 2>&1; then
    echo "missing or wrong boilerplate: $f" >&2
    status=1
  fi
done
exit "$status"
