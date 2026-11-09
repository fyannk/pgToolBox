#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/lib/net.sh with no network: retry's attempts and exit code, and
# the sha256 check on a file:// fetch (a mismatch fails and leaves no file).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
# shellcheck source=hack/lib/net.sh
source hack/lib/net.sh

export RETRY_BASE_DELAY=0 FETCH_ATTEMPTS=3
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-net-test.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Fails twice, then succeeds.
count=0
flaky() {
  count=$((count + 1))
  ((count >= 3))
}
retry 5 flaky 2>/dev/null || fail "retry did not succeed on the third attempt"
((count == 3)) || fail "retry ran ${count} attempts, want 3"

# Always fails: bounded, and the last exit code comes back.
count=0
always() {
  count=$((count + 1))
  return 7
}
rc=0
retry 4 always 2>/dev/null || rc=$?
((rc == 7)) || fail "retry returned ${rc}, want 7"
((count == 4)) || fail "retry ran ${count} attempts, want 4"

# Checksums, on a local file served through file://.
printf 'kind: ConfigMap\n' >"${work}/src.yaml"
good="$(sha256sum "${work}/src.yaml" | cut -d' ' -f1)"
fetch_verified "file://${work}/src.yaml" "${work}/ok.yaml" "${good}" || fail "a matching sha256 was refused"
cmp -s "${work}/src.yaml" "${work}/ok.yaml" || fail "the fetched file differs"
if fetch_verified "file://${work}/src.yaml" "${work}/bad.yaml" "$(printf '0%.0s' {1..64})" 2>/dev/null; then
  fail "a sha256 mismatch was accepted"
fi
[[ ! -e "${work}/bad.yaml" ]] || fail "a mismatching download was left behind"
if fetch_url "file://${work}/missing.yaml" "${work}/missing.out" 2>/dev/null; then
  fail "a missing source was accepted"
fi

# Only the pinned defaults are checksummed; an override and a local file are not.
[[ -n "$(pinned_sha256 "$(cnpg_manifest_url "${CNPG_DEFAULT_VERSION}")")" ]] || fail "the CNPG default has no pin"
[[ -n "$(pinned_sha256 "$(cert_manager_manifest_url "${CERT_MANAGER_DEFAULT_VERSION}")")" ]] || fail "the cert-manager default has no pin"
[[ -n "$(pinned_sha256 "$(calico_manifest_url "${CALICO_DEFAULT_VERSION}")")" ]] || fail "the Calico default has no pin"
[[ -z "$(pinned_sha256 "$(cnpg_manifest_url 1.99.0)")" ]] || fail "an overridden version was checksummed"
[[ -z "$(pinned_sha256 "${work}/src.yaml")" ]] || fail "a local file was checksummed"
fetch_manifest "${work}/src.yaml" "${work}/copy.yaml"
cmp -s "${work}/src.yaml" "${work}/copy.yaml" || fail "a local manifest was not copied as it is"

echo "ok: hack/lib/net.sh"
