#!/bin/sh
# Tool pin freshness. Dependabot reads manifests — go.mod, package-lock.json,
# the Dockerfile, action refs — and nothing else. A tool version pinned in a
# Makefile variable is invisible to it and rots silently, which is how
# GOVULNCHECK_VERSION came to sit a release behind with nothing reporting it.
#
# This reports the pins Dependabot cannot see. It is deliberately NOT part of
# `make check`: it needs the network, and it would turn CI red the day
# upstream tags a release, which has nothing to do with the change under
# test. It runs on a schedule instead, and opens a pull request.
#
#   ./hack/check-tool-pins.sh          report drift, exit 1 if any
#   ./hack/check-tool-pins.sh --bump   rewrite the Makefile to the latest
set -eu

bump=false
case "${1:-}" in
  --bump) bump=true ;;
  "") ;;
  *) echo "usage: $0 [--bump]" >&2; exit 2 ;;
esac

# Makefile variable, and the module whose tags define its versions. Both are
# resolved through the Go module proxy rather than a forge API: no token, and
# the same source the build itself would use.
#
# Not listed, and why: the Go toolchain is compared against the Dockerfile by
# check-go-version.sh instead, and the Node and envtest pins track an LTS line
# and a published asset set rather than a module's tags. Those stay
# hand-maintained; this file records that the choice was made.
tools='GOVULNCHECK_VERSION golang.org/x/vuln
GOLANGCI_LINT_VERSION github.com/golangci/golangci-lint/v2'

status=0

# Fed by redirect rather than a pipe: a pipe would run the loop in a
# subshell, where `status` dies with it and the first drift would end the
# report instead of continuing to the next pin.
while IFS=' ' read -r var module; do
  [ -n "$var" ] || continue

  current=$(sed -n "s/^$var ?= //p" Makefile)
  if [ -z "$current" ]; then
    echo "no $var in Makefile" >&2
    exit 2
  fi

  # Stable tags only: a release candidate is not something to bump into.
  # No sort: `go list -m -versions` already returns semver order, and
  # `sort -V` is a GNU extension this script would otherwise need on a
  # macOS contributor's machine.
  latest=$(go list -m -versions "$module" 2>/dev/null \
    | tr ' ' '\n' \
    | grep -e '^v[0-9][0-9.]*$' \
    | tail -1)
  if [ -z "$latest" ]; then
    echo "could not resolve versions for $module" >&2
    exit 2
  fi

  if [ "$current" = "$latest" ]; then
    echo "$var $current (current)"
    continue
  fi

  echo "$var $current -> $latest"
  status=1
  if [ "$bump" = true ]; then
    # Not `sed -i`: the in-place flag differs between GNU and BSD, and a
    # substitution that silently matches nothing would leave the Makefile
    # untouched while the caller was told there was drift — which reaches
    # the workflow as a commit with nothing staged. Rewrite through a
    # temporary file and refuse to continue unless the new value is there.
    tmp="Makefile.pins.$$"
    sed "s|^$var ?= $current\$|$var ?= $latest|" Makefile > "$tmp"
    if ! grep -q "^$var ?= $latest\$" "$tmp"; then
      rm -f "$tmp"
      echo "failed to rewrite $var in Makefile" >&2
      exit 2
    fi
    # cat rather than mv: keeps the original file's mode and inode.
    cat "$tmp" > Makefile
    rm -f "$tmp"
  fi
done <<EOF
$tools
EOF

exit "$status"
