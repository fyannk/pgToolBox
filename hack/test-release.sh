#!/usr/bin/env bash
# Copyright 2026 The pgToolBox Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Tests of hack/release.sh with no network and no registry: the CHANGELOG
# "Unreleased" refusal and the release-candidate rules of `check`, the
# anonymous-pull gate (`check-public`, with a curl stub) and the scan outcomes
# (`scan-digests`, with a grype stub): a fixable critical is exit 1, a registry
# error is exit 2 and never reads as a finding.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
root="$(pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/ptb-release-test.XXXXXX")"
trap 'rm -rf "${work}"' EXIT
fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# expect WANT_EXIT GREP_PATTERN -- command...
expect() {
  local want="$1" pattern="$2" rc=0 out
  shift 3
  out="$("$@" 2>&1)" || rc=$?
  ((rc == want)) || fail "$*: exit ${rc}, want ${want}: ${out}"
  grep -Eq -- "${pattern}" <<<"${out}" || fail "$*: output lacks /${pattern}/: ${out}"
}

# ---- check: a scratch repository with one commit, the real script and charts.
repo="${work}/repo"
mkdir -p "${repo}/hack" "${repo}/charts"
cp hack/release.sh "${repo}/hack/"
cp -r charts/pgtoolbox charts/pgtools "${repo}/charts/"
version="$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/pgtoolbox/Chart.yaml)"
changelog() { printf '# Changelog\n\n%s\n\nbody\n\n## [0.0.1] - 2020-01-01\n\nold\n' "$1" > "${repo}/CHANGELOG.md"; }
check() { (cd "${repo}" && ./hack/release.sh check "$@"); }

changelog "## [${version}] - Unreleased"
expect 1 'still says Unreleased' -- check "v${version}"
# The dry run reports an open section instead of refusing it, but a real
# release (REQUIRE_TAG=1) still refuses it.
expect 0 'note: .*still says Unreleased' -- env ALLOW_UNRELEASED=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}"
expect 1 'still says Unreleased' -- env ALLOW_UNRELEASED=1 REQUIRE_TAG=1 bash -c 'cd "$1" && ./hack/release.sh check "$2"' _ "${repo}" "v${version}"
changelog "## [${version}] - 2026-11-09"
expect 0 'ok: semver' -- check "v${version}"
changelog "## [${version}]"
expect 0 'ok: semver' -- check "v${version}"
changelog "## [9.9.9] - 2026-11-09"
expect 1 'no .## \[' -- check "v${version}"
# A release candidate may use the open section of its final version ...
changelog "## [${version}] - Unreleased"
expect 0 'ok: semver' -- check "v${version}-rc.1"
# ... but not an Unreleased section of its own.
changelog "## [${version}-rc.1] - Unreleased"
expect 1 'still says Unreleased' -- check "v${version}-rc.1"
changelog "## [${version}-rc.1] - 2026-11-09"
expect 0 'ok: semver' -- check "v${version}-rc.1"
# Charts that match neither the rc nor its core version are refused.
expect 1 'tag says' -- check "v9.9.9-rc.1"
expect 1 'semantic' -- check "0.1.0"

# ---- check-public and scan-digests: stubs first on PATH.
stubs="${work}/stubs"
mkdir -p "${stubs}"
cat > "${stubs}/curl" <<'STUB'
#!/usr/bin/env bash
# The token endpoint answers; manifests of "private" packages answer 404.
for a in "$@"; do
  case "${a}" in
    *"/token?"*) echo '{"token":"t"}'; exit 0 ;;
    *"/manifests/"*)
      case "${a}" in *private*) echo 404 ;; *) echo 200 ;; esac
      exit 0 ;;
  esac
done
exit 22
STUB
cat > "${stubs}/grype" <<'STUB'
#!/usr/bin/env bash
case "${GRYPE_STUB}" in
  unauthorized) echo "failed to get image: UNAUTHORIZED" >&2; exit 1 ;;
  critical) echo '{"matches":[{"vulnerability":{"id":"CVE-1","severity":"Critical","fix":{"state":"fixed"}},"artifact":{"name":"a","version":"1"}}]}' ;;
  unfixed) echo '{"matches":[{"vulnerability":{"id":"CVE-2","severity":"Critical","fix":{"state":"not-fixed"}},"artifact":{"name":"a","version":"1"}}]}' ;;
  clean) echo '{"matches":[]}' ;;
esac
STUB
chmod +x "${stubs}/curl" "${stubs}/grype"

printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/charts/pgtools@sha256:%064d\n' 1 2 > "${work}/ok.txt"
printf 'ghcr.io/o/pgtoolbox-operator@sha256:%064d\nghcr.io/o/private-thing@sha256:%064d\nghcr.io/o/charts/privatechart@sha256:%064d\n' 1 2 3 > "${work}/mixed.txt"
export PATH="${stubs}:${PATH}"
expect 0 'every digest is publicly pullable' -- ./hack/release.sh check-public "${work}/ok.txt"
expect 1 'private-thing' -- ./hack/release.sh check-public "${work}/mixed.txt"
expect 1 'charts/privatechart' -- ./hack/release.sh check-public "${work}/ok.txt" "${work}/mixed.txt"
expect 1 'Re-run failed jobs' -- ./hack/release.sh check-public "${work}/mixed.txt"

export GITHUB_STEP_SUMMARY="${work}/summary.md"
GRYPE_STUB=clean expect 0 'no fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=unfixed expect 0 'no fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=critical expect 1 'fixable critical' -- ./hack/release.sh scan-digests "${work}/ok.txt"
GRYPE_STUB=unauthorized expect 2 'scan error, not a finding' -- ./hack/release.sh scan-digests "${work}/ok.txt"
# An unauthorized scan must never be reported as a vulnerability.
out="$(GRYPE_STUB=unauthorized ./hack/release.sh scan-digests "${work}/ok.txt" 2>&1 || true)"
! grep -q 'fixable critical vulnerabilit' <<<"${out}" || fail "a registry error was reported as a vulnerability"
# Both platforms are scanned.
out="$(GRYPE_STUB=clean ./hack/release.sh scan-digests "${work}/ok.txt" 2>&1)"
grep -q 'linux/amd64' <<<"${out}" && grep -q 'linux/arm64' <<<"${out}" || fail "scan did not cover both platforms"

# ---- pin-refs, install-manifests and the bundle/catalog pinning, for the
# final version and for a release candidate cut from the same tree (which
# carries the core version): fake digests, the real committed files.
digests="${work}/digests.txt"
: > "${digests}"
for n in operator console evidence-agent bundle catalog; do
  printf 'ghcr.io/fyannk/pgtoolbox-%s@sha256:%064d\n' "${n}" "${#n}" >> "${digests}"
done
tree="$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' charts/pgtoolbox/Chart.yaml)"
for v in "${tree}" "${tree}-rc.1"; do
  d="${work}/pin-${v}"
  rm -rf "${d}"
  mkdir -p "${d}"
  cp -r deploy/olm/bundle "${d}/bundle"
  cp -r deploy/olm/catalog "${d}/catalog"
  while IFS= read -r f; do ./hack/release.sh pin-refs "${f}" "${digests}" "${v}"; done \
    < <(grep -rlE 'ghcr\.io/[^/]+/pgtoolbox-' "${d}/bundle")
  ! grep -rEq 'ghcr\.io/[^/ ]+/pgtoolbox-[a-z-]+:[0-9]' "${d}/bundle" || fail "bundle for ${v} still has a tag reference"
  grep -rq 'pgtoolbox-operator@sha256:' "${d}/bundle" || fail "bundle for ${v} pins no operator digest"
  # The catalog names the bundle, pinned with the digests so far.
  while IFS= read -r f; do ./hack/release.sh pin-refs "${f}" "${digests}" "${v}"; done \
    < <(grep -rlE 'ghcr\.io/[^/]+/pgtoolbox-' "${d}/catalog")
  grep -rq 'pgtoolbox-bundle@sha256:' "${d}/catalog" || fail "catalog for ${v} pins no bundle digest"
  if command -v kubectl >/dev/null; then
    OUT_DIR="${d}/out" ./hack/release.sh install-manifests "${digests}" "${v}" >/dev/null \
      || fail "install-manifests failed for ${v}"
    grep -q 'pgtoolbox-console@sha256:' "${d}/out/install-console.yaml" || fail "install-console.yaml for ${v} is not pinned"
  else
    echo "note: kubectl is not on PATH; install-manifests not exercised for ${v}"
  fi
done
# Negative: a tree on another version pins nothing, and that is an error.
printf 'image: ghcr.io/fyannk/pgtoolbox-operator:9.9.9\n' > "${work}/other.yaml"
expect 1 'still references' -- ./hack/release.sh pin-refs "${work}/other.yaml" "${digests}" "${tree}-rc.1"
printf 'image: docker.io/library/busybox:1\n' > "${work}/none.yaml"
expect 1 'nothing was pinned' -- ./hack/release.sh pin-refs "${work}/none.yaml" "${digests}" "${tree}-rc.1"
# Negative: a reference left on another tag beside a pinned one is an error.
printf 'a: ghcr.io/fyannk/pgtoolbox-operator:%s\nb: ghcr.io/fyannk/pgtoolbox-operator:latest\n' "${tree}" > "${work}/left.yaml"
expect 1 'still references' -- ./hack/release.sh pin-refs "${work}/left.yaml" "${digests}" "${tree}-rc.1"
# The dry run rehearses an rc end to end with placeholder digests.
if command -v helm >/dev/null && command -v jq >/dev/null; then
  OUT_DIR="${work}/dry" DRY_RUN_FAKE_DIGESTS=1 DRY_RUN_TAG="v${tree}-rc.1" ./hack/release.sh dry-run >/dev/null \
    || fail "dry-run of v${tree}-rc.1 failed"
  [[ -f "${work}/dry/pgtoolbox-${tree}-rc.1.tgz" ]] || fail "dry-run did not package the rc chart"
else
  echo "note: helm or jq missing; the rc dry run was not exercised"
fi
# SHA256SUMS covers chart-digests.txt.
mkdir -p "${work}/sums"
printf x > "${work}/sums/digests.txt"
printf y > "${work}/sums/chart-digests.txt"
OUT_DIR="${work}/sums" ./hack/release.sh sums >/dev/null
grep -q ' chart-digests.txt$' "${work}/sums/SHA256SUMS" || fail "SHA256SUMS lacks chart-digests.txt"

echo "ok: hack/release.sh"
cd "${root}"
