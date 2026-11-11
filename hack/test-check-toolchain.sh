#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Tests of the Dockerfile rules of hack/check-toolchain.sh against modified
# copies (DOCKERFILES): equal digests across files, a FROM per tool, no FROM
# behind an unresolvable ARG.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-toolchain-test.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cp Dockerfile "${work}/Dockerfile"
cp deploy/olm/catalog.Dockerfile "${work}/catalog.Dockerfile"
cp ui/e2e.Dockerfile "${work}/e2e.Dockerfile"
files() { echo "${work}/Dockerfile ${work}/catalog.Dockerfile ${work}/e2e.Dockerfile"; }

run() { DOCKERFILES="$(files)" sh hack/check-toolchain.sh 2>&1; }

# expect_fail PATTERN: the check fails and says so.
expect_fail() {
  local out rc=0
  out="$(run)" || rc=$?
  ((rc != 0)) || fail "expected a failure matching /$1/, got success: ${out}"
  grep -Eq -- "$1" <<<"${out}" || fail "output lacks /$1/: ${out}"
}

run >/dev/null || fail "unmodified copies do not pass: $(run)"

# A different golang digest in the catalog Dockerfile.
sed -i '0,/^FROM .*golang:[0-9.]*@sha256:[0-9a-f]\{64\}/s/\(golang:[0-9.]*@sha256:\)[0-9a-f]\{64\}/\1'"$(printf '0%.0s' {1..64})"'/' "${work}/catalog.Dockerfile"
expect_fail 'golang base images carry different digests'
cp deploy/olm/catalog.Dockerfile "${work}/catalog.Dockerfile"

# A node digest that differs inside the Dockerfile.
sed -i '0,/node:[0-9.]*-bookworm-slim@sha256:[0-9a-f]\{64\}/s/\(node:[0-9.]*-bookworm-slim@sha256:\)[0-9a-f]\{64\}/\1'"$(printf '1%.0s' {1..64})"'/' "${work}/Dockerfile"
expect_fail 'node base images carry different digests'
cp Dockerfile "${work}/Dockerfile"

# No node image at all.
sed -i 's|^FROM \(--platform=[^ ]* \)\{0,1\}node:.*|FROM \1scratch AS ui|' "${work}/Dockerfile"
expect_fail 'no node base image'
cp Dockerfile "${work}/Dockerfile"

# shellcheck disable=SC2016  # the $ are Dockerfile syntax
# A golang FROM behind a build argument with no default.
sed -i 's|^FROM --platform=\$BUILDPLATFORM golang:[^ ]* AS builder|FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder|' "${work}/Dockerfile"
expect_fail 'does not resolve'
cp Dockerfile "${work}/Dockerfile"

# ... but a default declared before the first FROM resolves, and is checked.
sed -i '0,/^FROM /s|^FROM |ARG GO_IMAGE=golang:9.9.9\nFROM |' "${work}/Dockerfile"
# shellcheck disable=SC2016
sed -i 's|^FROM --platform=\$BUILDPLATFORM golang:[^ ]* AS builder|FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder|' "${work}/Dockerfile"
expect_fail 'golang:9.9.9, mise.toml pins'
cp Dockerfile "${work}/Dockerfile"

# Docker keywords are case-insensitive: a lowercase from is still a base image.
printf 'from golang:9.9.9 as lower\n' >> "${work}/Dockerfile"
expect_fail 'golang:9.9.9, mise.toml pins'
cp Dockerfile "${work}/Dockerfile"

# --platform with a space is a flag, not the image; the real builder line
# written that way still passes, and a wrong version behind it is found.
# shellcheck disable=SC2016
sed -i 's|^FROM --platform=\$BUILDPLATFORM golang:|FROM --platform $BUILDPLATFORM golang:|' "${work}/Dockerfile"
grep -q '^FROM --platform \$BUILDPLATFORM golang:' "${work}/Dockerfile" || fail "the space-form fixture was not written"
run >/dev/null || fail "FROM --platform X (space form) is misread: $(run)"
printf 'FROM --platform $BUILDPLATFORM golang:9.9.9 AS spaced\n' >> "${work}/Dockerfile"
expect_fail 'golang:9.9.9, mise.toml pins'

echo "check-toolchain: Dockerfile rules ok"
