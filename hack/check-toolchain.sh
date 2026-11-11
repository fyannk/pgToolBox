#!/bin/sh
# The Go toolchain is stated twice by necessity (mise.toml for people and CI,
# go.mod/go.work for the go command). They must say the same thing, and the
# language floor must stay above the standard-library advisories recorded in
# go.mod.
set -eu

want=$(awk -F'"' '$2 == "go" { print $4; exit }' mise.toml)
[ -n "$want" ] || { echo "mise.toml pins no go" >&2; exit 1; }
status=0
for f in go.mod go.work pkg/evidence/go.mod deploy/olm/catalog/opm/go.mod; do
  tc=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2; exit }' "$f")
  if [ "$tc" != "$want" ]; then
    echo "$f: toolchain go$tc, mise.toml pins go $want" >&2
    status=1
  fi
done
for f in go.mod go.work pkg/evidence/go.mod deploy/olm/catalog/opm/go.mod; do
  floor=$(awk '$1 == "go" { print $2; exit }' "$f")
  case "$floor" in
    1.26.[6-9]|1.26.[1-9][0-9]|1.2[7-9]*|1.[3-9][0-9]*) ;;
    *) echo "$f: go $floor is below the 1.26.6 floor (stdlib advisories, see go.mod)" >&2; status=1 ;;
  esac
done
# The release workflow installs cosign through an action that takes a release
# number, which no tool manager can read from mise.toml: it must say the same.
cosign_want=$(awk -F'"' '$2 == "aqua:sigstore/cosign" { print $4; exit }' mise.toml)
for f in .github/workflows/*.yml; do
  grep -q 'cosign-release:' "$f" || continue
  have=$(sed -n "s/^ *cosign-release: *['\"]\{0,1\}v\{0,1\}\([0-9.]*\).*/\1/p" "$f" | sort -u)
  for v in $have; do
    if [ "$v" != "$cosign_want" ]; then
      echo "$f: cosign-release v$v, mise.toml pins cosign $cosign_want" >&2
      status=1
    fi
  done
done
# The Dockerfiles install pnpm from its release tarballs, by URL and checksum:
# every URL must carry the version mise.toml pins (a bump updates the checksums
# with it, from the release's assets).
pnpm_want=$(awk -F'"' '$2 == "pnpm" { print $4; exit }' mise.toml)
for f in Dockerfile ui/e2e.Dockerfile; do
  pnpm_have=$(sed -n 's|.*/pnpm/pnpm/releases/download/v\([0-9.]*\)/.*|\1|p' "$f" | sort -u)
  for v in $pnpm_have; do
    if [ "$v" != "$pnpm_want" ]; then
      echo "$f: pnpm v$v, mise.toml pins pnpm $pnpm_want" >&2
      status=1
    fi
  done
  grep -q "/pnpm/pnpm/releases/download/v$pnpm_want/" "$f" || {
    echo "$f: no pnpm $pnpm_want release download" >&2
    status=1
  }
done
# Every Go and Node base image in a Dockerfile must be the mise.toml pin: the
# shipped binaries are built by the Go CI, govulncheck and the linters use. The
# docker ecosystem of Dependabot ignores golang and node for that reason; the
# toolchain day bumps mise.toml and the Dockerfiles together (digests included).
node_want=$(awk -F'"' '$2 == "node" { print $4; exit }' mise.toml)
# DOCKERFILES lets the test suite point the check at scratch copies.
dockerfiles=${DOCKERFILES:-$(git ls-files '*Dockerfile' '*.Dockerfile' 2>/dev/null || find . -name '*Dockerfile*' -not -path '*/node_modules/*')}
for f in $dockerfiles; do
  for v in $(sed -n 's/^FROM .*[ /]golang:\([0-9][0-9.]*\)[@ -].*/\1/p; s/^FROM .*[ /]golang:\([0-9][0-9.]*\)$/\1/p' "$f" | sort -u); do
    if [ "$v" != "$want" ]; then
      echo "$f: golang:$v, mise.toml pins go $want" >&2
      status=1
    fi
  done
  for v in $(sed -n 's/^FROM .*[ /]node:\([0-9][0-9.]*\)[@ -].*/\1/p; s/^FROM .*[ /]node:\([0-9][0-9.]*\)$/\1/p' "$f" | sort -u); do
    if [ "$v" != "$node_want" ]; then
      echo "$f: node:$v, mise.toml pins node $node_want" >&2
      status=1
    fi
  done
done
# The Playwright image the UI suite runs in carries the version of
# @playwright/test: a mismatch fails at run time with missing browsers.
pw_want=$(sed -n 's/.*"@playwright\/test": *"[~^]\{0,1\}\([0-9.]*\)".*/\1/p' ui/package.json)
pw_have=$(sed -n 's|^FROM mcr.microsoft.com/playwright:v\([0-9.]*\)-.*|\1|p' ui/e2e.Dockerfile)
if [ "$pw_want" != "$pw_have" ]; then
  echo "ui/e2e.Dockerfile: Playwright $pw_have, ui/package.json pins @playwright/test $pw_want" >&2
  status=1
fi
# The playwright npm package moves with @playwright/test.
pw_pkg=$(sed -n 's/.*"playwright": *"[~^]\{0,1\}\([0-9.]*\)".*/\1/p' ui/package.json)
if [ "$pw_pkg" != "$pw_want" ]; then
  echo "ui/package.json: playwright $pw_pkg, @playwright/test $pw_want" >&2
  status=1
fi
# envtest covers the whole supported Kubernetes range: the Makefile's default
# (the oldest, 1.34) on every change, the rest in ci.yml's nightly matrix.
for minor in 1.34 1.35 1.36; do
  grep -Eq "(ENVTEST_K8S_VERSION_API \?= |k8s: \[.*\")${minor}\." Makefile .github/workflows/ci.yml || {
    echo "envtest does not cover Kubernetes $minor (Makefile default or ci.yml envtest-k8s matrix)" >&2
    status=1
  }
done
[ "$status" = 0 ] && echo "toolchain consistent (go $want, node $node_want, cosign $cosign_want, pnpm $pnpm_want, playwright $pw_want)"
exit "$status"
